package up

import (
	"github.com/dio/transit/down"
	_ "github.com/dio/transit/down/abi_impl"
	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"
)

// HandlerFunc is called on every request.
type HandlerFunc func(w *Writer, r *Request)

// LogLevel aliases.
const (
	LogTrace    = shared.LogLevelTrace
	LogDebug    = shared.LogLevelDebug
	LogInfo     = shared.LogLevelInfo
	LogWarn     = shared.LogLevelWarn
	LogError    = shared.LogLevelError
	LogCritical = shared.LogLevelCritical
)

var registry = map[string]HandlerFunc{}

// Register registers a named HTTP filter handler and wires it into the Envoy
// SDK. Must be called from an init() function. Panics on duplicate names.
func Register(name string, h HandlerFunc) {
	if _, ok := registry[name]; ok {
		panic("up: filter already registered: " + name)
	}
	registry[name] = h
	down.RegisterHttpFilter(name, &configFactory{handler: h})
}

// RegisterWithResponse registers a named HTTP filter with both a request and a
// response handler. Must be called from an init() function. Panics on duplicate names.
func RegisterWithResponse(name string, h HandlerFunc, r ResponseHandlerFunc) {
	if _, ok := registry[name]; ok {
		panic("up: filter already registered: " + name)
	}
	registry[name] = h
	down.RegisterHttpFilter(name, &configFactory{handler: h, responseHandler: r})
}

// RegisterAccessLogger registers a named access logger factory. Must be called
// from an init() function. Panics on duplicate names.
func RegisterAccessLogger(name string, f down.AccessLoggerConfigFactory) {
	down.RegisterAccessLoggerConfigFactory(name, f)
}
