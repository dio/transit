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

// CheckSharedLibrary verifies that the example Makefile has already built the
// dynamic module Envoy will load. Example e2e tests should not run go build
// themselves; make -C examples/<name> e2e owns that build step.
func CheckSharedLibrary(examplesRoot, exampleName, output string) error {
	path := filepath.Join(examplesRoot, exampleName, output)
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("%s not found; run `make -C examples/%s build` before direct e2e: %w", path, exampleName, err)
	}
	return nil
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

// StartEnvoy starts Envoy with the standard dynamic-module flags and waits for
// its admin /ready endpoint. searchPath sets ENVOY_DYNAMIC_MODULES_SEARCH_PATH.
// extraEnv is appended after GODEBUG and ENVOY_DYNAMIC_MODULES_SEARCH_PATH.
// The returned stop func kills the process and removes cfgPath; call it before
// os.Exit. Returns (nil, false) on failure after printing to stderr.
func StartEnvoy(bin, cfgPath, searchPath string, adminPort int, extraEnv []string) (stop func(), ok bool) {
	cmd := exec.Command(bin, "-c", cfgPath, "--log-level", "warning",
		"--component-log-level", "dynamic_modules:info")
	cmd.Env = append(os.Environ(),
		"GODEBUG=cgocheck=0",
		"ENVOY_DYNAMIC_MODULES_SEARCH_PATH="+searchPath,
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	stop = func() {
		cmd.Process.Kill() //nolint:errcheck
		cmd.Wait()         //nolint:errcheck
		os.Remove(cfgPath) //nolint:errcheck
	}

	if err := cmd.Start(); err != nil {
		os.Remove(cfgPath) //nolint:errcheck
		fmt.Fprintf(os.Stderr, "e2e: envoy start failed: %v\n", err)
		return nil, false
	}
	fmt.Fprintf(os.Stderr, "e2e: envoy pid=%d\n", cmd.Process.Pid)

	if !WaitURL(fmt.Sprintf("http://127.0.0.1:%d/ready", adminPort), 15*time.Second) {
		stop()
		fmt.Fprintln(os.Stderr, "e2e: envoy not ready in time")
		return nil, false
	}
	return stop, true
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
