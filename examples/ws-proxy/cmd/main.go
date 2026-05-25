package main

import (
	_ "github.com/dio/transit/down/abi_impl"
	"github.com/dio/transit/examples/ws-proxy"
)

func init() {
	wsproxy.Register()
}

func main() {}
