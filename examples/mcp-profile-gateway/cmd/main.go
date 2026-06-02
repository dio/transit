package main

import (
	"log"

	_ "github.com/dio/transit/down/abi_impl"
	mcpprofilegateway "github.com/dio/transit/examples/mcp-profile-gateway"
)

func init() {
	pc, err := mcpprofilegateway.LoadPipelineConfig()
	if err != nil {
		log.Printf("mcp-profile-gateway: %v", err)
		return
	}
	mcpprofilegateway.RegisterTransitFilter("mcp-profile-gateway", pc.Snapshot())
}

func main() {}
