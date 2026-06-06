package resources

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/types/known/timestamppb"

	egressv1 "github.com/dio/transit/examples/orange/api/orange/egress/admin/v1"
	adminv1connect "github.com/dio/transit/examples/orange/api/orange/egress/admin/v1/adminv1connect"
)

// EgressService implements adminv1connect.EgressAdminServiceHandler using a PostgreSQL pool.
type EgressService struct {
	adminv1connect.UnimplementedEgressAdminServiceHandler
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// NewEgressService creates the egresses, egress_identities, egress_keypairs, and cp_validation_keys
// tables if they do not exist and returns a new EgressService.
func NewEgressService(pool *pgxpool.Pool, logger *slog.Logger) (*EgressService, error) {
	ddl := `
CREATE TABLE IF NOT EXISTS egresses (
  egress_id    TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL UNIQUE,
  status       TEXT NOT NULL DEFAULT 'inactive' CHECK (status IN ('active','inactive')),
  identity_id  TEXT,
  keypair_id   TEXT,
  description  TEXT,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS egress_identities (
  identity_id     TEXT PRIMARY KEY,
  egress_id       TEXT NOT NULL,
  certificate_pem TEXT NOT NULL,
  serial_number   TEXT NOT NULL,
  active          BOOLEAN NOT NULL DEFAULT true,
  issued_at       TIMESTAMPTZ NOT NULL,
  expires_at      TIMESTAMPTZ NOT NULL
);
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
CREATE TABLE IF NOT EXISTS cp_validation_keys (
  cpvalidation_key_id TEXT PRIMARY KEY,
  algorithm           TEXT NOT NULL,
  public_key_pem      TEXT NOT NULL,
  purpose             TEXT NOT NULL,
  active              BOOLEAN NOT NULL DEFAULT true,
  created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at          TIMESTAMPTZ NOT NULL
)`
	if _, err := pool.Exec(context.Background(), ddl); err != nil {
		return nil, err
	}
	return &EgressService{pool: pool, logger: logger}, nil
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

// ── Egress CRUD ───────────────────────────────────────────────────────────────

// CreateEgress inserts a new egress instance and returns it.
func (s *EgressService) CreateEgress(ctx context.Context, req *connect.Request[egressv1.CreateEgressRequest]) (*connect.Response[egressv1.CreateEgressResponse], error) {
	egressID := uuid.Must(uuid.NewV7()).String()
	now := time.Now().UTC()

	const q = `
INSERT INTO egresses (egress_id, workspace_id, status, identity_id, keypair_id, description, created_at, updated_at)
VALUES ($1, $2, 'inactive', '', '', $3, $4, $4)
RETURNING egress_id, workspace_id, status, identity_id, keypair_id, description, created_at, updated_at`

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
	const q = `SELECT egress_id, workspace_id, status, identity_id, keypair_id, description, created_at, updated_at FROM egresses WHERE egress_id = $1`

	eg, err := scanEgress(s.pool.QueryRow(ctx, q, req.Msg.GetEgressId()))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&egressv1.GetEgressResponse{Egress: eg}), nil
}

// UpdateEgress updates description and/or status of an egress.
func (s *EgressService) UpdateEgress(ctx context.Context, req *connect.Request[egressv1.UpdateEgressRequest]) (*connect.Response[egressv1.UpdateEgressResponse], error) {
	now := time.Now().UTC()

	// Fetch current egress to get existing status in case it's not being updated.
	const fetchQ = `SELECT status FROM egresses WHERE egress_id = $1`
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
UPDATE egresses SET description = $2, status = $3, updated_at = $4
WHERE egress_id = $1
RETURNING egress_id, workspace_id, status, identity_id, keypair_id, description, created_at, updated_at`

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

// IssueIdentity provisions a placeholder identity certificate for an Egress.
func (s *EgressService) IssueIdentity(ctx context.Context, req *connect.Request[egressv1.IssueIdentityRequest]) (*connect.Response[egressv1.IssueIdentityResponse], error) {
	identityID := uuid.Must(uuid.NewV7()).String()
	serialNumber := uuid.Must(uuid.NewV7()).String()
	now := time.Now().UTC()
	expiresAt := now.Add(time.Duration(req.Msg.GetTtlDays()) * 24 * time.Hour)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	const insertIdentity = `
INSERT INTO egress_identities (identity_id, egress_id, certificate_pem, serial_number, active, issued_at, expires_at)
VALUES ($1, $2, 'PLACEHOLDER_CERT', $3, true, $4, $5)
RETURNING identity_id, egress_id, certificate_pem, serial_number, active, issued_at, expires_at`

	var (
		id             string
		egressID       string
		certPEM        string
		serial         string
		active         bool
		issuedAt       time.Time
		identExpiresAt time.Time
	)
	err = tx.QueryRow(ctx, insertIdentity, identityID, req.Msg.GetEgressId(), serialNumber, now, expiresAt).
		Scan(&id, &egressID, &certPEM, &serial, &active, &issuedAt, &identExpiresAt)
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
			CertificatePem: certPEM,
			SerialNumber:   serial,
			Active:         active,
			IssuedAt:       timestamppb.New(issuedAt),
			ExpiresAt:      timestamppb.New(identExpiresAt),
		},
	}), nil
}

// GetIdentity returns the identity record by identity_id.
func (s *EgressService) GetIdentity(ctx context.Context, req *connect.Request[egressv1.GetIdentityRequest]) (*connect.Response[egressv1.GetIdentityResponse], error) {
	const q = `SELECT identity_id, egress_id, certificate_pem, serial_number, active, issued_at, expires_at FROM egress_identities WHERE identity_id = $1`

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
		const q = `SELECT identity_id, egress_id, certificate_pem, serial_number, active, issued_at, expires_at
FROM egress_identities WHERE egress_id = $1 AND active = true AND identity_id > $2 ORDER BY identity_id LIMIT $3`
		rows, err = s.pool.Query(ctx, q, req.Msg.GetEgressId(), pageToken, limit)
	case pageToken != "":
		const q = `SELECT identity_id, egress_id, certificate_pem, serial_number, active, issued_at, expires_at
FROM egress_identities WHERE egress_id = $1 AND identity_id > $2 ORDER BY identity_id LIMIT $3`
		rows, err = s.pool.Query(ctx, q, req.Msg.GetEgressId(), pageToken, limit)
	case activeOnly:
		const q = `SELECT identity_id, egress_id, certificate_pem, serial_number, active, issued_at, expires_at
FROM egress_identities WHERE egress_id = $1 AND active = true ORDER BY identity_id LIMIT $2`
		rows, err = s.pool.Query(ctx, q, req.Msg.GetEgressId(), limit)
	default:
		const q = `SELECT identity_id, egress_id, certificate_pem, serial_number, active, issued_at, expires_at
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

// RotateKeyPair generates a placeholder key pair record for an Egress.
func (s *EgressService) RotateKeyPair(ctx context.Context, req *connect.Request[egressv1.RotateKeyPairRequest]) (*connect.Response[egressv1.RotateKeyPairResponse], error) {
	keypairID := uuid.Must(uuid.NewV7()).String()
	now := time.Now().UTC()
	algoStr := keyPairAlgorithmToString(req.Msg.GetAlgorithm())

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	const insertKP = `
INSERT INTO egress_keypairs (keypair_id, egress_id, algorithm, public_key_pem, private_key_ref, active, created_at, rotated_at)
VALUES ($1, $2, $3, 'PLACEHOLDER_PUB', 'PLACEHOLDER_REF', true, $4, $4)
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
	err = tx.QueryRow(ctx, insertKP, keypairID, req.Msg.GetEgressId(), algoStr, now).
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

// ── scan helpers ──────────────────────────────────────────────────────────────

func scanEgress(row pgx.Row) (*egressv1.Egress, error) {
	var (
		id          string
		workspaceID string
		status      string
		identityID  string
		keypairID   string
		description *string
		createdAt   time.Time
		updatedAt   time.Time
	)
	if err := row.Scan(&id, &workspaceID, &status, &identityID, &keypairID, &description, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	return &egressv1.Egress{
		EgressId:    id,
		WorkspaceId: workspaceID,
		Status:      egressStatusFromString(status),
		IdentityId:  identityID,
		KeypairId:   keypairID,
		Description: description,
		CreatedAt:   timestamppb.New(createdAt),
		UpdatedAt:   timestamppb.New(updatedAt),
	}, nil
}

func scanIdentity(row pgx.Row) (*egressv1.EgressIdentity, error) {
	var (
		id           string
		egressID     string
		certPEM      string
		serialNumber string
		active       bool
		issuedAt     time.Time
		expiresAt    time.Time
	)
	if err := row.Scan(&id, &egressID, &certPEM, &serialNumber, &active, &issuedAt, &expiresAt); err != nil {
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
		serialNumber string
		active       bool
		issuedAt     time.Time
		expiresAt    time.Time
	)
	if err := rows.Scan(&id, &egressID, &certPEM, &serialNumber, &active, &issuedAt, &expiresAt); err != nil {
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
