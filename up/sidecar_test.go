package up

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// startSidecar wires the sidecar into a Group using its unexported execute/stop
// methods and starts the group. Returns a stop function.
func startSidecar(s *Sidecar, name string) func() {
	g := NewGroup()
	g.Add(func() error { return s.execute(name) }, s.stop)
	g.Start()
	return g.Stop
}

// waitReady waits for the sidecar to be ready, timing out after 2s.
func waitReady(t *testing.T, s *Sidecar) {
	t.Helper()
	select {
	case <-s.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("sidecar did not become ready within 2s")
	}
}

func TestSidecar_ListenAddrEmptyBeforeReady(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	s := NewSidecar(h, SidecarOptions{
		ListenAddr: "127.0.0.1:0",
		EgressURL:  "http://example.com", // suppress direct-dial log
	})

	// Before starting, ListenAddr should be "".
	if got := s.ListenAddr(); got != "" {
		t.Errorf("ListenAddr() before ready = %q, want \"\"", got)
	}

	stop := startSidecar(s, "test")
	defer stop()
	waitReady(t, s)
}

func TestSidecar_Ready(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	s := NewSidecar(h, SidecarOptions{
		ListenAddr: "127.0.0.1:0",
		EgressURL:  "http://example.com",
	})

	stop := startSidecar(s, "test")
	defer stop()

	waitReady(t, s)

	addr := s.ListenAddr()
	if addr == "" {
		t.Fatal("ListenAddr() is empty after Ready() closed")
	}

	// Verify we can actually connect.
	resp, err := http.Get("http://" + addr + "/ping")
	if err != nil {
		t.Fatalf("http.Get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestSidecar_ShutdownGraceful(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	s := NewSidecar(h, SidecarOptions{
		ListenAddr:      "127.0.0.1:0",
		ShutdownTimeout: 2 * time.Second,
		EgressURL:       "http://example.com",
	})

	stop := startSidecar(s, "test")
	waitReady(t, s)

	addr := s.ListenAddr()

	// Stop should return without hanging.
	done := make(chan struct{})
	go func() {
		stop()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stop() did not return within 5s")
	}

	// After shutdown, connections should be refused.
	_, err := http.Get("http://" + addr + "/ping")
	if err == nil {
		t.Error("expected connection refused after shutdown, got nil error")
	}
}

func TestSidecar_OnSessionFires(t *testing.T) {
	var fired atomic.Bool
	sessionCh := make(chan SidecarSessionEvent, 1)

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "hello")
	})

	s := NewSidecar(h, SidecarOptions{
		ListenAddr: "127.0.0.1:0",
		EgressURL:  "http://example.com",
		OnSession: func(e SidecarSessionEvent) {
			fired.Store(true)
			sessionCh <- e
		},
	})

	stop := startSidecar(s, "test")
	defer stop()
	waitReady(t, s)

	resp, err := http.Get("http://" + s.ListenAddr() + "/test-path")
	if err != nil {
		t.Fatalf("http.Get: %v", err)
	}
	io.ReadAll(resp.Body) //nolint:errcheck
	resp.Body.Close()

	// Wait for OnSession to fire.
	select {
	case e := <-sessionCh:
		if e.Path != "/test-path" {
			t.Errorf("event.Path = %q, want %q", e.Path, "/test-path")
		}
		if e.Duration < 0 {
			t.Errorf("event.Duration = %v, want >= 0", e.Duration)
		}
		if e.Start.IsZero() {
			t.Error("event.Start is zero")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnSession did not fire within 2s")
	}

	if !fired.Load() {
		t.Error("OnSession callback was not called")
	}
}

func TestSidecar_StartupLogFile(t *testing.T) {
	f, err := os.CreateTemp("", "sidecar-startup-*.log")
	if err != nil {
		t.Fatal(err)
	}
	logFile := f.Name()
	f.Close()
	defer os.Remove(logFile)

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	s := NewSidecar(h, SidecarOptions{
		ListenAddr:     "127.0.0.1:0",
		EgressURL:      "", // direct-dial mode triggers the log
		Rationale:      "test-rationale-xyz",
		StartupLogFile: logFile,
	})

	stop := startSidecar(s, "test-filter")
	defer stop()
	waitReady(t, s)

	// Give the log a moment to be written (it's written after Ready() but before Serve).
	time.Sleep(10 * time.Millisecond)

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	content := string(data)
	if content == "" {
		t.Fatal("startup log file is empty")
	}
	if !contains(content, "test-rationale-xyz") {
		t.Errorf("startup log %q does not contain rationale %q", content, "test-rationale-xyz")
	}
	if !contains(content, "direct-dial mode") {
		t.Errorf("startup log %q does not contain 'direct-dial mode'", content)
	}
}

func TestSidecar_StartupWarningNoRationale(t *testing.T) {
	f, err := os.CreateTemp("", "sidecar-startup-warn-*.log")
	if err != nil {
		t.Fatal(err)
	}
	logFile := f.Name()
	f.Close()
	defer os.Remove(logFile)

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	s := NewSidecar(h, SidecarOptions{
		ListenAddr:     "127.0.0.1:0",
		EgressURL:      "",  // direct-dial mode
		Rationale:      "",  // no rationale — should warn
		StartupLogFile: logFile,
	})

	stop := startSidecar(s, "test-filter-warn")
	defer stop()
	waitReady(t, s)

	time.Sleep(10 * time.Millisecond)

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	content := string(data)
	if !contains(content, "WARNING") {
		t.Errorf("startup log %q does not contain WARNING", content)
	}
	if !contains(content, "no rationale") {
		t.Errorf("startup log %q does not contain 'no rationale'", content)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		func() bool {
			for i := 0; i <= len(s)-len(substr); i++ {
				if s[i:i+len(substr)] == substr {
					return true
				}
			}
			return false
		}())
}
