// Package testpg provides a per-test-binary embedded postgres helper.
//
// One embedded instance boots lazily on the first [Pool] call and lives until
// [Cleanup] is called. Each instance gets a random free port and a
// [os.MkdirTemp] data directory so multiple packages running their test
// binaries in parallel under "go test ./..." never collide on port or path.
//
// Canonical usage:
//
//	func TestMain(m *testing.M) {
//	    code := m.Run()
//	    testpg.Cleanup()
//	    os.Exit(code)
//	}
//
//	func TestSomething(t *testing.T) {
//	    pool := testpg.Pool(t)
//	    // ... use pool ...
//	}
package testpg

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dio/transit/examples/orange/internal/embeddedpg"
)

var (
	once sync.Once
	inst *embeddedpg.Instance
	pool *pgxpool.Pool
	err  error

	stopOnce sync.Once
)

// Pool returns a pgx pool connected to the shared embedded postgres for this
// test binary. The first call boots the instance; subsequent calls reuse it.
// Calls t.Fatalf on any startup error. Callers must invoke [Cleanup] once at
// the end of the test binary (typically from TestMain after m.Run()).
func Pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	once.Do(func() {
		port, perr := freePort()
		if perr != nil {
			err = perr
			return
		}
		root, derr := os.MkdirTemp("", "orange-testpg-*")
		if derr != nil {
			err = derr
			return
		}
		inst, err = embeddedpg.Start(embeddedpg.Config{Root: root, Port: uint32(port)})
		if err != nil {
			return
		}
		pool, err = pgxpool.New(context.Background(), inst.DSN())
	})
	if err != nil {
		t.Fatalf("testpg: %v", err)
	}
	return pool
}

// Cleanup closes the pool, stops the embedded postgres, and removes the temp
// data directory. Safe to call when Pool was never invoked. Idempotent.
func Cleanup() {
	stopOnce.Do(func() {
		if pool != nil {
			pool.Close()
		}
		if inst != nil {
			_ = inst.Stop()
			_ = os.RemoveAll(inst.Config().Root)
		}
	})
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("listen :0: %w", err)
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		_ = ln.Close()
		return 0, fmt.Errorf("invalid address type")
	}
	port := addr.Port
	if err := ln.Close(); err != nil {
		return 0, fmt.Errorf("close listener: %w", err)
	}
	return port, nil
}
