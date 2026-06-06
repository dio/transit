# Cryptographic Identity and Attestation

Complete reference for cryptographic artefacts: egress identity, key pairs, PASETO tokens, and certificate lifecycle.

## Overview: Five Cryptographic Artefacts

When an admin onboards an egress to a workspace, **five** cryptographic
artefacts must be established:

1. **Egress identity** — an X.509 certificate binding the egress instance to its workspace
2. **Egress key pair** — an asymmetric key pair for egress→control-plane authentication
3. **Control plane validation key** — the CP public key distributed to egress instances for verifying signed telemetry
4. **Egress PASETO keypair #1** — Ed25519 keypair used by the Egress to sign/verify PASETO tokens (public key held by Egress, private key in secret service)
5. **Egress PASETO keypair #2** — second Ed25519 keypair for the same purpose (provides key rotation / redundancy)

### Why Five Separate Artefacts?

| Artefact | Purpose | Lifecycle |
|----------|---------|-----------|
| `EgressIdentity` (X.509) | Attests "this egress instance belongs to this workspace" | Bound to workspace; rotated on re-deployment |
| `EgressKeyPair` | Authenticates telemetry and usage meters egress → CP | Independent of identity; can be rotated without re-deploying egress |
| `CPValidationKey` | CP public key embedded in egress for verifying signed requests *from* CP and from other egress instances | Org-wide; single key pair per purpose |
| `EgressPASETOKeyPair` (×2) | Egress-owned Ed25519 keypairs used to sign/verify PASETO tokens issued on behalf of users | Created at egress onboarding; rotated independently |

## EgressIdentity (X.509 Certificate)

```
Certificate:
  Subject: CN=egress.workspace1.project1.organization
  Issuer:  CN=organization.internal-CA
  Serial:  <UUID>
  Validity:
    Not Before: 2026-06-06T00:00:00Z
    Not After:  2026-09-04T00:00:00Z
  Subject Alternative Names:
    DNS: egress.workspace1.project1.organization
    URI: workspace://workspace1.project1.organization
  Extensions:
    Key Usage: digitalSignature, keyEncipherment
    Extended Key Usage: serverAuth, clientAuth
```

The identity certificate is used:
- When egress boots: proves to the control plane that it is the legitimate egress for workspace1
- During mTLS handshake: egress presents this cert; CP validates against org CA
- In audit logs: all egress actions are attributed via the identity cert serial number

## EgressKeyPair (Asymmetric Authentication)

```
EgressKeyPair {
  id,
  egress_id FK,
  algorithm: "Ed25519",
  public_key: "-----BEGIN PUBLIC KEY-----\nMCow...\n-----END PUBLIC KEY-----",
  private_key_ref: "secret-service://vault/egress/workspace1/private-key-v2",
  created_at: "2026-06-06T00:00:00Z",
  rotated_at: "2026-06-06T00:00:00Z",
  active: true
}
```

The private key is **never exposed** to the admin, the CLI, or any API.
It is generated inside the secret service (e.g., HashiCorp Vault, AWS KMS,
Azure Key Vault) and only an opaque reference is stored in the control plane.
The egress instance retrieves the private key at runtime via the secret
service API, authenticated with its EgressIdentity certificate.

## Egress-to-Control-Plane Authentication

Egress instances send usage telemetry, health heartbeats, and audit logs
to the control plane. These messages must be authenticated to prevent
spoofing of usage data.

### Authentication Flow

```
┌──────────────┐                                ┌──────────────────────────┐
│   Egress     │                                │    Control Plane         │
│  Instance    │                                │                          │
│              │                                │                          │
│  1. Retrieve │                                │                          │
│     private  │◄────secret service API────────►│  Secret Service          │
│     key via  │   (authenticated with          │  (private key storage)   │
│     identity │    EgressIdentity cert)        │                          │
│     cert     │                                │                          │
│              │                                │                          │
│  2. Sign     │                                │                          │
│     telemetry│                                │                          │
│     payload: │                                │                          │
│     {        │                                │                          │
│       "egress_id": "egress-123",              │                          │
│       "workspace_id": "ws-456",               │                          │
│       "timestamp": "2026-06-06T12:00:00Z",    │                          │
│       "usage": { ... },                       │                          │
│       "signature": "<Ed25519 sig>"            │                          │
│     }        │                                │                          │
│              │────POST /v1/telemetry─────────►│                          │
│              │                                │  3. Look up EgressKeyPair│
│              │                                │     public key for       │
│              │                                │     egress-123           │
│              │                                │                          │
│              │                                │  4. Verify signature     │
│              │                                │     against public key   │
│              │                                │                          │
│              │◄────204 OK / 401 Unauthorized──┤                          │
│              │                                │                          │
└──────────────┘                                └──────────────────────────┘
```

### Telemetry Payload Structure

```json
{
  "header": {
    "egress_id": "egress-123",
    "workspace_id": "ws-456",
    "timestamp": "2026-06-06T12:00:00Z",
    "sequence_num": 15001,
    "prev_hash": "sha256:abc123..."
  },
  "body": {
    "usage": {
      "upstream1": { "requests": 1200, "tokens_in": 45000, "tokens_out": 89000 },
      "upstream2": { "requests": 45, "tokens_in": 1200, "tokens_out": 3400 }
    },
    "health": { "status": "healthy", "uptime_sec": 86400 },
    "active_keys": 4,
    "policy_version": "v3"
  },
  "signature": "<base64-encoded Ed25519 signature of canonicalized header+body>"
}
```

**Telemetry Properties:**

- Every telemetry payload is signed with the egress private key.
- The control plane verifies the signature using the registered public key.
- Signatures include timestamps; CP rejects messages with stale timestamps (>5 min skew).
- Sequence numbers and hash chaining prevent replay attacks and detect dropped messages.
- The private key never leaves the secret service boundary.

## Stateless User Key Verification with PASETO

Users obtain PASETO tokens via the CLI (`orange token generate`). The platform signs tokens using the Egress-owned PASETO keypairs (created during egress onboarding).
This enables **local verification** at the egress (no CP round-trip in the common case) — the egress can validate
a user's request without querying the control plane for every request.

### Why PASETO v4.public

| Feature | PASETO v4.public |
|---------|------------------|
| Verification | Local (cached pubkey + revocation list); no CP round-trip in common case |
| Latency | Local signature verification only |
| Offline operation | Egress can verify without CP |
| Key rotation | User generates and rotates locally |
| Revocation | Revocation list check (cached) |

### PASETO Key Format

```
PASETO v4.public token structure
─────────────────────────────────

Token format:
  v4.public.<payload>.<signature>

Payload (JSON, base64url-encoded then signed):
  {
    "kid": "key3@workspace1.project1.organization",
    "sub": "user1@organization",
    "wsk": "workspace1@project1.organization",
    "iat": "2026-06-06T10:00:00Z",
    "exp": "2026-06-06T11:00:00Z",
    "jti": "<unique-token-id>",
    "pol": { "allow": ["upstream1", "upstream2"] },
    "kht": "<SHA-256-thumbprint-of-user-public-key>"
  }

Signature: Ed25519 signature over the token header + payload
```

### Stateless Verification at Egress

```
Request arrives at egress
        │
        ▼
┌─────────────────────────────────────────────────────┐
│  1. Extract PASETO token from Authorization header  │
│     Authorization: Paseto v4.public.<payload>.<sig> │
│                                                      │
│  2. Verify PASETO signature locally                 │
│     → Validate token format (v4.public)             │
│     → Extract embedded key thumbprint ("kht")       │
│     → Look up cached EgressPASETOKeyPair by thumbprint │
│     → Verify Ed25519 signature against public key   │
│                                                      │
│  3. Extract claims                                  │
│     → "kid": key identity                           │
│     → "wsk": workspace scope (must match this       │
│              egress's workspace)                    │
│     → "exp": check not expired                      │
│     → "pol": key policy (embedded, user-signed)     │
│                                                      │
│  4. Check key revocation list                       │
│     → Query CP: is this key revoked?                │
│     → (Can be cached with TTL; stale-while-revalidate)│
│                                                      │
│  5. Proceed to policy evaluation (see 02-policy-system.md) │
│     → Embedded "pol" claim is treated as key        │
│       flexible policy — still subject to workspace  │
│       floor and project floor (narrowing invariant) │
└─────────────────────────────────────────────────────┘
```

### Revocation Check Strategy

Since PASETO verification is stateless, revocation requires explicit checking:

```
Strategy: "mostly-stateless" with revocation list polling

┌──────────────────────────────────────────────────────┐
│  Egress maintains a local revocation cache            │
│                                                       │
│  On first request with a new PASETO key:             │
│    → Query CP: "is key3 revoked?"                    │
│    → Cache result for 5 minutes (configurable)       │
│    → If revoked → DENIED immediately                 │
│                                                       │
│  On subsequent requests (cache hit):                 │
│    → Use cached revocation status                    │
│    → If cache expired → refresh in background        │
│      (stale-while-revalidate: old value used for     │
│       current request, async refresh)                │
│                                                       │
│  Revocation is rare → cache hit rate >99%            │
│  Effectively stateless for the common case           │
└──────────────────────────────────────────────────────┘
```

## Certificate and Key Lifecycle

The cryptographic artefacts follow a structured lifecycle: **creation → rotation → revocation → expiry**.

### Lifecycle Matrix

```
┌──────────────────────────────────────────────────────────────────────┐
│  Artefact          │  Creation          │  Rotation    │  Revocation │
│────────────────────┼────────────────────┼──────────────┼─────────────│
│  EgressIdentity    │  Admin onboards    │  Re-deploy   │  Workspace  │
│  (X.509)           │  egress (auto-     │  egress      │  deletion   │
│                    │  generated)        │              │             │
│  EgressKeyPair     │  Admin onboards    │  Admin CLI   │  Admin CLI  │
│                    │  egress (auto-     │  (secret     │  (regenerates│
│                    │  generated)        │  service     │  pair)      │
│                    │                    │  rotates)    │             │
│  CPValidationKey         │  Org setup (admin) │  Admin CLI   │  Admin CLI  │
│                          │                    │  (org-wide)  │             │
│  EgressPASETOKeyPair (×2)│  Egress onboarding │  Admin CLI   │  Admin CLI  │
│                          │                    │  (egress-specific)│         │
└──────────────────────────────────────────────────────────────────────┘
```

### Rotation — EgressKeyPair

```
Admin initiates rotation
        │
        ▼
┌─────────────────────────────────────────────────────┐
│  1. Secret service generates new Ed25519 key pair  │
│     private key → new path in secret service       │
│     public key  → registered as "next" key          │
│                                                      │
│  2. Control plane marks:                             │
│     EgressKeyPair v1: active → false               │
│     EgressKeyPair v2: active → true (new)          │
│                                                      │
│  3. Egress instance detects new active key:          │
│     → Retrieves new private key from secret service  │
│     → Begins signing telemetry with v2               │
│     → Drops v1 private key from memory               │
│                                                      │
│  4. CP begins verifying with v2 public key           │
│     → Telemetry signed with v1: accepted during     │
│       grace window (e.g., 5 minutes)                 │
│     → After grace window: v1 signatures rejected    │
└─────────────────────────────────────────────────────┘
```

### Rotation — CPValidationKey (Org-Wide)

```
Admin rotates CPValidationKey
        │
        ▼
┌─────────────────────────────────────────────────────┐
│  1. CP generates new Ed25519 key pair                │
│                                                      │
│  2. CP publishes new public key:                     │
│     → All egress instances receive new key           │
│       (via config push or polling)                   │
│     → New key marked "active"                        │
│     → Old key marked "retired" (verify-only,         │
│       no new signatures)                             │
│                                                      │
│  3. Grace period (e.g., 24 hours):                   │
│     → Egress accepts signatures from both old and   │
│       new CPValidationKey                             │
│                                                      │
│  4. After grace period:                              │
│     → Old CPValidationKey deactivated                │
│     → Egress rejects signatures from old key         │
└─────────────────────────────────────────────────────┘
```

### Revocation — Emergency Procedures

```
Emergency: EgressKeyPair compromised
────────────────────────────────────

Admin:
  1. egress identity revoke-keypair --egress <egress> \
       --workspace <ws> --project <proj>
     → Immediately deactivates current EgressKeyPair
     → CP rejects all telemetry signed with compromised key

  2. egress identity rotate-keypair --egress <egress> \
       --workspace <ws> --project <proj>
     → Generates new EgressKeyPair in secret service
     → New key active immediately

  3. egress identity status --egress <egress> \
       --workspace <ws> --project <proj>
     → Verify new key active, old key revoked

Emergency: EgressPASETOKeyPair compromised
────────────────────────────────────

Admin:
  1. Revoke the compromised EgressPASETOKeyPair
     → Immediately deactivates the affected keypair
     → CP rejects all tokens signed with compromised key

  2. Rotate to a new EgressPASETOKeyPair
     → Generates new Ed25519 keypair in secret service
     → New key active immediately

  3. Verify status
     → Confirm new keypair active, old key revoked
```

### Expiry Handling

```
EgressIdentity (X.509) expiry:
  - CP monitors certificate expiry (30, 14, 7, 1 day warnings)
  - Auto-renewal: if egress is healthy, CP issues new certificate
    before expiry, pushes to egress via secure channel
  - If egress is offline during expiry window:
    → Admin must re-deploy egress (new identity generated)

EgressKeyPair expiry:
  - Key pairs do not auto-expire (no X.509 validity window)
  - Rotation is admin-initiated or policy-driven
  - Recommended: rotate every 90 days (configurable org policy)

EgressPASETOKeyPair expiry:
  - Key pairs do not auto-expire
  - Rotation is admin-initiated or policy-driven
  - Recommended: rotate every 90 days (configurable org policy)
```

## Related Topics

- See **05-onboarding-workflow.md** for step-by-step deployment flow
- See **10-cryptographic-scenarios.md** for detailed identity and key scenarios
- See **06-admin-operations.md** for admin CLI commands for key management
- See **11-cli-reference.md** for complete CLI reference
