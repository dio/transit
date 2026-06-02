package main

import (
	_ "github.com/dio/transit/down/abi_impl"
	mcpsidecar "github.com/dio/transit/examples/mcp-streaming-sidecar"
)

func init() {
	mcpsidecar.Register()
}

func main() {}
