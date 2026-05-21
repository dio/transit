package main

import (
	"github.com/dio/transit/examples/hello"
	"github.com/dio/transit/up"
)

func init() {
	up.Register("hello", hello.Handler)
}

func main() {}
