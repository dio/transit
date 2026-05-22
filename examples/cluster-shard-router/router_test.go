package clustershardrouter

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestParseRouterConfigDefaults(t *testing.T) {
	cfg, err := parseRouterConfig(nil)
	require.NoError(t, err)
	require.Equal(t, defaultRefresh, cfg.refresh())
	require.Equal(t, defaultTimeout, cfg.timeout())
}

func TestParseRouterConfigValues(t *testing.T) {
	cfg, err := parseRouterConfig([]byte(`{"config_url":"http://127.0.0.1/shards.json","refresh_millis":25,"timeout_millis":50}`))
	require.NoError(t, err)
	require.Equal(t, "http://127.0.0.1/shards.json", cfg.ConfigURL)
	require.Equal(t, 25*time.Millisecond, cfg.refresh())
	require.Equal(t, 50*time.Millisecond, cfg.timeout())
}

func TestParseRouterConfigInvalidJSON(t *testing.T) {
	_, err := parseRouterConfig([]byte(`{`))
	require.Error(t, err)
}

func TestResolveConfigSnapshot(t *testing.T) {
	target := net.JoinHostPort("localhost", "8080")
	snap, err := resolveConfigSnapshot(context.Background(), configSnapshot{
		Version:      "v1",
		DefaultShard: "a",
		Shards: map[string]shardConfig{
			"a": {
				Target:   target,
				Prefixes: []string{"a", "tenant-a"},
				Shard:    "a",
				Status:   "active",
			},
		},
	}, time.Second)
	require.NoError(t, err)

	route := snap.Shards["a"]
	require.Equal(t, target, route.Target)
	require.Equal(t, "a", route.Shard)
	require.Contains(t, route.Address, ":8080")
	require.Equal(t, []string{"a", "tenant-a"}, route.Prefixes)
	require.Equal(t, "active", route.Status)
}

func TestResolveConfigSnapshotRejectsMissingTarget(t *testing.T) {
	_, err := resolveConfigSnapshot(context.Background(), configSnapshot{
		DefaultShard: "a",
		Shards:       map[string]shardConfig{"a": {Prefixes: []string{"a"}}},
	}, time.Second)
	require.Error(t, err)
}

func TestResolveConfigSnapshotRejectsMissingDefaultShard(t *testing.T) {
	_, err := resolveConfigSnapshot(context.Background(), configSnapshot{
		DefaultShard: "missing",
		Shards: map[string]shardConfig{
			"a": {Target: net.JoinHostPort("localhost", "8080")},
		},
	}, time.Second)
	require.Error(t, err)
}

func TestDeriveTagPriority(t *testing.T) {
	headers := map[string]string{
		tagHeader:       " B-Demo ",
		byokKeyIDHeader: "key-a",
		userKeyHeader:   "user-a",
		tenantHeader:    "tenant-a",
	}
	tag, source := deriveTag(func(name string) string { return headers[name] })
	require.Equal(t, "b-demo", tag)
	require.Equal(t, "tag", source)
}

func TestDeriveTagHashesBYOKBeforeUserKey(t *testing.T) {
	headers := map[string]string{
		byokKeyIDHeader: "key-a",
		userKeyHeader:   "user-a",
		tenantHeader:    "tenant-a",
	}
	tag, source := deriveTag(func(name string) string { return headers[name] })
	require.Equal(t, hashTag("key-a"), tag)
	require.Equal(t, "byok-key-id", source)
}

func TestDeriveTagUsesTenant(t *testing.T) {
	tag, source := deriveTag(func(name string) string {
		if name == tenantHeader {
			return " Tenant-A "
		}
		return ""
	})
	require.Equal(t, "tenant-a", tag)
	require.Equal(t, "tenant", source)
}

func TestChooseShardUsesLongestPrefix(t *testing.T) {
	snap := shardSnapshot{
		DefaultShard: "a",
		Shards: map[string]shardRoute{
			"a":  {Name: "a", Shard: "a", Prefixes: []string{"a"}},
			"ab": {Name: "ab", Shard: "ab", Prefixes: []string{"ab"}},
		},
	}
	route, ok := chooseShard(snap, "ab-demo")
	require.True(t, ok)
	require.Equal(t, "ab", route.Shard)
}

func TestChooseShardFallsBackToDefault(t *testing.T) {
	snap := shardSnapshot{
		DefaultShard: "a",
		Shards: map[string]shardRoute{
			"a": {Name: "a", Shard: "a", Prefixes: []string{"a"}},
			"b": {Name: "b", Shard: "b", Prefixes: []string{"b"}},
		},
	}
	route, ok := chooseShard(snap, "z-demo")
	require.True(t, ok)
	require.Equal(t, "a", route.Shard)
}

func TestShardStorePublishesSnapshotCopy(t *testing.T) {
	store := newShardStore()
	snap := shardSnapshot{
		Version:      "v1",
		DefaultShard: "a",
		Shards: map[string]shardRoute{
			"a": {Target: "localhost:8080", Address: "127.0.0.1:8080", Prefixes: []string{"a"}},
		},
	}
	store.Publish(snap)
	snap.Shards["a"] = shardRoute{Target: "mutated", Address: "mutated"}

	got := store.Current().Shards["a"]
	require.Equal(t, "localhost:8080", got.Target)
	require.Equal(t, "127.0.0.1:8080", got.Address)
}

func TestDebugSnapshotIncludesActiveShards(t *testing.T) {
	store := newShardStore()
	store.Publish(shardSnapshot{
		Version:      "v1",
		DefaultShard: "a",
		Shards: map[string]shardRoute{
			"a": {
				Shard:    "a",
				Target:   "localhost:8080",
				Address:  "127.0.0.1:8080",
				Prefixes: []string{"a"},
			},
		},
	})

	body, err := json.Marshal(store.DebugSnapshot())
	require.NoError(t, err)
	require.Contains(t, string(body), `"default_shard":"a"`)
	require.Contains(t, string(body), `"target":"localhost:8080"`)
}
