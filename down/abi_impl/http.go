// Package abi_impl wires the Envoy dynamic module ABI by forwarding to the
// official SDK's abi package. Importing this package (blank import) from a
// binary's main package is sufficient to register all HTTP filter and network
// filter //export symbols.
package abi_impl

import _ "github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/abi"
