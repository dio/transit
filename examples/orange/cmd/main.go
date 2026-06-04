// Package main is the dynamic-module entrypoint. It blank-imports the orange
// subpackages so their init() registrations land in the shared SDK registry
// before Envoy queries it.
package main

import (
	_ "github.com/dio/transit/down/abi_impl"
	_ "github.com/dio/transit/examples/orange/internal/debug"
	_ "github.com/dio/transit/examples/orange/internal/pipeline/adapt"
	_ "github.com/dio/transit/examples/orange/internal/pipeline/match"
	_ "github.com/dio/transit/examples/orange/internal/pipeline/meter"
	_ "github.com/dio/transit/examples/orange/internal/pipeline/pick"
	_ "github.com/dio/transit/examples/orange/internal/pipeline/ws"
	_ "github.com/dio/transit/examples/orange/internal/translator"
)

func main() {}
