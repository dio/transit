package config

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"github.com/klauspost/compress/zstd"
	"github.com/shopspring/decimal"
	"google.golang.org/protobuf/proto"
	"gopkg.in/yaml.v3"

	configv1 "github.com/dio/transit/examples/orange/api/orange/config/v1"
)

// ── Decode pipeline ───────────────────────────────────────────────────────────

// decodeRawConfig decompresses, verifies, and decodes a SnapshotEnvelope into
// the RawConfig shape consumed by compile(). It is the bridge between the wire
// layer (envelopeFromProto) and the compilation layer (compile).
//
// Processing order:
//  1. Decompress payload (CompressionNone is a no-op).
//  2. Verify SHA-256 checksum against the decompressed bytes (skipped when
//     Checksum is empty — useful for local dev/seed payloads without checksums).
//  3. Dispatch to the format-specific decoder (proto, YAML, or JSON).
//
// The YAML and JSON paths decode directly into RawConfig. The proto path calls
// protoToRaw() to expand string-table indices before returning RawConfig.
func decodeRawConfig(env SnapshotEnvelope) (*RawConfig, error) {
	payload, err := decompress(env.Compression, env.Payload)
	if err != nil {
		return nil, fmt.Errorf("decompress: %w", err)
	}
	if err := verifyChecksum(payload, env.Checksum); err != nil {
		return nil, fmt.Errorf("checksum mismatch: %w", err)
	}

	switch env.Format {
	case SnapshotFormatProto:
		var pb configv1.ConfigPayload
		if err := proto.Unmarshal(payload, &pb); err != nil {
			return nil, fmt.Errorf("proto unmarshal: %w", err)
		}
		return protoToRaw(&pb)

	case SnapshotFormatYAML:
		var raw RawConfig
		if err := yaml.Unmarshal(payload, &raw); err != nil {
			return nil, fmt.Errorf("yaml unmarshal: %w", err)
		}
		return &raw, nil

	case SnapshotFormatJSON:
		var raw RawConfig
		if err := json.Unmarshal(payload, &raw); err != nil {
			return nil, fmt.Errorf("json unmarshal: %w", err)
		}
		return &raw, nil

	default:
		// SnapshotFormatMsgpack is defined but not yet implemented.
		return nil, fmt.Errorf("unsupported format %q", env.Format)
	}
}

// DecodeRaw decodes a SnapshotEnvelope (Go struct) into a RawConfig. It is the
// exported counterpart of decodeRawConfig for callers outside this package that
// hold a Go SnapshotEnvelope directly (e.g. the config service's Fetch path).
func DecodeRaw(env SnapshotEnvelope) (*RawConfig, error) {
	return decodeRawConfig(env)
}

// MarshalRawYAML marshals a RawConfig to YAML bytes. Used by the config service
// to re-encode a workspace-projected RawConfig before delivering it to an egress.
func MarshalRawYAML(raw *RawConfig) ([]byte, error) {
	return yaml.Marshal(raw)
}

// DecodeRawFromProtoEnvelope converts a wire-level configv1.SnapshotEnvelope
// into a RawConfig. It is the exported decode path for callers outside the
// config package (e.g. the egress emulator CLI).
//
// Why exported separately from the rest of the pipeline:
//
//	envelopeFromProto and decodeRawConfig are intentionally unexported — callers
//	inside the package always go through the full AppState.Apply path, which
//	handles version gating, user-table seeding, and atomic snapshot swap. Exporting
//	a combined one-shot function gives external callers (tooling, tests, the
//	emulator) a clean entry point without exposing the intermediate types or
//	inviting callers to bypass the AppState contract in production code.
func DecodeRawFromProtoEnvelope(pb *configv1.SnapshotEnvelope) (*RawConfig, error) {
	env, err := envelopeFromProto(pb)
	if err != nil {
		return nil, err
	}
	return decodeRawConfig(env)
}

// ── Checksum ──────────────────────────────────────────────────────────────────

// verifyChecksum computes SHA-256(payload) and compares it byte-for-byte with
// checksum. A zero-length checksum is treated as "not provided" and skips
// verification — this is intentional for local dev and seed files that omit the
// field. A non-zero checksum that does not equal the computed SHA-256 is an error.
func verifyChecksum(payload, checksum []byte) error {
	if len(checksum) == 0 {
		return nil // checksum omitted — skip verification
	}
	if len(checksum) != 32 {
		return fmt.Errorf("checksum must be 32 bytes (SHA-256), got %d", len(checksum))
	}
	got := sha256.Sum256(payload)
	for i, b := range got {
		if checksum[i] != b {
			return fmt.Errorf("SHA-256 mismatch: expected %x, got %x", checksum, got[:])
		}
	}
	return nil
}

// ── Decompression ─────────────────────────────────────────────────────────────

// zstdDec is a package-level stateless zstd decoder. DecodeAll is safe for
// concurrent use; a single decoder instance avoids repeated allocation.
var zstdDec *zstd.Decoder

func init() {
	var err error
	// NewReader with a nil reader produces a stateless decoder used only via
	// DecodeAll; it does not hold reader state between calls.
	zstdDec, err = zstd.NewReader(nil, zstd.WithDecoderConcurrency(0))
	if err != nil {
		panic("config: zstd decoder init: " + err.Error())
	}
}

// decompress returns the decompressed bytes for the given CompressionKind.
// CompressionNone returns the input slice unchanged (zero allocation).
// CompressionZstd allocates a new slice for the decompressed output.
func decompress(kind CompressionKind, payload []byte) ([]byte, error) {
	switch kind {
	case CompressionNone:
		return payload, nil
	case CompressionZstd:
		out, err := zstdDec.DecodeAll(payload, nil)
		if err != nil {
			return nil, fmt.Errorf("zstd: %w", err)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unknown compression kind %q", kind)
	}
}

// ── Proto expansion ───────────────────────────────────────────────────────────

// protoToRaw converts a ConfigPayload proto into the RawConfig shape consumed
// by compile(). All string-table indices are expanded to their string values
// here; compile() never sees proto types.
//
// Enum-to-string conversions use internal switch-based helpers rather than the
// proto-generated String() methods, which return uppercase names like
// "PROVIDER_KIND_ANTHROPIC" rather than the lowercase values compile() expects
// (e.g. "anthropic").
//
// Key-scope rate limit rules (Key.rate_limit_rules) are stored in
// raw.RateLimit.Policies under the key ID, following the same 3-segment scope
// convention. compile() Phase 3 skips 3-segment scopes; Phase 4 picks them up
// from raw.RateLimit.Policies when processing each KeyRecord.
func protoToRaw(p *configv1.ConfigPayload) (*RawConfig, error) {
	if p == nil {
		return nil, fmt.Errorf("protoToRaw: nil ConfigPayload")
	}

	// str returns the string at idx in the string table.
	// Index 0 is reserved (empty/unset); out-of-range also returns "".
	str := func(idx uint32) string {
		if idx == 0 || p.Strings == nil || int(idx) >= len(p.Strings.Strings) {
			return ""
		}
		return p.Strings.Strings[idx]
	}

	// ── Providers ─────────────────────────────────────────────────────────────

	providers := make(map[string]RawProvider, len(p.Providers))
	for _, prov := range p.Providers {
		id := str(prov.NameIdx)
		if id == "" {
			return nil, fmt.Errorf("provider with zero name_idx")
		}
		extra := make(map[string]string, len(prov.Extra))
		for _, kv := range prov.Extra {
			extra[str(kv.KeyIdx)] = str(kv.ValueIdx)
		}
		providers[id] = RawProvider{
			Kind:          providerKindToString(prov.Kind),
			BackendSchema: str(prov.BackendSchemaIdx),
			Endpoint:      str(prov.EndpointIdx),
			Auth: RawAuth{
				Type:      authTypeToString(prov.Auth.GetType()),
				SecretRef: str(prov.Auth.GetSecretIdx()),
			},
			Extra: extra,
		}
	}

	// ── Models ────────────────────────────────────────────────────────────────

	models := make(map[string]RawModel, len(p.Models))
	for _, m := range p.Models {
		id := str(m.NameIdx)
		overrides := make(map[string]string, len(m.EndpointOverrides))
		for _, eo := range m.EndpointOverrides {
			overrides[str(eo.OperationIdx)] = str(eo.ProviderIdx)
		}
		// Pricing is nil when both input and output are zero — no pricing block.
		var pricing *RawModelPricing
		if m.Pricing != nil && (m.Pricing.InputMtok != 0 || m.Pricing.OutputMtok != 0) {
			pricing = &RawModelPricing{
				InputMTok:      decimal.NewFromFloat(m.Pricing.InputMtok),
				OutputMTok:     decimal.NewFromFloat(m.Pricing.OutputMtok),
				CacheReadMTok:  decimal.NewFromFloat(m.Pricing.CacheReadMtok),
				CacheWriteMTok: decimal.NewFromFloat(m.Pricing.CacheWriteMtok),
			}
		}
		// Metadata is nil when all fields are zero/empty.
		var metadata *RawMetadata
		if m.Metadata != nil && (m.Metadata.DescriptionIdx != 0 ||
			m.Metadata.ContextLength != 0 ||
			m.Metadata.MaxTokens != 0 ||
			len(m.Metadata.TagIdxs) > 0) {
			tags := make([]string, len(m.Metadata.TagIdxs))
			for i, idx := range m.Metadata.TagIdxs {
				tags[i] = str(idx)
			}
			metadata = &RawMetadata{
				Description:   str(m.Metadata.DescriptionIdx),
				ContextLength: int(m.Metadata.ContextLength),
				MaxTokens:     int(m.Metadata.MaxTokens),
				Tags:          tags,
			}
		}
		models[id] = RawModel{
			Provider:          str(m.ProviderIdx),
			Name:              str(m.ApiNameIdx),
			EndpointOverrides: overrides,
			Pricing:           pricing,
			Metadata:          metadata,
		}
	}

	// ── Servers ───────────────────────────────────────────────────────────────

	servers := make(map[string]RawServer, len(p.Servers))
	for _, s := range p.Servers {
		id := str(s.NameIdx)
		if id == "" {
			return nil, fmt.Errorf("server with zero name_idx")
		}
		// Auth is nil when type is UNSPECIFIED (zero value = not configured).
		var auth *RawAuth
		if s.Auth != nil && s.Auth.Type != configv1.AuthType_AUTH_TYPE_UNSPECIFIED {
			v := RawAuth{
				Type:      authTypeToString(s.Auth.Type),
				SecretRef: str(s.Auth.SecretIdx),
			}
			auth = &v
		}
		tools := make([]string, len(s.ToolsIncludeIdxs))
		for i, idx := range s.ToolsIncludeIdxs {
			tools[i] = str(idx)
		}
		servers[id] = RawServer{
			Endpoint:     str(s.EndpointIdx),
			Namespace:    str(s.NamespaceIdx),
			Auth:         auth,
			ToolsInclude: tools,
		}
	}

	// ── Profiles ──────────────────────────────────────────────────────────────

	profiles := make(map[string]RawProfile, len(p.Profiles))
	for _, prof := range p.Profiles {
		id := str(prof.IdIdx)
		if id == "" {
			return nil, fmt.Errorf("profile with zero id_idx")
		}
		tools := make(map[string]RawToolFilter, len(prof.Tools))
		for _, tf := range prof.Tools {
			serverID := str(tf.ServerIdx)
			includes := make([]string, len(tf.IncludeIdxs))
			for i, idx := range tf.IncludeIdxs {
				includes[i] = str(idx)
			}
			tools[serverID] = RawToolFilter{
				Include:  includes,
				Optional: tf.Optional,
			}
		}
		auths := make(map[string]RawAuth, len(prof.AuthOverrides))
		for _, ao := range prof.AuthOverrides {
			serverID := str(ao.ServerIdx)
			auths[serverID] = RawAuth{
				Type:      authTypeToString(ao.Auth.GetType()),
				SecretRef: str(ao.Auth.GetSecretIdx()),
			}
		}
		profiles[id] = RawProfile{
			Tools: tools,
			Auth:  auths,
		}
	}

	// ── Rate limits (workspace and user scopes) ────────────────────────────────

	// rateLimits accumulates both admin-owned scope entries and key-scope entries.
	// Admin-owned entries come from ConfigPayload.rate_limits (1-2 segment scopes).
	// Key-scope entries come from Key.rate_limit_rules (3-segment scope = key ID).
	// The proto path never carries tier definitions; Tiers is left empty.
	rateLimits := make(map[string][]RawRateLimitPolicyEntry)
	for _, scope := range p.RateLimits {
		id := str(scope.ScopeIdx)
		rules, err := rawRateLimitPolicyEntriesFromProto(scope.Rules, str)
		if err != nil {
			return nil, fmt.Errorf("rate_limit.policies scope %q: %w", id, err)
		}
		rateLimits[id] = rules
	}

	// ── Keys ──────────────────────────────────────────────────────────────────

	keys := make(map[string]RawKey, len(p.Keys))
	for _, k := range p.Keys {
		id := str(k.IdIdx)
		if id == "" {
			return nil, fmt.Errorf("key with zero id_idx")
		}
		overrides := make(map[string]RawRoutingNode, len(k.RoutingOverrides))
		for _, ro := range k.RoutingOverrides {
			modelID := str(ro.ModelIdx)
			node, err := routingNodeFromProto(ro.Node, str)
			if err != nil {
				return nil, fmt.Errorf("key %q routing override for model %q: %w", id, modelID, err)
			}
			overrides[modelID] = node
		}
		keys[id] = RawKey{RoutingOverrides: overrides}

		// Key-scope rate limit rules: stored under the key ID so compile() Phase 4
		// can find them via raw.RateLimit.Policies[keyID].
		if len(k.RateLimitRules) > 0 {
			rules, err := rawRateLimitPolicyEntriesFromProto(k.RateLimitRules, str)
			if err != nil {
				return nil, fmt.Errorf("key %q rate_limit_rules: %w", id, err)
			}
			rateLimits[id] = rules
		}
	}

	return &RawConfig{
		LLM:       RawLLM{Providers: providers, Models: models},
		MCP:       RawMCP{Servers: servers},
		Profiles:  profiles,
		Keys:      keys,
		RateLimit: RawRateLimit{Policies: rateLimits},
	}, nil
}

// ── Routing tree expansion ────────────────────────────────────────────────────

// routingNodeFromProto recursively converts a proto RoutingNode into a
// RawRoutingNode. Exactly one of target/chain/split must be set; a nil or
// empty oneof is an error.
func routingNodeFromProto(n *configv1.RoutingNode, str func(uint32) string) (RawRoutingNode, error) {
	if n == nil {
		return RawRoutingNode{}, fmt.Errorf("nil routing node")
	}
	switch kind := n.Kind.(type) {
	case *configv1.RoutingNode_Target:
		if kind.Target == nil {
			return RawRoutingNode{}, fmt.Errorf("target routing node with nil target")
		}
		return RawRoutingNode{
			Target: &RawRoutingTarget{
				Provider: str(kind.Target.ProviderIdx),
				Name:     str(kind.Target.NameIdx),
			},
		}, nil

	case *configv1.RoutingNode_Chain:
		chain := kind.Chain
		if chain == nil {
			return RawRoutingNode{}, fmt.Errorf("chain routing node with nil chain")
		}
		children := make([]RawRoutingNode, len(chain.Children))
		for i, child := range chain.Children {
			c, err := routingNodeFromProto(child, str)
			if err != nil {
				return RawRoutingNode{}, fmt.Errorf("chain child %d: %w", i, err)
			}
			children[i] = c
		}
		raw := RawChain{Children: children}
		if chain.Retry != nil && (chain.Retry.RetryOnIdx != 0 || chain.Retry.PerTryTimeoutMs != 0) {
			raw.Retry = &RawRetry{
				RetryOn:         str(chain.Retry.RetryOnIdx),
				PerTryTimeoutMs: int(chain.Retry.PerTryTimeoutMs),
			}
		}
		return RawRoutingNode{Chain: &raw}, nil

	case *configv1.RoutingNode_Split:
		split := kind.Split
		if split == nil {
			return RawRoutingNode{}, fmt.Errorf("split routing node with nil split")
		}
		children := make([]RawSplitChild, len(split.Children))
		for i, child := range split.Children {
			if child.Node == nil {
				return RawRoutingNode{}, fmt.Errorf("split child %d: nil node", i)
			}
			node, err := routingNodeFromProto(child.Node, str)
			if err != nil {
				return RawRoutingNode{}, fmt.Errorf("split child %d: %w", i, err)
			}
			children[i] = RawSplitChild{
				Weight:         int(child.Weight),
				RawRoutingNode: node,
			}
		}
		return RawRoutingNode{Split: &RawSplit{Children: children}}, nil

	default:
		// nil oneof or unknown variant
		return RawRoutingNode{}, fmt.Errorf("routing node has no kind set (nil oneof)")
	}
}

// ── Rate limit rule expansion ─────────────────────────────────────────────────

// rawRateLimitPolicyEntriesFromProto converts a slice of proto RateLimitRule
// messages into RawRateLimitPolicyEntry values. The proto path never carries
// tier names (Rule is always empty — entries are already expanded server-side).
// USD double fields are converted to decimal.Decimal via decimal.NewFromFloat.
// OnExceed_UNSPECIFIED is converted to "" so compile() can default it to "reject".
func rawRateLimitPolicyEntriesFromProto(rules []*configv1.RateLimitRule, str func(uint32) string) ([]RawRateLimitPolicyEntry, error) {
	out := make([]RawRateLimitPolicyEntry, 0, len(rules))
	for _, r := range rules {
		models := make([]string, len(r.ModelIdxs))
		for i, idx := range r.ModelIdxs {
			models[i] = str(idx)
		}
		out = append(out, RawRateLimitPolicyEntry{
			Models: models,

			USDPerMinute: decimal.NewFromFloat(r.UsdPerMinute),
			USDPerHour:   decimal.NewFromFloat(r.UsdPerHour),
			USDPerDay:    decimal.NewFromFloat(r.UsdPerDay),

			RPM: int(r.Rpm),
			RPH: int(r.Rph),
			RPD: int(r.Rpd),

			InputTokensPerMinute: int(r.InputTokensPerMinute),
			InputTokensPerHour:   int(r.InputTokensPerHour),
			InputTokensPerDay:    int(r.InputTokensPerDay),

			OutputTokensPerMinute: int(r.OutputTokensPerMinute),
			OutputTokensPerHour:   int(r.OutputTokensPerHour),
			OutputTokensPerDay:    int(r.OutputTokensPerDay),

			CacheReadTokensPerHour: int(r.CacheReadTokensPerHour),
			CacheReadTokensPerDay:  int(r.CacheReadTokensPerDay),

			CacheWriteTokensPerHour: int(r.CacheWriteTokensPerHour),
			CacheWriteTokensPerDay:  int(r.CacheWriteTokensPerDay),

			OnExceed: onExceedToString(r.OnExceed),
		})
	}
	return out, nil
}

// ── Enum converters ───────────────────────────────────────────────────────────
// Each function maps a proto enum to the lowercase string compile() expects.
// UNSPECIFIED and unknown values return "" so that compile() can apply its own
// defaults or reject with a descriptive error.

func providerKindToString(k configv1.ProviderKind) string {
	switch k {
	case configv1.ProviderKind_PROVIDER_KIND_ANTHROPIC:
		return "anthropic"
	case configv1.ProviderKind_PROVIDER_KIND_OPENAI:
		return "openai"
	case configv1.ProviderKind_PROVIDER_KIND_BEDROCK:
		return "bedrock"
	default:
		return "" // UNSPECIFIED or future value — compile() will reject
	}
}

func authTypeToString(t configv1.AuthType) string {
	switch t {
	case configv1.AuthType_AUTH_TYPE_BEARER:
		return "bearer"
	case configv1.AuthType_AUTH_TYPE_ANTHROPIC:
		return "anthropic"
	case configv1.AuthType_AUTH_TYPE_GCP:
		return "gcp"
	case configv1.AuthType_AUTH_TYPE_AWS:
		return "aws"
	case configv1.AuthType_AUTH_TYPE_GEMINI:
		return "gemini"
	default:
		return "" // UNSPECIFIED — caller (compile/request handler) interprets as "no auth"
	}
}

// onExceedToString converts the OnExceed proto enum to the lowercase string
// compile() expects. ON_EXCEED_UNSPECIFIED returns "" so compile() can default
// to "reject" per the rate-limit rule specification.
func onExceedToString(o configv1.OnExceed) string {
	switch o {
	case configv1.OnExceed_ON_EXCEED_REJECT:
		return "reject"
	case configv1.OnExceed_ON_EXCEED_THROTTLE:
		return "throttle"
	case configv1.OnExceed_ON_EXCEED_LOG_ONLY:
		return "log_only"
	default:
		return "" // ON_EXCEED_UNSPECIFIED → compile() defaults to "reject"
	}
}
