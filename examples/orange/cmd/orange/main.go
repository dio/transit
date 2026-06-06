package main

import (
	"fmt"
	"os"

	"github.com/dio/transit/examples/orange/internal/orange"
)

func main() {
	if err := orange.NewCommand().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
