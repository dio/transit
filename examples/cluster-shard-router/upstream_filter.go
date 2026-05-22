package clustershardrouter

import (
	"encoding/json"

	"github.com/dio/transit/up"
)

func upstreamHeaderFilter(w *up.Writer, r *up.Request) {
	decision, ok := activeShards.Decide(r.Header)
	if !ok {
		return
	}
	w.SetRequestHeader(tagHeader, decision.Tag)
	w.SetRequestHeader(tagSourceHeader, decision.Source)
	w.SetRequestHeader(shardHeader, decision.Route.Shard)
	w.SetRequestHeader(targetHeader, decision.Route.Target)
	if version := activeShards.Current().Version; version != "" {
		w.SetRequestHeader(versionHeader, version)
	}
}

func debugHandler(w *up.Writer, r *up.Request) {
	if r.Path != debugPath {
		return
	}
	body, err := json.MarshalIndent(activeShards.DebugSnapshot(), "", "  ")
	if err != nil {
		w.SendLocalResponse(500, []byte(`{"error":"marshal config"}`),
			[2]string{"content-type", "application/json"})
		return
	}
	w.SendLocalResponse(200, body, [2]string{"content-type", "application/json"})
}

type debugSnapshot struct {
	Version      string                `json:"version"`
	DefaultShard string                `json:"default_shard"`
	Shards       map[string]debugShard `json:"shards"`
}

type debugShard struct {
	Shard    string   `json:"shard"`
	Target   string   `json:"target"`
	Address  string   `json:"address"`
	Prefixes []string `json:"prefixes,omitempty"`
	Status   string   `json:"status,omitempty"`
}

func (s *shardStore) DebugSnapshot() debugSnapshot {
	snap := s.Current()
	out := debugSnapshot{
		Version:      snap.Version,
		DefaultShard: snap.DefaultShard,
		Shards:       make(map[string]debugShard, len(snap.Shards)),
	}
	for name, route := range snap.Shards {
		out.Shards[name] = debugShard{
			Shard:    route.Shard,
			Target:   route.Target,
			Address:  route.Address,
			Prefixes: append([]string(nil), route.Prefixes...),
			Status:   route.Status,
		}
	}
	return out
}
