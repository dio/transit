package resources

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/types/known/timestamppb"

	adminv1 "github.com/dio/transit/examples/orange/api/orange/config/admin/v1"
	adminv1connect "github.com/dio/transit/examples/orange/api/orange/config/admin/v1/adminv1connect"
	configv1 "github.com/dio/transit/examples/orange/api/orange/config/v1"
	configv1connect "github.com/dio/transit/examples/orange/api/orange/config/v1/configv1connect"
	"github.com/dio/transit/examples/orange/internal/config"
	"github.com/dio/transit/examples/orange/internal/server/egressauth"
)

// ConfigService implements both ConfigAdminServiceHandler (management-plane
// publish/list/get/rollback) and SnapshotServiceHandler (data-plane fetch).
// All unimplemented admin RPCs delegate to the embedded stub.
type ConfigService struct {
	adminv1connect.UnimplementedConfigAdminServiceHandler
	store    config.SnapshotStore
	resolver config.HierarchyResolver // optional; enables three-level hierarchy merge
	rl       *rateLimitDB             // nil until InitRateLimit is called
	logger   *slog.Logger
}

// InitRateLimit creates the rate-limit DB tables if they do not exist and
// enables the tier and scope management RPCs. Call once during server setup.
func (s *ConfigService) InitRateLimit(ctx context.Context, pool *pgxpool.Pool) error {
	rl, err := newRateLimitDB(ctx, pool, s.logger)
	if err != nil {
		return fmt.Errorf("init rate limit db: %w", err)
	}
	s.rl = rl
	return nil
}

// Compile-time interface assertions.
var (
	_ adminv1connect.ConfigAdminServiceHandler = (*ConfigService)(nil)
	_ configv1connect.SnapshotServiceHandler   = (*ConfigService)(nil)
)

// NewConfigService returns a ConfigService backed by store.
func NewConfigService(store config.SnapshotStore, logger *slog.Logger) *ConfigService {
	return &ConfigService{store: store, logger: logger}
}

// SetHierarchyResolver enables three-level (org → project → workspace) config
// merging at Fetch time. Call once during server setup, before serving traffic.
func (s *ConfigService) SetHierarchyResolver(r config.HierarchyResolver) {
	s.resolver = r
}

// ── ConfigAdminService ────────────────────────────────────────────────────────

// PublishSnapshot validates the YAML config, compiles it into a snapshot
// envelope, and stores it for the workspace.
func (s *ConfigService) PublishSnapshot(ctx context.Context, req *connect.Request[adminv1.PublishSnapshotRequest]) (*connect.Response[adminv1.PublishSnapshotResponse], error) {
	wsID := req.Msg.GetWorkspaceId()
	if wsID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("workspace_id is required"))
	}
	yamlBytes := []byte(req.Msg.GetYamlConfig())
	if len(yamlBytes) == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("yaml_config is required"))
	}
	publishedBy := req.Msg.GetPublishedBy()
	if publishedBy == "" {
		publishedBy = "unknown"
	}

	// Validate config; store compile error for audit even on failure.
	tmpState := config.NewAppState()
	compileErr := tmpState.ValidateConfig(yamlBytes)

	sum := sha256.Sum256(yamlBytes)
	checksum := sum[:]

	nextVer, err := s.store.NextVersion(ctx, wsID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("next version: %w", err))
	}

	env := &config.SnapshotEnvelope{
		Version:     nextVer,
		Format:      config.SnapshotFormatYAML,
		Compression: config.CompressionNone,
		Payload:     yamlBytes,
		Checksum:    checksum,
	}
	if err := s.store.Store(ctx, env, wsID, publishedBy, compileErr); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("store snapshot: %w", err))
	}

	s.logger.Info("snapshot published",
		"workspace_id", wsID,
		"version", nextVer,
		"compiled_ok", compileErr == nil,
		"published_by", publishedBy,
	)

	var compileErrPtr *string
	if compileErr != nil {
		msg := compileErr.Error()
		compileErrPtr = &msg
	}
	now := timestamppb.New(time.Now().UTC())
	meta := &adminv1.SnapshotMeta{
		WorkspaceId:  wsID,
		Version:      nextVer,
		Format:       string(config.SnapshotFormatYAML),
		Compression:  string(config.CompressionNone),
		ByteSize:     int32(len(yamlBytes)),
		Checksum:     checksum,
		CompiledOk:   compileErr == nil,
		CompileError: compileErrPtr,
		CreatedBy:    publishedBy,
		CreatedAt:    now,
	}
	return connect.NewResponse(&adminv1.PublishSnapshotResponse{Snapshot: meta}), nil
}

// ListSnapshots returns snapshot metadata for a workspace in descending version
// order. The page token is the lowest version from the previous page.
func (s *ConfigService) ListSnapshots(ctx context.Context, req *connect.Request[adminv1.ListSnapshotsRequest]) (*connect.Response[adminv1.ListSnapshotsResponse], error) {
	wsID := req.Msg.GetWorkspaceId()
	if wsID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("workspace_id is required"))
	}

	limit := int(req.Msg.GetLimit())
	if limit <= 0 || limit > 500 {
		limit = 50
	}

	var afterVersion uint64
	if pt := req.Msg.GetPageToken(); pt != "" {
		v, err := strconv.ParseUint(pt, 10, 64)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid page_token: %w", err))
		}
		afterVersion = v
	}

	entries, err := s.store.List(ctx, wsID, limit, afterVersion)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	metas := make([]*adminv1.SnapshotMeta, 0, len(entries))
	for _, e := range entries {
		metas = append(metas, entryToMeta(wsID, e))
	}

	var nextPageToken string
	if len(entries) == limit {
		// Use the lowest version on this page as the cursor for the next page.
		nextPageToken = strconv.FormatUint(entries[len(entries)-1].Envelope.Version, 10)
	}

	return connect.NewResponse(&adminv1.ListSnapshotsResponse{
		Snapshots:     metas,
		NextPageToken: nextPageToken,
	}), nil
}

// GetSnapshot returns the full envelope for a specific version.
func (s *ConfigService) GetSnapshot(ctx context.Context, req *connect.Request[adminv1.GetSnapshotRequest]) (*connect.Response[adminv1.GetSnapshotResponse], error) {
	wsID := req.Msg.GetWorkspaceId()
	if wsID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("workspace_id is required"))
	}
	targetVer := req.Msg.GetVersion()
	if targetVer == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("version must be > 0"))
	}

	// Use List with cursor=targetVer+1, limit=1 to get full metadata including
	// compiled_ok/compile_error/created_at for both OK and failed entries.
	entries, err := s.store.List(ctx, wsID, 1, targetVer+1)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if len(entries) == 0 || entries[0].Envelope.Version != targetVer {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("snapshot version %d not found", targetVer))
	}

	e := entries[0]
	resp := &adminv1.GetSnapshotResponse{Snapshot: entryToMeta(wsID, e)}
	if e.Envelope.Format == config.SnapshotFormatYAML {
		p := string(e.Envelope.Payload)
		resp.YamlPayload = &p
	}
	return connect.NewResponse(resp), nil
}

// RollbackSnapshot re-publishes a prior version's payload as a new snapshot
// with an incremented version number.
func (s *ConfigService) RollbackSnapshot(ctx context.Context, req *connect.Request[adminv1.RollbackSnapshotRequest]) (*connect.Response[adminv1.RollbackSnapshotResponse], error) {
	wsID := req.Msg.GetWorkspaceId()
	if wsID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("workspace_id is required"))
	}
	targetVer := req.Msg.GetTargetVersion()
	if targetVer == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("target_version must be > 0"))
	}
	rolledBackBy := req.Msg.GetRolledBackBy()
	if rolledBackBy == "" {
		rolledBackBy = "unknown"
	}

	src, err := s.store.FetchVersion(ctx, wsID, targetVer)
	if err != nil {
		if errors.Is(err, config.ErrSnapshotNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("snapshot version %d not found", targetVer))
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	nextVer, err := s.store.NextVersion(ctx, wsID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("next version: %w", err))
	}

	env := &config.SnapshotEnvelope{
		Version:     nextVer,
		Format:      src.Format,
		Compression: src.Compression,
		Payload:     src.Payload,
		Checksum:    src.Checksum,
	}
	if err := s.store.Store(ctx, env, wsID, rolledBackBy, nil); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("store rollback snapshot: %w", err))
	}

	s.logger.Info("snapshot rolled back",
		"workspace_id", wsID,
		"target_version", targetVer,
		"new_version", nextVer,
		"rolled_back_by", rolledBackBy,
	)

	now := timestamppb.New(time.Now().UTC())
	meta := &adminv1.SnapshotMeta{
		WorkspaceId: wsID,
		Version:     nextVer,
		Format:      string(src.Format),
		Compression: string(src.Compression),
		ByteSize:    int32(len(src.Payload)),
		Checksum:    src.Checksum,
		CompiledOk:  true,
		CreatedBy:   rolledBackBy,
		CreatedAt:   now,
	}
	return connect.NewResponse(&adminv1.RollbackSnapshotResponse{
		NewVersion: nextVer,
		Snapshot:   meta,
	}), nil
}

// ── SnapshotService ───────────────────────────────────────────────────────────

// Watch is not yet implemented; data-plane proxies should use Fetch for now.
func (s *ConfigService) Watch(_ context.Context, _ *connect.Request[configv1.WatchRequest], _ *connect.ServerStream[configv1.WatchResponse]) error {
	return connect.NewError(connect.CodeUnimplemented, errors.New("Watch is not yet implemented; use Fetch"))
}

// Fetch returns the current snapshot for a workspace. The returned payload is
// the fully-materialised, workspace-scoped config — the result of merging org,
// project, and workspace YAML configs (when a HierarchyResolver is set) and
// then projecting the merged result down to only the records that belong to
// this workspace. Staleness is detected via the projected SHA-256 checksum so
// that changes at any level in the hierarchy are surfaced to the egress.
func (s *ConfigService) Fetch(ctx context.Context, req *connect.Request[configv1.FetchRequest]) (*connect.Response[configv1.FetchResponse], error) {
	identity, ok := egressauth.EgressIdentityFromContext(ctx)
	if !ok || identity.WorkspaceID == "" {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("missing egress identity in context"))
	}
	wsID := identity.WorkspaceID
	lastChecksum := req.Msg.GetLastChecksum()

	// Always fetch the latest workspace snapshot (version 0 = no lower-bound
	// filter; we rely on the projected checksum for staleness detection instead).
	wsEnv, err := s.store.FetchLatest(ctx, wsID, 0)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	merged, err := s.buildMergedRaw(ctx, wsID, wsEnv)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if merged == nil {
		// Nothing published at any scope yet.
		return connect.NewResponse(&configv1.FetchResponse{
			Result: &configv1.FetchResponse_Unchanged{Unchanged: &configv1.Unchanged{}},
		}), nil
	}

	projected := config.ProjectForWorkspace(merged, wsID)
	payload, err := config.MarshalRawYAML(projected)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("encode projected snapshot: %w", err))
	}
	projectedSum := sha256.Sum256(payload)
	projectedChecksum := projectedSum[:]

	if len(lastChecksum) > 0 && bytes.Equal(lastChecksum, projectedChecksum) {
		return connect.NewResponse(&configv1.FetchResponse{
			Result: &configv1.FetchResponse_Unchanged{Unchanged: &configv1.Unchanged{}},
		}), nil
	}

	var version uint64
	if wsEnv != nil {
		version = wsEnv.Version
	}

	pbEnv := &configv1.SnapshotEnvelope{
		Version:     version,
		Format:      configv1.PayloadFormat_PAYLOAD_FORMAT_YAML,
		Compression: configv1.Compression_COMPRESSION_NONE,
		Payload:     payload,
		Checksum:    projectedChecksum,
	}
	return connect.NewResponse(&configv1.FetchResponse{
		Result: &configv1.FetchResponse_Snapshot{Snapshot: pbEnv},
	}), nil
}

// buildMergedRaw produces the merged RawConfig for wsID by layering org →
// project → workspace configs. Returns nil when no config has been published
// at any scope. A missing intermediate scope (org or project) is treated as an
// empty config and does not prevent serving from narrower scopes.
func (s *ConfigService) buildMergedRaw(ctx context.Context, wsID string, wsEnv *config.SnapshotEnvelope) (*config.RawConfig, error) {
	var orgRaw, projRaw, wsRaw *config.RawConfig

	if s.resolver != nil {
		hier, err := s.resolver.ResolveWorkspaceHierarchy(ctx, wsID)
		if err != nil {
			s.logger.Warn("hierarchy resolution failed; serving workspace-only config",
				"workspace_id", wsID, "err", err)
			// Don't abort — serve whatever the workspace has.
		} else {
			if hier.OrgID != "" {
				if env, err := s.store.FetchLatest(ctx, config.OrgScopeID(hier.OrgID), 0); err != nil {
					return nil, fmt.Errorf("fetch org config: %w", err)
				} else if env != nil {
					if orgRaw, err = config.DecodeRaw(*env); err != nil {
						return nil, fmt.Errorf("decode org config: %w", err)
					}
				}
			}
			if hier.ProjectID != "" {
				if env, err := s.store.FetchLatest(ctx, config.ProjectScopeID(hier.ProjectID), 0); err != nil {
					return nil, fmt.Errorf("fetch project config: %w", err)
				} else if env != nil {
					if projRaw, err = config.DecodeRaw(*env); err != nil {
						return nil, fmt.Errorf("decode project config: %w", err)
					}
				}
			}
		}
	}

	if wsEnv != nil {
		var err error
		if wsRaw, err = config.DecodeRaw(*wsEnv); err != nil {
			return nil, fmt.Errorf("decode workspace config: %w", err)
		}
	}

	if orgRaw == nil && projRaw == nil && wsRaw == nil {
		return nil, nil
	}

	return config.MergeRawConfigs(config.MergeRawConfigs(orgRaw, projRaw), wsRaw), nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

func entryToMeta(workspaceID string, e *config.SnapshotListEntry) *adminv1.SnapshotMeta {
	m := &adminv1.SnapshotMeta{
		WorkspaceId: workspaceID,
		Version:     e.Envelope.Version,
		Format:      string(e.Envelope.Format),
		Compression: string(e.Envelope.Compression),
		ByteSize:    int32(len(e.Envelope.Payload)),
		Checksum:    e.Envelope.Checksum,
		CompiledOk:  e.CompiledOK,
		CreatedBy:   e.CreatedBy,
	}
	if !e.CreatedAt.IsZero() {
		m.CreatedAt = timestamppb.New(e.CreatedAt)
	}
	if e.CompileErr != "" {
		msg := e.CompileErr
		m.CompileError = &msg
	}
	return m
}

func toProtoFormat(f config.SnapshotFormat) configv1.PayloadFormat {
	switch f {
	case config.SnapshotFormatProto:
		return configv1.PayloadFormat_PAYLOAD_FORMAT_PROTO
	case config.SnapshotFormatYAML:
		return configv1.PayloadFormat_PAYLOAD_FORMAT_YAML
	case config.SnapshotFormatJSON:
		return configv1.PayloadFormat_PAYLOAD_FORMAT_JSON
	case config.SnapshotFormatMsgpack:
		return configv1.PayloadFormat_PAYLOAD_FORMAT_MSGPACK
	default:
		return configv1.PayloadFormat_PAYLOAD_FORMAT_UNSPECIFIED
	}
}

func toProtoCompression(c config.CompressionKind) configv1.Compression {
	if c == config.CompressionZstd {
		return configv1.Compression_COMPRESSION_ZSTD
	}
	return configv1.Compression_COMPRESSION_NONE
}
