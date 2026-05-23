package main

import (
	"log"

	_ "github.com/dio/transit/down/abi_impl"
	mcpcatalogrouter "github.com/dio/transit/examples/mcp-catalog-router"
)

func init() {
	config, err := mcpcatalogrouter.LoadConfigFromEnv()
	if err != nil {
		log.Printf("mcp-catalog-router: %v", err)
		return
	}
	mcpcatalogrouter.RegisterTransitFilter("mcp-catalog-router", config)
}

func main() {}
