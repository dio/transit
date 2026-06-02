package up

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestAdminServer(t *testing.T) {
	t.Run("pprof serves 200", func(t *testing.T) {
		admin := NewAdminServer(AdminServerOptions{ListenAddr: "127.0.0.1:0"})
		admin.RegisterPprof()

		go func() { admin.sidecar.execute("test") }() //nolint:errcheck

		select {
		case <-admin.Ready():
		case <-time.After(3 * time.Second):
			t.Fatal("timeout waiting for Ready()")
		}
		defer admin.sidecar.stop()

		resp, err := http.Get("http://" + admin.ListenAddr() + "/debug/pprof/")
		if err != nil {
			t.Fatalf("GET /debug/pprof/: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("want 200, got %d", resp.StatusCode)
		}
	})

	t.Run("custom handler", func(t *testing.T) {
		admin := NewAdminServer(AdminServerOptions{ListenAddr: "127.0.0.1:0"})
		admin.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, "ok")
		})

		go func() { admin.sidecar.execute("test") }() //nolint:errcheck

		select {
		case <-admin.Ready():
		case <-time.After(3 * time.Second):
			t.Fatal("timeout waiting for Ready()")
		}
		defer admin.sidecar.stop()

		resp, err := http.Get("http://" + admin.ListenAddr() + "/healthz")
		if err != nil {
			t.Fatalf("GET /healthz: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("want 200, got %d", resp.StatusCode)
		}
		if string(body) != "ok" {
			t.Errorf("want \"ok\", got %q", string(body))
		}
	})

	t.Run("ListenAddr valid after Ready", func(t *testing.T) {
		admin := NewAdminServer(AdminServerOptions{ListenAddr: "127.0.0.1:0"})

		go func() { admin.sidecar.execute("test") }() //nolint:errcheck

		select {
		case <-admin.Ready():
		case <-time.After(3 * time.Second):
			t.Fatal("timeout waiting for Ready()")
		}
		defer admin.sidecar.stop()

		addr := admin.ListenAddr()
		if addr == "" {
			t.Fatal("ListenAddr() is empty after Ready()")
		}
		if _, _, err := net.SplitHostPort(addr); err != nil {
			t.Errorf("ListenAddr() not parseable as host:port: %v", err)
		}
	})

	t.Run("stop is graceful", func(t *testing.T) {
		admin := NewAdminServer(AdminServerOptions{ListenAddr: "127.0.0.1:0"})

		done := make(chan struct{})
		go func() {
			admin.sidecar.execute("test") //nolint:errcheck
			close(done)
		}()

		select {
		case <-admin.Ready():
		case <-time.After(3 * time.Second):
			t.Fatal("timeout waiting for Ready()")
		}

		addr := admin.ListenAddr()
		admin.sidecar.stop()

		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("server did not stop within timeout")
		}

		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			t.Error("expected dial to fail after stop, but it succeeded")
		}
	})
}
