package up

import (
	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"

	"github.com/dio/transit/down"
)

// MetricID is an opaque handle to an Envoy metric defined at config time via ConfigHandle.
type MetricID uint64

// LogLevel controls Envoy log severity for Writer.Log.
type LogLevel uint32

// ConfigHandle is passed to config callbacks at filter config creation time.
// Use it to define metrics once — before any requests arrive.
type ConfigHandle interface {
	// DefineCounter defines an Envoy counter metric. tagKeys are optional dimension names.
	DefineCounter(name string, tagKeys ...string) (MetricID, error)
	// DefineGauge defines an Envoy gauge metric. tagKeys are optional dimension names.
	DefineGauge(name string, tagKeys ...string) (MetricID, error)
	// DefineHistogram defines an Envoy histogram metric. tagKeys are optional dimension names.
	DefineHistogram(name string, tagKeys ...string) (MetricID, error)
}

// ConfigFunc is called once at filter config creation time on the main thread.
// Use it to define metrics via ConfigHandle.DefineCounter, etc.
type ConfigFunc func(h ConfigHandle) error

// HandlerFunc is called on every request.
type HandlerFunc func(w *Writer, r *Request)

// OnStreamCompleteFunc is called exactly once per stream after Envoy
// terminates it, regardless of how the stream ended (normal end-of-stream,
// reset, idle/request timeout, local reply from another filter). Use it
// for cleanup that must run even when the request/response handlers did
// not — e.g. removing entries from process-wide registries.
//
// ctx is the per-stream context slot (same value Request.Context and
// BodyChunk.Context point at). Mutations are no-ops at this point: the
// stream is dead and any queued header/filter-state changes will not be
// applied. Do not call Writer methods from here.
type OnStreamCompleteFunc func(ctx *any)

// FinalizedInfo holds finalized stream fields delivered to an
// [OnStreamFinalizedFunc] after Envoy completes the stream. The values
// mirror what an [AccessLoggerHandle] would expose at
// [AccessLogTypeDownstreamEnd]: durations, byte counts, response flags,
// upstream attempts, trace ids, and a local-reply body if one was sent.
//
// All durations are in nanoseconds; -1 in a duration means the timing is
// unavailable. ResponseFlags is the raw bitmask — use [ResponseFlagsString]
// to render the human-readable form.
type FinalizedInfo struct {
	Timing                      TimingInfo
	Bytes                       BytesInfo
	ResponseCode                uint32
	ResponseCodeDetails         string
	ResponseFlags               uint64
	UpstreamFailure             string
	UpstreamLocalAddress        string
	UpstreamAddress             string
	RequestProtocol             string
	UpstreamPoolReadyDurationNs int64
	UpstreamRequestAttempts     uint32
	TraceID                     string
	SpanID                      string
	TraceSampled                bool
	LocalReplyBody              string
}

// OnStreamFinalizedFunc fires on the worker thread after Envoy finalizes
// the stream, before the per-stream context is released. Like
// [OnStreamCompleteFunc] it carries no Writer and mutations are not
// possible — the stream is dead. The strict superset over OnStreamComplete
// is the finalized stream fields in [FinalizedInfo], which Envoy only
// exposes through the access-logger path.
//
// Delivery: when a filter is registered with [WithOnStreamFinalized] the
// SDK auto-registers an internal access logger under the same name and
// correlates filter ↔ logger via the request id (x-request-id). The
// listener Envoy YAML must include an access_log entry pointing at this
// dynamic module with logger_name equal to the filter name (see
// examples/request-ui/envoy.yaml). If the request id is empty or the
// access-logger entry is missing from the YAML, the callback will not
// fire — Envoy delivers finalized fields only through that path.
type OnStreamFinalizedFunc func(ctx *any, info FinalizedInfo)

// FilterOption configures filter registration. Pass options to [Register].
type FilterOption func(*configFactory)

// WithConfig attaches a config callback invoked once on the main thread when
// Envoy loads the filter config. Use it to define metrics via ConfigHandle.
func WithConfig(fn ConfigFunc) FilterOption {
	return func(cf *configFactory) { cf.configFn = fn }
}

// WithResponse attaches a response observer.
func WithResponse(r ResponseHandlerFunc) FilterOption {
	return func(cf *configFactory) { cf.responseHandler = r }
}

// WithStreamingBody attaches a request body handler in streaming mode: the
// handler is called once per chunk as data arrives. For bodyless requests
// (GET etc.) the handler is called once with Data: nil. Mutually exclusive
// with [WithMutableBody].
func WithStreamingBody(rb RequestBodyHandlerFunc) FilterOption {
	return func(cf *configFactory) {
		cf.requestBodyHandler = rb
		cf.bufferBody = false
	}
}

// WithMutableBody enables buffered body handling. If rb is non-nil, it is
// called once with the full accumulated request body; pass nil to buffer the
// response body only (useful when WithResponse needs the full body but the
// request body is not of interest). Use Writer.SetRequestBody /
// SetResponseBody to replace body content. Mutually exclusive with
// [WithStreamingBody].
func WithMutableBody(rb RequestBodyHandlerFunc) FilterOption {
	return func(cf *configFactory) {
		cf.requestBodyHandler = rb
		cf.bufferBody = true
	}
}

// WithGroup attaches a [Group] of background goroutines. The group is started
// when Envoy loads the filter config and stopped (via [Group.Stop]) when
// Envoy destroys the filter factory. The handler and the goroutines in g
// share state via closure — no package-level variables needed.
//
// Note: if any goroutine in the group exits for any reason — including a
// normal return — the entire group is stopped via Group.Stop. Goroutines
// that must survive transient errors should loop internally or use [RunRetry].
//
// For background work in cluster extensions (which have no filter factory),
// use [ClusterGroup] with [ClusterGroup.Start] called from
// [Cluster.ServerInitialized] instead.
func WithGroup(g *Group) FilterOption {
	return func(cf *configFactory) { cf.group = g }
}

// WithOnStreamComplete attaches a stream-termination callback to the
// filter. See [OnStreamCompleteFunc] for semantics.
func WithOnStreamComplete(fn OnStreamCompleteFunc) FilterOption {
	return func(cf *configFactory) { cf.onStreamComplete = fn }
}

// WithOnStreamFinalized attaches a callback that fires after Envoy
// finalizes the stream and delivers it through the access-logger path.
// See [OnStreamFinalizedFunc] for delivery semantics and the YAML
// requirements.
//
// Coexists with [WithOnStreamComplete]: cleanup-only consumers should
// keep using OnStreamComplete; OnStreamFinalized is the right hook when
// the callback needs finalized durations, byte counts, or response flags.
func WithOnStreamFinalized(fn OnStreamFinalizedFunc) FilterOption {
	return func(cf *configFactory) { cf.onStreamFinalized = fn }
}

// Middleware wraps a HandlerFunc, enabling before/after logic around a handler.
type Middleware func(next HandlerFunc) HandlerFunc

// Chain wraps h with the given middleware in left-to-right order: the first
// middleware in the list is outermost (runs first).
func Chain(h HandlerFunc, mw ...Middleware) HandlerFunc {
	for i := len(mw) - 1; i >= 0; i-- {
		h = mw[i](h)
	}
	return h
}

// LogLevel aliases.
const (
	LogTrace    LogLevel = LogLevel(shared.LogLevelTrace)
	LogDebug    LogLevel = LogLevel(shared.LogLevelDebug)
	LogInfo     LogLevel = LogLevel(shared.LogLevelInfo)
	LogWarn     LogLevel = LogLevel(shared.LogLevelWarn)
	LogError    LogLevel = LogLevel(shared.LogLevelError)
	LogCritical LogLevel = LogLevel(shared.LogLevelCritical)
)

// MetadataSource identifies which metadata store to read from.
type MetadataSource uint32

const (
	MetadataSourceDynamic      MetadataSource = MetadataSource(shared.MetadataSourceTypeDynamic)
	MetadataSourceRoute        MetadataSource = MetadataSource(shared.MetadataSourceTypeRoute)
	MetadataSourceCluster      MetadataSource = MetadataSource(shared.MetadataSourceTypeCluster)
	MetadataSourceHost         MetadataSource = MetadataSource(shared.MetadataSourceTypeHost)
	MetadataSourceHostLocality MetadataSource = MetadataSource(shared.MetadataSourceTypeHostLocality)
)

// registry is a duplicate-name sentinel for up.Register.
// The canonical runtime registry lives in down; this catches name collisions
// at init() time with a clear "up: filter already registered" message.
var registry = map[string]HandlerFunc{}

// Register registers a named HTTP filter handler and wires it into the Envoy
// SDK. Must be called from an init() function. Panics on duplicate names.
//
// Optional features are configured via FilterOptions: WithConfig, WithResponse,
// WithStreamingBody, WithMutableBody, WithGroup, WithOnStreamComplete.
func Register(name string, h HandlerFunc, opts ...FilterOption) {
	if _, ok := registry[name]; ok {
		panic("up: filter already registered: " + name)
	}
	registry[name] = h
	cf := &configFactory{name: name, handler: h}
	applyFilterOptions(cf, opts)
	down.RegisterHttpFilter(name, cf)
	if cf.onStreamFinalized != nil {
		registerStreamFinalizedAccessLogger(name)
	}
}

func applyFilterOptions(cf *configFactory, opts []FilterOption) {
	for _, o := range opts {
		if o != nil {
			o(cf)
		}
	}
}

// RegisterAccessLogger registers a named access logger factory. Must be called
// from an init() function. Panics on duplicate names.
func RegisterAccessLogger(name string, f AccessLoggerConfigFactory) {
	registerAccessLogger(name, f)
}
