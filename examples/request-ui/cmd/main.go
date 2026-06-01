// Package main builds the request-ui dynamic module.
//
// Starts the sink (HTTP server + SSE broadcaster + writer goroutine) then
// registers the transit filter that feeds records into it. The filter uses
// up.WithOnStreamFinalized so the SDK's internal access-logger bridge
// delivers finalized stream fields to the same callback path that builds
// each record — no separate RegisterLogger needed.
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

var globalSink = sink.New()

func init() {
	globalSink.Start()
	requestui.Register("request-ui", globalSink)
}

func main() {}
