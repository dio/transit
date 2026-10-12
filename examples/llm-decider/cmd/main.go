// Command llm-decider is the shared-library entrypoint for this example.
//
// Envoy loads this .so and calls the registration callbacks; it never calls
// main(). Registration therefore happens in init().
package main

import (
	"log"

	// The Envoy callback exports. This blank import belongs only in the
	// c-shared entrypoint package: on Linux, ordinary test binaries fail to link
	// if they pull in unresolved Envoy callback symbols.
	_ "github.com/dio/transit/down/abi_impl"

	llmdecider "github.com/dio/transit/examples/llm-decider"
)

func init() {
	config, err := llmdecider.ConfigFromEnv()
	if err != nil {
		// Returning here leaves the module loaded but nothing registered, which is
		// loud in the Envoy logs and does not take the process down.
		log.Printf("llm-decider: %v", err)
		return
	}
	if err := llmdecider.Register(config); err != nil {
		log.Printf("llm-decider: %v", err)
	}
}

func main() {}
