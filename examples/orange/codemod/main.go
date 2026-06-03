// Package main is the orange codemod tool.
// It transforms ai-gateway translator source files into orange-compatible translators,
// and syncs apischema/helper packages.
//
// Usage:
//
//	# Transform a single file:
//	go run ./examples/orange/codemod -mode transform -src /path/to/openai_foo.go -out /path/to/output.go
//
//	# Transform ALL openai_*.go translator files at once (recommended):
//	go run ./examples/orange/codemod -mode sync-translators -upstream /path/to/ai-gateway -out-root .
//
//	# Sync API schema structs (openai/anthropic/awsbedrock wire types):
//	go run ./examples/orange/codemod -mode sync-apischema -upstream /path/to/ai-gateway -out-root .
//
//	# Sync shared helper utilities:
//	go run ./examples/orange/codemod -mode sync-helpers -upstream /path/to/ai-gateway -out-root .
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
)

func main() {
	mode := flag.String("mode", "transform", "Operation mode: transform, sync-translators, sync-apischema, sync-helpers")
	src := flag.String("src", "", "Source file path (transform mode)")
	out := flag.String("out", "", "Output file path (transform mode)")
	upstream := flag.String("upstream", "", "Path to ai-gateway repo root (sync modes)")
	outRoot := flag.String("out-root", ".", "Output root directory (sync modes)")
	flag.Parse()

	switch *mode {
	case "transform", "transform-translators":
		if *src == "" || *out == "" {
			fmt.Fprintln(os.Stderr, "transform mode requires -src and -out")
			flag.Usage()
			os.Exit(1)
		}
		if err := runTransform(*src, *out); err != nil {
			log.Fatalf("transform: %v", err)
		}
	case "sync-translators":
		if *upstream == "" {
			fmt.Fprintln(os.Stderr, "sync-translators mode requires -upstream")
			flag.Usage()
			os.Exit(1)
		}
		outDir := filepath.Join(*outRoot, "examples/orange/internal/translator")
		if err := syncTranslators(*upstream, outDir); err != nil {
			log.Fatalf("sync-translators: %v", err)
		}
	case "sync-apischema":
		if *upstream == "" {
			fmt.Fprintln(os.Stderr, "sync-apischema mode requires -upstream")
			flag.Usage()
			os.Exit(1)
		}
		outDir := filepath.Join(*outRoot, "examples/orange/internal/apischema")
		if err := syncApischema(*upstream, outDir); err != nil {
			log.Fatalf("sync-apischema: %v", err)
		}
	case "sync-helpers":
		if *upstream == "" {
			fmt.Fprintln(os.Stderr, "sync-helpers mode requires -upstream")
			flag.Usage()
			os.Exit(1)
		}
		outDir := filepath.Join(*outRoot, "examples/orange/internal/translator")
		if err := syncHelpers(*upstream, outDir); err != nil {
			log.Fatalf("sync-helpers: %v", err)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown mode %q; valid: transform, sync-translators, sync-apischema, sync-helpers\n", *mode)
		os.Exit(1)
	}
}
