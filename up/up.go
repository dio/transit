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

// FilterOption configures filter registration. Apply with one of the
// RegisterWith* functions.
type FilterOption func(*configFactory)

// WithOnStreamComplete attaches a stream-termination callback to the
// filter. See [OnStreamCompleteFunc] for semantics.
func WithOnStreamComplete(fn OnStreamCompleteFunc) FilterOption {
	return func(cf *configFactory) { cf.onStreamComplete = fn }
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

// registry is a duplicate-name sentinel for up.Register variants.
// The canonical runtime registry lives in down; this catches name collisions
// at init() time with a clear "up: filter already registered" message.
var registry = map[string]HandlerFunc{}

// Register registers a named HTTP filter handler and wires it into the Envoy
// SDK. Must be called from an init() function. Panics on duplicate names.
func Register(name string, h HandlerFunc, opts ...FilterOption) {
	if _, ok := registry[name]; ok {
		panic("up: filter already registered: " + name)
	}
	registry[name] = h
	cf := &configFactory{name: name, handler: h}
	applyFilterOptions(cf, opts)
	down.RegisterHttpFilter(name, cf)
}

// RegisterWithGroup registers a named HTTP filter with a [Group] of background
// goroutines. The group is started when Envoy loads the filter config and stopped
// (via [Group.Stop]) when Envoy destroys the filter factory. The handler and the
// goroutines in g share state via closure — no package-level variables needed.
// Must be called from an init() function. Panics on duplicate names.
//
// Note: if any goroutine in the group exits for any reason — including a normal
// return — the entire group is stopped via Group.Stop. Goroutines that must
// survive transient errors should loop internally or use [RunRetry].
//
// For background work in cluster extensions (which have no filter factory),
// use [ClusterGroup] with [ClusterGroup.Start] called from
// [Cluster.ServerInitialized] instead.
func RegisterWithGroup(name string, g *Group, h HandlerFunc, opts ...FilterOption) {
	if _, ok := registry[name]; ok {
		panic("up: filter already registered: " + name)
	}
	registry[name] = h
	cf := &configFactory{name: name, handler: h, group: g}
	applyFilterOptions(cf, opts)
	down.RegisterHttpFilter(name, cf)
}

// RegisterWithResponse registers a named HTTP filter with both a request and a
// response handler. Must be called from an init() function. Panics on duplicate names.
func RegisterWithResponse(name string, h HandlerFunc, r ResponseHandlerFunc, opts ...FilterOption) {
	if _, ok := registry[name]; ok {
		panic("up: filter already registered: " + name)
	}
	registry[name] = h
	cf := &configFactory{name: name, handler: h, responseHandler: r}
	applyFilterOptions(cf, opts)
	down.RegisterHttpFilter(name, cf)
}

// RegisterWithBody registers a named HTTP filter with request body handling in
// streaming mode: the body handler is called once per chunk as data arrives.
// For bodyless requests (GET etc.) the handler is called once with Data: nil.
func RegisterWithBody(name string, h HandlerFunc, rb RequestBodyHandlerFunc, r ResponseHandlerFunc, opts ...FilterOption) {
	if _, ok := registry[name]; ok {
		panic("up: filter already registered: " + name)
	}
	registry[name] = h
	cf := &configFactory{
		name:               name,
		handler:            h,
		responseHandler:    r,
		requestBodyHandler: rb,
	}
	applyFilterOptions(cf, opts)
	down.RegisterHttpFilter(name, cf)
}

// RegisterWithMutableBody registers a named HTTP filter with buffered body
// handling: the body handler is called once with the full accumulated body.
// Use Writer.SetRequestBody / SetResponseBody to replace body content.
func RegisterWithMutableBody(name string, h HandlerFunc, rb RequestBodyHandlerFunc, r ResponseHandlerFunc, opts ...FilterOption) {
	if _, ok := registry[name]; ok {
		panic("up: filter already registered: " + name)
	}
	registry[name] = h
	cf := &configFactory{
		name:               name,
		handler:            h,
		responseHandler:    r,
		requestBodyHandler: rb,
		bufferBody:         true,
	}
	applyFilterOptions(cf, opts)
	down.RegisterHttpFilter(name, cf)
}

// RegisterWithConfig registers a named HTTP filter with a config callback, a
// request handler, and a response observer. The config callback runs once on the
// main thread when Envoy loads the filter config — use it to define metrics via
// ConfigHandle.DefineCounter. Must be called from an init() function.
// Panics on duplicate names.
func RegisterWithConfig(name string, cfg ConfigFunc, h HandlerFunc, r ResponseHandlerFunc, opts ...FilterOption) {
	if _, ok := registry[name]; ok {
		panic("up: filter already registered: " + name)
	}
	registry[name] = h
	cf := &configFactory{
		name:            name,
		configFn:        cfg,
		handler:         h,
		responseHandler: r,
	}
	applyFilterOptions(cf, opts)
	down.RegisterHttpFilter(name, cf)
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
