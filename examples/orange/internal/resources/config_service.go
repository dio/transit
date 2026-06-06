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
	"google.golang.org/protobuf/types/known/timestamppb"

	adminv1 "github.com/dio/transit/examples/orange/api/orange/config/admin/v1"
	adminv1connect "github.com/dio/transit/examples/orange/api/orange/config/admin/v1/adminv1connect"
	configv1 "github.com/dio/transit/examples/orange/api/orange/config/v1"
	configv1connect "github.com/dio/transit/examples/orange/api/orange/config/v1/configv1connect"
	"github.com/dio/transit/examples/orange/internal/config"
)

// ConfigService implements both ConfigAdminServiceHandler (management-plane
// publish/list/get/rollback) and SnapshotServiceHandler (data-plane fetch).
// All unimplemented admin RPCs delegate to the embedded stub.
type ConfigService struct {
	adminv1connect.UnimplementedConfigAdminServiceHandler
	store  config.SnapshotStore
	logger *slog.Logger
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
	_, compileErr := config.Load(yamlBytes)

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

// Fetch returns the current snapshot for a workspace. When the client's
// last_version and last_checksum already match the server's latest, it returns
// Unchanged to avoid re-sending the full payload.
func (s *ConfigService) Fetch(ctx context.Context, req *connect.Request[configv1.FetchRequest]) (*connect.Response[configv1.FetchResponse], error) {
	wsID := req.Msg.GetWorkspaceId()
	if wsID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("workspace_id is required"))
	}
	lastVersion := req.Msg.GetLastVersion()
	lastChecksum := req.Msg.GetLastChecksum()

	env, err := s.store.FetchLatest(ctx, wsID, lastVersion)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if env == nil {
		// No newer version than what the client already has.
		return connect.NewResponse(&configv1.FetchResponse{
			Result: &configv1.FetchResponse_Unchanged{
				Unchanged: &configv1.Unchanged{},
			},
		}), nil
	}

	// Even if there is a newer version, skip re-sending if the checksum matches.
	if len(lastChecksum) > 0 && bytes.Equal(lastChecksum, env.Checksum) {
		return connect.NewResponse(&configv1.FetchResponse{
			Result: &configv1.FetchResponse_Unchanged{
				Unchanged: &configv1.Unchanged{},
			},
		}), nil
	}

	pbEnv := &configv1.SnapshotEnvelope{
		Version:     env.Version,
		Format:      toProtoFormat(env.Format),
		Compression: toProtoCompression(env.Compression),
		Payload:     env.Payload,
		Checksum:    env.Checksum,
	}
	return connect.NewResponse(&configv1.FetchResponse{
		Result: &configv1.FetchResponse_Snapshot{
			Snapshot: pbEnv,
		},
	}), nil
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
