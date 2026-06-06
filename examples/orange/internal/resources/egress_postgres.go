package resources

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/types/known/timestamppb"

	egressv1 "github.com/dio/transit/examples/orange/api/orange/egress/admin/v1"
	adminv1connect "github.com/dio/transit/examples/orange/api/orange/egress/admin/v1/adminv1connect"
	secretv1 "github.com/dio/transit/examples/orange/api/orange/secret/admin/v1"
	"github.com/dio/transit/examples/orange/internal/secret"
)

// EgressService implements adminv1connect.EgressAdminServiceHandler using a PostgreSQL pool.
type EgressService struct {
	adminv1connect.UnimplementedEgressAdminServiceHandler
	pool      *pgxpool.Pool
	logger    *slog.Logger
	serverURL string
	secretSvc *secret.Service
}

// NewEgressService creates the egresses, egress_identities, egress_keypairs,
// egress_paseto_keypairs, and cp_validation_keys tables if they do not exist
// and returns a new EgressService.
func NewEgressService(pool *pgxpool.Pool, logger *slog.Logger, serverURL string, secretSvc *secret.Service) (*EgressService, error) {
	ddl := `
CREATE TABLE IF NOT EXISTS egresses (
  egress_id           TEXT PRIMARY KEY,
  workspace_id        TEXT NOT NULL UNIQUE,
  admin_status        TEXT NOT NULL DEFAULT 'inactive' CHECK (admin_status IN ('active','inactive')),
  online_status       TEXT NOT NULL DEFAULT 'unknown',
  last_seen_at        TIMESTAMPTZ,
  identity_id         TEXT NOT NULL DEFAULT '',
  keypair_id          TEXT NOT NULL DEFAULT '',
  paseto_keypair_1_id TEXT NOT NULL DEFAULT '',
  paseto_keypair_2_id TEXT NOT NULL DEFAULT '',
  description         TEXT,
  created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- Safe rename for existing installs: metadata-only in Postgres 10+, ~ms lock.
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='egresses' AND column_name='status') THEN
    ALTER TABLE egresses RENAME COLUMN status TO admin_status;
  END IF;
END $$;
-- Add columns if missing: metadata-only in Postgres 11+ for literal defaults.
ALTER TABLE egresses ADD COLUMN IF NOT EXISTS online_status TEXT NOT NULL DEFAULT 'unknown';
ALTER TABLE egresses ADD COLUMN IF NOT EXISTS last_seen_at TIMESTAMPTZ;
ALTER TABLE egresses ADD COLUMN IF NOT EXISTS paseto_keypair_1_id TEXT NOT NULL DEFAULT '';
ALTER TABLE egresses ADD COLUMN IF NOT EXISTS paseto_keypair_2_id TEXT NOT NULL DEFAULT '';
CREATE TABLE IF NOT EXISTS egress_identities (
  identity_id     TEXT PRIMARY KEY,
  egress_id       TEXT NOT NULL,
  certificate_pem TEXT NOT NULL,
  private_key_ref TEXT NOT NULL DEFAULT '',
  serial_number   TEXT NOT NULL,
  active          BOOLEAN NOT NULL DEFAULT true,
  issued_at       TIMESTAMPTZ NOT NULL,
  expires_at      TIMESTAMPTZ NOT NULL
);
ALTER TABLE egress_identities ADD COLUMN IF NOT EXISTS private_key_ref TEXT NOT NULL DEFAULT '';
CREATE TABLE IF NOT EXISTS egress_keypairs (
  keypair_id      TEXT PRIMARY KEY,
  egress_id       TEXT NOT NULL,
  algorithm       TEXT NOT NULL,
  public_key_pem  TEXT NOT NULL,
  private_key_ref TEXT NOT NULL,
  active          BOOLEAN NOT NULL DEFAULT true,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  rotated_at      TIMESTAMPTZ
);
CREATE TABLE IF NOT EXISTS egress_paseto_keypairs (
  paseto_keypair_id TEXT PRIMARY KEY,
  egress_id         TEXT NOT NULL,
  slot              INTEGER NOT NULL CHECK (slot IN (1, 2)),
  public_key_pem    TEXT NOT NULL,
  private_key_ref   TEXT NOT NULL,
  active            BOOLEAN NOT NULL DEFAULT true,
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  rotated_at        TIMESTAMPTZ
);
CREATE TABLE IF NOT EXISTS cp_validation_keys (
  cpvalidation_key_id TEXT PRIMARY KEY,
  algorithm           TEXT NOT NULL,
  public_key_pem      TEXT NOT NULL,
  purpose             TEXT NOT NULL,
  active              BOOLEAN NOT NULL DEFAULT true,
  created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at          TIMESTAMPTZ NOT NULL
);`
	if _, err := pool.Exec(context.Background(), ddl); err != nil {
		return nil, err
	}
	return &EgressService{pool: pool, logger: logger, serverURL: serverURL, secretSvc: secretSvc}, nil
}

// ── enum conversions ─────────────────────────────────────────────────────────

func egressStatusToString(s egressv1.EgressStatus) string {
	switch s {
	case egressv1.EgressStatus_EGRESS_STATUS_ACTIVE:
		return "active"
	case egressv1.EgressStatus_EGRESS_STATUS_INACTIVE:
		return "inactive"
	default:
		return "inactive"
	}
}

func egressStatusFromString(s string) egressv1.EgressStatus {
	switch s {
	case "active":
		return egressv1.EgressStatus_EGRESS_STATUS_ACTIVE
	case "inactive":
		return egressv1.EgressStatus_EGRESS_STATUS_INACTIVE
	default:
		return egressv1.EgressStatus_EGRESS_STATUS_UNSPECIFIED
	}
}

func egressOnlineStatusToString(s egressv1.EgressOnlineStatus) string {
	switch s {
	case egressv1.EgressOnlineStatus_EGRESS_ONLINE_STATUS_ONLINE:
		return "online"
	case egressv1.EgressOnlineStatus_EGRESS_ONLINE_STATUS_OFFLINE:
		return "offline"
	default:
		return "unknown"
	}
}

func egressOnlineStatusFromString(s string) egressv1.EgressOnlineStatus {
	switch s {
	case "online":
		return egressv1.EgressOnlineStatus_EGRESS_ONLINE_STATUS_ONLINE
	case "offline":
		return egressv1.EgressOnlineStatus_EGRESS_ONLINE_STATUS_OFFLINE
	default:
		return egressv1.EgressOnlineStatus_EGRESS_ONLINE_STATUS_UNKNOWN
	}
}

func keyPairAlgorithmToString(a egressv1.KeyPairAlgorithm) string {
	switch a {
	case egressv1.KeyPairAlgorithm_KEY_PAIR_ALGORITHM_ED25519:
		return "Ed25519"
	case egressv1.KeyPairAlgorithm_KEY_PAIR_ALGORITHM_ECDSA_P256:
		return "ECDSA_P256"
	case egressv1.KeyPairAlgorithm_KEY_PAIR_ALGORITHM_RSA_2048:
		return "RSA-2048"
	default:
		return "Ed25519"
	}
}

func keyPairAlgorithmFromString(s string) egressv1.KeyPairAlgorithm {
	switch s {
	case "Ed25519":
		return egressv1.KeyPairAlgorithm_KEY_PAIR_ALGORITHM_ED25519
	case "ECDSA_P256":
		return egressv1.KeyPairAlgorithm_KEY_PAIR_ALGORITHM_ECDSA_P256
	case "RSA-2048":
		return egressv1.KeyPairAlgorithm_KEY_PAIR_ALGORITHM_RSA_2048
	default:
		return egressv1.KeyPairAlgorithm_KEY_PAIR_ALGORITHM_UNSPECIFIED
	}
}

func cpValidationKeyAlgorithmToString(a egressv1.CPValidationKeyAlgorithm) string {
	switch a {
	case egressv1.CPValidationKeyAlgorithm_CP_VALIDATION_KEY_ALGORITHM_ED25519:
		return "Ed25519"
	case egressv1.CPValidationKeyAlgorithm_CP_VALIDATION_KEY_ALGORITHM_ECDSA_P256:
		return "ECDSA_P256"
	default:
		return "Ed25519"
	}
}

func cpValidationKeyAlgorithmFromString(s string) egressv1.CPValidationKeyAlgorithm {
	switch s {
	case "Ed25519":
		return egressv1.CPValidationKeyAlgorithm_CP_VALIDATION_KEY_ALGORITHM_ED25519
	case "ECDSA_P256":
		return egressv1.CPValidationKeyAlgorithm_CP_VALIDATION_KEY_ALGORITHM_ECDSA_P256
	default:
		return egressv1.CPValidationKeyAlgorithm_CP_VALIDATION_KEY_ALGORITHM_UNSPECIFIED
	}
}

func cpValidationKeyPurposeToString(p egressv1.CPValidationKeyPurpose) string {
	switch p {
	case egressv1.CPValidationKeyPurpose_CP_VALIDATION_KEY_PURPOSE_TELEMETRY:
		return "telemetry"
	case egressv1.CPValidationKeyPurpose_CP_VALIDATION_KEY_PURPOSE_REQUEST_VALIDATION:
		return "request_validation"
	case egressv1.CPValidationKeyPurpose_CP_VALIDATION_KEY_PURPOSE_BOTH:
		return "both"
	default:
		return "telemetry"
	}
}

func cpValidationKeyPurposeFromString(s string) egressv1.CPValidationKeyPurpose {
	switch s {
	case "telemetry":
		return egressv1.CPValidationKeyPurpose_CP_VALIDATION_KEY_PURPOSE_TELEMETRY
	case "request_validation":
		return egressv1.CPValidationKeyPurpose_CP_VALIDATION_KEY_PURPOSE_REQUEST_VALIDATION
	case "both":
		return egressv1.CPValidationKeyPurpose_CP_VALIDATION_KEY_PURPOSE_BOTH
	default:
		return egressv1.CPValidationKeyPurpose_CP_VALIDATION_KEY_PURPOSE_UNSPECIFIED
	}
}

// ── key generation helpers ────────────────────────────────────────────────────

// generateEd25519PEM generates an Ed25519 keypair and returns (publicKeyPEM, privDER, error).
func generateEd25519PEM() (pubPEM string, privDER []byte, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return
	}
	pubDER, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return
	}
	privDER, err = x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return
	}
	pubPEM = string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}))
	return
}

// generateIdentityCert generates a self-signed Ed25519 X.509 certificate for the egress.
// Returns (certPEM, privDER, serialNumber, error).
func generateIdentityCert(workspaceID string, ttlDays int) (certPEM string, privDER []byte, serial string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return
	}
	serialNum, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return
	}
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber:          serialNum,
		Subject:               pkix.Name{CommonName: "egress.workspace." + workspaceID},
		NotBefore:             now,
		NotAfter:              now.Add(time.Duration(ttlDays) * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, template, pub, priv)
	if err != nil {
		return
	}
	privDER, err = x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return
	}
	certPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}))
	serial = serialNum.Text(16)
	return
}

// storeKey stores a private key DER blob in the secret service and returns a ref
// in the form "<realm>/<secret_id>". The workspace_id is the encryption scope.
func (s *EgressService) storeKey(ctx context.Context, workspaceID, realm, secretID string, der []byte) (string, error) {
	_, err := s.secretSvc.CreateVersion(ctx, connect.NewRequest(&secretv1.CreateVersionRequest{
		WorkspaceId: workspaceID,
		Realm:       realm,
		SecretId:    secretID,
		Material:    der,
		Enable:      true,
	}))
	if err != nil {
		return "", fmt.Errorf("store key %s/%s: %w", realm, secretID, err)
	}
	return realm + "/" + secretID, nil
}

// resolveKey decrypts and returns the PEM-encoded private key for a ref.
// Ref format: "<realm>/<secret_id>" (workspace_id is passed explicitly).
func (s *EgressService) resolveKey(ctx context.Context, workspaceID, ref string) (string, error) {
	idx := strings.Index(ref, "/")
	if idx < 0 {
		return "", fmt.Errorf("malformed key ref: %q", ref)
	}
	realm, secretID := ref[:idx], ref[idx+1:]
	der, _, _, err := s.secretSvc.ResolveSecret(ctx, workspaceID, realm, secretID)
	if err != nil {
		return "", fmt.Errorf("resolve secret %s/%s: %w", realm, secretID, err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), nil
}

// ── Egress CRUD ───────────────────────────────────────────────────────────────

// CreateEgress inserts a new egress instance and returns it.
func (s *EgressService) CreateEgress(ctx context.Context, req *connect.Request[egressv1.CreateEgressRequest]) (*connect.Response[egressv1.CreateEgressResponse], error) {
	egressID := uuid.Must(uuid.NewV7()).String()
	now := time.Now().UTC()

	const q = `
INSERT INTO egresses (egress_id, workspace_id, admin_status, online_status, last_seen_at, identity_id, keypair_id, paseto_keypair_1_id, paseto_keypair_2_id, description, created_at, updated_at)
VALUES ($1, $2, 'inactive', 'unknown', NULL, '', '', '', '', $3, $4, $4)
RETURNING egress_id, workspace_id, admin_status, online_status, last_seen_at, identity_id, keypair_id, paseto_keypair_1_id, paseto_keypair_2_id, description, created_at, updated_at`

	eg, err := scanEgress(s.pool.QueryRow(ctx, q, egressID, req.Msg.GetWorkspaceId(), req.Msg.Description, now))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, connect.NewError(connect.CodeAlreadyExists, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&egressv1.CreateEgressResponse{Egress: eg}), nil
}

// GetEgress fetches an egress by egress_id.
func (s *EgressService) GetEgress(ctx context.Context, req *connect.Request[egressv1.GetEgressRequest]) (*connect.Response[egressv1.GetEgressResponse], error) {
	const q = `SELECT egress_id, workspace_id, admin_status, online_status, last_seen_at, identity_id, keypair_id, paseto_keypair_1_id, paseto_keypair_2_id, description, created_at, updated_at FROM egresses WHERE egress_id = $1`

	eg, err := scanEgress(s.pool.QueryRow(ctx, q, req.Msg.GetEgressId()))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&egressv1.GetEgressResponse{Egress: eg}), nil
}

// GetEgressByWorkspace fetches an egress by workspace_id.
func (s *EgressService) GetEgressByWorkspace(ctx context.Context, req *connect.Request[egressv1.GetEgressByWorkspaceRequest]) (*connect.Response[egressv1.GetEgressByWorkspaceResponse], error) {
	const q = `SELECT egress_id, workspace_id, admin_status, online_status, last_seen_at, identity_id, keypair_id, paseto_keypair_1_id, paseto_keypair_2_id, description, created_at, updated_at FROM egresses WHERE workspace_id = $1`

	eg, err := scanEgress(s.pool.QueryRow(ctx, q, req.Msg.GetWorkspaceId()))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&egressv1.GetEgressByWorkspaceResponse{Egress: eg}), nil
}

// UpdateEgress updates description and/or status of an egress.
func (s *EgressService) UpdateEgress(ctx context.Context, req *connect.Request[egressv1.UpdateEgressRequest]) (*connect.Response[egressv1.UpdateEgressResponse], error) {
	now := time.Now().UTC()

	// Fetch current egress to get existing admin_status in case it's not being updated.
	const fetchQ = `SELECT admin_status FROM egresses WHERE egress_id = $1`
	var currentStatus string
	if err := s.pool.QueryRow(ctx, fetchQ, req.Msg.GetEgressId()).Scan(&currentStatus); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	newStatus := currentStatus
	if req.Msg.Status != nil && *req.Msg.Status != egressv1.EgressStatus_EGRESS_STATUS_UNSPECIFIED {
		newStatus = egressStatusToString(*req.Msg.Status)
	}

	const q = `
UPDATE egresses SET description = $2, admin_status = $3, updated_at = $4
WHERE egress_id = $1
RETURNING egress_id, workspace_id, admin_status, online_status, last_seen_at, identity_id, keypair_id, paseto_keypair_1_id, paseto_keypair_2_id, description, created_at, updated_at`

	eg, err := scanEgress(s.pool.QueryRow(ctx, q, req.Msg.GetEgressId(), req.Msg.Description, newStatus, now))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&egressv1.UpdateEgressResponse{Egress: eg}), nil
}

// DeleteEgress removes an egress by egress_id.
func (s *EgressService) DeleteEgress(ctx context.Context, req *connect.Request[egressv1.DeleteEgressRequest]) (*connect.Response[egressv1.DeleteEgressResponse], error) {
	const q = `DELETE FROM egresses WHERE egress_id = $1`

	tag, err := s.pool.Exec(ctx, q, req.Msg.GetEgressId())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if tag.RowsAffected() == 0 {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("egress not found"))
	}

	return connect.NewResponse(&egressv1.DeleteEgressResponse{}), nil
}

// ── EgressIdentity management ─────────────────────────────────────────────────

// IssueIdentity provisions a real X.509 identity certificate for an Egress.
func (s *EgressService) IssueIdentity(ctx context.Context, req *connect.Request[egressv1.IssueIdentityRequest]) (*connect.Response[egressv1.IssueIdentityResponse], error) {
	identityID := uuid.Must(uuid.NewV7()).String()
	ttlDays := int(req.Msg.GetTtlDays())
	if ttlDays <= 0 {
		ttlDays = 90
	}

	var workspaceID string
	if err := s.pool.QueryRow(ctx, `SELECT workspace_id FROM egresses WHERE egress_id = $1`, req.Msg.GetEgressId()).Scan(&workspaceID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	certPEM, privDER, serial, err := generateIdentityCert(workspaceID, ttlDays)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("generate identity cert: %w", err))
	}
	privRef, err := s.storeKey(ctx, workspaceID, "egress-identity", identityID, privDER)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("store identity key: %w", err))
	}
	now := time.Now().UTC()
	expiresAt := now.Add(time.Duration(ttlDays) * 24 * time.Hour)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	const insertIdentity = `
INSERT INTO egress_identities (identity_id, egress_id, certificate_pem, private_key_ref, serial_number, active, issued_at, expires_at)
VALUES ($1, $2, $3, $4, $5, true, $6, $7)
RETURNING identity_id, egress_id, certificate_pem, private_key_ref, serial_number, active, issued_at, expires_at`

	var (
		id             string
		egressID       string
		retCertPEM     string
		retPrivRef     string
		retSerial      string
		active         bool
		issuedAt       time.Time
		identExpiresAt time.Time
	)
	err = tx.QueryRow(ctx, insertIdentity, identityID, req.Msg.GetEgressId(), certPEM, privRef, serial, now, expiresAt).
		Scan(&id, &egressID, &retCertPEM, &retPrivRef, &retSerial, &active, &issuedAt, &identExpiresAt)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	if _, err := tx.Exec(ctx,
		`UPDATE egresses SET identity_id = $1, updated_at = $2 WHERE egress_id = $3`,
		identityID, now, req.Msg.GetEgressId(),
	); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&egressv1.IssueIdentityResponse{
		Identity: &egressv1.EgressIdentity{
			IdentityId:     id,
			EgressId:       egressID,
			CertificatePem: retCertPEM,
			SerialNumber:   retSerial,
			Active:         active,
			IssuedAt:       timestamppb.New(issuedAt),
			ExpiresAt:      timestamppb.New(identExpiresAt),
		},
	}), nil
}

// GetIdentity returns the identity record by identity_id.
func (s *EgressService) GetIdentity(ctx context.Context, req *connect.Request[egressv1.GetIdentityRequest]) (*connect.Response[egressv1.GetIdentityResponse], error) {
	const q = `SELECT identity_id, egress_id, certificate_pem, private_key_ref, serial_number, active, issued_at, expires_at FROM egress_identities WHERE identity_id = $1`

	ident, err := scanIdentity(s.pool.QueryRow(ctx, q, req.Msg.GetIdentityId()))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&egressv1.GetIdentityResponse{Identity: ident}), nil
}

// ListIdentities lists identity records for an egress.
func (s *EgressService) ListIdentities(ctx context.Context, req *connect.Request[egressv1.ListIdentitiesRequest]) (*connect.Response[egressv1.ListIdentitiesResponse], error) {
	limit := int(req.Msg.GetLimit())
	if limit <= 0 {
		limit = 100
	}

	activeOnly := req.Msg.GetActiveOnly()
	pageToken := req.Msg.GetPageToken()

	var (
		rows pgx.Rows
		err  error
	)

	switch {
	case pageToken != "" && activeOnly:
		const q = `SELECT identity_id, egress_id, certificate_pem, private_key_ref, serial_number, active, issued_at, expires_at
FROM egress_identities WHERE egress_id = $1 AND active = true AND identity_id > $2 ORDER BY identity_id LIMIT $3`
		rows, err = s.pool.Query(ctx, q, req.Msg.GetEgressId(), pageToken, limit)
	case pageToken != "":
		const q = `SELECT identity_id, egress_id, certificate_pem, private_key_ref, serial_number, active, issued_at, expires_at
FROM egress_identities WHERE egress_id = $1 AND identity_id > $2 ORDER BY identity_id LIMIT $3`
		rows, err = s.pool.Query(ctx, q, req.Msg.GetEgressId(), pageToken, limit)
	case activeOnly:
		const q = `SELECT identity_id, egress_id, certificate_pem, private_key_ref, serial_number, active, issued_at, expires_at
FROM egress_identities WHERE egress_id = $1 AND active = true ORDER BY identity_id LIMIT $2`
		rows, err = s.pool.Query(ctx, q, req.Msg.GetEgressId(), limit)
	default:
		const q = `SELECT identity_id, egress_id, certificate_pem, private_key_ref, serial_number, active, issued_at, expires_at
FROM egress_identities WHERE egress_id = $1 ORDER BY identity_id LIMIT $2`
		rows, err = s.pool.Query(ctx, q, req.Msg.GetEgressId(), limit)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer rows.Close()

	var identities []*egressv1.EgressIdentity
	for rows.Next() {
		ident, err := scanIdentityRow(rows)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		identities = append(identities, ident)
	}
	if err := rows.Err(); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	var nextPageToken string
	if len(identities) == limit {
		nextPageToken = identities[len(identities)-1].IdentityId
	}

	return connect.NewResponse(&egressv1.ListIdentitiesResponse{
		Identities:    identities,
		NextPageToken: nextPageToken,
	}), nil
}

// RevokeIdentity deactivates an identity certificate.
func (s *EgressService) RevokeIdentity(ctx context.Context, req *connect.Request[egressv1.RevokeIdentityRequest]) (*connect.Response[egressv1.RevokeIdentityResponse], error) {
	const q = `UPDATE egress_identities SET active = false WHERE identity_id = $1`

	tag, err := s.pool.Exec(ctx, q, req.Msg.GetIdentityId())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if tag.RowsAffected() == 0 {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("identity not found"))
	}

	return connect.NewResponse(&egressv1.RevokeIdentityResponse{}), nil
}

// ── EgressKeyPair management ──────────────────────────────────────────────────

// RotateKeyPair generates a real Ed25519 key pair record for an Egress.
func (s *EgressService) RotateKeyPair(ctx context.Context, req *connect.Request[egressv1.RotateKeyPairRequest]) (*connect.Response[egressv1.RotateKeyPairResponse], error) {
	keypairID := uuid.Must(uuid.NewV7()).String()
	now := time.Now().UTC()
	algoStr := keyPairAlgorithmToString(req.Msg.GetAlgorithm())

	var workspaceID string
	if err := s.pool.QueryRow(ctx, `SELECT workspace_id FROM egresses WHERE egress_id = $1`, req.Msg.GetEgressId()).Scan(&workspaceID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	pubPEM, privDER, err := generateEd25519PEM()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("generate keypair: %w", err))
	}
	privRef, err := s.storeKey(ctx, workspaceID, "egress-keypair", keypairID, privDER)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("store keypair key: %w", err))
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	const insertKP = `
INSERT INTO egress_keypairs (keypair_id, egress_id, algorithm, public_key_pem, private_key_ref, active, created_at, rotated_at)
VALUES ($1, $2, $3, $4, $5, true, $6, $6)
RETURNING keypair_id, egress_id, algorithm, public_key_pem, private_key_ref, active, created_at, rotated_at`

	var (
		id            string
		egressID      string
		algorithm     string
		publicKeyPEM  string
		privateKeyRef string
		active        bool
		createdAt     time.Time
		rotatedAt     *time.Time
	)
	err = tx.QueryRow(ctx, insertKP, keypairID, req.Msg.GetEgressId(), algoStr, pubPEM, privRef, now).
		Scan(&id, &egressID, &algorithm, &publicKeyPEM, &privateKeyRef, &active, &createdAt, &rotatedAt)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	if _, err := tx.Exec(ctx,
		`UPDATE egresses SET keypair_id = $1, updated_at = $2 WHERE egress_id = $3`,
		keypairID, now, req.Msg.GetEgressId(),
	); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	kp := &egressv1.EgressKeyPair{
		KeypairId:     id,
		EgressId:      egressID,
		Algorithm:     keyPairAlgorithmFromString(algorithm),
		PublicKeyPem:  publicKeyPEM,
		PrivateKeyRef: privateKeyRef,
		Active:        active,
		CreatedAt:     timestamppb.New(createdAt),
	}
	if rotatedAt != nil {
		kp.RotatedAt = timestamppb.New(*rotatedAt)
	}

	return connect.NewResponse(&egressv1.RotateKeyPairResponse{Keypair: kp}), nil
}

// GetKeyPair returns the key pair record by keypair_id.
func (s *EgressService) GetKeyPair(ctx context.Context, req *connect.Request[egressv1.GetKeyPairRequest]) (*connect.Response[egressv1.GetKeyPairResponse], error) {
	const q = `SELECT keypair_id, egress_id, algorithm, public_key_pem, private_key_ref, active, created_at, rotated_at FROM egress_keypairs WHERE keypair_id = $1`

	kp, err := scanKeyPair(s.pool.QueryRow(ctx, q, req.Msg.GetKeypairId()))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&egressv1.GetKeyPairResponse{Keypair: kp}), nil
}

// ListKeyPairs lists key pair records for an egress.
func (s *EgressService) ListKeyPairs(ctx context.Context, req *connect.Request[egressv1.ListKeyPairsRequest]) (*connect.Response[egressv1.ListKeyPairsResponse], error) {
	limit := int(req.Msg.GetLimit())
	if limit <= 0 {
		limit = 100
	}

	activeOnly := req.Msg.GetActiveOnly()
	pageToken := req.Msg.GetPageToken()

	var (
		rows pgx.Rows
		err  error
	)

	switch {
	case pageToken != "" && activeOnly:
		const q = `SELECT keypair_id, egress_id, algorithm, public_key_pem, private_key_ref, active, created_at, rotated_at
FROM egress_keypairs WHERE egress_id = $1 AND active = true AND keypair_id > $2 ORDER BY keypair_id LIMIT $3`
		rows, err = s.pool.Query(ctx, q, req.Msg.GetEgressId(), pageToken, limit)
	case pageToken != "":
		const q = `SELECT keypair_id, egress_id, algorithm, public_key_pem, private_key_ref, active, created_at, rotated_at
FROM egress_keypairs WHERE egress_id = $1 AND keypair_id > $2 ORDER BY keypair_id LIMIT $3`
		rows, err = s.pool.Query(ctx, q, req.Msg.GetEgressId(), pageToken, limit)
	case activeOnly:
		const q = `SELECT keypair_id, egress_id, algorithm, public_key_pem, private_key_ref, active, created_at, rotated_at
FROM egress_keypairs WHERE egress_id = $1 AND active = true ORDER BY keypair_id LIMIT $2`
		rows, err = s.pool.Query(ctx, q, req.Msg.GetEgressId(), limit)
	default:
		const q = `SELECT keypair_id, egress_id, algorithm, public_key_pem, private_key_ref, active, created_at, rotated_at
FROM egress_keypairs WHERE egress_id = $1 ORDER BY keypair_id LIMIT $2`
		rows, err = s.pool.Query(ctx, q, req.Msg.GetEgressId(), limit)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer rows.Close()

	var keypairs []*egressv1.EgressKeyPair
	for rows.Next() {
		kp, err := scanKeyPairRow(rows)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		keypairs = append(keypairs, kp)
	}
	if err := rows.Err(); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	var nextPageToken string
	if len(keypairs) == limit {
		nextPageToken = keypairs[len(keypairs)-1].KeypairId
	}

	return connect.NewResponse(&egressv1.ListKeyPairsResponse{
		Keypairs:      keypairs,
		NextPageToken: nextPageToken,
	}), nil
}

// ── EgressPASETOKeyPair management ────────────────────────────────────────────

// RotatePASETOKeyPair generates a new Ed25519 PASETO keypair for a given egress + slot.
func (s *EgressService) RotatePASETOKeyPair(ctx context.Context, req *connect.Request[egressv1.RotatePASETOKeyPairRequest]) (*connect.Response[egressv1.RotatePASETOKeyPairResponse], error) {
	pasetoKPID := uuid.Must(uuid.NewV7()).String()
	slot := req.Msg.GetSlot()
	now := time.Now().UTC()

	var workspaceID string
	if err := s.pool.QueryRow(ctx, `SELECT workspace_id FROM egresses WHERE egress_id = $1`, req.Msg.GetEgressId()).Scan(&workspaceID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	pubPEM, privDER, err := generateEd25519PEM()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("generate paseto keypair: %w", err))
	}
	privRef, err := s.storeKey(ctx, workspaceID, "egress-paseto", pasetoKPID, privDER)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("store paseto key: %w", err))
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// Mark existing active keypair for this egress+slot as inactive.
	if _, err := tx.Exec(ctx,
		`UPDATE egress_paseto_keypairs SET active = false WHERE egress_id = $1 AND slot = $2 AND active = true`,
		req.Msg.GetEgressId(), slot,
	); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	const insertPKP = `
INSERT INTO egress_paseto_keypairs (paseto_keypair_id, egress_id, slot, public_key_pem, private_key_ref, active, created_at, rotated_at)
VALUES ($1, $2, $3, $4, $5, true, $6, $6)
RETURNING paseto_keypair_id, egress_id, slot, public_key_pem, private_key_ref, active, created_at, rotated_at`

	kp, err := scanPASETOKeyPair(tx.QueryRow(ctx, insertPKP, pasetoKPID, req.Msg.GetEgressId(), slot, pubPEM, privRef, now))
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	// Update the relevant slot reference on the egress row.
	var updateCol string
	switch slot {
	case 1:
		updateCol = "paseto_keypair_1_id"
	case 2:
		updateCol = "paseto_keypair_2_id"
	default:
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("slot must be 1 or 2, got %d", slot))
	}
	if _, err := tx.Exec(ctx,
		`UPDATE egresses SET `+updateCol+` = $1, updated_at = $2 WHERE egress_id = $3`,
		pasetoKPID, now, req.Msg.GetEgressId(),
	); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&egressv1.RotatePASETOKeyPairResponse{Keypair: kp}), nil
}

// GetPASETOKeyPair returns a PASETO keypair record by paseto_keypair_id.
func (s *EgressService) GetPASETOKeyPair(ctx context.Context, req *connect.Request[egressv1.GetPASETOKeyPairRequest]) (*connect.Response[egressv1.GetPASETOKeyPairResponse], error) {
	const q = `SELECT paseto_keypair_id, egress_id, slot, public_key_pem, private_key_ref, active, created_at, rotated_at FROM egress_paseto_keypairs WHERE paseto_keypair_id = $1`

	kp, err := scanPASETOKeyPair(s.pool.QueryRow(ctx, q, req.Msg.GetPasetoKeypairId()))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&egressv1.GetPASETOKeyPairResponse{Keypair: kp}), nil
}

// ListPASETOKeyPairs lists PASETO keypair records for an egress.
func (s *EgressService) ListPASETOKeyPairs(ctx context.Context, req *connect.Request[egressv1.ListPASETOKeyPairsRequest]) (*connect.Response[egressv1.ListPASETOKeyPairsResponse], error) {
	limit := int(req.Msg.GetLimit())
	if limit <= 0 {
		limit = 100
	}

	activeOnly := req.Msg.GetActiveOnly()
	pageToken := req.Msg.GetPageToken()

	var (
		rows pgx.Rows
		err  error
	)

	switch {
	case pageToken != "" && activeOnly:
		const q = `SELECT paseto_keypair_id, egress_id, slot, public_key_pem, private_key_ref, active, created_at, rotated_at
FROM egress_paseto_keypairs WHERE egress_id = $1 AND active = true AND paseto_keypair_id > $2 ORDER BY paseto_keypair_id LIMIT $3`
		rows, err = s.pool.Query(ctx, q, req.Msg.GetEgressId(), pageToken, limit)
	case pageToken != "":
		const q = `SELECT paseto_keypair_id, egress_id, slot, public_key_pem, private_key_ref, active, created_at, rotated_at
FROM egress_paseto_keypairs WHERE egress_id = $1 AND paseto_keypair_id > $2 ORDER BY paseto_keypair_id LIMIT $3`
		rows, err = s.pool.Query(ctx, q, req.Msg.GetEgressId(), pageToken, limit)
	case activeOnly:
		const q = `SELECT paseto_keypair_id, egress_id, slot, public_key_pem, private_key_ref, active, created_at, rotated_at
FROM egress_paseto_keypairs WHERE egress_id = $1 AND active = true ORDER BY paseto_keypair_id LIMIT $2`
		rows, err = s.pool.Query(ctx, q, req.Msg.GetEgressId(), limit)
	default:
		const q = `SELECT paseto_keypair_id, egress_id, slot, public_key_pem, private_key_ref, active, created_at, rotated_at
FROM egress_paseto_keypairs WHERE egress_id = $1 ORDER BY paseto_keypair_id LIMIT $2`
		rows, err = s.pool.Query(ctx, q, req.Msg.GetEgressId(), limit)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer rows.Close()

	var keypairs []*egressv1.EgressPASETOKeyPair
	for rows.Next() {
		kp, err := scanPASETOKeyPairRow(rows)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		keypairs = append(keypairs, kp)
	}
	if err := rows.Err(); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	var nextPageToken string
	if len(keypairs) == limit {
		nextPageToken = keypairs[len(keypairs)-1].PasetoKeypairId
	}

	return connect.NewResponse(&egressv1.ListPASETOKeyPairsResponse{
		Keypairs:      keypairs,
		NextPageToken: nextPageToken,
	}), nil
}

// ── GetEgressBundle ───────────────────────────────────────────────────────────

// GetEgressBundle assembles and returns the full egress bootstrap bundle.
func (s *EgressService) GetEgressBundle(ctx context.Context, req *connect.Request[egressv1.GetEgressBundleRequest]) (*connect.Response[egressv1.GetEgressBundleResponse], error) {
	// 1. Fetch egress row.
	const egressQ = `SELECT egress_id, workspace_id, admin_status, online_status, last_seen_at, identity_id, keypair_id, paseto_keypair_1_id, paseto_keypair_2_id, description, created_at, updated_at FROM egresses WHERE egress_id = $1`
	eg, err := scanEgress(s.pool.QueryRow(ctx, egressQ, req.Msg.GetEgressId()))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	// 2. Fetch identity cert (public cert only; private key stays server-side).
	var identCertPEM string
	if eg.IdentityId != "" {
		if err := s.pool.QueryRow(ctx,
			`SELECT certificate_pem FROM egress_identities WHERE identity_id = $1`,
			eg.IdentityId,
		).Scan(&identCertPEM); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("fetch identity: %w", err))
		}
	}

	// 3. Fetch egress keypair private_key_ref.
	var kpPrivRef string
	if eg.KeypairId != "" {
		if err := s.pool.QueryRow(ctx,
			`SELECT private_key_ref FROM egress_keypairs WHERE keypair_id = $1`,
			eg.KeypairId,
		).Scan(&kpPrivRef); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("fetch keypair: %w", err))
		}
	}

	// 4. Resolve egress keypair private key PEM.
	var kpPrivKeyPEM string
	if kpPrivRef != "" {
		kpPrivKeyPEM, err = s.resolveKey(ctx, eg.WorkspaceId, kpPrivRef)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("resolve keypair key: %w", err))
		}
	}

	// 5. Fetch PASETO public keys for slot 1 and slot 2.
	pasetoPublicKeys := map[string]string{}
	pasetoIDs := []string{}
	if eg.PasetoKeypair_1Id != "" {
		pasetoIDs = append(pasetoIDs, eg.PasetoKeypair_1Id)
	}
	if eg.PasetoKeypair_2Id != "" {
		pasetoIDs = append(pasetoIDs, eg.PasetoKeypair_2Id)
	}
	for _, pid := range pasetoIDs {
		var pubPEM string
		if err := s.pool.QueryRow(ctx,
			`SELECT public_key_pem FROM egress_paseto_keypairs WHERE paseto_keypair_id = $1`,
			pid,
		).Scan(&pubPEM); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("fetch paseto keypair %s: %w", pid, err))
		}
		pasetoPublicKeys[pid] = pubPEM
	}

	// 6. Build bundle.
	bundle := &egressv1.EgressBundle{
		EgressId:                   eg.EgressId,
		WorkspaceId:                eg.WorkspaceId,
		ServerUrl:                  s.serverURL,
		IdentityCertPem:            identCertPEM,
		EgressKeypairPrivateKeyPem: kpPrivKeyPEM,
		PasetoPublicKey_1Pem:       pasetoPublicKeys[eg.PasetoKeypair_1Id],
		PasetoPublicKey_2Pem:       pasetoPublicKeys[eg.PasetoKeypair_2Id],
	}

	return connect.NewResponse(&egressv1.GetEgressBundleResponse{Bundle: bundle}), nil
}

// ── CPValidationKey management ────────────────────────────────────────────────

// CreateCPValidationKey registers a control-plane public key.
func (s *EgressService) CreateCPValidationKey(ctx context.Context, req *connect.Request[egressv1.CreateCPValidationKeyRequest]) (*connect.Response[egressv1.CreateCPValidationKeyResponse], error) {
	cpvKeyID := uuid.Must(uuid.NewV7()).String()
	now := time.Now().UTC()

	var expiresAt time.Time
	if req.Msg.GetExpiresAt() != nil {
		expiresAt = req.Msg.GetExpiresAt().AsTime()
	} else {
		expiresAt = now.Add(365 * 24 * time.Hour)
	}

	const q = `
INSERT INTO cp_validation_keys (cpvalidation_key_id, algorithm, public_key_pem, purpose, active, created_at, expires_at)
VALUES ($1, $2, $3, $4, true, $5, $6)
RETURNING cpvalidation_key_id, algorithm, public_key_pem, purpose, active, created_at, expires_at`

	cpvKey, err := scanCPValidationKey(s.pool.QueryRow(ctx, q,
		cpvKeyID,
		cpValidationKeyAlgorithmToString(req.Msg.GetAlgorithm()),
		req.Msg.GetPublicKeyPem(),
		cpValidationKeyPurposeToString(req.Msg.GetPurpose()),
		now,
		expiresAt,
	))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, connect.NewError(connect.CodeAlreadyExists, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&egressv1.CreateCPValidationKeyResponse{Key: cpvKey}), nil
}

// GetCPValidationKey returns a control-plane validation key by ID.
func (s *EgressService) GetCPValidationKey(ctx context.Context, req *connect.Request[egressv1.GetCPValidationKeyRequest]) (*connect.Response[egressv1.GetCPValidationKeyResponse], error) {
	const q = `SELECT cpvalidation_key_id, algorithm, public_key_pem, purpose, active, created_at, expires_at FROM cp_validation_keys WHERE cpvalidation_key_id = $1`

	cpvKey, err := scanCPValidationKey(s.pool.QueryRow(ctx, q, req.Msg.GetCpvalidationKeyId()))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&egressv1.GetCPValidationKeyResponse{Key: cpvKey}), nil
}

// ListCPValidationKeys lists control-plane validation keys with optional filters.
func (s *EgressService) ListCPValidationKeys(ctx context.Context, req *connect.Request[egressv1.ListCPValidationKeysRequest]) (*connect.Response[egressv1.ListCPValidationKeysResponse], error) {
	limit := int(req.Msg.GetLimit())
	if limit <= 0 {
		limit = 100
	}

	activeOnly := req.Msg.GetActiveOnly()
	pageToken := req.Msg.GetPageToken()

	args := []any{}
	where := ""
	idx := 1

	addWhere := func(clause string, val any) {
		if where == "" {
			where = " WHERE "
		} else {
			where += " AND "
		}
		where += clause
		args = append(args, val)
		idx++
	}

	if pageToken != "" {
		addWhere("cpvalidation_key_id > $"+itoa(idx), pageToken)
	}
	if activeOnly {
		addWhere("active = true", nil)
		// Remove the nil we just added — replace with a real boolean arg.
		args[len(args)-1] = true
	}
	if req.Msg.Purpose != nil && *req.Msg.Purpose != egressv1.CPValidationKeyPurpose_CP_VALIDATION_KEY_PURPOSE_UNSPECIFIED {
		addWhere("purpose = $"+itoa(idx), cpValidationKeyPurposeToString(*req.Msg.Purpose))
	}

	limitArg := "$" + itoa(idx)
	args = append(args, limit)

	q := "SELECT cpvalidation_key_id, algorithm, public_key_pem, purpose, active, created_at, expires_at FROM cp_validation_keys" +
		where + " ORDER BY cpvalidation_key_id LIMIT " + limitArg

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer rows.Close()

	var keys []*egressv1.CPValidationKey
	for rows.Next() {
		cpvKey, err := scanCPValidationKeyRow(rows)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		keys = append(keys, cpvKey)
	}
	if err := rows.Err(); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	var nextPageToken string
	if len(keys) == limit {
		nextPageToken = keys[len(keys)-1].CpvalidationKeyId
	}

	return connect.NewResponse(&egressv1.ListCPValidationKeysResponse{
		Keys:          keys,
		NextPageToken: nextPageToken,
	}), nil
}

// DeactivateCPValidationKey marks a control-plane key as inactive.
func (s *EgressService) DeactivateCPValidationKey(ctx context.Context, req *connect.Request[egressv1.DeactivateCPValidationKeyRequest]) (*connect.Response[egressv1.DeactivateCPValidationKeyResponse], error) {
	const q = `UPDATE cp_validation_keys SET active = false WHERE cpvalidation_key_id = $1`

	tag, err := s.pool.Exec(ctx, q, req.Msg.GetCpvalidationKeyId())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if tag.RowsAffected() == 0 {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("cp validation key not found"))
	}

	return connect.NewResponse(&egressv1.DeactivateCPValidationKeyResponse{}), nil
}

// ── ProvisionForWorkspace ─────────────────────────────────────────────────────

// ProvisionForWorkspace creates the egress and all its cryptographic artefacts
// for a newly created workspace, all within the provided transaction.
func (s *EgressService) ProvisionForWorkspace(ctx context.Context, tx pgx.Tx, workspaceID string) (*egressv1.Egress, error) {
	egressID := uuid.Must(uuid.NewV7()).String()
	now := time.Now().UTC()

	// 1. INSERT egress with admin_status='active'.
	const insertEgress = `
INSERT INTO egresses (egress_id, workspace_id, admin_status, online_status, last_seen_at, identity_id, keypair_id, paseto_keypair_1_id, paseto_keypair_2_id, description, created_at, updated_at)
VALUES ($1, $2, 'active', 'unknown', NULL, '', '', '', '', NULL, $3, $3)`
	if _, err := tx.Exec(ctx, insertEgress, egressID, workspaceID, now); err != nil {
		return nil, fmt.Errorf("insert egress: %w", err)
	}

	// 2. Generate X.509 identity (90 days), store private key in secret service, INSERT egress_identities.
	identityID := uuid.Must(uuid.NewV7()).String()
	certPEM, identPrivDER, identSerial, err := generateIdentityCert(workspaceID, 90)
	if err != nil {
		return nil, fmt.Errorf("generate identity cert: %w", err)
	}
	identPrivRef, err := s.storeKey(ctx, workspaceID, "egress-identity", identityID, identPrivDER)
	if err != nil {
		return nil, fmt.Errorf("store identity key: %w", err)
	}
	identExpiresAt := now.Add(90 * 24 * time.Hour)
	const insertIdentity = `
INSERT INTO egress_identities (identity_id, egress_id, certificate_pem, private_key_ref, serial_number, active, issued_at, expires_at)
VALUES ($1, $2, $3, $4, $5, true, $6, $7)`
	if _, err := tx.Exec(ctx, insertIdentity, identityID, egressID, certPEM, identPrivRef, identSerial, now, identExpiresAt); err != nil {
		return nil, fmt.Errorf("insert egress identity: %w", err)
	}

	// 3. Generate Ed25519 EgressKeyPair, store private key, INSERT egress_keypairs.
	keypairID := uuid.Must(uuid.NewV7()).String()
	kpPubPEM, kpPrivDER, err := generateEd25519PEM()
	if err != nil {
		return nil, fmt.Errorf("generate egress keypair: %w", err)
	}
	kpPrivRef, err := s.storeKey(ctx, workspaceID, "egress-keypair", keypairID, kpPrivDER)
	if err != nil {
		return nil, fmt.Errorf("store egress keypair key: %w", err)
	}
	const insertKP = `
INSERT INTO egress_keypairs (keypair_id, egress_id, algorithm, public_key_pem, private_key_ref, active, created_at, rotated_at)
VALUES ($1, $2, 'Ed25519', $3, $4, true, $5, $5)`
	if _, err := tx.Exec(ctx, insertKP, keypairID, egressID, kpPubPEM, kpPrivRef, now); err != nil {
		return nil, fmt.Errorf("insert egress keypair: %w", err)
	}

	// 4. Generate Ed25519 PASETO slot 1, store private key, INSERT egress_paseto_keypairs (slot=1).
	pasetoKP1ID := uuid.Must(uuid.NewV7()).String()
	p1PubPEM, p1PrivDER, err := generateEd25519PEM()
	if err != nil {
		return nil, fmt.Errorf("generate paseto keypair slot 1: %w", err)
	}
	p1PrivRef, err := s.storeKey(ctx, workspaceID, "egress-paseto", pasetoKP1ID, p1PrivDER)
	if err != nil {
		return nil, fmt.Errorf("store paseto key slot 1: %w", err)
	}
	const insertPKP = `
INSERT INTO egress_paseto_keypairs (paseto_keypair_id, egress_id, slot, public_key_pem, private_key_ref, active, created_at, rotated_at)
VALUES ($1, $2, $3, $4, $5, true, $6, $6)`
	if _, err := tx.Exec(ctx, insertPKP, pasetoKP1ID, egressID, 1, p1PubPEM, p1PrivRef, now); err != nil {
		return nil, fmt.Errorf("insert paseto keypair slot 1: %w", err)
	}

	// 5. Generate Ed25519 PASETO slot 2, store private key, INSERT egress_paseto_keypairs (slot=2).
	pasetoKP2ID := uuid.Must(uuid.NewV7()).String()
	p2PubPEM, p2PrivDER, err := generateEd25519PEM()
	if err != nil {
		return nil, fmt.Errorf("generate paseto keypair slot 2: %w", err)
	}
	p2PrivRef, err := s.storeKey(ctx, workspaceID, "egress-paseto", pasetoKP2ID, p2PrivDER)
	if err != nil {
		return nil, fmt.Errorf("store paseto key slot 2: %w", err)
	}
	if _, err := tx.Exec(ctx, insertPKP, pasetoKP2ID, egressID, 2, p2PubPEM, p2PrivRef, now); err != nil {
		return nil, fmt.Errorf("insert paseto keypair slot 2: %w", err)
	}

	// 6. UPDATE egress with identity_id, keypair_id, paseto_keypair_1_id, paseto_keypair_2_id.
	const updateEgress = `
UPDATE egresses
SET identity_id = $2, keypair_id = $3, paseto_keypair_1_id = $4, paseto_keypair_2_id = $5, updated_at = $6
WHERE egress_id = $1`
	if _, err := tx.Exec(ctx, updateEgress, egressID, identityID, keypairID, pasetoKP1ID, pasetoKP2ID, now); err != nil {
		return nil, fmt.Errorf("update egress artefacts: %w", err)
	}

	// 7. Return the fully scanned Egress.
	const selectEgress = `SELECT egress_id, workspace_id, admin_status, online_status, last_seen_at, identity_id, keypair_id, paseto_keypair_1_id, paseto_keypair_2_id, description, created_at, updated_at FROM egresses WHERE egress_id = $1`
	return scanEgress(tx.QueryRow(ctx, selectEgress, egressID))
}

// ── scan helpers ──────────────────────────────────────────────────────────────

func scanEgress(row pgx.Row) (*egressv1.Egress, error) {
	var (
		id               string
		workspaceID      string
		adminStatus      string
		onlineStatus     string
		lastSeenAt       *time.Time
		identityID       string
		keypairID        string
		pasetoKP1ID      string
		pasetoKP2ID      string
		description      *string
		createdAt        time.Time
		updatedAt        time.Time
	)
	if err := row.Scan(
		&id, &workspaceID, &adminStatus, &onlineStatus, &lastSeenAt,
		&identityID, &keypairID, &pasetoKP1ID, &pasetoKP2ID,
		&description, &createdAt, &updatedAt,
	); err != nil {
		return nil, err
	}
	eg := &egressv1.Egress{
		EgressId:          id,
		WorkspaceId:       workspaceID,
		AdminStatus:       egressStatusFromString(adminStatus),
		OnlineStatus:      egressOnlineStatusFromString(onlineStatus),
		IdentityId:        identityID,
		KeypairId:         keypairID,
		PasetoKeypair_1Id: pasetoKP1ID,
		PasetoKeypair_2Id: pasetoKP2ID,
		Description:       description,
		CreatedAt:         timestamppb.New(createdAt),
		UpdatedAt:         timestamppb.New(updatedAt),
	}
	if lastSeenAt != nil {
		eg.LastSeenAt = timestamppb.New(*lastSeenAt)
	}
	return eg, nil
}

func scanIdentity(row pgx.Row) (*egressv1.EgressIdentity, error) {
	var (
		id           string
		egressID     string
		certPEM      string
		privKeyRef   string
		serialNumber string
		active       bool
		issuedAt     time.Time
		expiresAt    time.Time
	)
	if err := row.Scan(&id, &egressID, &certPEM, &privKeyRef, &serialNumber, &active, &issuedAt, &expiresAt); err != nil {
		return nil, err
	}
	return &egressv1.EgressIdentity{
		IdentityId:     id,
		EgressId:       egressID,
		CertificatePem: certPEM,
		SerialNumber:   serialNumber,
		Active:         active,
		IssuedAt:       timestamppb.New(issuedAt),
		ExpiresAt:      timestamppb.New(expiresAt),
	}, nil
}

func scanIdentityRow(rows pgx.Rows) (*egressv1.EgressIdentity, error) {
	var (
		id           string
		egressID     string
		certPEM      string
		privKeyRef   string
		serialNumber string
		active       bool
		issuedAt     time.Time
		expiresAt    time.Time
	)
	if err := rows.Scan(&id, &egressID, &certPEM, &privKeyRef, &serialNumber, &active, &issuedAt, &expiresAt); err != nil {
		return nil, err
	}
	return &egressv1.EgressIdentity{
		IdentityId:     id,
		EgressId:       egressID,
		CertificatePem: certPEM,
		SerialNumber:   serialNumber,
		Active:         active,
		IssuedAt:       timestamppb.New(issuedAt),
		ExpiresAt:      timestamppb.New(expiresAt),
	}, nil
}

func scanKeyPair(row pgx.Row) (*egressv1.EgressKeyPair, error) {
	var (
		id            string
		egressID      string
		algorithm     string
		publicKeyPEM  string
		privateKeyRef string
		active        bool
		createdAt     time.Time
		rotatedAt     *time.Time
	)
	if err := row.Scan(&id, &egressID, &algorithm, &publicKeyPEM, &privateKeyRef, &active, &createdAt, &rotatedAt); err != nil {
		return nil, err
	}
	kp := &egressv1.EgressKeyPair{
		KeypairId:     id,
		EgressId:      egressID,
		Algorithm:     keyPairAlgorithmFromString(algorithm),
		PublicKeyPem:  publicKeyPEM,
		PrivateKeyRef: privateKeyRef,
		Active:        active,
		CreatedAt:     timestamppb.New(createdAt),
	}
	if rotatedAt != nil {
		kp.RotatedAt = timestamppb.New(*rotatedAt)
	}
	return kp, nil
}

func scanKeyPairRow(rows pgx.Rows) (*egressv1.EgressKeyPair, error) {
	var (
		id            string
		egressID      string
		algorithm     string
		publicKeyPEM  string
		privateKeyRef string
		active        bool
		createdAt     time.Time
		rotatedAt     *time.Time
	)
	if err := rows.Scan(&id, &egressID, &algorithm, &publicKeyPEM, &privateKeyRef, &active, &createdAt, &rotatedAt); err != nil {
		return nil, err
	}
	kp := &egressv1.EgressKeyPair{
		KeypairId:     id,
		EgressId:      egressID,
		Algorithm:     keyPairAlgorithmFromString(algorithm),
		PublicKeyPem:  publicKeyPEM,
		PrivateKeyRef: privateKeyRef,
		Active:        active,
		CreatedAt:     timestamppb.New(createdAt),
	}
	if rotatedAt != nil {
		kp.RotatedAt = timestamppb.New(*rotatedAt)
	}
	return kp, nil
}

func scanPASETOKeyPair(row pgx.Row) (*egressv1.EgressPASETOKeyPair, error) {
	var (
		id            string
		egressID      string
		slot          int32
		publicKeyPEM  string
		privateKeyRef string
		active        bool
		createdAt     time.Time
		rotatedAt     *time.Time
	)
	if err := row.Scan(&id, &egressID, &slot, &publicKeyPEM, &privateKeyRef, &active, &createdAt, &rotatedAt); err != nil {
		return nil, err
	}
	kp := &egressv1.EgressPASETOKeyPair{
		PasetoKeypairId: id,
		EgressId:        egressID,
		Slot:            slot,
		PublicKeyPem:    publicKeyPEM,
		PrivateKeyRef:   privateKeyRef,
		Active:          active,
		CreatedAt:       timestamppb.New(createdAt),
	}
	if rotatedAt != nil {
		kp.RotatedAt = timestamppb.New(*rotatedAt)
	}
	return kp, nil
}

func scanPASETOKeyPairRow(rows pgx.Rows) (*egressv1.EgressPASETOKeyPair, error) {
	var (
		id            string
		egressID      string
		slot          int32
		publicKeyPEM  string
		privateKeyRef string
		active        bool
		createdAt     time.Time
		rotatedAt     *time.Time
	)
	if err := rows.Scan(&id, &egressID, &slot, &publicKeyPEM, &privateKeyRef, &active, &createdAt, &rotatedAt); err != nil {
		return nil, err
	}
	kp := &egressv1.EgressPASETOKeyPair{
		PasetoKeypairId: id,
		EgressId:        egressID,
		Slot:            slot,
		PublicKeyPem:    publicKeyPEM,
		PrivateKeyRef:   privateKeyRef,
		Active:          active,
		CreatedAt:       timestamppb.New(createdAt),
	}
	if rotatedAt != nil {
		kp.RotatedAt = timestamppb.New(*rotatedAt)
	}
	return kp, nil
}

func scanCPValidationKey(row pgx.Row) (*egressv1.CPValidationKey, error) {
	var (
		id        string
		algorithm string
		pubKeyPEM string
		purpose   string
		active    bool
		createdAt time.Time
		expiresAt time.Time
	)
	if err := row.Scan(&id, &algorithm, &pubKeyPEM, &purpose, &active, &createdAt, &expiresAt); err != nil {
		return nil, err
	}
	return &egressv1.CPValidationKey{
		CpvalidationKeyId: id,
		Algorithm:         cpValidationKeyAlgorithmFromString(algorithm),
		PublicKeyPem:      pubKeyPEM,
		Purpose:           cpValidationKeyPurposeFromString(purpose),
		Active:            active,
		CreatedAt:         timestamppb.New(createdAt),
		ExpiresAt:         timestamppb.New(expiresAt),
	}, nil
}

func scanCPValidationKeyRow(rows pgx.Rows) (*egressv1.CPValidationKey, error) {
	var (
		id        string
		algorithm string
		pubKeyPEM string
		purpose   string
		active    bool
		createdAt time.Time
		expiresAt time.Time
	)
	if err := rows.Scan(&id, &algorithm, &pubKeyPEM, &purpose, &active, &createdAt, &expiresAt); err != nil {
		return nil, err
	}
	return &egressv1.CPValidationKey{
		CpvalidationKeyId: id,
		Algorithm:         cpValidationKeyAlgorithmFromString(algorithm),
		PublicKeyPem:      pubKeyPEM,
		Purpose:           cpValidationKeyPurposeFromString(purpose),
		Active:            active,
		CreatedAt:         timestamppb.New(createdAt),
		ExpiresAt:         timestamppb.New(expiresAt),
	}, nil
}
