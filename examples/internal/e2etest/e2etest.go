// Package e2etest holds the small bits of Envoy e2e harness code that every
// example otherwise ends up copying.
package e2etest

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"text/template"
	"time"
)

// EnvoyBin returns the binary selected by ENVOY_BIN, falling back to the root
// .bin/envoy path used by make download-envoy.
func EnvoyBin(examplesRoot string) string {
	if b := os.Getenv("ENVOY_BIN"); b != "" {
		return b
	}
	return filepath.Join(examplesRoot, "..", ".bin", "envoy")
}

// BuildSharedLibrary compiles one example command as the dynamic module Envoy
// will load. Tests can set TRANSIT_SKIP_BUILD=1 while iterating on assertions.
func BuildSharedLibrary(examplesRoot, exampleName, output string) error {
	if os.Getenv("TRANSIT_SKIP_BUILD") != "" {
		if _, err := os.Stat(filepath.Join(examplesRoot, exampleName, output)); err != nil {
			return fmt.Errorf("TRANSIT_SKIP_BUILD=1 but %s not found: %w", output, err)
		}
		return nil
	}
	cmd := exec.Command("go", "build", "-trimpath", "-buildmode=c-shared",
		"-o", filepath.Join(exampleName, output), "./"+exampleName+"/cmd")
	cmd.Dir = examplesRoot
	cmd.Env = append(os.Environ(), "CGO_ENABLED=1")
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// FreePort reserves a loopback port long enough for the caller to learn it.
// There is still a normal close-and-bind race, but this matches the rest of the
// test harness and keeps every e2e process on an isolated port.
func FreePort() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic("freePort: " + err.Error())
	}
	defer func() { _ = l.Close() }()
	addr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		panic("freePort: listener did not return a TCP address")
	}
	return addr.Port
}

// WriteEnvoyConfig renders an embedded template to a temporary Envoy bootstrap.
func WriteEnvoyConfig(name, tmpl string, data any) string {
	parsed := template.Must(template.New("envoy").Parse(tmpl))
	f, err := os.CreateTemp("", name+"-*.yaml")
	if err != nil {
		panic(err)
	}
	if err := parsed.Execute(f, data); err != nil {
		panic(err)
	}
	if err := f.Close(); err != nil {
		panic(err)
	}
	return f.Name()
}

// WaitURL polls a URL until it returns 200 OK or the timeout expires.
func WaitURL(url string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url) //nolint:noctx
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}
