package main

import (
	"log"

	_ "github.com/dio/transit/down/abi_impl"
	mcpprofilerouter "github.com/dio/transit/examples/mcp-profile-router"
)

func init() {
	profile, err := mcpprofilerouter.LoadProfileFromEnv()
	if err != nil {
		log.Printf("mcp-profile-router: %v", err)
		return
	}
	mcpprofilerouter.RegisterTransitFilter("mcp-profile-router", profile)
}

func main() {}
