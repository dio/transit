// Package down bridges the official Envoy SDK registration and ABI layer.
// Importing this package (via transit/up) is sufficient to wire all ABI
// callbacks; callers do not need to import the SDK or abi_impl directly.
package down

import (
	sdk "github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go"
	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"

	_ "github.com/dio/transit/down/abi_impl"
)

// RegisterHttpFilter wires one named HTTP filter factory into the SDK registry.
// Called by up.Register; must be called from an init() function.
func RegisterHttpFilter(name string, factory shared.HttpFilterConfigFactory) {
	sdk.RegisterHttpFilterConfigFactories(map[string]shared.HttpFilterConfigFactory{
		name: factory,
	})
}
