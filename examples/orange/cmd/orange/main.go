package main

import (
	"fmt"
	"os"

	"github.com/dio/transit/examples/orange/internal/server"
)

func main() {
	if err := server.NewCommand().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
