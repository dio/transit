package clusterrouter

import (
	"encoding/json"

	"github.com/dio/transit/up"
)

func upstreamHeaderFilterForStore(store *routeStore) func(*up.Writer, *up.Request) {
	return func(w *up.Writer, r *up.Request) {
		// Upstream filters run after host selection. Keep this filter focused on
		// request shaping: provider headers, auth, and traceable router version.
		model := r.Header(modelHeader)
		if model == "" {
			return
		}
		snap := store.Current()
		route, ok := snap.Models[model]
		if !ok {
			return
		}
		if auth := resolveAuthHeader(snap, route, r.Header(tenantHeader)); auth != "" {
			w.SetRequestHeader("authorization", auth)
		}
		if route.Provider != "" {
			w.SetRequestHeader("x-llm-provider", route.Provider)
		}
		if route.Profile != "" {
			w.SetRequestHeader("x-user-profile", route.Profile)
		}
		if route.BYOKKeyID != "" {
			w.SetRequestHeader("x-byok-key-id", route.BYOKKeyID)
		}
		if snap.Version != "" {
			w.SetRequestHeader("x-cluster-router-version", snap.Version)
		}
	}
}

func debugHandler(w *up.Writer, r *up.Request) {
	if r.Path != debugPath {
		return
	}
	// Dump the active snapshot, not the last fetched config document. Operators
	// care about what requests are using right now.
	body, err := json.MarshalIndent(activeRoutes.DebugSnapshot(), "", "  ")
	if err != nil {
		w.SendLocalResponse(500, []byte(`{"error":"marshal config"}`),
			[2]string{"content-type", "application/json"})
		return
	}
	w.SendLocalResponse(200, body, [2]string{"content-type", "application/json"})
}

func resolveAuthHeader(snap routeSnapshot, route modelRoute, tenant string) string {
	// The early examples use literal auth_header values. The auth_ref path is
	// here so BYOK can be added without changing the routing decision.
	if route.AuthHeader != "" {
		return route.AuthHeader
	}
	if route.AuthRef == "" {
		return ""
	}
	policy, ok := snap.Auth[route.AuthRef]
	if !ok {
		return ""
	}
	switch policy.Type {
	case "static":
		return policy.Header
	case "byok":
		if tenant == "" {
			return ""
		}
		if providers, ok := snap.BYOK[tenant]; ok {
			return providers[route.Provider]
		}
	}
	return ""
}

type debugSnapshot struct {
	Version string                `json:"version"`
	Models  map[string]debugModel `json:"models"`
	Auth    map[string]debugAuth  `json:"auth,omitempty"`
}

type debugModel struct {
	Target    string `json:"target"`
	Address   string `json:"address"`
	Provider  string `json:"provider"`
	AuthRef   string `json:"auth_ref,omitempty"`
	Profile   string `json:"profile,omitempty"`
	BYOKKeyID string `json:"byok_key_id,omitempty"`
}

type debugAuth struct {
	Type       string `json:"type"`
	Configured bool   `json:"configured"`
}

func (s *routeStore) DebugSnapshot() debugSnapshot {
	snap := s.Current()
	out := debugSnapshot{
		Version: snap.Version,
		Models:  make(map[string]debugModel, len(snap.Models)),
		Auth:    make(map[string]debugAuth, len(snap.Auth)),
	}
	for name, route := range snap.Models {
		out.Models[name] = debugModel{
			Target:    route.Target,
			Address:   route.Address,
			Provider:  route.Provider,
			AuthRef:   route.AuthRef,
			Profile:   route.Profile,
			BYOKKeyID: route.BYOKKeyID,
		}
	}
	for name, auth := range snap.Auth {
		// Deliberately omit auth.Header and BYOK values. The dump should prove
		// config is active without leaking credentials into logs or test output.
		out.Auth[name] = debugAuth{Type: auth.Type, Configured: true}
	}
	return out
}
