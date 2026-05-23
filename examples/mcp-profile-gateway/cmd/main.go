package main

import (
	"log"

	_ "github.com/dio/transit/down/abi_impl"
	mcpprofilegateway "github.com/dio/transit/examples/mcp-profile-gateway"
)

func init() {
	config, err := mcpprofilegateway.LoadConfigFromEnv()
	if err != nil {
		log.Printf("mcp-profile-gateway: %v", err)
		return
	}
	mcpprofilegateway.RegisterTransitFilter("mcp-profile-gateway", config)
}

func main() {}
