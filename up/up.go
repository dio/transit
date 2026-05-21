package up

import (
	"github.com/dio/transit/down"
	_ "github.com/dio/transit/down/abi_impl"
	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"
)

// HandlerFunc is called on every request.
type HandlerFunc func(w *Writer, r *Request)

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
	down.RegisterHttpFilter(name, &configFactory{name: name, handler: h})
}

// RegisterWithResponse registers a named HTTP filter with both a request and a
// response handler. Must be called from an init() function. Panics on duplicate names.
func RegisterWithResponse(name string, h HandlerFunc, r ResponseHandlerFunc) {
	if _, ok := registry[name]; ok {
		panic("up: filter already registered: " + name)
	}
	registry[name] = h
	down.RegisterHttpFilter(name, &configFactory{name: name, handler: h, responseHandler: r})
}

// RegisterWithBody registers a named HTTP filter with request body handling in
// streaming mode: the body handler is called once per chunk as data arrives.
// For bodyless requests (GET etc.) the handler is called once with Data: nil.
func RegisterWithBody(name string, h HandlerFunc, rb RequestBodyHandlerFunc, r ResponseHandlerFunc) {
	if _, ok := registry[name]; ok {
		panic("up: filter already registered: " + name)
	}
	registry[name] = h
	down.RegisterHttpFilter(name, &configFactory{
		name:               name,
		handler:            h,
		responseHandler:    r,
		requestBodyHandler: rb,
	})
}

// RegisterWithMutableBody registers a named HTTP filter with buffered body
// handling: the body handler is called once with the full accumulated body.
// Use Writer.SetRequestBody / SetResponseBody to replace body content.
func RegisterWithMutableBody(name string, h HandlerFunc, rb RequestBodyHandlerFunc, r ResponseHandlerFunc) {
	if _, ok := registry[name]; ok {
		panic("up: filter already registered: " + name)
	}
	registry[name] = h
	down.RegisterHttpFilter(name, &configFactory{
		name:               name,
		handler:            h,
		responseHandler:    r,
		requestBodyHandler: rb,
		bufferBody:         true,
	})
}

// RegisterAccessLogger registers a named access logger factory. Must be called
// from an init() function. Panics on duplicate names.
func RegisterAccessLogger(name string, f down.AccessLoggerConfigFactory) {
	down.RegisterAccessLoggerConfigFactory(name, f)
}
