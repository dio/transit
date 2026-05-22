package main

import (
	_ "github.com/dio/transit/down/abi_impl"
	"github.com/dio/transit/examples/hello"
	"github.com/dio/transit/up"
)

func init() {
	up.Register("hello", hello.Handler)
}

func main() {}
