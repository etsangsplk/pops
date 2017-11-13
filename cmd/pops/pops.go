package main

import (
	"expvar"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	lumberjack "gopkg.in/natefinch/lumberjack.v2"

	"github.com/signalfx/pops/cmd/debugServer"

	"github.com/gorilla/mux"
	"github.com/signalfx/com_signalfx_metrics_protobuf"
	"github.com/signalfx/golib/datapoint"
	"github.com/signalfx/golib/datapoint/dpsink"
	"github.com/signalfx/golib/distconf"
	"github.com/signalfx/golib/log"
	"github.com/signalfx/golib/logkey"
	"github.com/signalfx/golib/reportsha"
	"github.com/signalfx/golib/sfxclient"
	"github.com/signalfx/golib/timekeeper"
	"github.com/signalfx/golib/web"
	"github.com/signalfx/metricproxy/protocol/collectd"
	"github.com/signalfx/metricproxy/protocol/signalfx"

	"github.com/signalfx/golib/clientcfg"
	"golang.org/x/net/context"
)

// stats are internal tracking stats about pops's core main server
type stats struct {
	RequestCounter         web.RequestCounter
	BucketRequestCounter   web.BucketRequestCounter
	NotFoundRequestCounter web.RequestCounter
	TotalDecodeErrors      int64
	TotalHealthChecks      int64
}

type popsConfig struct {
	minimalGracefulWaitTime *distconf.Duration
	maxGracefulWaitTime     *distconf.Duration
	gracefulCheckInterval   *distconf.Duration
	silentGracefulTime      *distconf.Duration
	machineID               *distconf.Str
	ingestPort              *distconf.Int
}

// Load the client config values from distconf
func (c *popsConfig) Load(conf *distconf.Distconf) {
	c.minimalGracefulWaitTime = conf.Duration("POPS_GRACEFUL_MIN_WAIT_TIME", 5*time.Second)
	c.maxGracefulWaitTime = conf.Duration("POPS_GRACEFUL_MAX_WAIT_TIME", 25*time.Second)
	c.gracefulCheckInterval = conf.Duration("POPS_GRACEFUL_CHECK_INTERVAL", time.Second)
	c.silentGracefulTime = conf.Duration("POPS_GRACEFUL_SILENT_TIME", time.Second*3)
	c.machineID = conf.Str("SF_SOURCE_NAME", "")
	c.ingestPort = conf.Int("POPS_PORT", 8100)
}

type dataSinkConfig struct {
	DatapointEndpoint *distconf.Str
	EventEndpoint     *distconf.Str
	ShutdownTimeout   *distconf.Duration
	NumWorkers        *distconf.Int
	BufferSize        *distconf.Int
	BatchSize         *distconf.Int
}

// Load the dataSink config values from distconf
func (c *dataSinkConfig) Load(conf *distconf.Distconf) {
	c.DatapointEndpoint = conf.Str("DATA_SINK_DP_ENDPOINT", sfxclient.IngestEndpointV2)
	c.EventEndpoint = conf.Str("DATA_SINK_EVENT_ENDPOINT", sfxclient.EventIngestEndpointV2)
	c.ShutdownTimeout = conf.Duration("DATA_SINK_SHUTDOWN_TIMEOUT", 3*time.Second)
	c.NumWorkers = conf.Int("NUM_DRAINING_THREADS", 50)
	c.BufferSize = conf.Int("CHANEL_SIZE", 1000000)
	c.BatchSize = conf.Int("MAX_DRAIN_SIZE", 5000)
}

// clientConfig is a wrapper for clientcfg.ClientConfig.  It has an alternate Load function
// which bypasses the Load function in clientcfg to watch environment variables for configuration
type clientConfig struct {
	clientConfig clientcfg.ClientConfig
}

// Load loads the specified environment variables into the sfxclientConfig
func (c *clientConfig) Load(conf *distconf.Distconf) {
	// sf.metrics.auth_token
	c.clientConfig.AuthToken = conf.Str("SF_METRICS_AUTH_TOKEN", "")
	// sf.metrics.sourceName
	c.clientConfig.SourceName = conf.Str("SF_SOURCE_NAME", "")
	// sf.metrics.statsendpoint
	c.clientConfig.Endpoint = conf.Str("SF_METRICS_STATSENDPOINT", sfxclient.IngestEndpointV2)
	// sf.metrics.report_interval
	c.clientConfig.ReportingInterval = conf.Duration("SF_METRICS_REPORT_INTERVAL", 5*time.Second)
	c.clientConfig.TimeKeeper = timekeeper.RealTime{}
	c.clientConfig.OsHostname = os.Hostname
}

type decodeErrorTracker struct {
	reader      signalfx.ErrorReader
	TotalErrors *int64
}

func (e *decodeErrorTracker) ServeHTTPC(ctx context.Context, rw http.ResponseWriter, req *http.Request) {
	if err := e.reader.Read(ctx, req); err != nil {
		atomic.AddInt64(e.TotalErrors, 1)
		rw.WriteHeader(http.StatusBadRequest)
		_, _ = rw.Write([]byte(err.Error()))
		return
	}

	rw.Header().Set("Content-Type", "application/json; charset=UTF-8")
	_, _ = rw.Write([]byte(`"OK"`))
}

type libraryConfigs struct {
	clientConfig   clientConfig
	debugConfig    debugServer.Config
	mainConfig     popsConfig
	dataSinkConfig dataSinkConfig
}

type configLoader interface {
	Load(conf *distconf.Distconf)
}

func (l *libraryConfigs) Load(conf *distconf.Distconf) {
	loaders := []configLoader{
		&l.clientConfig,
		&l.debugConfig,
		&l.mainConfig,
		&l.dataSinkConfig,
	}
	for _, l := range loaders {
		l.Load(conf)
	}
}

type scheduledServices struct {
	wg            sync.WaitGroup
	closedService chan struct{}
	ErrorHandler  func(error)
	mu            sync.Mutex
}

func (s *scheduledServices) Close() error {
	close(s.closedService)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.wg.Wait()
	return nil
}

func (s *scheduledServices) checkClosed(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		// Already done with context.  Don't bother with f
		return true
	case <-s.closedService:
		// Already closed scheduler.  Don't bother with f
		return true
	default:
		return false
	}
}

// Add will run f() until either ctx is closed or Close() is called on this service
func (s *scheduledServices) Add(ctx context.Context, f func(context.Context) error) {
	if s.checkClosed(ctx) {
		return
	}
	s.mu.Lock()
	if s.checkClosed(ctx) {
		s.mu.Unlock()
		return
	}
	s.wg.Add(1)
	s.mu.Unlock()
	defer s.wg.Done()
	ctx, cancelFunc := context.WithCancel(ctx)
	errResult := make(chan error)
	defer close(errResult)
	go func() {
		errResult <- f(ctx)
	}()
	select {
	case <-s.closedService:
		cancelFunc()
		<-errResult
	case err := <-errResult:
		s.ErrorHandler(err)
	}
}

// Server is our pops server written in golang
type Server struct {
	ctx                context.Context
	conf               *distconf.Distconf
	stats              stats
	signalChan         chan os.Signal
	closeChan          chan struct{}
	setupDone          chan struct{}
	SetupRetryDelay    time.Duration
	standardHeaders    web.HeadersInRequest
	flagInRemote       web.CtxWithFlag
	ctxLog             log.CtxDimensions
	debugServer        *debugServer.DebugServer
	httpListener       net.Listener
	timeKeeper         timekeeper.TimeKeeper
	sfxclient          *sfxclient.Scheduler
	scheduler          *scheduledServices
	versionMetric      reportsha.SHA1Reporter
	server             *http.Server
	logger             log.Logger
	sfxClientLogger    log.Logger
	configs            libraryConfigs
	dataSink           *sfxclient.AsyncMultiTokenSink
	osStat             func(string) (os.FileInfo, error)
	closeHeader        web.CloseHeader
	SetupRetryAttempts int32
}

func (m *Server) defaultClientErrorHandler(err error) error {
	m.logger.Log(log.Err, err, "Unable to handle error in sfxclient")
	return nil
}

func (m *Server) defaultSchedulerErrorHandler(err error) {
	m.logger.Log(log.Err, err, "Error on scheduled service")
}

func (m *Server) newIncomingCounter(sink dpsink.Sink, name string) dpsink.Sink {
	count := &dpsink.Counter{
		Logger: m.sfxClientLogger,
	}
	endingSink := dpsink.FromChain(sink, dpsink.NextWrap(count))
	m.sfxclient.AddGroupedCallback(name, count)
	dims := m.getDefaultDims(&m.configs.clientConfig.clientConfig)
	dims["protocol"] = name
	dims["reason"] = "incoming_counter"
	m.sfxclient.GroupedDefaultDimensions(name, dims)
	return endingSink
}

func (m *Server) setupJSONV2(r *mux.Router, sink dpsink.Sink) sfxclient.Collector {
	j2 := &signalfx.JSONDecoderV2{Sink: sink, Logger: m.sfxClientLogger}
	m.setupDatapointEndpoint(r, j2, signalfx.SetupJSONV2DatapointPaths)
	m.setupDatapointEndpoint(r, &signalfx.JSONEventDecoderV2{Sink: sink, Logger: m.sfxClientLogger}, signalfx.SetupJSONV2EventPaths)
	return j2
}

func (m *Server) setupProtobufV2(r *mux.Router, sink dpsink.Sink) {
	m.setupDatapointEndpoint(r, &signalfx.ProtobufDecoderV2{Sink: sink, Logger: m.sfxClientLogger}, signalfx.SetupProtobufV2DatapointPaths)
	m.setupDatapointEndpoint(r, &signalfx.ProtobufEventDecoderV2{Sink: sink, Logger: m.sfxClientLogger}, signalfx.SetupProtobufV2EventPaths)
}

func (m *Server) setupCollectd(r *mux.Router, sink dpsink.Sink) {
	m.setupDatapointEndpoint(r, &collectd.JSONDecoder{SendTo: sink, Logger: m.sfxClientLogger}, func(r *mux.Router, handler http.Handler) {
		collectd.SetupCollectdPaths(r, handler, "/v1/collectd")
	})
}

type constTypeGetter com_signalfx_metrics_protobuf.MetricType

func (c constTypeGetter) GetMetricTypeFromMap(metricName string) com_signalfx_metrics_protobuf.MetricType {
	return com_signalfx_metrics_protobuf.MetricType(c)
}

func (m *Server) setupJSONV1(r *mux.Router, sink dpsink.DSink) {
	m.setupDatapointEndpoint(r, &signalfx.JSONDecoderV1{Sink: sink, TypeGetter: constTypeGetter(com_signalfx_metrics_protobuf.MetricType_GAUGE), Logger: m.sfxClientLogger}, signalfx.SetupJSONV1Paths)
}

func (m *Server) setupProtobufV1(r *mux.Router, sink dpsink.DSink) {
	m.setupDatapointEndpoint(r, &signalfx.ProtobufDecoderV1{Sink: sink, TypeGetter: constTypeGetter(com_signalfx_metrics_protobuf.MetricType_GAUGE), Logger: m.sfxClientLogger}, signalfx.SetupProtobufV1Paths)
}

func (m *Server) setupDatapointEndpoint(r *mux.Router, reader signalfx.ErrorReader, handlerSetup func(r *mux.Router, handler http.Handler)) {
	tracker := &decodeErrorTracker{
		reader:      reader,
		TotalErrors: &m.stats.TotalDecodeErrors,
	}
	middleLayers := []web.Constructor{
		web.NextConstructor(m.PutTokenOnContext),
		&m.standardHeaders,
		&m.flagInRemote,
		web.NextConstructor(m.closeHeader.OptionallyAddCloseHeader),
		web.NextConstructor(web.AddRequestTime),
		web.NextHTTP(m.stats.RequestCounter.ServeHTTP),
		web.NextHTTP(m.stats.BucketRequestCounter.ServeHTTP),
	}
	handler := web.NewHandler(m.ctx, tracker).Add(middleLayers...)
	handlerSetup(r, handler)
}

// PutTokenOnContext extracts an access token from the request headers and assigns it to the context
func (m *Server) PutTokenOnContext(ctx context.Context, rw http.ResponseWriter, r *http.Request, next web.ContextHandler) {
	var token string
	if token = r.Header.Get(sfxclient.TokenHeaderName); token != "" {
		next.ServeHTTPC(context.WithValue(ctx, sfxclient.TokenCtxKey, token), rw, r)
	} else if username, password, ok := r.BasicAuth(); ok && (username == "auth" || username == "") {
		token = password
		next.ServeHTTPC(context.WithValue(ctx, sfxclient.TokenCtxKey, token), rw, r)
	} else {
		m.logger.Log(log.Err, "Authentication failed without error (bad auth token)")
		rw.WriteHeader(http.StatusUnauthorized)
		_, _ = rw.Write([]byte("Unauthorized"))
		return
	}
}

func (m *Server) getDefaultDims(conf *clientcfg.ClientConfig) map[string]string {
	defaultDims, err := clientcfg.DefaultDimensions(conf)
	if err != nil {
		m.logger.Log(log.Err, err, "cannot fetch default dimensions")
		defaultDims = map[string]string{"sf_source": "unknown"}
	}
	return defaultDims
}

func (m *Server) setupHealthCheck(r *mux.Router) {
	f := http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if atomic.LoadInt32(&m.closeHeader.SetCloseHeader) != 0 {
			rw.WriteHeader(http.StatusNotFound)
			_, _ = rw.Write([]byte("graceful shutdown"))
			return
		}
		_, _ = rw.Write([]byte("OK"))
		atomic.AddInt64(&m.stats.TotalHealthChecks, 1)
	})
	handler := web.NewHandler(m.ctx, web.FromHTTP(f)).Add(web.NextConstructor(m.closeHeader.OptionallyAddCloseHeader))
	r.Path("/healthz").Handler(handler)
}

// setupDataSink sets up the sink for Pops with a DatapointEndpoint and EventEndpoint
func (m *Server) setupDataSink() (err error) {
	numWorkers := m.configs.dataSinkConfig.NumWorkers.Get()
	m.logger.Log(fmt.Sprintf("dataSink configured with %d workers", numWorkers))
	bufferSize := int(m.configs.dataSinkConfig.BufferSize.Get())
	m.logger.Log(fmt.Sprintf("dataSink configured with %d bufferSize", bufferSize))
	batchSize := int(m.configs.dataSinkConfig.BatchSize.Get())
	m.logger.Log(fmt.Sprintf("dataSink configured with %d batchSize", bufferSize))
	datapointEndpoint := m.configs.dataSinkConfig.DatapointEndpoint.Get()
	m.logger.Log(fmt.Sprintf("dataSink datapoint endpoint configured with: %s", datapointEndpoint))
	eventEndpoint := m.configs.dataSinkConfig.EventEndpoint.Get()
	m.logger.Log(fmt.Sprintf("dataSink event endpoint configured with: %s", eventEndpoint))
	// Setup the sink
	m.dataSink = sfxclient.NewAsyncMultiTokenSink(
		numWorkers,
		bufferSize,
		batchSize,
		datapointEndpoint,
		eventEndpoint,
		"",
		nil,
		nil)
	m.dataSink.ShutdownTimeout = m.configs.dataSinkConfig.ShutdownTimeout.Get()
	m.sfxclient.AddCallback(m.dataSink)
	return
}

func (m *Server) setupHTTPServer() error {
	m.logger.Log("Setting up http server")
	sbPort := m.configs.mainConfig.ingestPort.Get()
	m.flagInRemote.CtxFlagger = &m.ctxLog
	m.standardHeaders.Headers = map[string]string{}
	listenAddr := fmt.Sprintf(":%d", sbPort)

	clientTimeout := time.Second * 60
	handler := mux.NewRouter()

	handler.NotFoundHandler = web.NewHandler(m.ctx, web.FromHTTP(http.NotFoundHandler())).Add(web.NextHTTP(m.stats.NotFoundRequestCounter.ServeHTTP))

	// setup the endpoints for differetnt data types
	dims := m.getDefaultDims(&m.configs.clientConfig.clientConfig)
	m.setupProtobufV2(handler, m.newIncomingCounter(m.dataSink, "sfx_protobuf_v2"))
	m.sfxclient.AddGroupedCallback("JSONV2", m.setupJSONV2(handler, m.newIncomingCounter(m.dataSink, "sfx_json_v2")))
	m.sfxclient.GroupedDefaultDimensions("JSONV2", datapoint.AddMaps(dims, map[string]string{"instance": "pops", "path": "decoding", "protocol": "sfx_json_v2"}))
	m.setupCollectd(handler, m.newIncomingCounter(m.dataSink, "sfx_collectd_v1"))
	m.setupProtobufV1(handler, m.newIncomingCounter(m.dataSink, "sfx_protobuf_v1"))
	m.setupJSONV1(handler, m.newIncomingCounter(m.dataSink, "sfx_json_v1"))

	m.setupHealthCheck(handler)
	m.server = &http.Server{
		Handler:      handler,
		ReadTimeout:  clientTimeout,
		WriteTimeout: clientTimeout,
	}

	setupListener := func(addr string, storeInto *net.Listener) error {
		m.logger.Log(logkey.PublishAddr, addr, "Setting up listener")
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			return err
		}
		*storeInto = listener
		go func() {
			if err := m.server.Serve(listener); err != nil {
				m.logger.Log(err)
			}
		}()
		return nil
	}

	return setupListener(listenAddr, &m.httpListener)
}

type setupFunction func() error

func (m *Server) setupRetry(setups []setupFunction) error {
	m.logger.Log(logkey.Size, len(setups), "setup retry")
outerLoop:
	for setupIndex, setup := range setups {
		var err error
		for i := int32(0); i <= m.SetupRetryAttempts; i++ {
			m.logger.Log(logkey.Index, setupIndex, logkey.RetryAttempt, i, logkey.Name, runtime.FuncForPC(reflect.ValueOf(setup).Pointer()).Name(), "trying setup")
			if err = setup(); err == nil {
				continue outerLoop
			}
			m.logger.Log(log.Err, err, "Setup failed.  Trying again after a sleep")
			m.timeKeeper.Sleep(m.SetupRetryDelay)
		}
		return err
	}
	return nil
}

// Datapoints about basic server stats.  Note many of the datapoints are registered when they are created.
func (m *Server) Datapoints() []*datapoint.Datapoint {
	dps := m.stats.BucketRequestCounter.Datapoints()
	dims := map[string]string{
		"instance": "pops",
	}

	return append(dps,
		sfxclient.CumulativeP("pointforwarder.addDataPoints.count", dims, &m.stats.RequestCounter.TotalConnections),
		sfxclient.CumulativeP("TotalProcessingTimeNs", dims, &m.stats.RequestCounter.TotalProcessingTimeNs),
		sfxclient.Gauge("active_connections", dims, atomic.LoadInt64(&m.stats.RequestCounter.ActiveConnections)),
		sfxclient.CumulativeP("TotalDecodeErrors", datapoint.AddMaps(dims, map[string]string{"result": "dropped_request"}), &m.stats.TotalDecodeErrors),
		sfxclient.CumulativeP("total_health_checks", dims, &m.stats.TotalHealthChecks),
		sfxclient.CumulativeP("HttpNotFound.Count", datapoint.AddMaps(dims, map[string]string{"http_code": "404"}), &m.stats.NotFoundRequestCounter.TotalConnections),
	)
}

// BuildDate is (eventually) filled in during compile time by the build framework
var BuildDate = ""

func (m *Server) setupDebugServer() error {
	var err error
	handler := mux.NewRouter()
	m.debugServer, err = debugServer.NewDebugServer(&m.configs.debugConfig, m, handler)
	if err != nil {
		return err
	}
	m.debugServer.ExpvarHandler.Exported["distconf"] = m.conf.Var()
	m.debugServer.ExpvarHandler.Exported["distinfo"] = m.conf.Info()
	m.debugServer.ExpvarHandler.Exported["goruntime"] = expvar.Func(func() interface{} {
		return runtime.Version()
	})
	m.debugServer.ExpvarHandler.Exported["build_date"] = expvar.Func(func() interface{} {
		return BuildDate
	})
	m.debugServer.ExpvarHandler.Exported["buildinfo"] = m.versionMetric.Var()
	m.debugServer.ExpvarHandler.Exported["datapoints"] = m.sfxclient.Var()
	return nil
}

func (m *Server) setupSelfReportingStats() error {
	m.sfxclient.AddCallback(sfxclient.GoMetricsSource)
	m.sfxclient.AddCallback(m)
	return nil
}

func (m *Server) setupConfig() error {
	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		m.configs.Load(m.conf)
		wg.Done()
	}()
	wg.Wait()
	m.versionMetric.Logger = m.logger
	return nil
}

func (m *Server) setupSfxClient() error {
	m.configs.clientConfig.clientConfig.TimeKeeper = m.timeKeeper

	m.sfxclient.ReportingDelay(m.configs.clientConfig.clientConfig.ReportingInterval.Get())
	f := func(duration *distconf.Duration, oldValue time.Duration) {
		m.sfxclient.ReportingDelay(duration.Get())
	}
	f(m.configs.clientConfig.clientConfig.ReportingInterval, time.Duration(0))
	m.configs.clientConfig.clientConfig.ReportingInterval.Watch(f)
	m.sfxclient.Timer = m.timeKeeper
	m.sfxclient.Sink = clientcfg.WatchSinkChanges(m.sfxclient.Sink, &m.configs.clientConfig.clientConfig, m.logger)
	m.sfxclient.DefaultDimensions(m.getDefaultDims(&m.configs.clientConfig.clientConfig))
	m.versionMetric.RepoURL = "https://github.com/signalfx/pops"
	m.versionMetric.FileName = "/buildInfo.json"
	m.sfxclient.AddCallback(&m.versionMetric)

	ctx, can := context.WithCancel(context.Background())
	go func() {
		<-m.closeChan
		can()
	}()
	go m.scheduler.Add(ctx, m.sfxclient.Schedule)

	return nil
}

func (m *Server) setupConf() error {
	backs := make([]distconf.BackingLoader, 0, 1)
	backs = append(backs, distconf.EnvLoader())
	m.conf = distconf.FromLoaders(backs)
	return nil
}

func (m *Server) setupServer() error {
	m.logger.Log(logkey.Env, strings.Join(os.Environ(), " "), "setting up POPS server")
	setups := []setupFunction{
		m.setupConfig,
		//Note: The above two need to always be first, in that order
		m.setupSfxClient,
		m.setupDataSink, // Note: must come before setupHTTPServer
		m.setupHTTPServer,
		m.setupDebugServer,
		m.setupSelfReportingStats,
	}

	if err := m.setupRetry(setups); err != nil {
		return err
	}

	m.logger.Log("Starting the server")
	return nil
}

func (m *Server) gracefulShutdown() {
	m.logger.Log("Starting graceful shutdown")
	defer m.logger.Log("Graceful shutdown done")
	totalWaitTime := m.timeKeeper.After(m.configs.mainConfig.maxGracefulWaitTime.Get())
	atomic.StoreInt32(&m.closeHeader.SetCloseHeader, 1)
	<-m.timeKeeper.After(m.configs.mainConfig.minimalGracefulWaitTime.Get())
	m.logger.Log("Waiting for connections to drain")
	previousTotalConnections := atomic.LoadInt64(&m.stats.RequestCounter.TotalConnections)
	startingTimeGood := m.timeKeeper.Now()
	for {
		select {
		case <-totalWaitTime:
			m.logger.Log("Connections never drained.  This could be bad ...")
			return
		case <-m.timeKeeper.After(m.configs.mainConfig.gracefulCheckInterval.Get()):
			m.logger.Log("Waking up for graceful shutdown")
			now := m.timeKeeper.Now()
			currentTotalConnections := atomic.LoadInt64(&m.stats.RequestCounter.TotalConnections)
			if currentTotalConnections != previousTotalConnections {
				m.logger.Log(logkey.ConnCount, currentTotalConnections-previousTotalConnections, "Still seeing connections")
				previousTotalConnections = currentTotalConnections
				startingTimeGood = now
				continue
			}
			if now.Sub(startingTimeGood) >= m.configs.mainConfig.silentGracefulTime.Get() {
				m.logger.Log("I've been silent.  Graceful shutdown done")
				return
			}
		}
	}
}

// Close close this server, closing any non nil injected parameters
func (m *Server) Close() error {
	m.logger.Log("Close called")
	defer m.logger.Log("Close done")
	type canClose interface {
		Close()
	}
	type canCloseErr interface {
		Close() error
	}
	checkedClose := func(c canClose) {
		// Why reflect: https://groups.google.com/forum/#!topic/golang-nuts/wnH302gBa4I
		if c != nil && !reflect.ValueOf(c).IsNil() {
			c.Close()
		}
	}
	var err error
	checkedCloseErr := func(c canCloseErr) {
		if c != nil && !reflect.ValueOf(c).IsNil() {
			e := c.Close()
			if e != nil && err == nil {
				err = e
			}
		}
	}
	close(m.closeChan)
	checkedCloseErr(m.debugServer)
	checkedCloseErr(m.httpListener)
	checkedClose(m.conf)
	// must unregister the data sink as a datapoint collector from sfxclient
	m.sfxclient.RemoveCallback(m.dataSink)
	checkedCloseErr(m.dataSink)
	checkedCloseErr(m.scheduler)

	return err
}

func (m *Server) main() {
	m.logger.Log("Setting up server")

	// Keep the instance global so we can close it when done
	err := m.setupServer()
	if err != nil {
		m.logger.Log(log.Err, err, "unable to setup server")
		panic(err)
	}

	if m.setupDone != nil {
		m.logger.Log("Close on setup chan")
		close(m.setupDone)
	}
	m.logger.Log("Blocking on close chan")
	select {
	case <-m.closeChan:
	case <-m.signalChan:
		m.gracefulShutdown()
		_ = m.Close()
	}
	m.logger.Log("Close chan unblocked")
}

var failsafeLogger = log.NewLogfmtLogger(os.Stderr, log.Discard)

// ErrorLogger logs the error to the failsafe logger to stderr
func (m *Server) ErrorLogger(err error) log.Logger {
	failsafeLogger.Log(log.Err, err, "error issuing log")
	return failsafeLogger
}

// NewServer returns a new instance of the pops server
func NewServer() *Server {
	s := &Server{
		SetupRetryAttempts: 10,
		SetupRetryDelay:    time.Second,
		setupDone:          make(chan struct{}),
		closeChan:          make(chan struct{}),
		signalChan:         make(chan os.Signal, 1),
		ctx:                context.Background(),
		timeKeeper:         &timekeeper.RealTime{},
		sfxclient:          sfxclient.NewScheduler(),
		logger:             log.Discard,
		scheduler: &scheduledServices{
			closedService: make(chan struct{}),
		},
		stats: stats{
			BucketRequestCounter: web.BucketRequestCounter{
				Bucket: sfxclient.NewRollingBucket("reqtime.sec", map[string]string{
					"endpoint": "v2datapoint",
				}),
			},
		},
		osStat: os.Stat,
	}
	s.scheduler.ErrorHandler = s.defaultSchedulerErrorHandler
	s.sfxclient.ErrorHandler = s.defaultClientErrorHandler
	return s
}

func getLogger(conf *distconf.Distconf) (logOut io.Writer) {
	if logDir := conf.Str("LOG_DIR", "").Get(); logDir != "" {
		filename := filepath.Join(logDir, "pops.log.json")
		logOut = &lumberjack.Logger{
			Filename:   filename,
			MaxSize:    100,
			MaxBackups: 3,
		}
	} else {
		logOut = os.Stderr
	}
	return
}

// MainServerInstance is the server instance populated by calls to main
var MainServerInstance = NewServer()

func main() {
	// Assume you have multiple pops servers running at once on this golang process.  Only put
	// things here that should happen once for them all.
	runtime.GOMAXPROCS(runtime.NumCPU())
	signal.Notify(MainServerInstance.signalChan, syscall.SIGTERM)
	signal.Notify(MainServerInstance.signalChan, syscall.SIGINT)
	_ = MainServerInstance.setupConf()
	MainServerInstance.logger = log.NewContext(log.NewJSONLogger(getLogger(MainServerInstance.conf), MainServerInstance)).With(logkey.Time, log.DefaultTimestamp, logkey.Caller, log.DefaultCaller)
	MainServerInstance.sfxClientLogger = log.NewOnePerSecond(MainServerInstance.logger)
	MainServerInstance.main()
}