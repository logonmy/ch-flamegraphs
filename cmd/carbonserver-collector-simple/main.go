package main

import (
	"context"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"net"
	"net/http"
	_ "net/http/pprof"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	fgpb "github.com/Civil/ch-flamegraphs/flamegraphpb"
	"github.com/Civil/ch-flamegraphs/helper"
	pb "github.com/go-graphite/carbonzipper/carbonzipperpb3"
	"github.com/lomik/zapwriter"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
	_ "google.golang.org/grpc/balancer/roundrobin"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/resolver/manual"
	"gopkg.in/yaml.v2"
	"io"
)

var defaultLoggerConfig = zapwriter.Config{
	Logger:           "",
	File:             "stdout",
	Level:            "info",
	Encoding:         "json",
	EncodingTime:     "iso8601",
	EncodingDuration: "seconds",
}

var logger *zap.Logger
var errTimeout = fmt.Errorf("timeout exceeded")
var errMaxTries = fmt.Errorf("max maxTries exceeded")
var errUnknown = fmt.Errorf("unknown error")

type carbonserverCollector struct {
	endpoint     string
	server       string
	root         *fgpb.FlameGraphNode
	maxTries     int
	fetchTimeout time.Duration

	client  fgpb.FlamegraphV1Client
	cleanup func()

	httpClient *http.Client
}

func newCarbonserverCollector(hostname string) (*carbonserverCollector, error) {
	// TODO: Implement normal load balancing here with dynamic or semi-dynamic reconfiguration
	// TODO: etcd? consul?
	r, cleanup := manual.GenerateAndRegisterManualResolver()

	var resolvedAddrs []resolver.Address
	for _, addr := range config.SendHosts {
		resolvedAddrs = append(resolvedAddrs, resolver.Address{Addr: addr})
	}

	opts := []grpc.DialOption{
		grpc.WithUserAgent("carbonserver-collector-simple/cluster=" + config.Cluster + "/hostname=" + hostname),
		grpc.WithCompressor(grpc.NewGZIPCompressor()),
		grpc.WithDecompressor(grpc.NewGZIPDecompressor()),
		grpc.WithBalancerName("round_robin"),
		grpc.WithMaxMsgSize(int(config.MaxMessageSize)),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time: 10 * time.Minute,
			Timeout: 20 * time.Second,
			PermitWithoutStream: true,
		}),
	}

	if config.ConnectOptions.Insecure {
		opts = append(opts, grpc.WithInsecure())
	}

	conn, err := grpc.Dial(r.Scheme()+":///server", opts...)
	if err != nil {
		return nil, err
	}

	r.NewAddress(resolvedAddrs)

	fetchTimeout := config.FetchTimeout
	if fetchTimeout == 0 {
		// 70% of all collect time can be spent fetching data (just some random assumption)
		fetchTimeout = time.Duration(float64(config.RerunInterval) * 0.7)
	}

	collector := &carbonserverCollector{
		endpoint:     config.Carbonserver,
		server:       hostname,
		client:       fgpb.NewFlamegraphV1Client(conn),
		cleanup:      cleanup,
		httpClient:   &http.Client{},
		maxTries:     config.MaxTries,
		fetchTimeout: fetchTimeout,
	}

	collector.httpClient.Transport = &http.Transport{
		MaxIdleConnsPerHost: config.MaxIdleConnsPerHost,
		DialContext: (&net.Dialer{
			Timeout:   config.TimeoutConnect,
			KeepAlive: config.KeepAliveInterval,
			DualStack: true,
		}).DialContext,
	}

	return collector, nil
}

func (c *carbonserverCollector) constructTree(root *fgpb.FlameGraphNode, details *pb.MetricDetailsResponse) {
	total := int64(details.TotalSpace)
	occupiedByMetrics := int64(0)
	seen := make(map[string]*fgpb.FlameGraphNode)
	parentMapping := make(map[int64]*fgpb.FlameGraphNode)
	var seenSoFar string
	var seenSoFarPrev string
	seenSoFarBase := "[disk]"

	for metric, data := range details.Metrics {
		occupiedByMetrics += int64(data.Size_)
		seenSoFar = seenSoFarBase
		parts := strings.Split(metric, ".")
		l := len(parts) - 1
		for i, part := range parts {
			if part == "" {
				continue
			}
			seenSoFarPrev = seenSoFar
			seenSoFar = seenSoFar + "." + part
			if n, ok := seen[seenSoFar]; ok {
				n.Count++
				n.Value += int64(data.Size_)
				if n.ModTime < data.ModTime {
					n.ModTime = data.ModTime
				}
				if n.RdTime < data.RdTime {
					n.RdTime = data.RdTime
				}
				if n.ATime < data.ATime {
					n.ATime = data.ATime
				}
			} else {
				var parent *fgpb.FlameGraphNode
				if seenSoFarPrev != seenSoFarBase {
					parent = seen[seenSoFarPrev]
				} else {
					parent = root
				}
				parentMapping[parent.Id] = parent

				v := int64(0)
				if i == l {
					v = int64(data.Size_)
				}

				id := helper.NameToIdInt64(seenSoFar)
				m := &fgpb.FlameGraphNode{
					Id:       id,
					Name:     seenSoFar,
					Value:    v,
					ModTime:  data.ModTime,
					RdTime:   data.RdTime,
					ATime:    data.ATime,
					Total:    total,
					ParentID: parent.Id,
				}
				seen[seenSoFar] = m
				parent.Children = append(parent.Children, m)
				parent.ChildrenIds = append(parent.ChildrenIds, id)
			}
		}
	}

	if occupiedByMetrics+int64(details.FreeSpace) < total {
		occupiedByRest := total - occupiedByMetrics - int64(details.FreeSpace)
		id := helper.NameToIdInt64("[disk].[not-whisper]")
		m := &fgpb.FlameGraphNode{
			Id:       id,
			Name:     "[disk].[not-whisper]",
			Value:    occupiedByRest,
			ModTime:  root.ModTime,
			Total:    total,
			ParentID: root.Id,
		}

		root.ChildrenIds = append(root.ChildrenIds, id)
		root.Children = append(root.Children, m)
	} else {
		logger.Error("occupiedByMetrics > totalSpace-freeSpace",
			zap.Int64("occupied_by_metrics", occupiedByMetrics),
			zap.Uint64("free_space", details.FreeSpace),
			zap.Uint64("total_space", details.TotalSpace),
		)
	}
}

func (c *carbonserverCollector) fetchData(ctx context.Context, handler string) (*pb.MetricDetailsResponse, error) {
	var metricsResponse pb.MetricDetailsResponse
	var response *http.Response

	httpCtx, cancel := context.WithTimeout(ctx, c.fetchTimeout)
	defer cancel()

	for try := 1; try < c.maxTries; try++ {
		select {
		case <-ctx.Done():
			logger.Error("global timout exceeded",
				zap.String("server", c.endpoint),
				zap.String("handler", handler),
				zap.Int("try", try-1),
			)
			return nil, errTimeout
		case <-httpCtx.Done():
			logger.Error("fetch timout exceeded",
				zap.String("server", c.endpoint),
				zap.String("handler", handler),
				zap.Int("try", try-1),
			)
			return nil, errTimeout
		default:
			if try > c.maxTries {
				logger.Error("tries exceeded while trying to fetch data",
					zap.String("server", c.endpoint),
					zap.String("handler", handler),
					zap.Int("try", try-1),
				)
				return nil, errMaxTries
			}
		}

		u, err := url.Parse(c.endpoint + handler)

		req, err := http.NewRequest("GET", u.String(), nil)

		response, err = c.httpClient.Do(req.WithContext(httpCtx))
		if err != nil {
			logger.Error("Error during communication with client",
				zap.String("url", u.String()),
				zap.Int("try", try),
				zap.Error(err),
			)
			time.Sleep(300 * time.Millisecond)
		} else {
			body, err := ioutil.ReadAll(response.Body)
			if err != nil {
				logger.Error("error while reading client's response",
					zap.String("server", c.endpoint),
					zap.String("handler", handler),
					zap.Int("try", try),
					zap.Error(err),
				)
				response.Body.Close()
				time.Sleep(300 * time.Millisecond)
				continue
			}

			err = metricsResponse.Unmarshal(body)
			if err != nil || len(metricsResponse.Metrics) == 0 {
				logger.Error("error while parsing client's response",
					zap.String("server", c.endpoint),
					zap.String("handler", handler),
					zap.Int("try", try),
					zap.Error(err),
				)
				response.Body.Close()
				time.Sleep(300 * time.Millisecond)
				continue
			}

			response.Body.Close()
			break
		}
	}

	return &metricsResponse, nil
}

func (c *carbonserverCollector) getDetails(ctx context.Context) (*pb.MetricDetailsResponse, error) {
	handler := "/metrics/details/?format=protobuf"
	response, err := c.fetchData(ctx, handler)
	if err != nil {
		return nil, err
	}

	return response, nil
}

func (c *carbonserverCollector) sendMetricstats(ctx context.Context, timestamp int64, details *pb.MetricDetailsResponse) error {
	sender, err := c.client.SendMetricsStats(ctx)
	if err != nil {
		return err
	}

	for k, v := range details.Metrics {
		flatStat := &fgpb.FlatMetricInfo{
			Timestamp: timestamp,
			Cluster:   config.Cluster,
			Server:    c.server,
			Path:      k,
			ModTime:   v.ModTime,
			ATime:     v.ATime,
			RdTime:    v.RdTime,
			Count:     1,
			Size_:     v.Size_,
		}
		err = sender.Send(flatStat)
		if err != nil {
			logger.Error("failed to send metricstats",
				zap.Error(err),
				zap.Stack("stack"),
			)
			_, err2 := sender.CloseAndRecv()
			if err2 != nil {
				logger.Error("failed to close stream",
					zap.Error(err2),
					zap.Stack("stack"),
				)
			}
			return fmt.Errorf("failed to send metricstats: %v", err)
		}
	}

	_, err = sender.CloseAndRecv()

	return err
}

func (c *carbonserverCollector) createTree(ctx context.Context, timestamp int64) (err error) {
	logger.Info("fetching results",
		zap.Any("cluster", config.Cluster),
	)

	defer func() {
		if r := recover(); r != nil {
			errRecovered, ok := r.(error)
			if !ok {
				errRecovered = errUnknown
			}
			err = errRecovered
			logger.Error("panic constructing tree",
				zap.String("cluster", config.Cluster),
				zap.Error(err),
				zap.Stack("stack"),
			)
		}
	}()

	var wg sync.WaitGroup

	errChan := make(chan error, 2)
	defer close(errChan)

	details, err := c.getDetails(ctx)
	if err != nil {
		return
	}

	logger.Info("got results",
		zap.String("cluster", config.Cluster),
		zap.Int("metrics", len(details.Metrics)),
	)
	if !config.DryRun {
		go func() {
			wg.Add(1)
			err := c.sendMetricstats(ctx, timestamp, details)
			if err != nil && err != io.EOF {
				logger.Error("error sending metricstats",
					zap.Error(err),
				)
			}
			wg.Done()
		}()
	}

	flameGraphTreeRoot := &fgpb.FlameGraphNode{
		Id:       helper.NameToIdInt64("[disk]"),
		Name:     "[disk]",
		Value:    0,
		Total:    int64(details.TotalSpace),
		ParentID: 0,
	}

	freeSpaceNode := &fgpb.FlameGraphNode{
		Id:       helper.NameToIdInt64("[disk].[free]"),
		Name:     "[disk].[free]",
		Value:    int64(details.FreeSpace),
		Total:    int64(details.TotalSpace),
		ParentID: flameGraphTreeRoot.Id,
	}

	flameGraph := &fgpb.FlameGraph{
		Cluster:   config.Cluster,
		Server:    c.server,
		Timestamp: timestamp,
		Tree:      flameGraphTreeRoot,
	}

	flameGraphTreeRoot.ChildrenIds = append(flameGraphTreeRoot.ChildrenIds, helper.NameToIdInt64("[disk].[free]"))
	flameGraphTreeRoot.Children = append(flameGraphTreeRoot.Children, freeSpaceNode)

	c.constructTree(flameGraphTreeRoot, details)

	flameGraphTreeRoot.Value = int64(details.TotalSpace)

	// Convert to clickhouse format
	if !config.DryRun {
		go func() {
			wg.Add(1)
			err := c.streamFlamegraph(ctx, flameGraph)
			if err != nil && err != io.EOF {
				logger.Error("failed sending flamegraph",
					zap.Error(err),
				)
			}
			wg.Done()
		}()
	} else {
		logger.Info("dry run mode specified",
			zap.Any("output", flameGraphTreeRoot),
		)
	}

	wg.Wait()

	if len(errChan) != 0 {
		return <-errChan
	}

	return nil
}

func (c *carbonserverCollector) streamFlamegraphNode(ctx context.Context, node *fgpb.FlameGraphNode, sender fgpb.FlamegraphV1_SendFlatFlamegraphClient, level int64, timestamp int64) error {
	data := fgpb.FlameGraphFlat{
		Timestamp:   timestamp,
		Cluster:     config.Cluster,
		Server:      c.server,
		Id:          node.Id,
		Name:        node.Name,
		Total:       node.Total,
		Value:       node.Value,
		ParentID:    node.ParentID,
		ChildrenIds: node.ChildrenIds,
		Level:       level,
		ModTime:     node.ModTime,
	}

	err := sender.Send(&data)
	if err != nil {
		return err
	}

	level += 1
	for _, n := range node.Children {
		if n != nil {
			err = c.streamFlamegraphNode(ctx, n, sender, level, timestamp)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (c *carbonserverCollector) streamFlamegraph(ctx context.Context, root *fgpb.FlameGraph) error {
	sender, err := c.client.SendFlatFlamegraph(ctx)
	if err != nil {
		return err
	}

	err = c.streamFlamegraphNode(ctx, root.Tree, sender, int64(helper.BaseLevel), root.Timestamp)

	if err != nil {
		logger.Error("failed to send flamegraphs",
			zap.Error(err),
			zap.Stack("stack"),
		)
	}

	_, err2 := sender.CloseAndRecv()
	if err2 != nil {
		logger.Error("failed to close stream",
			zap.Error(err2),
			zap.Stack("stack"),
		)
		if err == nil {
			err = err2
		}
	}
	return err
}

func (c *carbonserverCollector) ProcessData() {
	defer c.cleanup()
	firstRun := true
	for {
		if firstRun {
			firstRun = false
			t0 := time.Now()
			timeStamp := int64(t0.Unix()) - (t0.Unix() % int64(config.RerunInterval.Seconds()))
			sleepTime := time.Unix(timeStamp+int64(config.RerunInterval.Seconds()+config.ExtraDelay.Seconds()), 0).Sub(time.Now())
			time.Sleep(sleepTime)
			continue
		}

		t0 := time.Now()
		timeStamp := int64(t0.Unix()) - (t0.Unix() % int64(config.RerunInterval.Seconds()))
		nextRun := time.Unix(timeStamp+int64(config.RerunInterval.Seconds()+config.ExtraDelay.Seconds()), 0)

		ctx, cancel := context.WithDeadline(context.Background(), nextRun)
		status := "ok"
		logger.Info("iteration started")

		err := c.createTree(ctx, timeStamp)
		if err != nil {
			status = err.Error()
		}

		spentTime := time.Since(t0)
		sleepTime := nextRun.Sub(time.Now())
		select {
		case <-ctx.Done():
			status = "timeout exceeded"
		default:

		}
		logger.Info("iteration done",
			zap.String("status", status),
			zap.Duration("total_processing_time", spentTime),
			zap.Duration("sleep_time", sleepTime),
			zap.Duration("rerun_interval", config.RerunInterval),
			zap.Duration("extra_delay", config.ExtraDelay.Duration),
			zap.Error(ctx.Err()),
		)
		cancel()
		if sleepTime > 0 {
			time.Sleep(sleepTime)
		}
	}
}

type connectOptions struct {
	Insecure bool `yaml:"insecure"`
}

type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var text string
	err := unmarshal(&text)
	if err != nil {
		return err
	}
	var t time.Duration

	if strings.ToLower(text) == "auto" {
		config.autoDelay = true
		return nil
	}
	config.autoDelay = false
	t, err = time.ParseDuration(text)
	if err != nil {
		return err
	}

	d.Duration = t

	return nil
}

func (d *Duration) MarshalYAML() ([]byte, error) {
	return yaml.Marshal(d.Duration)
}

var config = struct {
	Carbonserver   string         `yaml:"carbonserver"`
	Cluster        string         `yaml:"cluster"`
	FetchTimeout   time.Duration  `yaml:"fetch_timeout"`
	ExtraDelay     Duration       `yaml:"extra_delay"`
	RerunInterval  time.Duration  `yaml:"rerun_interval"`
	DryRun         bool           `yaml:"dry_run"`
	SendHosts      []string       `yaml:"send_hosts"`
	Listen         string         `yaml:"listen"`
	ConnectOptions connectOptions `yaml:"connect_options"`
	MaxTries       int            `yaml:"max_tries"`
	MaxMessageSize uint32         `yaml:"max_message_size"`
	OwnHostname    string         `yaml:"own_hostname"`

	MaxIdleConnsPerHost int
	TimeoutConnect      time.Duration
	KeepAliveInterval   time.Duration

	Logger    []zapwriter.Config `yaml:"logger"`
	autoDelay bool
}{
	Carbonserver:   "http://localhost:8080",
	RerunInterval:  30 * time.Minute,
	DryRun:         true,
	SendHosts:      []string{"127.0.0.1"},
	Listen:         "[::]:8088",
	MaxTries:       3,
	MaxMessageSize: 1.5 * 1024 * 1024 * 1024,
	ConnectOptions: connectOptions{
		Insecure: true,
	},

	MaxIdleConnsPerHost: 10,
	TimeoutConnect:      120 * time.Second,
	KeepAliveInterval:   10 * time.Second,

	Logger: []zapwriter.Config{defaultLoggerConfig},

	autoDelay: true,
}

func validateConfig() {
	switch {
	case config.Cluster == "":
		logger.Fatal("cluster can't be empty")
	case config.Carbonserver == "":
		logger.Fatal("you must specify carbonserver url in your config")
	case len(config.SendHosts) == 0:
		logger.Fatal("no hosts to send data")
	}
	if config.ExtraDelay.Duration >= config.RerunInterval {
		logger.Fatal("extra_delay > rerun_interval")
	}
}

func main() {
	// var flameGraph flameGraphNode
	err := zapwriter.ApplyConfig([]zapwriter.Config{defaultLoggerConfig})
	if err != nil {
		log.Fatal("failed to initialize logger with default configuration")

	}
	logger = zapwriter.Logger("main")

	// TODO: Migrate to viper
	cfgPath := flag.String("config", "config.yaml", "path to the config file")
	flag.Parse()

	configRaw, err := ioutil.ReadFile(*cfgPath)
	if err != nil {
		logger.Fatal("error reading config",
			zap.String("config", *cfgPath),
			zap.Error(err),
		)
	}

	err = yaml.Unmarshal(configRaw, &config)
	if err != nil {
		logger.Fatal("error parsing config file",
			zap.String("config", *cfgPath),
			zap.Error(err),
		)
	}

	validateConfig()
	if config.autoDelay {
		rand.Seed(time.Now().UnixNano())
		r := rand.New(rand.NewSource(time.Now().UnixNano()))
		tmp := int64(r.Float64()*config.RerunInterval.Seconds()*1000) * 1000000
		t := time.Duration(tmp) * time.Nanosecond
		config.ExtraDelay.Duration = t
	}

	err = zapwriter.ApplyConfig(config.Logger)
	if err != nil {
		logger.Fatal("failed to apply config",
			zap.String("config", *cfgPath),
			zap.Any("logger_config", config.Logger),
			zap.Error(err),
		)
	}
	// Reinitialize logger with a new config
	logger = zapwriter.Logger("main")

	if config.OwnHostname == "" {
		config.OwnHostname, err = os.Hostname()
		if err != nil {
			logger.Fatal("failed to get hostname",
				zap.Error(err),
			)
		}
	}

	if config.OwnHostname == "" {
		logger.Fatal("empty hostname",
			zap.Error(fmt.Errorf("something went wrong and os returned empty hostname")),
		)
	}

	collector, err := newCarbonserverCollector(config.OwnHostname)
	if err != nil {
		logger.Fatal("failed to initialize collector",
			zap.Error(err),
		)
	}

	logger.Info("started",
		zap.Any("config", config),
	)

	go collector.ProcessData()

	http.ListenAndServe(config.Listen, nil)
}
