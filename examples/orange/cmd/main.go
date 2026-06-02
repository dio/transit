// Package main is the dynamic-module entrypoint. It blank-imports the orange
// subpackages so their init() registrations land in the shared SDK registry
// before Envoy queries it.
package main

import (
	_ "github.com/dio/transit/down/abi_impl"
	_ "github.com/dio/transit/examples/orange/classify"
	_ "github.com/dio/transit/examples/orange/debug"
	_ "github.com/dio/transit/examples/orange/hostpick"
	_ "github.com/dio/transit/examples/orange/tap"
	_ "github.com/dio/transit/examples/orange/translate"
)

func main() {}
