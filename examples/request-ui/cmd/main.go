// Package main builds the request-ui dynamic module.
//
// Starts the sink (HTTP server + SSE broadcaster + writer goroutine) then
// registers the transit filter and access logger that feed records into it.
//
// Environment variables (see sink package for details):
//
//	REQUI_MODE    "postgres" (default) or "memory"
//	REQUI_DSN     Postgres DSN (postgres mode only)
//	REQUI_ADDR    HTTP listen address (default 0.0.0.0:6062)
package main

import (
	_ "github.com/dio/transit/down/abi_impl"
	requestui "github.com/dio/transit/examples/request-ui"
	"github.com/dio/transit/examples/request-ui/sink"
)

var (
	globalSink    = sink.New()
	globalPending = &requestui.PendingRecords{}
)

func init() {
	globalSink.Start()
	requestui.Register("request-ui", globalSink, globalPending)
	requestui.RegisterLogger("request-ui", globalSink, globalPending)
}

func main() {}
