package main

import (
	"log"
	"os"

	_ "github.com/dio/transit/down/abi_impl"
	"github.com/dio/transit/examples/ratelimit"
	"github.com/dio/transit/up"
)

func init() {
	data, err := os.ReadFile(os.Getenv("RATELIMIT_CONFIG"))
	if err != nil {
		log.Printf("ratelimit: read RATELIMIT_CONFIG: %v", err)
		return
	}
	policy, err := ratelimit.Decode(data)
	if err != nil {
		log.Printf("ratelimit: invalid config: %v", err)
		return
	}
	up.Register("ratelimit", ratelimit.NewHandler(up.NewStaticConfig(policy)))
}

func main() {}
