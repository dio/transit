package main

import (
	_ "github.com/dio/transit/down/abi_impl"
	"github.com/dio/transit/integrations/ws-proxy-eg/echo"
)

func init() {
	echo.Register()
}

func main() {}
