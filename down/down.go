// Package down bridges the official Envoy SDK registration and ABI layer.
// This file contains the HTTP-filter registration bridge; access logger types,
// cluster extension types, LB policy types, and response-flag utilities are in
// their own files (access_logger.go, cluster.go, lb.go, response_flags.go).
//
// Callers never import this package directly; transit/up re-exports everything.
package down

import (
	sdk "github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go"
	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"
)

// RegisterHttpFilter wires one named HTTP filter factory into the official SDK
// registry. Called by up.Register; must be called from an init() function.
func RegisterHttpFilter(name string, factory shared.HttpFilterConfigFactory) {
	sdk.RegisterHttpFilterConfigFactories(map[string]shared.HttpFilterConfigFactory{
		name: factory,
	})
}
