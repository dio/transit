// Package main is a structural placeholder: it blocks forever so that Envoy
// Gateway sees a ready endpoint and generates the CDS cluster that the
// EnvoyPatchPolicy then replaces. No traffic reaches this process.
package main

func main() { select {} }
