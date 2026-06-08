// Package secret implements SecretAdminService: the management plane for
// the orange secret lifecycle.
//
// The service wires three layers:
//   - kms.MasterKEKProvider: retrieves the MASTER_KEK from an external source
//   - crypto.Encryptor: XChaCha20-Poly1305 + HKDF-SHA256 seal/open
//   - store.SecretStore: versioned persistence for Key and Secret records
//
// # Key hierarchy
//
//	MASTER_KEK  (external provider — never touches the database)
//	   ↓ wraps
//	SERVICE_KEK (stored in realm="system"; see KEKMode for selection strategy)
//	   ↓ wraps
//	DEK         (one per secret version)
//	   ↓ encrypts
//	Secret material
//
// # SERVICE_KEK modes
//
// The default mode is KEKModePooled. A small pool of SERVICE_KEKs is shared
// across all realm boundaries so that MASTER_KEK is only accessed once —
// when the pool is first populated — and never again at runtime. Set
// Config.PoolSize to have the pool auto-provisioned on the first CreateVersion
// call.
//
// KEKModePerBoundary provisions a dedicated SERVICE_KEK per canonical realm
// ("org/<uuid>/purpose", "proj/<uuid>/purpose", "ws/<uuid>/purpose"). Stronger
// isolation; requires MASTER_KEK access for every new boundary.
package secret

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	adminv1 "github.com/dio/transit/examples/orange/api/orange/secret/admin/v1"
	"github.com/dio/transit/examples/orange/api/orange/secret/admin/v1/adminv1connect"
	"github.com/dio/transit/examples/orange/internal/server/secret/crypto"
	"github.com/dio/transit/examples/orange/internal/server/secret/kms"
	"github.com/dio/transit/examples/orange/internal/server/secret/store"
)

const (
	systemRealm      = "system"
	poolMemberPrefix = "svc-kek-pool-"
)

// KEKMode controls how SERVICE_KEKs are selected when creating DEKs.
type KEKMode string

const (
	KEKModePooled      KEKMode = "pooled"
	KEKModePerBoundary KEKMode = "per-boundary"
)

type svcKEKEntry struct {
	raw     []byte
	version int
}

// Service implements SecretAdminService.
type Service struct {
	adminv1connect.UnimplementedSecretAdminServiceHandler

	provider  kms.MasterKEKProvider
	encryptor crypto.Encryptor
	st        store.SecretStore
	log       *slog.Logger
	kekMode   KEKMode
	poolSize  int

	// svcKEKCache caches decrypted plaintext SERVICE_KEK material keyed by "kekID:version".
	svcKEKCache sync.Map
}

// Config holds the dependencies for a Service.
type Config struct {
	Provider  kms.MasterKEKProvider
	Encryptor crypto.Encryptor
	Store     store.SecretStore
	Logger    *slog.Logger
	KEKMode   KEKMode
	// PoolSize is the minimum number of active pool SERVICE_KEKs to maintain
	// when KEKMode == KEKModePooled. Defaults to 1.
	PoolSize int
}

// New creates a Service. No KMS call is made at construction time.
func New(_ context.Context, cfg Config) (*Service, error) {
	mode := cfg.KEKMode
	if mode == "" {
		mode = KEKModePooled
	}
	poolSize := cfg.PoolSize
	if mode == KEKModePooled && poolSize == 0 {
		poolSize = 1
	}
	return &Service{
		provider:  cfg.Provider,
		encryptor: cfg.Encryptor,
		st:        cfg.Store,
		log:       cfg.Logger,
		kekMode:   mode,
		poolSize:  poolSize,
	}, nil
}

// ResolveSecret decrypts the latest-enabled secret version. realm must be a
// canonical realm string ("org/<uuid>/…", "proj/<uuid>/…", or "ws/<uuid>/…").
// wsID/projID/orgID are the egress ancestry — the realm must fall under one of
// those levels or the call is rejected with a permission error.
func (s *Service) ResolveSecret(ctx context.Context, realm, secretID, wsID, projID, orgID string) (material []byte, versionID, checksum string, err error) {
	if !RealmInAncestry(realm, wsID, projID, orgID) {
		return nil, "", "", fmt.Errorf("realm %q is not accessible from egress ancestry (ws=%s proj=%s org=%s)", realm, wsID, projID, orgID)
	}
	sv, err := s.st.GetLatestEnabledSecret(ctx, realm, secretID)
	if err != nil {
		return nil, "", "", err
	}
	rawDEK, err := s.unwrapDEK(ctx, sv.DEKID, sv.DEKVersion)
	if err != nil {
		return nil, "", "", fmt.Errorf("unwrap DEK: %w", err)
	}
	defer crypto.Zeroize(rawDEK)
	blob, err := base64ToBlob(sv.Ciphertext)
	if err != nil {
		return nil, "", "", fmt.Errorf("decode ciphertext: %w", err)
	}
	aad := crypto.SecretAAD(sv.Realm, sv.Name, sv.VersionID, sv.DEKID, strconv.Itoa(sv.DEKVersion))
	plain, err := s.encryptor.Open(crypto.ContextData, rawDEK, blob, aad)
	if err != nil {
		return nil, "", "", fmt.Errorf("decrypt: %w", err)
	}
	return plain, sv.VersionID, sv.Checksum, nil
}

// --- RPC handlers ---

func (s *Service) CreateServiceKEK(ctx context.Context, req *connect.Request[adminv1.CreateServiceKEKRequest]) (*connect.Response[adminv1.CreateServiceKEKResponse], error) {
	r := req.Msg
	// Empty realm = new pool member. Non-empty realm = per-boundary KEK; realm
	// must be canonical ("org/<uuid>/…", "proj/<uuid>/…", "ws/<uuid>/…").
	isPool := r.Realm == ""
	if !isPool {
		if _, _, _, err := ParseRealm(r.Realm); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
	}

	var kekID string
	var entry *svcKEKEntry
	var err error

	if isPool {
		kekID, entry, err = s.createPoolMember(ctx)
	} else {
		kekID, entry, err = s.getOrCreateServiceKEK(ctx, r.Realm)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&adminv1.CreateServiceKEKResponse{
		KekId:      kekID,
		KekVersion: int32(entry.version),
	}), nil
}

func (s *Service) RotateServiceKEK(ctx context.Context, req *connect.Request[adminv1.RotateServiceKEKRequest]) (*connect.Response[adminv1.RotateServiceKEKResponse], error) {
	r := req.Msg
	isPool := r.Realm == ""
	if !isPool {
		if _, _, _, err := ParseRealm(r.Realm); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
	}

	var targets []*store.Key
	if isPool {
		members, err := s.st.ListKeysByPrefix(ctx, poolMemberPrefix)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list pool members: %w", err))
		}
		targets = members
	} else {
		kekID := svcKEKID(r.Realm)
		versions, err := s.st.ListKeyVersions(ctx, kekID)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list SERVICE_KEK versions: %w", err))
		}
		var latest *store.Key
		for i := len(versions) - 1; i >= 0; i-- {
			if versions[i].State == store.KeyStateActive {
				latest = versions[i]
				break
			}
		}
		if latest == nil {
			return nil, connect.NewError(connect.CodeNotFound,
				fmt.Errorf("no active SERVICE_KEK for realm=%q", r.Realm))
		}
		targets = []*store.Key{latest}
	}

	rotated := make([]*adminv1.RotatedKEK, 0, len(targets))

	for _, k := range targets {
		plain, err := s.decryptServiceKEKRecord(ctx, k)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal,
				fmt.Errorf("unwrap SERVICE_KEK %s v%d: %w", k.ID, k.Version, err))
		}

		masterKey, masterVer, err := s.provider.MasterKEK(ctx, kms.LatestVersion)
		if err != nil {
			crypto.Zeroize(plain)
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get MASTER_KEK: %w", err))
		}

		newVer := k.Version + 1
		aad := crypto.ServiceKEKWrapAAD(k.ID, strconv.Itoa(newVer), "master", strconv.Itoa(masterVer))
		blob, err := s.encryptor.Seal(crypto.ContextWrap, masterKey, plain, aad)
		crypto.Zeroize(masterKey)
		if err != nil {
			crypto.Zeroize(plain)
			return nil, connect.NewError(connect.CodeInternal,
				fmt.Errorf("seal SERVICE_KEK %s v%d: %w", k.ID, newVer, err))
		}

		putErr := s.st.PutKey(ctx, &store.Key{
			ID:              k.ID,
			Version:         newVer,
			Purpose:         store.PurposeServiceKEK,
			Realm:           systemRealm,
			State:           store.KeyStateActive,
			ParentID:        "master",
			ParentVersion:   masterVer,
			WrappedMaterial: blobToBase64(blob),
			CreatedAt:       time.Now(),
		})
		if putErr != nil {
			crypto.Zeroize(plain)
			return nil, connect.NewError(connect.CodeInternal,
				fmt.Errorf("put SERVICE_KEK %s v%d: %w", k.ID, newVer, putErr))
		}

		s.svcKEKCache.Store(k.ID+":"+strconv.Itoa(newVer), &svcKEKEntry{raw: plain, version: newVer})
		s.log.Info("secretstore: rotated SERVICE_KEK",
			"kek_id", k.ID,
			"old_version", k.Version,
			"new_version", newVer,
			"master_kek_version", masterVer,
		)

		rotated = append(rotated, &adminv1.RotatedKEK{
			KekId:            k.ID,
			OldVersion:       int32(k.Version),
			NewVersion:       int32(newVer),
			MasterKekVersion: int32(masterVer),
		})
	}

	return connect.NewResponse(&adminv1.RotateServiceKEKResponse{Rotated: rotated}), nil
}

func (s *Service) CreateVersion(ctx context.Context, req *connect.Request[adminv1.CreateVersionRequest]) (*connect.Response[adminv1.CreateVersionResponse], error) {
	r := req.Msg
	if r.Realm == "" || r.SecretId == "" || len(r.Material) == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("realm, secret_id, and material are required"))
	}
	if _, _, _, err := ParseRealm(r.Realm); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	dekID, dekVer, rawDEK, err := s.createDEK(ctx, r.Realm)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer crypto.Zeroize(rawDEK)

	versionID := newUUID7()
	checksum := crypto.SHA256Hex(r.Material)
	aad := crypto.SecretAAD(r.Realm, r.SecretId, versionID, dekID, strconv.Itoa(dekVer))
	blob, err := s.encryptor.Seal(crypto.ContextData, rawDEK, r.Material, aad)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("seal material: %w", err))
	}

	now := time.Now()
	caller := callerIdentity(ctx)
	sv := &store.Secret{
		Realm:      r.Realm,
		Name:       r.SecretId,
		VersionID:  versionID,
		DEKID:      dekID,
		DEKVersion: dekVer,
		Ciphertext: blobToBase64(blob),
		Checksum:   checksum,
		State:      store.VersionStateDisabled,
		CreatedAt:  now,
		CreatedBy:  caller,
	}
	if r.Enable {
		sv.State = store.VersionStateEnabled
		sv.EnabledAt = &now
		sv.EnabledBy = caller
	}

	if err := s.st.PutSecret(ctx, sv); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("put secret: %w", err))
	}
	return connect.NewResponse(&adminv1.CreateVersionResponse{Version: toProto(sv, r.Material)}), nil
}

func (s *Service) EnableVersion(ctx context.Context, req *connect.Request[adminv1.EnableVersionRequest]) (*connect.Response[adminv1.EnableVersionResponse], error) {
	r := req.Msg
	sv, err := s.st.GetSecretVersion(ctx, r.Realm, r.SecretId, r.VersionId)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	if sv.State == store.VersionStateRetired {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("version %s is retired and cannot be re-enabled", r.VersionId))
	}

	now := time.Now()
	sv.State = store.VersionStateEnabled
	sv.EnabledAt = &now
	sv.EnabledBy = callerIdentity(ctx)

	if err := s.st.PutSecret(ctx, sv); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&adminv1.EnableVersionResponse{Version: toProto(sv, nil)}), nil
}

func (s *Service) DisableVersion(ctx context.Context, req *connect.Request[adminv1.DisableVersionRequest]) (*connect.Response[adminv1.DisableVersionResponse], error) {
	r := req.Msg
	sv, err := s.st.GetSecretVersion(ctx, r.Realm, r.SecretId, r.VersionId)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	if sv.State == store.VersionStateRetired {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("version %s is retired and cannot be disabled", r.VersionId))
	}

	now := time.Now()
	sv.State = store.VersionStateDisabled
	sv.DisabledAt = &now
	sv.DisabledBy = callerIdentity(ctx)

	if err := s.st.PutSecret(ctx, sv); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&adminv1.DisableVersionResponse{Version: toProto(sv, nil)}), nil
}

func (s *Service) RetireVersion(ctx context.Context, req *connect.Request[adminv1.RetireVersionRequest]) (*connect.Response[adminv1.RetireVersionResponse], error) {
	r := req.Msg
	sv, err := s.st.GetSecretVersion(ctx, r.Realm, r.SecretId, r.VersionId)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	if sv.State == store.VersionStateRetired {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("version %s is already retired", r.VersionId))
	}

	now := time.Now()
	sv.State = store.VersionStateRetired
	sv.RetiredAt = &now
	sv.RetiredBy = callerIdentity(ctx)
	if r.Shred {
		sv.Ciphertext = ""
		sv.ShreddedAt = &now
	}

	if err := s.st.PutSecret(ctx, sv); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&adminv1.RetireVersionResponse{Version: toProto(sv, nil)}), nil
}

func (s *Service) ResolveVersion(ctx context.Context, req *connect.Request[adminv1.ResolveVersionRequest]) (*connect.Response[adminv1.ResolveVersionResponse], error) {
	r := req.Msg
	sv, err := s.st.GetLatestEnabledSecret(ctx, r.Realm, r.SecretId)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}

	rawDEK, err := s.unwrapDEK(ctx, sv.DEKID, sv.DEKVersion)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("unwrap DEK: %w", err))
	}
	defer crypto.Zeroize(rawDEK)

	blob, err := base64ToBlob(sv.Ciphertext)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("decode ciphertext: %w", err))
	}
	aad := crypto.SecretAAD(sv.Realm, sv.Name, sv.VersionID, sv.DEKID, strconv.Itoa(sv.DEKVersion))
	material, err := s.encryptor.Open(crypto.ContextData, rawDEK, blob, aad)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("decrypt material: %w", err))
	}

	return connect.NewResponse(&adminv1.ResolveVersionResponse{Version: toProto(sv, material)}), nil
}

func (s *Service) ListVersions(ctx context.Context, req *connect.Request[adminv1.ListVersionsRequest]) (*connect.Response[adminv1.ListVersionsResponse], error) {
	r := req.Msg
	versions, err := s.st.ListSecretVersions(ctx, r.Realm, r.SecretId)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	protos := make([]*adminv1.SecretVersion, 0, len(versions))
	for _, sv := range versions {
		protos = append(protos, toProto(sv, nil))
	}
	return connect.NewResponse(&adminv1.ListVersionsResponse{Versions: protos}), nil
}

func (s *Service) ListSecrets(ctx context.Context, req *connect.Request[adminv1.ListSecretsRequest]) (*connect.Response[adminv1.ListSecretsResponse], error) {
	r := req.Msg
	// r.Realm is used as a prefix filter. Empty = list all; a canonical realm
	// prefix like "org/<uuid>/" lists secrets across that scope.
	ids, err := s.st.ListSecrets(ctx, r.Realm)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	summaries := make([]*adminv1.SecretSummary, 0, len(ids))
	for _, id := range ids {
		summaries = append(summaries, &adminv1.SecretSummary{
			Realm:    id.Realm,
			SecretId: id.Name,
		})
	}
	return connect.NewResponse(&adminv1.ListSecretsResponse{Secrets: summaries}), nil
}

// --- internal helpers ---

func (s *Service) getOrCreateServiceKEK(ctx context.Context, realm string) (string, *svcKEKEntry, error) {
	kekID := svcKEKID(realm)

	load := func() (*svcKEKEntry, error) {
		versions, err := s.st.ListKeyVersions(ctx, kekID)
		if err != nil {
			return nil, err
		}
		for i := len(versions) - 1; i >= 0; i-- {
			if versions[i].State == store.KeyStateActive {
				return s.loadOrCacheKEK(ctx, versions[i])
			}
		}
		return nil, nil
	}

	if e, err := load(); err != nil || e != nil {
		return kekID, e, err
	}

	versions, err := s.st.ListKeyVersions(ctx, kekID)
	if err != nil {
		return "", nil, err
	}
	e, err := s.generateServiceKEK(ctx, kekID, realm, len(versions)+1)
	if err != nil && !errors.Is(err, store.ErrKeyExists) {
		return "", nil, err
	}
	if e != nil {
		cacheKey := kekID + ":" + strconv.Itoa(e.version)
		s.svcKEKCache.Store(cacheKey, e)
		return kekID, e, nil
	}
	e, err = load()
	if err != nil {
		return "", nil, err
	}
	if e == nil {
		return "", nil, fmt.Errorf("SERVICE_KEK %s: created by peer but not found in store", kekID)
	}
	return kekID, e, nil
}

func (s *Service) selectPoolKEK(ctx context.Context) (string, *svcKEKEntry, error) {
	members, err := s.st.ListKeysByPrefix(ctx, poolMemberPrefix)
	if err != nil {
		return "", nil, fmt.Errorf("list pool SERVICE_KEKs: %w", err)
	}
	for s.poolSize > 0 && len(members) < s.poolSize {
		if _, _, err := s.createPoolMember(ctx); err != nil {
			return "", nil, fmt.Errorf("auto-provision pool member: %w", err)
		}
		if members, err = s.st.ListKeysByPrefix(ctx, poolMemberPrefix); err != nil {
			return "", nil, fmt.Errorf("list pool SERVICE_KEKs: %w", err)
		}
	}
	if len(members) == 0 {
		return "", nil, fmt.Errorf("no active pool SERVICE_KEKs; provision some via CreateServiceKEK")
	}
	k := members[randIntn(len(members))]
	entry, err := s.loadOrCacheKEK(ctx, k)
	if err != nil {
		return "", nil, err
	}
	return k.ID, entry, nil
}

func (s *Service) createPoolMember(ctx context.Context) (string, *svcKEKEntry, error) {
	seq, err := s.st.AllocatePoolSeq(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("allocate pool seq: %w", err)
	}
	id := poolMemberID(seq)
	e, err := s.generateServiceKEK(ctx, id, systemRealm, 1)
	if err != nil {
		return "", nil, err
	}
	s.svcKEKCache.Store(id+":1", e)
	return id, e, nil
}

func poolMemberID(n int) string { return poolMemberPrefix + strconv.Itoa(n) }

// svcKEKID returns the stable identifier for a per-boundary SERVICE_KEK.
// realm is the canonical realm string ("org/<uuid>/api-keys", etc.).
func svcKEKID(realm string) string { return "svc-kek-" + realm }

func randIntn(n int) int {
	if n <= 1 {
		return 0
	}
	b := make([]byte, 8)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		panic(fmt.Sprintf("secretstore: rand read: %v", err))
	}
	v := int(b[0]) | int(b[1])<<8 | int(b[2])<<16 | int(b[3])<<24 |
		int(b[4])<<32 | int(b[5])<<40 | int(b[6])<<48 | int(b[7])<<56
	if v < 0 {
		v = -v
	}
	return v % n
}

func (s *Service) loadOrCacheKEK(ctx context.Context, k *store.Key) (*svcKEKEntry, error) {
	cacheKey := k.ID + ":" + strconv.Itoa(k.Version)
	if v, ok := s.svcKEKCache.Load(cacheKey); ok {
		e, ok := v.(*svcKEKEntry)
		if !ok {
			return nil, fmt.Errorf("invalid cache entry type: %T", v)
		}
		return e, nil
	}
	raw, err := s.decryptServiceKEKRecord(ctx, k)
	if err != nil {
		return nil, fmt.Errorf("unwrap SERVICE_KEK %s v%d: %w", k.ID, k.Version, err)
	}
	e := &svcKEKEntry{raw: raw, version: k.Version}
	s.svcKEKCache.Store(cacheKey, e)
	s.log.Info("secretstore: loaded SERVICE_KEK", "kek_id", k.ID, "version", k.Version)
	return e, nil
}

func (s *Service) getServiceKEKVersion(ctx context.Context, kekID string, version int) ([]byte, error) {
	cacheKey := kekID + ":" + strconv.Itoa(version)
	if v, ok := s.svcKEKCache.Load(cacheKey); ok {
		e, ok := v.(*svcKEKEntry)
		if !ok {
			return nil, fmt.Errorf("invalid cache entry type: %T", v)
		}
		raw := make([]byte, len(e.raw))
		copy(raw, e.raw)
		return raw, nil
	}
	k, err := s.st.GetKey(ctx, kekID, version)
	if err != nil {
		return nil, fmt.Errorf("get SERVICE_KEK %s@%d: %w", kekID, version, err)
	}
	return s.decryptServiceKEKRecord(ctx, k)
}

func (s *Service) generateServiceKEK(ctx context.Context, kekID, realm string, version int) (*svcKEKEntry, error) {
	rawKEK := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, rawKEK); err != nil {
		return nil, fmt.Errorf("rand SERVICE_KEK: %w", err)
	}

	masterKey, masterVer, err := s.provider.MasterKEK(ctx, kms.LatestVersion)
	if err != nil {
		crypto.Zeroize(rawKEK)
		return nil, fmt.Errorf("get MASTER_KEK: %w", err)
	}
	defer crypto.Zeroize(masterKey)

	aad := crypto.ServiceKEKWrapAAD(kekID, strconv.Itoa(version), "master", strconv.Itoa(masterVer))
	blob, err := s.encryptor.Seal(crypto.ContextWrap, masterKey, rawKEK, aad)
	if err != nil {
		crypto.Zeroize(rawKEK)
		return nil, fmt.Errorf("seal SERVICE_KEK: %w", err)
	}

	if err := s.st.PutKey(ctx, &store.Key{
		ID:              kekID,
		Version:         version,
		Purpose:         store.PurposeServiceKEK,
		Realm:           systemRealm,
		State:           store.KeyStateActive,
		ParentID:        "master",
		ParentVersion:   masterVer,
		WrappedMaterial: blobToBase64(blob),
		CreatedAt:       time.Now(),
	}); err != nil {
		crypto.Zeroize(rawKEK)
		return nil, fmt.Errorf("put SERVICE_KEK: %w", err)
	}

	s.log.Info("secretstore: created SERVICE_KEK",
		"kek_id", kekID,
		"realm", realm,
		"version", version,
	)
	return &svcKEKEntry{raw: rawKEK, version: version}, nil
}

func (s *Service) decryptServiceKEKRecord(ctx context.Context, k *store.Key) ([]byte, error) {
	masterKey, _, err := s.provider.MasterKEK(ctx, k.ParentVersion)
	if err != nil {
		return nil, fmt.Errorf("get MASTER_KEK v%d: %w", k.ParentVersion, err)
	}
	defer crypto.Zeroize(masterKey)

	blob, err := base64ToBlob(k.WrappedMaterial)
	if err != nil {
		return nil, fmt.Errorf("decode SERVICE_KEK blob: %w", err)
	}
	aad := crypto.ServiceKEKWrapAAD(k.ID, strconv.Itoa(k.Version), k.ParentID, strconv.Itoa(k.ParentVersion))
	return s.encryptor.Open(crypto.ContextWrap, masterKey, blob, aad)
}

func (s *Service) createDEK(ctx context.Context, realm string) (dekID string, dekVersion int, rawDEK []byte, err error) {
	rawDEK = make([]byte, 32)
	if _, err = io.ReadFull(rand.Reader, rawDEK); err != nil {
		return "", 0, nil, fmt.Errorf("rand DEK: %w", err)
	}

	var kekID string
	var entry *svcKEKEntry
	switch s.kekMode {
	case KEKModePooled:
		kekID, entry, err = s.selectPoolKEK(ctx)
	default:
		kekID, entry, err = s.getOrCreateServiceKEK(ctx, realm)
	}
	if err != nil {
		crypto.Zeroize(rawDEK)
		return "", 0, nil, err
	}

	svcKey := make([]byte, len(entry.raw))
	copy(svcKey, entry.raw)
	defer crypto.Zeroize(svcKey)

	dekID = "dek-" + newUUID7()
	dekVersion = 1
	aad := crypto.DEKWrapAAD(realm, dekID, strconv.Itoa(dekVersion), kekID, strconv.Itoa(entry.version))
	blob, err := s.encryptor.Seal(crypto.ContextWrap, svcKey, rawDEK, aad)
	if err != nil {
		crypto.Zeroize(rawDEK)
		return "", 0, nil, fmt.Errorf("seal DEK: %w", err)
	}

	if err = s.st.PutKey(ctx, &store.Key{
		ID:              dekID,
		Version:         dekVersion,
		Purpose:         store.PurposeDEK,
		Realm:           realm,
		State:           store.KeyStateActive,
		ParentID:        kekID,
		ParentVersion:   entry.version,
		WrappedMaterial: blobToBase64(blob),
		CreatedAt:       time.Now(),
	}); err != nil {
		crypto.Zeroize(rawDEK)
		return "", 0, nil, fmt.Errorf("put DEK: %w", err)
	}
	return dekID, dekVersion, rawDEK, nil
}

func (s *Service) unwrapDEK(ctx context.Context, dekID string, dekVersion int) ([]byte, error) {
	key, err := s.st.GetKey(ctx, dekID, dekVersion)
	if err != nil {
		return nil, fmt.Errorf("get DEK %s@%d: %w", dekID, dekVersion, err)
	}
	if key.State != store.KeyStateActive {
		return nil, fmt.Errorf("DEK %s@%d is not active (state=%s)", dekID, dekVersion, key.State)
	}

	svcKey, err := s.getServiceKEKVersion(ctx, key.ParentID, key.ParentVersion)
	if err != nil {
		return nil, fmt.Errorf("get SERVICE_KEK for DEK %s: %w", dekID, err)
	}
	defer crypto.Zeroize(svcKey)

	blob, err := base64ToBlob(key.WrappedMaterial)
	if err != nil {
		return nil, fmt.Errorf("decode DEK blob: %w", err)
	}
	aad := crypto.DEKWrapAAD(key.Realm, dekID, strconv.Itoa(dekVersion), key.ParentID, strconv.Itoa(key.ParentVersion))
	return s.encryptor.Open(crypto.ContextWrap, svcKey, blob, aad)
}

func toProto(sv *store.Secret, material []byte) *adminv1.SecretVersion {
	p := &adminv1.SecretVersion{
		VersionId:  sv.VersionID,
		Realm:      sv.Realm,
		SecretId:   sv.Name,
		Checksum:   sv.Checksum,
		State:      stateToProto(sv.State),
		CreatedAt:  timestamppb.New(sv.CreatedAt),
		CreatedBy:  sv.CreatedBy,
		EnabledBy:  sv.EnabledBy,
		DisabledBy: sv.DisabledBy,
		RetiredBy:  sv.RetiredBy,
	}
	if material != nil {
		p.Material = material
	}
	if sv.EnabledAt != nil {
		p.EnabledAt = timestamppb.New(*sv.EnabledAt)
	}
	if sv.DisabledAt != nil {
		p.DisabledAt = timestamppb.New(*sv.DisabledAt)
	}
	if sv.RetiredAt != nil {
		p.RetiredAt = timestamppb.New(*sv.RetiredAt)
	}
	if sv.ShreddedAt != nil {
		p.ShreddedAt = timestamppb.New(*sv.ShreddedAt)
	}
	return p
}

func stateToProto(s store.VersionState) adminv1.VersionState {
	switch s {
	case store.VersionStateEnabled:
		return adminv1.VersionState_VERSION_STATE_ENABLED
	case store.VersionStateDisabled:
		return adminv1.VersionState_VERSION_STATE_DISABLED
	case store.VersionStateRetired:
		return adminv1.VersionState_VERSION_STATE_RETIRED
	default:
		return adminv1.VersionState_VERSION_STATE_UNSPECIFIED
	}
}

func callerIdentity(_ context.Context) string { return "system" }

func newUUID7() string {
	id, err := uuid.NewV7()
	if err != nil {
		panic(fmt.Sprintf("secretstore: uuid.NewV7: %v", err))
	}
	return id.String()
}

func blobToBase64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func base64ToBlob(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }
