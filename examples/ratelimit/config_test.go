package ratelimit

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dio/transit/up"
)

func validConfig() Config {
	return Config{Cluster: "rls", Domain: "orange", Descriptors: [][]Entry{{
		{Key: "key_id", Header: "x-tars-id"}, {Key: "dim", Value: "rpm"},
	}}}
}

func TestCompileOwnsSnapshot(t *testing.T) {
	c := validConfig()
	p, err := Compile(c)
	require.NoError(t, err)
	c.Descriptors[0][0].Key = "changed"
	require.Equal(t, "key_id", p.config.Descriptors[0][0].Key)
	require.Equal(t, "closed", p.config.FailureMode)
	require.EqualValues(t, 50, p.config.TimeoutMillis)
}

func TestValidation(t *testing.T) {
	for name, change := range map[string]func(*Config){
		"no cluster":        func(c *Config) { c.Cluster = "" },
		"no domain":         func(c *Config) { c.Domain = "" },
		"unbounded timeout": func(c *Config) { c.TimeoutMillis = 60001 },
		"bad failure mode":  func(c *Config) { c.FailureMode = "ignore" },
		"no descriptors":    func(c *Config) { c.Descriptors = nil },
		"empty descriptor":  func(c *Config) { c.Descriptors[0] = nil },
		"no key":            func(c *Config) { c.Descriptors[0][0].Key = "" },
		"no value source":   func(c *Config) { c.Descriptors[0][0].Header = "" },
		"ambiguous source":  func(c *Config) { c.Descriptors[0][0].Value = "fixed" },
		"uppercase header":  func(c *Config) { c.Descriptors[0][0].Header = "X-User" },
	} {
		t.Run(name, func(t *testing.T) {
			c := validConfig()
			change(&c)
			_, err := Compile(c)
			require.Error(t, err)
		})
	}
	for _, input := range []string{`null`, `{}`, `{"typo":true}`, `{} {}`} {
		_, err := Decode([]byte(input))
		require.Error(t, err)
	}
}

func TestCustomSourceKeepsLastGoodPolicy(t *testing.T) {
	c := validConfig()
	data, err := json.Marshal(c)
	require.NoError(t, err)
	var sourceErr error
	pc := up.NewPollingConfig(func(context.Context) ([]byte, error) {
		return data, sourceErr
	}, Decode, up.PollOptions{})
	require.Nil(t, pc.Snapshot())
	require.NoError(t, pc.RefreshOnce(context.Background()))
	old := pc.Snapshot()
	data = []byte(`{"domain":"broken"}`)
	require.Error(t, pc.RefreshOnce(context.Background()))
	require.Same(t, old, pc.Snapshot())
	sourceErr = errors.New("source unavailable")
	require.Error(t, pc.RefreshOnce(context.Background()))
	require.Same(t, old, pc.Snapshot())
	sourceErr = nil
	c.Domain, c.FailureMode = "new", "open"
	data, err = json.Marshal(c)
	require.NoError(t, err)
	require.NoError(t, pc.RefreshOnce(context.Background()))
	require.Equal(t, "new", pc.Snapshot().config.Domain)
	require.Equal(t, "orange", old.config.Domain)
	require.Equal(t, "closed", old.config.FailureMode)
}

func TestConcurrentSourceRefreshAndRequests(t *testing.T) {
	var raw atomic.Value
	c := validConfig()
	first, err := json.Marshal(c)
	require.NoError(t, err)
	c.Domain = "new"
	second, err := json.Marshal(c)
	require.NoError(t, err)
	raw.Store(first)
	pc := up.NewPollingConfig(func(context.Context) ([]byte, error) { return raw.Load().([]byte), nil }, Decode, up.PollOptions{})
	require.NoError(t, pc.RefreshOnce(context.Background()))
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 100 {
				p := pc.Snapshot()
				req, err := p.request(func(string) string { return "alice" }, func(string) (string, bool) { return "", false })
				if err != nil || req.GetDomain() != p.config.Domain {
					t.Error("request did not use one complete policy")
				}
			}
		})
	}
	for i := range 100 {
		raw.Store([][]byte{first, second}[i%2])
		require.NoError(t, pc.RefreshOnce(context.Background()))
	}
	wg.Wait()
}
