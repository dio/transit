// Package main — emit.go
// The emitFile function is the pipeline entry point: it calls transformText
// from transform.go and writes/goimports the result.
// All AST-level helpers that were in an earlier design have been removed;
// the implementation is now fully text-based (see transform.go).
package main

import (
	"os/exec"
)

// makeCmd builds an exec.Cmd.
func makeCmd(name string, args ...string) *exec.Cmd {
	return exec.Command(name, args...)
}
