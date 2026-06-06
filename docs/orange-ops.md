# Egress onboarding model

A complete reference for the egress proxy onboarding model — entities, relationships, policy rules, operational scenarios, CLI reference, and **cryptographic identity & attestation**.

---

## Table of contents

1. [Entity model](#1-entity-model)
2. [Hierarchy and containment](#2-hierarchy-and-containment)
3. [Key scoping](#3-key-scoping)
4. [Policy model](#4-policy-model)
5. [Policy evaluation](#5-policy-evaluation)
6. [Secret management and credential resolution](#6-secret-management-and-credential-resolution)
   - 6.1 [Workspace secrets](#61-workspace-secrets-admin-managed-shared)
   - 6.2 [User-bound upstream credentials (BYOK)](#62-user-bound-upstream-credentials-byok)
   - 6.3 [Credential resolution at dispatch](#63-credential-resolution-at-dispatch)
   - 6.4 [Egress identity and cryptographic onboarding](#64-egress-identity-and-cryptographic-onboarding)
   - 6.5 [Egress-to-control-plane authentication](#65-egress-to-control-plane-authentication)
   - 6.6 [Stateless user key verification with PASETO](#66-stateless-user-key-verification-with-paseto)
   - 6.7 [Certificate and key lifecycle](#67-certificate-and-key-lifecycle)
7. [Onboarding scenario (step by step)](#7-onboarding-scenario-step-by-step)
8. [All operational scenarios](#8-all-operational-scenarios)
   - 8.1 [Admin scenarios](#81-admin-scenarios)
   - 8.2 [User scenarios](#82-user-scenarios)
   - 8.3 [Policy conflict scenarios](#83-policy-conflict-scenarios)
   - 8.4 [Secret rotation scenarios](#84-secret-rotation-scenarios)
   - 8.5 [Edge cases and error scenarios](#85-edge-cases-and-error-scenarios)
   - 8.6 [Cryptographic identity scenarios](#86-cryptographic-identity-scenarios)
9. [CLI reference](#9-cli-reference)
   - 9.1 [Installation and authentication](#91-installation-and-authentication)
   - 9.2 [Non-interactive mode](#92-non-interactive-mode)
   - 9.3 [REPL mode](#93-repl-mode)
   - 9.4 [Admin commands](#94-admin-commands)
   - 9.5 [User commands](#95-user-commands)
   - 9.6 [Describe commands](#96-describe-commands)
   - 9.7 [Output formats](#97-output-formats)
10. [Summary: invariants and rules](#10-summary-invariants-and-rules)

---

## 1. Entity model

| Entity | Owned by | Description |
|---|---|---|
| `Organization` | — | Top-level tenant |
| `Project` | Organization | Groups workspaces under a common admin scope |
| `Workspace` | Project | Deploys one Egress; scopes keys and policies |
| `Egress` | Workspace | The proxy instance; holds secrets for upstream auth |
| `EgressIdentity` | Egress | X.509 identity certificate attesting egress ownership of a workspace |
| `EgressKeyPair` | Egress | Asymmetric key pair (egress private key → secret service; public key → control plane) for egress→CP authentication |
| `CPValidationKey` | Control Plane | Control plane public key for validating signed telemetry and requests from egress instances |
| `Secret` | Workspace / Egress | Versioned credential for one upstream service (shared, admin-managed) |
| `KeySecret` | Key | User-supplied versioned credential for one upstream, bound to a specific key; overrides workspace Secret at dispatch (BYOK) |
| `User` | Organization | Member; assigned to one or more workspaces |
| `Key` | User × Workspace | Auth token for requests; permanently bound to one workspace |
| `PASETOToken` | User | Metadata + hash of a generated PASETO token. The actual signed token value is **not** stored — only metadata (`jti`, `iat`, `exp`, `pol`, ...) and a hash are kept for auditing and revocation. |
| `Policy` | Admin or User | Rules attached at Project, Workspace, or Key scope |

### Schema sketch

```
Organization {
  id, name
}

Project {
  id, org_id FK, name,
  description?
}

Workspace {
  id, project_id FK, name,
  description?
}

Egress {
  id, workspace_id FK,
  status: "active" | "inactive",
  identity_id FK,        -- references EgressIdentity
  keypair_id FK,         -- references EgressKeyPair
  description?
}

EgressIdentity {
  id, egress_id FK,
  certificate PEM,       -- X.509 identity cert
  issued_at, expires_at, -- validity window
  serial_number,
  active BOOL
}

EgressKeyPair {
  id, egress_id FK,
  algorithm: "Ed25519" | "ECDSA_P256" | "RSA-2048",
  public_key PEM,        -- registered with control plane
  private_key_ref,       -- opaque reference to secret service (never exposed)
  created_at, rotated_at,
  active BOOL
}

CPValidationKey {
  id,
  algorithm: "Ed25519" | "ECDSA_P256",
  public_key PEM,        -- CP public key distributed to all egress instances
  purpose: "telemetry" | "request_validation" | "both",
  created_at, expires_at,
  active BOOL
}

Secret {
  id, workspace_id FK,
  upstream_target,
  version,           -- e.g. "v1", "v2"
  value (encrypted),
  active BOOL,
  description?
}

KeySecret {
  id, key_id FK,
  upstream_target,
  version,           -- e.g. "v1", "v2"
  value (encrypted),
  active BOOL,
  description?
}

User {
  id, org_id FK, email,
  description?
}

WorkspaceMember {
  workspace_id FK,
  user_id FK
  -- no role column: all members have equal rights
}

Key {
  id,
  workspace_id FK,   -- immutable after creation
  user_id FK,
  name,
  key_format: "paseto_v4.public",  -- only supported format
  description?
}

PASETOToken {
  id,
  key_id FK,
  jti,                 -- unique token identifier
  iat, exp,
  pol,                 -- policy embedded at issuance time
  token_hash,          -- hash of the signed PASETO token (for revocation/audit)
  revoked BOOL,
  created_at
}

Policy {
  id,
  scope_type: "project" | "workspace" | "key",
  scope_id,          -- project_id | workspace_id | key_id
  type: "floor" | "flexible",
  description?,
  rules[]
}
```

> `description` fields are free-text strings set by admins and users to document
> what a resource is for. They surface in `describe` output and audit logs.

---

## 2. Hierarchy and containment

```
Organization
├── Project
│   └── Workspace
│       ├── Egress
│       │   ├── EgressIdentity (X.509 certificate)
│       │   ├── EgressKeyPair (asymmetric key pair)
│       │   └── Secret (versioned)         ← workspace-level shared credential
│       └── WorkspaceMember (User)
│           └── Key (bound to this Workspace)
│               └── KeySecret (versioned)  ← user-supplied personal credential (BYOK)
└── User (org-level)
    └── Key (across multiple workspaces)
```

### Identity naming convention

Resources use a scoped dot-notation path:

```
admin@organization
user1@organization

project1@organization
workspace1@project1.organization
egress@workspace1.project1.organization

egress-identity@egress.workspace1.project1.organization
key1@workspace1.project1.organization   (owned by user1)
key3@workspace1.project1.organization   (owned by user1, BYOK bound)
```

This makes the audit surface unambiguous — you can always determine scope from the identifier alone.

---

## 3. Key scoping

A `Key` is permanently bound to a single `Workspace` at creation time.
The `workspace_id` foreign key is immutable.

```
User ──────────────────────────────── has many Keys
                                              │
                                    each Key has exactly one
                                              │
                                          Workspace
                                              │
                              each Key may optionally have
                                              │
                                  one KeySecret per upstream
                                   (user-supplied, BYOK)

                              each Key may optionally use
                                              │
                            PASETO format for stateless signing
                                              │
                           (Key records public key thumbprint
                            for verification without CP lookup)
```

```
┌────────────────────────────────────────────────────────────────┐
│  Workspace: workspace1@project1.organization                   │
│                                                                │
│  Members:  user1, user2                                        │
│                                                                │
│  Keys:     key1  (owner: user1, format: paseto_v4.public)      │
│            key2  (owner: user1, format: paseto_v4.public).     │
│            key3  (owner: user1, BYOK: upstream1)               │
│                                                                │
│  A key created here can NEVER be used in                       │
│  another workspace, and cannot be reassigned.                  │
│                                                                │
│  PASETO keys carry their own identity —                        │
│  verification is local (no CP round-trip) in the                │
│  common case, using cached public key + revocation             │
│  list.                                                         │
└────────────────────────────────────────────────────────────────┘
```

**Rules:**
- A user may hold keys in multiple workspaces (one or more keys per workspace).
- All workspace members have identical rights: create keys, attach policies to their own keys, send requests, bind personal upstream credentials to their own keys.
- There is no role distinction within a workspace.
- All keys use `paseto_v4.public` format — Ed25519 signed tokens verified statelessly at egress.

---

## 4. Policy model

Policies exist at three scopes and two types:

```
┌─────────────────────────────────────────────────────────┐
│  Scope       │  Type       │  Who sets it               │
│──────────────┼─────────────┼────────────────────────────│
│  Project     │  floor      │  admin only                 │
│  Project     │  flexible   │  admin                      │
│  Workspace   │  floor      │  admin only                 │
│  Workspace   │  flexible   │  admin                      │
│  Key         │  flexible   │  key owner (user)           │
│  Key         │  floor      │  not permitted              │
└─────────────────────────────────────────────────────────┘
```

**Floor policies** are hard limits. No scope below them can override or widen them.
Only admins can write floor policies.

**Flexible policies** can be restricted (narrowed) by lower scopes, but never widened.

### Narrowing invariant

```
key_policy ⊆ workspace_policy ⊆ project_floor
```

This invariant is **enforced at write time** for user-set key policies.
When a user attempts to create or update a key policy, the system validates
that the proposed rules do not exceed the workspace's current effective policy.
If they do, the write is rejected immediately.

### Workspace policy tightening — lazy invalidation

When an admin tightens a workspace policy, existing key policies may now
exceed the new boundary. **Lazy invalidation** is used:

```
Before:
  workspace1 flexible: allow upstream1 + upstream2
  key1 stored policy:  allow upstream1 + upstream2

Admin tightens workspace1: allow upstream1 ONLY

After:
  key1 stored policy:  allow upstream1 + upstream2  (unchanged on disk)
  effective key1:      intersect(workspace1, key1) = upstream1 ONLY

Request via key1 to upstream2: DENIED  (workspace clamps it at eval time)
Request via key1 to upstream1: ALLOWED
```

The stored key policy is never rewritten. The evaluator always computes
the intersection at request time. No cascade writes are needed.

---

## 5. Policy evaluation

### Evaluation order at request time

```
Incoming request (Key + payload)
        │
        ▼
┌───────────────────────┐
│  0. Key format check  │  PASETO keys verified statelessly
│     (PASETO verify)   │  Stateless Ed25519 sig verify
└──────────┬────────────┘
           │ pass
           ▼
┌───────────────────────┐
│  1. Project floor     │  admin-set hard limits
│     policy            │  any deny here → BLOCKED
└──────────┬────────────┘
           │ pass
           ▼
┌───────────────────────┐
│  2. Workspace floor   │  admin-set workspace limits
│     policy            │  any deny here → BLOCKED
└──────────┬────────────┘
           │ pass
           ▼
┌───────────────────────┐
│  3. Workspace         │  flexible workspace defaults
│     flexible policy   │  any deny here → BLOCKED
└──────────┬────────────┘
           │ pass
           ▼
┌───────────────────────┐
│  4. Key flexible      │  user-set key restrictions
│     policy            │  any deny here → BLOCKED
└──────────┬────────────┘
           │ all pass
           ▼
    POLICY PASSED
    → Credential resolution (see §6.3)
    → Egress proxies to upstream
```

**Rule:** deny always wins. A deny at any level blocks the request regardless
of what lower levels allow.

**Note:** BYOK (user-bound KeySecrets) affect *which credential* is used
at dispatch — not *whether the request is allowed*. Policy evaluation is
identical regardless of whether a key has a KeySecret bound.

### What users can and cannot do with policies

```
┌──────────────────────────────────────────────────────────┐
│  User action              │  Permitted?                  │
│───────────────────────────┼──────────────────────────────│
│  Attach policy to own key │  Yes                         │
│  Restrict key below       │  Yes                         │
│  workspace policy         │                              │
│  Widen key beyond         │  No — rejected at write time │
│  workspace policy         │                              │
│  Modify workspace policy  │  No — admin only             │
│  Modify floor policy       │  No — admin only             │
│  View floor policies       │  Yes (read-only)             │
│  Describe any resource    │  Yes (within their scope)    │
│  Bind own upstream        │  Yes — on own keys only      │
│  credential (BYOK)        │                              │
│  Create PASETO-format key │  Yes — user generates keys   │
│                           │  locally, provides public key │
└──────────────────────────────────────────────────────────┘
```

---

## 6. Secret management and credential resolution

---

### 6.1 Workspace secrets (admin-managed, shared)

Each `Secret` is a versioned credential tied to one upstream service within a workspace.
It is managed by the admin and shared by all keys that do not supply their own credential.

```
Workspace
└── Egress
    ├── EgressIdentity (X.509)
    ├── EgressKeyPair (asymmetric)
    ├── Secret: secret1  (upstream: upstream1)
    │   ├── v1  [superseded]
    │   └── v2  [active]      ← current
    └── Secret: secret2  (upstream: upstream2)
        └── v1  [active]
```

### Zero-downtime rotation (workspace secrets)

```
Admin rotates secret1: v1 → v2
        │
        ▼
┌─────────────────────────────────────────────┐
│  Secret store                               │
│  secret1:v1  →  active: false               │
│  secret1:v2  →  active: true                │
└─────────────────────────────────────────────┘
        │
        │  Egress resolves active version
        │  at request dispatch time
        │
        ▼
┌─────────────────────────────────────────────┐
│  In-flight requests using v1                │
│  drain naturally (no hard cut-off)          │
│                                             │
│  New requests resolve v2 immediately        │
└─────────────────────────────────────────────┘
        │
        ▼
  User Keys are unaffected.
  Keys never hold workspace Secret values directly.
```

---

### 6.2 User-bound upstream credentials (BYOK)

A user may supply their own upstream credential for a specific upstream and
bind it to one of their keys as a `KeySecret`. When egress dispatches a
request via that key to that upstream, the `KeySecret` is used instead of
the workspace-level `Secret`.

**Why this matters:**

```
┌─────────────────────────────────────────────────────────────┐
│  Without BYOK                                               │
│  key3 → upstream1 → uses workspace Secret                   │
│  metering: against workspace's upstream1 account            │
│                                                             │
│  With BYOK                                                  │
│  key3 → upstream1 → uses user1's personal KeySecret         │
│  metering: against user1's own upstream1 account            │
└─────────────────────────────────────────────────────────────┘
```

**Properties of KeySecrets:**

- A `KeySecret` is bound to exactly one `Key` and one `upstream_target`.
- A key may hold at most one active `KeySecret` per upstream.
- Different keys owned by the same user may use different upstream credentials
  for the same upstream, or a mix of BYOK and shared.
- Keys with no `KeySecret` for an upstream fall back to the workspace `Secret`
  for that upstream (if one exists).
- `KeySecret` values are encrypted at rest and never returned by any describe
  or list command — only their existence and upstream target are surfaced.
- Admin cannot read or modify a user's `KeySecret` values. Admins can see
  that a `KeySecret` is configured for a key/upstream pair (in describe output)
  but not its value.
- A user may configure a `KeySecret` for an upstream that has no workspace
  `Secret`, provided the workspace policy permits access to that upstream.
  This lets a user self-provision access to an upstream without requiring
  the admin to configure a shared credential.
- Removing a user from a workspace cascades to purge all their `KeySecrets`
  along with their keys.

**KeySecret versioning and zero-downtime rotation:**

`KeySecrets` follow the same versioning model as workspace `Secrets`.
The user (key owner) manages rotation; it does not require any admin action
and does not affect the workspace `Secret` or other users' keys.

```
user1 rotates KeySecret for upstream1 on key3: v1 → v2
  KeySecret:v1  active: false
  KeySecret:v2  active: true

In-flight requests via key3: drain on v1
New requests via key3:       use v2

Workspace Secret for upstream1: unaffected
Other keys (key1, key2):        unaffected
```

---

### 6.3 Credential resolution at dispatch

When egress is ready to proxy a request (all policy checks have passed)
it resolves which credential to inject for the target upstream:

```
Request via Key K to upstream U
        │
        ▼
┌───────────────────────────────────────┐
│  Does K have an active KeySecret      │
│  for upstream U?                      │
│                                       │
│  YES → use KeySecret (BYOK)           │
│         metering → user's account     │
│                                       │
│  NO  → does workspace have an active  │
│        Secret for upstream U?         │
│                                       │
│        YES → use workspace Secret     │
│               (shared)                │
│               metering → workspace    │
│               account                 │
│                                       │
│        NO  → DENIED                   │
│              "no credential           │
│               configured for          │
│               this upstream"          │
└───────────────────────────────────────┘
```

**Summary table:**

```
┌─────────────────────────────────────────────────────────────────┐
│  Key has KeySecret?  │  Workspace has Secret?  │  Outcome       │
│──────────────────────┼─────────────────────────┼────────────────│
│  Yes (active)        │  Yes or No              │  Use KeySecret │
│  No                  │  Yes (active)           │  Use workspace │
│                      │                         │  Secret        │
│  No                  │  No / all inactive      │  DENIED        │
└─────────────────────────────────────────────────────────────────┘
```

---

### 6.4 Egress identity and cryptographic onboarding

When an admin onboards an egress to a workspace, **five** cryptographic
artefacts must be established:

1. **Egress identity** — an X.509 certificate binding the egress instance to its workspace
2. **Egress key pair** — an asymmetric key pair for egress→control-plane authentication
3. **Control plane validation key** — the CP public key distributed to egress instances for verifying signed telemetry
4. **Egress PASETO keypair #1** — Ed25519 keypair used by the Egress to sign/verify PASETO tokens (public key held by Egress, private key in secret service)
5. **Egress PASETO keypair #2** — second Ed25519 keypair for the same purpose (provides key rotation / redundancy)

```
Admin onboards egress
        │
        ├──► 1. Generate EgressIdentity (X.509)
        │       subject: egress@workspace1.project1.organization
        │       issued by: organization CA
        │       validity: 90 days (configurable)
        │
        ├──► 2. Generate EgressKeyPair
        │       algorithm: Ed25519 (recommended)
        │       private key → encrypted, stored in secret service
        │       public key  → registered with control plane
        │
        └──► 3. Distribute CPValidationKey to egress
                CP public key for verifying telemetry signatures
                rotated independently of egress keys
```

**Why five separate artefacts?**

| Artefact | Purpose | Lifecycle |
|----------|---------|-----------|
| `EgressIdentity` (X.509) | Attests "this egress instance belongs to this workspace" | Bound to workspace; rotated on re-deployment |
| `EgressKeyPair` | Authenticates telemetry and usage meters egress → CP | Independent of identity; can be rotated without re-deploying egress |
| `CPValidationKey` | CP public key embedded in egress for verifying signed requests *from* CP and from other egress instances | Org-wide; single key pair per purpose |
| `EgressPASETOKeyPair` (×2) | Egress-owned Ed25519 keypairs used to sign/verify PASETO tokens issued on behalf of users | Created at egress onboarding; rotated independently |

**EgressIdentity (X.509 certificate):**

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

**EgressKeyPair (asymmetric authentication):**

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

---

### 6.5 Egress-to-control-plane authentication

Egress instances send usage telemetry, health heartbeats, and audit logs
to the control plane. These messages must be authenticated to prevent
spoofing of usage data.

**Authentication flow:**

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

**Telemetry payload structure:**

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

**Properties:**

- Every telemetry payload is signed with the egress private key.
- The control plane verifies the signature using the registered public key.
- Signatures include timestamps; CP rejects messages with stale timestamps (>5 min skew).
- Sequence numbers and hash chaining prevent replay attacks and detect dropped messages.
- The private key never leaves the secret service boundary.

---

### 6.6 Stateless user key verification with PASETO

Users obtain PASETO tokens via the CLI (`orange token generate`). The platform signs tokens using the Egress-owned PASETO keypairs (created during egress onboarding).
This enables **local verification** at the egress (no CP round-trip in the common case) — the egress can validate
a user's request without querying the control plane for every request.

**Why PASETO v4.public:**

| Feature | PASETO v4.public |
|---------|------------------|
| Verification | Local (cached pubkey + revocation list); no CP round-trip in common case |
| Latency | Local signature verification only |
| Offline operation | Egress can verify without CP |
| Key rotation | User generates and rotates locally |
| Revocation | Revocation list check (cached) |

**PASETO key format:**

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

**User key generation flow:**

```
User runs: orange key create key3 --workspace workspace1 --project project1
        │
        ▼
  Platform registers a Key for the user
  (tokens will be signed using the Egress-owned PASETO keypairs)

User can now request tokens:
  orange token generate --key key3 --workspace workspace1 --project project1
    → Returns a fresh signed PASETO token (valid for configured TTL)
    → A `PASETOToken` record is created (metadata + hash, **not** the token value)
```

**Stateless verification at egress:**

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
│              egress's workspace)                     │
│     → "exp": check not expired                      │
│     → "pol": key policy (embedded, user-signed)     │
│                                                      │
│  4. Check key revocation list                       │
│     → Query CP: is this key revoked?                │
│     → (Can be cached with TTL; stale-while-revalidate)│
│                                                      │
│  5. Proceed to policy evaluation (§5)               │
│     → Embedded "pol" claim is treated as key        │
│       flexible policy — still subject to workspace  │
│       floor and project floor (narrowing invariant) │
└─────────────────────────────────────────────────────┘
```

**Revocation check strategy:**

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

**PASETO token verification and policy evaluation:**

```
┌──────────────────────────────────────────────────────────────────┐
│  Token type     │  Step 0 verification  │  Step 4 key policy   │
│─────────────────┼───────────────────────┼──────────────────────│
│  PASETO v4.pub  │  Local Ed25519 sig    │  Embedded "pol"      │
│                 │  verify + revocation  │  claim (user-signed) │
│                 │  list check           │  + narrowing check   │
└──────────────────────────────────────────────────────────────────┘
```

---

### 6.7 Certificate and key lifecycle

The cryptographic artefacts introduced in §6.4–6.6 follow a structured
lifecycle: **creation → rotation → revocation → expiry**.

**Lifecycle matrix:**

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

**Rotation — EgressKeyPair:**

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

**Rotation — CPValidationKey (org-wide):**

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

**Revocation — emergency procedures:**

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

**Expiry handling:**

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

---

## 7. Onboarding scenario (step by step)

```
Actors
  admin  →  admin@organization
  user1  →  user1@organization
  user2  →  user2@organization
```

### Step 1 — Admin creates project

```
admin@organization
  creates project1@organization
```

### Step 2 — Admin creates workspace

```
admin@organization
  creates workspace1@project1.organization
```

### Step 3 — Admin onboards egress (with identity and key pair)

```
admin@organization
  deploys egress@workspace1.project1.organization
        │
        ├──► System automatically:
        │    1. Generates EgressIdentity (X.509)
        │       CN=egress.workspace1.project1.organization
        │       Validity: 90 days
        │
        │    2. Generates EgressKeyPair (Ed25519)
        │       Private key → secret service (vault path)
        │       Public key  → registered with CP
        │
        │    3. Distributes CPValidationKey to egress
        │       (CP public key for telemetry verification)
        │
        │    4. Generates EgressPASETOKeyPair #1 (Ed25519)
        │       Private key → secret service
        │       Public key  → held by Egress for token verification
        │
        │    5. Generates EgressPASETOKeyPair #2 (Ed25519)
        │       (second pair for rotation / redundancy)
        │
        └──► Egress instance boots with:
             - Identity certificate (mTLS to CP)
             - Private key reference (resolves from secret service)
             - CP validation key (verifies CP-signed config)
             - Two EgressPASETOKeyPairs (for signing/verifying user tokens)
```

### Step 4 — Admin sets workspace secrets

```
admin@organization
  sets secret1:v1  →  upstream1
  sets secret2:v1  →  upstream2
  on egress@workspace1.project1.organization
```

### Step 5 — Admin assigns users

```
admin@organization
  assigns user1@organization  →  workspace1@project1.organization
  assigns user2@organization  →  workspace1@project1.organization
```

### Steps 6–7 — Users create keys and attach policies

```
user1@organization
  runs: orange key create key1 --workspace workspace1 --project project1
    → Platform registers Key (signing done via EgressPASETOKeyPair)
  runs: orange key create key2 --workspace workspace1 --project project1
    → Platform registers second Key
  attaches policy-A  →  key1  (restricts to upstream1 only)
  attaches policy-B  →  key2  (restricts rate limit to 100 req/min)
```

### Step 8 — User sends request (PASETO token)

```
user1@organization
  obtains a signed PASETO token from the platform
    (via orange token generate --key key1 ...)
  sends request via key1@workspace1.project1.organization (PASETO)
    Authorization: Paseto v4.public.<payload>.<signature>
        │
        ▼
  Egress evaluates policies (project floor → workspace → key1)
        │ all pass
        ▼
  Credential resolution: key1 has no KeySecret → workspace Secret
  Egress proxies to upstream1 using active secret: secret1:v1
        │
        ▼
  upstream1 responds → user1 receives response
```

### Step 9 — User sends request (PASETO token)

```
user1@organization
  obtains a signed PASETO token from the platform
    (via orange token generate --key key3 ...)
  sends request via key3@workspace1.project1.organization (PASETO)
    Authorization: Paseto v4.public.<payload>.<signature>
        │
        ▼
  Egress receives request:
    1. Validates PASETO signature locally against cached EgressPASETOKeyPair
       (no CP round-trip for verification)
    2. Checks token not on revocation list (cached result)
    3. Evaluates policies (project floor → workspace → embedded "pol")
    4. Resolves credentials → workspace Secret
    5. Proxies to upstream
        │
        ▼
  upstream1 responds → user1 receives response
```

### Step 10 — Admin rotates secret

```
admin@organization
  rotates secret1:v1 → secret1:v2
  on egress@workspace1.project1.organization
        │
        ▼
  Egress now resolves secret1:v2 for all new requests to upstream1
  Existing keys: unaffected
  In-flight requests: drain on v1, new requests use v2
```

### Step 11 (optional) — User binds personal upstream credential (BYOK)

```
user1@organization
  creates a new key for BYOK:
    orange key create key3 --workspace workspace1 --project project1
  binds KeySecret:v1 for upstream1 on key3
    (user1's own upstream1 API key)
        │
        ▼
  Requests via key3 to upstream1 now use user1's personal credential
  Metering: against user1's upstream1 account
  key1 and key2 continue using workspace Secret (unaffected)
```

### Step 12 (optional) — Admin rotates egress key pair

```
admin@organization
  rotates EgressKeyPair for egress@workspace1.project1.organization
        │
        ▼
  Secret service generates new Ed25519 key pair
  New public key registered with CP
  Egress retrieves new private key, begins signing with it
  CP accepts both old and new signatures during 5-min grace window
  After grace window: old signatures rejected
```

### Full flow (ASCII sequence)

```
admin          egress/system     user1           upstream1
  │                 │               │                │
  ├─create proj────►│               │                │
  ├─create ws───────►               │                │
  ├─deploy egress──►│               │                │
  │  (auto-gen      │               │                │
  │   identity +    │               │                │
  │   keypair +     │               │                │
  │   2 PASETO pairs)│              │                │
  ├─set secrets─────►               │                │
  ├─assign user1────►               │                │
  │                 │◄──────────────┤ create key1    │
  │                 │◄──────────────┤ attach policy  │
  │                 │◄──────────────┤ create key3    │
  │                 │                │  (new Key)     │
  │                 │◄──────────────┤ send request   │
  │                 │  (PASETO token)│                │
  │                 │   (signed by   │                │
  │                 │    EgressPASETOKeyPair)         │
  │                 ├─eval policies─┤                │
  │                 ├─resolve cred (workspace Secret)│
  │                 ├───proxy req─────────────────────►
  │                 │◄──────────────────────response─┤
  │                 ├───────────────►response        │
  ├─rotate secret──►│               │                │
  │                 │ (v2 now active for workspace)   │
  │                 │◄──────────────┤ bind KeySecret │  (BYOK)
  │                 │◄──────────────┤ send request   │
  │                 ├─eval policies─┤                │
  │                 ├─resolve cred (KeySecret v1)────┤
  │                 ├───proxy req─────────────────────►
  │                 │◄──────────────────────response─┤
  │                 ├───────────────►response        │
  ├─rotate keypair─►│               │                │
  │                 │ (new Ed25519 pair active)       │
```

---

## 8. All operational scenarios

---

### 8.1 Admin scenarios

#### A1 — Create a project

```
Precondition: org exists
Actor: admin

admin creates project
  project.org_id = organization.id
  project.name   = "project1"

Result: project1@organization exists
```

#### A2 — Create a workspace within a project

```
Precondition: project exists
Actor: admin

admin creates workspace
  workspace.project_id = project1.id
  workspace.name       = "workspace1"

Result: workspace1@project1.organization exists
```

#### A3 — Onboard egress to a workspace (with identity and key pair)

```
Precondition: workspace exists, no egress yet
Actor: admin

admin deploys egress
  egress.workspace_id = workspace1.id
        │
        └──► System:
             1. Generates EgressIdentity (X.509)
                CN=egress.workspace1.project1.organization
                issued by: org CA
                validity: 90 days
             2. Generates EgressKeyPair (Ed25519)
                private key → secret service (encrypted)
                public key  → registered with CP
             3. Distributes CPValidationKey to egress

Result:
  egress@workspace1.project1.organization is live
  EgressIdentity created and active
  EgressKeyPair created, private key in secret service
  workspace now able to accept secrets and keys
```

#### A4 — Set a secret on an egress

```
Precondition: egress deployed
Actor: admin

admin creates secret
  secret.workspace_id    = workspace1.id
  secret.upstream_target = "upstream1"
  secret.version         = "v1"
  secret.active          = true

Result: egress can now authenticate to upstream1 (shared credential)
```

#### A5 — Assign a user to a workspace

```
Precondition: user exists in org, workspace exists
Actor: admin

admin creates WorkspaceMember
  workspace_id = workspace1.id
  user_id      = user1.id

Result: user1 can now create keys and send requests
        via workspace1
```

#### A6 — Remove a user from a workspace

```
Precondition: user is a workspace member
Actor: admin

admin deletes WorkspaceMember(workspace1, user1)

Result:
  user1 loses access to workspace1
  user1's existing keys in workspace1 are invalidated
  user1's KeySecrets bound to those keys are purged
  user1's token records purged (associated tokens invalidated)
  in-flight requests using those keys are rejected
```

#### A7 — Set a floor policy on a project

```
Precondition: project exists
Actor: admin

admin creates Policy {
  scope_type: "project",
  scope_id:   project1.id,
  type:       "floor",
  rules:      [{ deny: upstream3 }]
}

Result: no key in any workspace under project1
        can ever reach upstream3, regardless of
        what workspace or key policies allow —
        even if a user has a KeySecret for upstream3
```

#### A8 — Set a floor policy on a workspace

```
Precondition: workspace exists
Actor: admin

admin creates Policy {
  scope_type: "workspace",
  scope_id:   workspace1.id,
  type:       "floor",
  rules:      [{ max_req_per_min: 1000 }]
}

Result: all keys in workspace1 are hard-capped
        at 1000 req/min regardless of key policies
        or whether BYOK is in use
```

#### A9 — Tighten a workspace flexible policy (lazy invalidation)

```
Precondition: workspace1 allows upstream1 + upstream2
              key1 stored policy allows upstream1 + upstream2
Actor: admin

admin updates workspace1 flexible policy:
  now allows upstream1 ONLY (removes upstream2)

Stored key1 policy: unchanged
Effective key1:     intersect(workspace1, key1) = upstream1 only

Request via key1 to upstream2: DENIED (workspace clamps at eval time)
Request via key1 to upstream1: ALLOWED

Note: if key3 has a KeySecret for upstream2 but workspace no longer
      allows upstream2, the request is still DENIED at policy level
      before credential resolution is reached.

No write to key1 required.
```

#### A10 — Rotate a secret (zero-downtime)

```
Precondition: secret1:v1 active, requests flowing
Actor: admin

admin creates new secret version:
  secret1:v2  active: true
  secret1:v1  active: false

Egress resolves active version per request:
  requests dispatched before rotation: drain on v1
  requests dispatched after rotation:  use v2

Keys with KeySecrets for upstream1: unaffected
  (they never used the workspace Secret to begin with)

Result: no downtime, no key changes required
```

#### A11 — Add a second workspace to a project

```
Precondition: project1 exists with workspace1
Actor: admin

admin creates workspace2@project1.organization

Result:
  workspace2 is a sibling of workspace1 under project1
  project-level floor policies apply to workspace2
  workspace2 has its own egress, secrets, members, keys
  workspace1 is unaffected
```

#### A12 — Remove a workspace

```
Precondition: workspace1 exists with active members and keys
Actor: admin

admin deletes workspace1

Result:
  all keys in workspace1 are invalidated
  all KeySecrets bound to those keys are purged
  all PASETOToken records for workspace1 are purged
  all WorkspaceMember entries for workspace1 are removed
  egress is decommissioned
  EgressIdentity revoked
  EgressKeyPair deactivated
  workspace Secrets are purged
  in-flight requests are rejected
```

#### A13 — Describe a workspace (admin view)

```
Actor: admin

admin describes workspace1@project1.organization

Output:
  name:        workspace1
  project:     project1@organization
  description: "Production egress for ML pipeline"
  egress:      egress@workspace1.project1.organization  [active]
    identity:    CN=egress.workspace1... [expires: 2026-09-04]
    keypair:     Ed25519 [active, rotated: 2026-06-01]
  members:     user1@organization, user2@organization
  secrets:
    secret1  upstream: upstream1  active_version: v2
    secret2  upstream: upstream2  active_version: v1
  policies:
    [floor]    project1: deny upstream3
    [floor]    workspace1: max_req_per_min=1000
    [flexible] workspace1: allow upstream1, upstream2
  keys:
    key1   owner: user1   format: paseto_v4.public      policy: allow upstream1       byok: —
    key2   owner: user1   format: paseto_v4.public      policy: max_req_per_min=100   byok: —
    key3   owner: user1   format: paseto_v4.public   policy: (none)                byok: upstream1
    key4   owner: user2   format: paseto_v4.public      policy: (none)                byok: —
```

#### A14 — Describe a project (admin view)

```
Actor: admin

admin describes project1@organization

Output:
  name:        project1
  org:         organization
  description: "Internal ML infrastructure"
  workspaces:
    workspace1@project1.organization  egress: active   members: 2  keys: 4
    workspace2@project1.organization  egress: inactive members: 0  keys: 0
  floor policies:
    deny: upstream3
  flexible policies:
    (none set at project level)
```

#### A15 — Describe an egress (admin view)

```
Actor: admin

admin describes egress@workspace1.project1.organization

Output:
  workspace:   workspace1@project1.organization
  status:      active
  description: "Proxies ML API calls"
  identity:
    certificate: CN=egress.workspace1.project1.organization
    serial:      550e8400-e29b-41d4-a716-446655440000
    issued:      2026-06-06
    expires:     2026-09-04
    algorithm:   Ed25519
    active:      true
  keypair:
    algorithm:   Ed25519
    public_key:  MCowBQYDK2VwAyEA... (thumbprint: abc123)
    private_key: secret-service://vault/egress/ws1/key-v2 (not exposed)
    active:      true
    created:     2026-06-06
    rotated:     2026-06-01
  cp_validation_key:
    thumbprint:  def456
    active:      true
  secrets:
    secret1
      upstream:        upstream1
      active_version:  v2
      versions:        v1 [superseded], v2 [active]
      description:     "OpenAI API key"
    secret2
      upstream:        upstream2
      active_version:  v1
      versions:        v1 [active]
      description:     "Internal model service token"
  effective_floor_policies:
    project floor: deny upstream3
    workspace floor: max_req_per_min=1000
```

#### A16 — Describe a user (admin view)

```
Actor: admin

admin describes user1@organization

Output:
  email:       user1@organization
  description: "ML platform team"
  workspaces:
    workspace1@project1.organization
    workspace2@project1.organization
  keys:
    key1@workspace1  policy: allow upstream1        byok: —
    key2@workspace1  policy: max_req_per_min=100    byok: —
    key3@workspace1  policy: (none)                 byok: upstream1
    key5@workspace2  policy: (none)                 byok: —
```

#### A17 — Rotate egress key pair

```
Precondition: egress deployed, EgressKeyPair active
Actor: admin

admin rotates EgressKeyPair for egress@workspace1

Result:
  Secret service generates new Ed25519 key pair
  New EgressKeyPair created, marked active
  Old EgressKeyPair marked retired (verify-only for grace window)
  Egress instance retrieves new private key
  Telemetry signed with old key: accepted during 5-min grace
  Telemetry signed with old key: rejected after grace
```

#### A18 — Rotate control plane validation key

```
Precondition: CPValidationKey exists
Actor: admin

admin rotates CPValidationKey (org-wide)

Result:
  New CPValidationKey generated (Ed25519)
  Pushed to all active egress instances
  Grace window: both old and new keys accepted
  After grace: old key deactivated
```

---

### 8.2 User scenarios

#### U1 — Create a key

```
Precondition: user1 is a member of workspace1
Actor: user1

user1 creates Key {
  workspace_id: workspace1.id,  -- immutable after this point
  user_id:      user1.id,
  name:         "key1",
}

Result: key1@workspace1.project1.organization exists
        inherits workspace flexible policy by default
        no key policy attached yet
        no KeySecret bound yet
```

#### U2 — Create a PASETO-format key

```
Precondition: user1 is a member of workspace1
Actor: user1

User runs:
  orange key create key3 --workspace workspace1 --project project1

System behavior:
  - CLI generates a fresh Ed25519 keypair
  - Public key is stored in CP (EgressPASETOKeyPair)
  - Private key is stored in the secret service (referenced by EgressPASETOKeyPair)
  - User never sees or manages the private key

Result:
  key3@workspace1.project1.organization exists
  User can request signed tokens via:
    orange token generate --key key3 ...
  Egress verifies PASETO signatures statelessly using the public key
```

#### U3 — Attach a policy to a key (valid — narrowing)

```
Precondition: workspace1 allows upstream1 + upstream2
              key1 has no policy yet
Actor: user1

user1 creates Policy {
  scope_type: "key",
  scope_id:   key1.id,
  type:       "flexible",
  rules:      [{ allow: upstream1 ONLY }]
}

Validation: upstream1 ⊆ {upstream1, upstream2}  ✓

Result: key1 is now restricted to upstream1 only
        key2 (if it exists) is unaffected
```

#### U4 — Attach a policy to a key (invalid — widening attempt)

```
Precondition: workspace1 allows upstream1 ONLY
Actor: user1

user1 attempts Policy {
  scope_type: "key",
  scope_id:   key1.id,
  type:       "flexible",
  rules:      [{ allow: upstream1, upstream2 }]
}

Validation: upstream2 ⊄ {upstream1}  ✗

Result: write REJECTED
        error: "policy exceeds workspace permissions"
        key1 policy unchanged
```

#### U5 — Send a request (allowed, workspace Secret used, PASETO token)

```
Precondition:
  key1 policy: allow upstream1
  workspace policy: allow upstream1 + upstream2
  project floor: deny upstream3
  key1 has no KeySecret

Actor: user1

user1 sends: POST /request
  Authorization: Paseto v4.public.<payload>.<signature>
  target: upstream1

Evaluation:
  Step 0: PASETO signature verified locally (Ed25519) + revocation list check
  project floor:      upstream1 ≠ upstream3  ✓
  workspace flexible: upstream1 ∈ allowed     ✓
  key flexible:       upstream1 ∈ allowed     ✓

Credential resolution:
  key1 has no KeySecret for upstream1 → use workspace Secret
  active version: secret1:v2

Result: request proxied to upstream1 using workspace Secret
        metering against workspace's upstream1 account
```

#### U6 — Send a request (allowed, PASETO token)

```
Precondition:
  key3 format: paseto_v4.public
  key3 backed by EgressPASETOKeyPair
  workspace policy: allow upstream1 + upstream2
  key3 has no KeySecret

Actor: user1

Step 1: User constructs and signs PASETO token locally
  {
    "kid": "key3@workspace1.project1.organization",
    "sub": "user1@organization",
    "wsk": "workspace1@project1.organization",
    "iat": "2026-06-06T12:00:00Z",
    "exp": "2026-06-06T13:00:00Z",
    "pol": { "allow": ["upstream1", "upstream2"] }
  }
  → Signs with Ed25519 private key

Step 2: User sends request
  Authorization: Paseto v4.public.<payload>.<signature>
  target: upstream1

Step 3: Egress verifies
  PASETO signature valid ✓ (local verification, no CP lookup)
  workspace match: wsk == this workspace ✓
  Not expired: exp > now ✓
  Revocation check: key3 not on revocation list ✓ (cached)

Step 4: Policy evaluation
  project floor:      pass ✓
  workspace flexible: upstream1 ∈ allowed ✓
  key flexible:       (from embedded "pol") upstream1 ∈ allowed ✓

Credential resolution:
  key3 has no KeySecret → workspace Secret v2

Result: request proxied to upstream1
        metering against workspace's upstream1 account
        ZERO round-trips to CP for verification
```

#### U7 — Send a request (denied by key policy)

```
Precondition:
  key1 policy: allow upstream1 ONLY
  workspace policy: allow upstream1 + upstream2

Actor: user1

user1 sends: POST /request
  Authorization: key1
  target: upstream2

Evaluation:
  project floor:      pass  ✓
  workspace flexible: upstream2 ∈ allowed  ✓
  key flexible:       upstream2 ∉ {upstream1}  ✗

Result: DENIED at key policy level
        credential resolution not reached
```

#### U8 — Send a request (denied by floor policy)

```
Precondition:
  project floor: deny upstream3
  workspace policy: allow all
  key1 policy: allow all

Actor: user1

user1 sends: POST /request  target: upstream3

Evaluation:
  project floor: upstream3 = denied  ✗  → BLOCKED immediately

Result: DENIED at project floor
        evaluation stops — no further checks
        KeySecret for upstream3 (if any) is irrelevant
```

#### U9 — user2 creates a key independently

```
Precondition: user2 is a member of workspace1
Actor: user2

user2 creates key4@workspace1.project1.organization

Result:
  key4 is owned by user2, bound to workspace1
  user1's keys are unaffected
  key4 inherits workspace policy
  key4 has no KeySecret bound
```

#### U10 — User attempts to use another user's key

```
Precondition: key1 owned by user1
Actor: user2

user2 sends request with key1

Result: DENIED
        key1.user_id ≠ user2.id
        auth check fails before policy evaluation
```

#### U11 — Delete a key

```
Precondition: key1 owned by user1
Actor: user1

user1 deletes key1

Result:
  key1 is invalidated immediately
  any KeySecrets bound to key1 are purged
  any PASETOToken records for key1 are purged
  in-flight requests using key1 are rejected
  key2 is unaffected
  workspace membership unchanged
```

#### U12 — Describe own key (user view)

```
Actor: user1

user1 describes key1@workspace1.project1.organization
  (key1 has no KeySecret, paseto_v4.public format)

Output:
  name:             key1
  workspace:        workspace1@project1.organization
  description:      "Used by training pipeline"
  owner:            user1@organization
  format:           paseto_v4.public
  policy (stored):  allow upstream1
  bound credentials (BYOK):
    (none)
  effective policy (resolved):
    project floor:    deny upstream3
    workspace floor:  max_req_per_min=1000
    workspace flex:   allow upstream1, upstream2
    key flex:         allow upstream1
    ─────────────────────────────────────────
    resolved:         allow upstream1, max 1000 req/min
  credential resolution:
    upstream1 → workspace Secret v2 (shared)
```

#### U13 — Describe own PASETO key (user view)

```
Actor: user1

user1 describes key3@workspace1.project1.organization
  (key3 is PASETO format, no KeySecret)

Output:
  name:             key3
  workspace:        workspace1@project1.organization
  description:      "PASETO key for local signing"
  owner:            user1@organization
  format:           paseto_v4.public
  public_key_thumbprint: abc123...
  policy (embedded in PASETO): allow upstream1, upstream2
  bound credentials (BYOK):
    (none)
  effective policy (resolved):
    project floor:    deny upstream3
    workspace floor:  max_req_per_min=1000
    workspace flex:   allow upstream1, upstream2
    key flex:         allow upstream1, upstream2 (from token)
    ─────────────────────────────────────────
    resolved:         allow upstream1, upstream2, max 1000 req/min
  credential resolution:
    upstream1 → workspace Secret v2 (shared)
    upstream2 → workspace Secret v1 (shared)
  verification mode: stateless (PASETO signature verified locally)
  private_key_location: (managed securely by platform)
```

#### U14 — Describe own workspace (user view)

```
Actor: user1

user1 describes workspace1@project1.organization

Output:
  name:        workspace1
  project:     project1@organization
  description: "Production egress for ML pipeline"
  egress:      active
  upstreams available (from workspace secrets): upstream1, upstream2
  floor policies (read-only):
    project floor:   deny upstream3
    workspace floor: max_req_per_min=1000
  workspace flexible policy:
    allow upstream1, upstream2
  my keys:
    key1   format: paseto_v4.public      policy: allow upstream1         effective: allow upstream1         byok: —
    key2   format: paseto_v4.public      policy: max_req_per_min=100     effective: allow upstream1+2       byok: —
    key3   format: paseto_v4.public   policy: (embedded)              effective: allow upstream1+2       byok: upstream1
```

> Users see their own keys only. They cannot see other users' keys, secret
> values (workspace or KeySecret), or other users' BYOK configuration.
> PASETO private keys are always local to the user — never visible in CP.

#### U15 — Update a key policy (further restrict)

```
Precondition:
  key1 policy: allow upstream1 + upstream2
  workspace policy: allow upstream1 + upstream2

Actor: user1

user1 updates key1 policy: rules: [{ allow: upstream1 ONLY }]

Validation: upstream1 ⊆ {upstream1, upstream2}  ✓

Result: key1 now restricted to upstream1 only
        takes effect immediately for new requests
```

#### U16 — Update a key policy (attempt to widen)

```
Precondition:
  key1 policy: allow upstream1 ONLY
  workspace policy: allow upstream1 ONLY

Actor: user1

user1 attempts: rules: [{ allow: upstream1, upstream2 }]

Validation: upstream2 ⊄ workspace policy  ✗

Result: REJECTED — existing key1 policy unchanged
```

---

#### B1 — User binds own upstream credential to a key (BYOK)

```
Precondition:
  user1 owns key3@workspace1.project1.organization
  workspace1 allows upstream1 (policy permits it)
  workspace1 has workspace Secret for upstream1 (secret1:v2)
Actor: user1

user1 creates KeySecret {
  key_id:          key3.id,
  upstream_target: "upstream1",
  version:         "v1",
  value:           <user1's personal upstream1 API key>,
  active:          true
}

Result:
  key3 now has a personal KeySecret for upstream1
  requests via key3 to upstream1 use user1's credential
  metering: against user1's own upstream1 account
  key1, key2 continue using workspace Secret (unaffected)
  workspace Secret secret1:v2 is unaffected
```

#### B2 — Request dispatched using BYOK credential

```
Precondition:
  key3 has KeySecret for upstream1:v1 (active)
  workspace also has Secret for upstream1:v2 (active)
  no key policy restriction on key3
Actor: user1

user1 sends: POST /request
  Authorization: key3 (or PASETO)
  target: upstream1

Policy evaluation:
  project floor:      upstream1 ≠ upstream3  ✓
  workspace flexible: upstream1 ∈ allowed     ✓
  key flexible:       (none — inherits workspace) ✓

Credential resolution:
  key3 has active KeySecret for upstream1 → use KeySecret v1

Result:
  request proxied to upstream1 using user1's personal credential
  workspace Secret for upstream1 is NOT used
  metering: against user1's own upstream1 account
```

#### B3 — Request falls back to workspace Secret (no KeySecret)

```
Precondition:
  key1 has no KeySecret for upstream1
  workspace has active Secret for upstream1 (secret1:v2)
Actor: user1

user1 sends request via key1 to upstream1

Credential resolution:
  key1 has no KeySecret for upstream1 → fall back to workspace Secret
  active workspace Secret: secret1:v2

Result:
  request proxied using workspace Secret
  metering: against workspace's upstream1 account
```

#### B4 — User binds credential for upstream without a workspace Secret

```
Precondition:
  workspace policy: allow upstream1, upstream2, upstream3
  workspace Secrets: upstream1, upstream2 only (no Secret for upstream3)
  project floor: deny is only for upstream4+
  key3 has no policy restriction
Actor: user1

user1 creates KeySecret {
  key_id:          key3.id,
  upstream_target: "upstream3",
  version:         "v1",
  value:           <user1's upstream3 API key>,
  active:          true
}

user1 sends request via key3 to upstream3

Policy evaluation:
  project floor:      upstream3 not denied  ✓
  workspace flexible: upstream3 ∈ allowed   ✓
  key flexible:       pass ✓

Credential resolution:
  key3 has active KeySecret for upstream3 → use KeySecret v1
  (no workspace Secret for upstream3 exists — not needed)

Result:
  request proxied to upstream3 using user1's personal credential
  admin was not required to configure upstream3 in workspace Secrets
  other keys (key1, key2) cannot reach upstream3 (no workspace Secret)
  unless they also supply their own KeySecret for it
```

#### B5 — User rotates their bound credential (zero-downtime)

```
Precondition: key3 has KeySecret upstream1:v1 (active)
Actor: user1

user1 rotates KeySecret on key3 for upstream1:
  upstream1:v2  active: true
  upstream1:v1  active: false

In-flight requests via key3 to upstream1: drain on v1
New requests via key3 to upstream1:       use v2

workspace Secret for upstream1: unaffected
key1, key2: unaffected

Result: zero-downtime rotation of user1's personal credential
```

#### B6 — User removes their bound credential (fallback to workspace Secret)

```
Precondition: key3 has KeySecret upstream1:v1 (active)
Actor: user1

user1 removes KeySecret for upstream1 on key3

Result:
  key3 no longer has a bound credential for upstream1
  requests via key3 to upstream1 now use workspace Secret
  metering reverts to workspace's upstream1 account
  user1's personal upstream1 account no longer metered for key3 traffic
```

#### B7 — BYOK credential set but upstream blocked by policy

```
Precondition:
  project floor: deny upstream3
  key3 has KeySecret for upstream3 (user supplied it)
  workspace flexible: allows upstream1, upstream2 (not upstream3)
Actor: user1

user1 sends request via key3 to upstream3

Policy evaluation:
  project floor: upstream3 denied  ✗  → BLOCKED immediately

Result:
  DENIED at project floor
  BYOK credential is irrelevant — policy check runs before
  credential resolution; having a KeySecret does not grant access
  to upstreams that policies prohibit
```

---

### 8.3 Policy conflict scenarios

#### P1 — Key policy more permissive than workspace (write-time rejection)

```
workspace1 allows: upstream1
user1 sets key1 policy: allow upstream1 + upstream2

→ REJECTED at write time
  stored key policies always satisfy the narrowing invariant
```

#### P2 — Two keys, different policies, same workspace

```
workspace1 allows: upstream1 + upstream2

key1 policy (user1): allow upstream1 ONLY
key2 policy (user1): allow upstream2 ONLY

Request via key1 to upstream1:  ALLOWED
Request via key1 to upstream2:  DENIED  (key1 policy)
Request via key2 to upstream1:  DENIED  (key2 policy)
Request via key2 to upstream2:  ALLOWED
```

#### P3 — Floor policy blocks what workspace flexible allows

```
project floor: deny upstream2

workspace1 flexible: allow upstream1 + upstream2

Request via any key to upstream2:
  project floor check: upstream2 denied  ✗
  → BLOCKED immediately
  workspace flexible policy is irrelevant
  KeySecret for upstream2 (if any) is irrelevant
```

#### P4 — No key policy set (inherits workspace)

```
workspace1 policy: allow upstream1 + upstream2, max 500 req/min
key1: no policy attached

Effective policy for key1 = workspace policy
  allow upstream1 + upstream2, max 500 req/min
```

#### P5 — Workspace tightened after key policy set (lazy)

```
Before:
  workspace1: allow upstream1 + upstream2
  key1 stored policy: allow upstream1 + upstream2

Admin tightens workspace1: allow upstream1 ONLY

After:
  key1 stored policy: allow upstream1 + upstream2  (unchanged)
  effective key1:     intersect = upstream1 ONLY

Request via key1 to upstream2: DENIED
Request via key1 to upstream1: ALLOWED
```

#### P6 — Multiple floor policies stacked (project + workspace)

```
project floor:   deny upstream3
workspace floor: deny upstream2

workspace1 flexible: allow upstream1 + upstream2

Effective policy for all keys in workspace1:
  can reach: upstream1 only
  upstream2 blocked by workspace floor
  upstream3 blocked by project floor
```

#### P7 — PASETO embedded policy vs workspace floor (narrowing still applies)

```
workspace1 floor: max_req_per_min=1000
workspace1 flex:  allow upstream1 + upstream2

user1 creates PASETO token with embedded policy:
  "pol": { "allow": ["upstream1", "upstream2"], "max_req_per_min": 5000 }

Evaluation:
  Step 0: PASETO signature valid ✓
  Step 1: project floor: pass ✓
  Step 2: workspace floor: max_req_per_min=1000
           embedded request for 5000 > 1000 → DENIED

Result: DENIED at workspace floor
        Embedded "pol" cannot override floor policies
        Narrowing invariant applies to PASETO embedded claims
```

---

### 8.4 Secret rotation scenarios

#### R1 — Standard rotation (no in-flight requests)

```
State: no active requests

admin rotates secret1: v1 → v2
  secret1:v1  active: false
  secret1:v2  active: true

Next request to upstream1 (key without KeySecret):
  egress resolves active workspace Secret = v2

Next request to upstream1 (key with KeySecret):
  egress resolves active KeySecret (unaffected by workspace rotation)

Result: clean cutover, no disruption
```

#### R2 — Rotation with in-flight requests

```
State: 5 requests mid-flight using secret1:v1

admin rotates secret1: v1 → v2

In-flight requests: complete using v1 (already dispatched)
New requests:       egress resolves v2

Result: zero dropped requests
```

#### R3 — Rotation does not affect keys or KeySecrets

```
State: key1, key2 active (no KeySecrets); key3 active (KeySecret v1 for upstream1)
       secret1:v1 active

admin rotates secret1: v1 → v2

key1, key2 status: unchanged (keys never hold secret values)
key3 status: unchanged; its KeySecret is its own versioned entity

user1 sends request via key1 to upstream1:
  policy eval: pass
  credential: key1 has no KeySecret → workspace Secret v2

user1 sends request via key3 to upstream1:
  policy eval: pass
  credential: key3 has KeySecret v1 → use KeySecret v1 (unchanged)

Result: workspace rotation is transparent to BYOK keys
```

#### R4 — Rotate secret for one upstream, other unaffected

```
State:
  secret1: v1  (upstream1)
  secret2: v1  (upstream2)

admin rotates secret1: v1 → v2

secret2 status: unchanged (v1 still active)

Requests to upstream1 (no KeySecret): use secret1:v2
Requests to upstream2 (no KeySecret): use secret2:v1
Requests to upstream1 (with KeySecret): use KeySecret (unaffected)
```

#### R5 — Emergency revocation

```
State: secret1:v1 compromised

admin:
  1. rotates secret1: v1 → v2  (new secure value)
  2. optionally sets workspace floor policy: deny upstream1
     (full lockdown until investigation complete)

Result:
  if floor policy set: all keys blocked from upstream1
                       including keys with KeySecrets (policy fires first)
  if not set:          keys without KeySecrets use v2 immediately
                       keys with KeySecrets are unaffected by the rotation
                       (they never used the workspace Secret)

Note: if the KeySecret values themselves are compromised, each key owner
      must rotate their own KeySecret independently. Admin can deny the
      upstream at workspace level as a stopgap during that window.
```

---

### 8.5 Edge cases and error scenarios

#### E1 — Attempt to create key in unassigned workspace

```
Actor: user1 (member of workspace1 only)

user1 attempts to create key in workspace2

Result: DENIED
        user1 is not a WorkspaceMember of workspace2
```

#### E2 — Attempt to access egress without a key

```
Actor: user1

user1 sends request with no Authorization header

Result: DENIED immediately
        no key = no identity = no policy evaluation
```

#### E3 — Expired or revoked key

```
Precondition: key1 deleted

user1 sends request with key1

Result: DENIED
        key not found in active key store
```

#### E4 — Request to upstream with no credential configured

```
Precondition:
  key1 has no KeySecret for upstream2
  workspace has no Secret for upstream2

user1 sends request targeting upstream2

Result: DENIED at egress dispatch
        error: "no credential configured for this upstream"
```

#### E5 — Workspace has no egress deployed

```
Precondition: workspace2 exists, no egress deployed

user1 (member of workspace2) sends request

Result: DENIED — workspace2 has no egress to proxy through
```

#### E6 — All secrets for an upstream deactivated

```
Precondition:
  key1 has no KeySecret for upstream1
  secret1:v1 active: false
  secret1:v2 active: false

user1 sends request via key1 to upstream1

Result: DENIED at egress dispatch
        error: "no active credential for this upstream"

Note: if key3 has an active KeySecret for upstream1, requests via
      key3 would still succeed — deactivating workspace Secrets
      does not affect user-bound KeySecrets.
```

#### E7 — Policy write while request in flight

```
Precondition: request R mid-flight, key1 active

user1 deletes key1 policy

Result:
  R completes (policy snapshot taken at dispatch)
  subsequent requests via key1 inherit workspace policy
```

#### E8 — User removed from workspace mid-session

```
Precondition: user1 active, key1 valid, key3 has KeySecret and active tokens

admin removes user1 from workspace1

Result:
  key1 invalidated immediately; in-flight request rejected at next auth check
  key3 invalidated; its KeySecret is purged
  key3's token records are purged (tokens invalidated)
  user1's PASETO private key still exists locally but useless
    (public key removed from CP → signature verification fails)
  user1 has no path back without reassignment
```

#### E9 — Two users, same workspace, isolated key policies and KeySecrets

```
user1: key1 (allow upstream1), key3 (byok: upstream1, paseto_v4.public)
user2: key4 (allow upstream1 + upstream2)

user1 cannot see or modify key4
user2 cannot see or modify key1 or key3

Policy isolation is per-key, per-owner.
KeySecret isolation is per-key, per-owner.
PASETO key isolation is per-key — user2 cannot derive user1's private key.
```

#### E10 — Admin sets floor policy more restrictive than existing keys

```
Before:
  workspace1 flexible: allow upstream1 + upstream2
  key1 policy: allow upstream1 + upstream2
  key3 KeySecret: upstream2

Admin sets project floor: deny upstream2

After:
  floor blocks upstream2 globally
  key1 stored policy: unchanged
  effective key1: upstream1 only (floor clamps at eval time)
  key3 requests to upstream2: DENIED at policy level
    (KeySecret is present but irrelevant — policy check fires first)
  key3 PASETO embedded policy claiming upstream2: DENIED at floor
    (embedded "pol" cannot override floor)
```

#### E11 — User attempts to bind KeySecret for upstream denied by floor

```
Precondition: project floor: deny upstream3

user1 creates KeySecret for upstream3 on key3

Result:
  KeySecret write: PERMITTED
    (KeySecret storage is not gated by policy — it is a credential,
     not a policy artefact)
  Requests via key3 to upstream3: DENIED at policy eval
    (the KeySecret exists but is never reached in dispatch)

Note: the write is allowed because storing a credential is harmless;
      policy enforcement happens at request time. Users are not prevented
      from pre-configuring a credential for an upstream that is currently
      blocked — an admin may loosen the policy later.
```

#### E12 — PASETO token with invalid signature

```
Precondition: key3 is PASETO format, backed by EgressPASETOKeyPair

Attacker forges PASETO token (wrong private key)

Result: DENIED at Step 0 (PASETO verification)
  Signature verification fails against registered public key
  Request rejected before policy evaluation
  No CP lookup needed — purely local rejection
```

#### E13 — PASETO token expired but signature valid

```
Precondition: key3 is PASETO format, valid EgressPASETOKeyPair backing

user1 sends request with expired PASETO token (exp < now)

Result: DENIED at Step 0 (PASETO verification)
  "Token has expired" — exp claim checked before any policy eval
  User must generate a fresh signed token
```

#### E14 — PASETO token for wrong workspace

```
Precondition: key3 is bound to workspace1 (wsk claim)

user1 sends request to workspace2's egress with key3 token

Result: DENIED at Step 0
  Egress detects wsk == "workspace1" but this is workspace2
  "Token workspace mismatch"
  Prevents cross-workspace token replay
```

---

### 8.6 Cryptographic identity scenarios

#### C1 — Egress boot with identity certificate

```
Precondition: egress deployed, EgressIdentity and EgressKeyPair generated

Egress instance boots:
  1. Loads EgressIdentity certificate (X.509)
  2. Presents certificate to control plane via mTLS
  3. CP validates certificate against org CA
  4. CP checks certificate not expired, not revoked
  5. CP returns session token + configuration bundle
  6. Egress uses session token to retrieve private key from secret service
  7. Egress begins signing telemetry with EgressKeyPair private key

Result: egress is authenticated and operational
```

#### C2 — EgressKeyPair rotation

```
Precondition: EgressKeyPair v1 active
Actor: admin

admin rotates EgressKeyPair:
  1. Secret service generates new Ed25519 key pair (v2)
  2. New public key registered with CP as active
  3. Old key marked "retiring" (accept signatures during grace)
  4. Egress detects new active key via config push
  5. Egress retrieves new private key from secret service
  6. Egress begins signing with v2
  7. Old key fully deactivated after grace window

Result: seamless rotation, no egress restart required
```

#### C3 — EgressKeyPair emergency revocation

```
Precondition: EgressKeyPair v1 compromised (private key leaked)
Actor: admin

admin emergency-revokes EgressKeyPair:
  1. Revoke command sent to CP
  2. CP immediately marks v1 as "revoked" (not just retired)
  3. CP rejects ALL telemetry signed with v1 (no grace window)
  4. Admin rotates to new key pair (v2)
  5. Egress retrieves v2 private key
  6. Normal operation resumes with v2

Result: compromised key cannot be used for spoofed telemetry
        revocation is immediate (no grace period for security)
```

#### C4 — EgressIdentity certificate expiry

```
Precondition: EgressIdentity expires in 7 days

System auto-renewal (egress healthy):
  1. CP detects expiry within renewal window
  2. CP generates new EgressIdentity certificate
  3. Pushes to egress via secure config channel
  4. Egress hot-reloads new certificate
  5. Old certificate remains valid until expiry (overlap)
  6. At expiry: old cert discarded, new cert active

Manual renewal (egress was offline):
  1. Admin runs: egress identity renew --egress <egress>
  2. New certificate generated
  3. Admin re-deploys egress with new certificate

Result: no downtime if healthy; manual intervention if offline
```

#### C5 — CPValidationKey rotation (org-wide)

```
Precondition: CPValidationKey v1 active across all egress
Actor: admin

admin rotates CPValidationKey:
  1. CP generates new Ed25519 key pair (v2)
  2. New public key pushed to all active egress instances
  3. Old key marked "retiring" (accept verify-only during grace)
  4. All egress instances update local validation key cache
  5. Old key deactivated after 24-hour grace

Result: org-wide rotation, all egress instances updated
```

#### C6 — EgressPASETOKeyPair rotation

```
Precondition: key3 uses paseto_v4.public backed by EgressPASETOKeyPair
Actor: user1

user1 rotates PASETO key:
  orange key rotate key3 --workspace <ws> --project <proj>

  System behavior:
    - New Ed25519 keypair is generated by the platform
    - New public key stored in EgressPASETOKeyPair
    - New private key stored in secret service
    - Old keypair is deactivated (grace window for in-flight tokens)

Result:
  New tokens are issued using the new keypair
  Old tokens remain valid during configured grace window
  No local key management required by the user
```

#### C7 — Multiple PASETO keys in same workspace

```
Precondition:
  key1: paseto_v4.public format
  key3: paseto_v4.public format
  Both keys owned by user1, same workspace

Traffic:
  Request via key1 (PASETO):
    → Egress verifies PASETO signature locally (Ed25519)
    → Checks revocation list (cached)
    → Evaluates embedded policy + workspace floors
    → Proxies request (zero CP round-trips)

  Request via key3 (PASETO, with BYOK):
    → Same stateless verification
    → Credential resolution: KeySecret v1 (personal) used for upstream1
    → Proxies request using user's personal credential

Result:
  All keys use stateless verification
  Zero CP round-trips per request
  Each key independently scoped with its own embedded policy
```

---

## 9. CLI reference

The CLI is named `orange`. It supports both non-interactive (one-shot) and
interactive (REPL) modes. Identity is determined by the active session
(`admin` or `user`), which controls which commands are available.

---

### 9.1 Installation and authentication

```sh
# Install
curl -sSL https://orange.tetrate.io/install.sh | sh

# Authenticate (stores session token in ~/.orange/config)
orange auth login --org <org>
orange auth login --org <org> --user admin@organization
orange auth login --org <org> --user user1@organization

# Show current identity
orange auth whoami

# Output:
#   logged in as: user1@organization
#   org:          organization
#   role:         member
#   workspaces:   workspace1@project1.organization
#                 workspace2@project1.organization

# Switch identity without re-login
orange auth switch --user admin@organization
```

---

### 9.2 Non-interactive mode

One command, one result, exits. Suitable for scripts and CI pipelines.

```sh
orange <resource> <verb> [target] [flags]
```

Global flags available on every command:

```
--org <org>          Override org from config
--output, -o         Output format: table (default) | json | yaml
--quiet, -q          Suppress headers and decorations
--no-color           Disable ANSI color output
```

---

### 9.3 REPL mode

Interactive shell with context, history, and tab-completion.
Launched with no arguments or with `orange repl`.

```sh
orange
# or
orange repl
```

```
orange v0.1.0  |  org: organization  |  user: admin@organization
Type 'help' for commands, 'exit' to quit.

orange> _
```

**Context commands (REPL only):**

```
use project <name>      Set active project (scopes subsequent commands)
use workspace <name>    Set active workspace
use key <name>          Set active key (user mode)
ctx                     Show current context
ctx clear               Clear all context
```

**REPL context example:**

```
orange> use project project1
  context: project → project1@organization

orange> use workspace workspace1
  context: project → project1@organization
           workspace → workspace1

orange> describe egress
  # No need to specify the full path — context resolves it
  workspace:  workspace1@project1.organization
  status:     active
  secrets:    secret1 (upstream1, v2 active), secret2 (upstream2, v1 active)
  identity:   CN=egress.workspace1... [expires: 2026-09-04]
  keypair:    Ed25519 [active]
  ...

orange> secret rotate secret1
  rotated secret1: v1 → v2
  active version: v2

orange> identity rotate-keypair
  rotated EgressKeyPair: v1 → v2
  grace window: 5 minutes
```

**REPL navigation:**

```
Up/Down arrows    Command history
Tab               Autocomplete resource names, flags, and values
Ctrl+C            Cancel current input
Ctrl+D / exit     Exit REPL
history           Show command history
history clear     Clear history
```

**REPL output is always human-readable by default.** Pipe to `--output json`
for machine output even in REPL:

```
egress> describe workspace1 --output json
```

---

### 9.4 Admin commands

All commands below require admin identity (`egress auth whoami` shows `role: admin`).

#### Project management

```sh
# Create
orange project create <name> [--description <text>]

# List all projects
orange project list

# Delete
orange project delete <name>

# Update description
orange project set-description <name> --description <text>
```

#### Workspace management

```sh
# Create
orange workspace create <name> --project <project> [--description <text>]

# List workspaces in a project
orange workspace list --project <project>

# Delete
orange workspace delete <name> --project <project>

# Update description
orange workspace set-description <name> --project <project> --description <text>
```

#### Egress management

```sh
# Deploy egress to a workspace
# (auto-generates EgressIdentity + EgressKeyPair)
orange egress deploy --workspace <workspace> --project <project> \
  [--description <text>] \
  [--identity-algorithm Ed25519] \
  [--keypair-algorithm Ed25519] \
  [--identity-ttl 90d]

# Decommission
orange egress remove --workspace <workspace> --project <project>

# Show egress status
orange egress status --workspace <workspace> --project <project>
```

#### Egress identity management

```sh
# View identity details
orange identity describe --egress <egress> \
  --workspace <workspace> --project <project>

# Renew identity certificate (manual)
orange identity renew --egress <egress> \
  --workspace <workspace> --project <project> \
  [--ttl 90d]

# View key pair details
orange identity describe-keypair --egress <egress> \
  --workspace <workspace> --project <project>

# Rotate key pair
orange identity rotate-keypair --egress <egress> \
  --workspace <workspace> --project <project> \
  [--grace-window 5m]

# Emergency revoke key pair (immediate, no grace)
orange identity revoke-keypair --egress <egress> \
  --workspace <workspace> --project <project> \
  [--reason "compromised"] \
  [--auto-rotate]   # generate new key pair immediately

# List key pair rotation history
orange identity history --egress <egress> \
  --workspace <workspace> --project <project>
```

#### Control plane validation key management

```sh
# View current CP validation key
orange identity describe-cp-key

# Rotate CP validation key (org-wide)
orange identity rotate-cp-key \
  [--grace-window 24h] \
  [--algorithm Ed25519]

# View CP key history
orange identity cp-key-history
```

#### Secret management

```sh
# Set a new secret (creates v1)
orange secret set <name> \
  --upstream <upstream> \
  --workspace <workspace> \
  --project <project> \
  --value <value> \
  [--description <text>]

# Rotate (creates next version, deactivates previous)
orange secret rotate <name> \
  --workspace <workspace> \
  --project <project> \
  --value <new-value>

# List secrets in a workspace
orange secret list --workspace <workspace> --project <project>

# Show versions of a secret
orange secret versions <name> \
  --workspace <workspace> \
  --project <project>

# Deactivate all versions (emergency lockdown for an upstream)
orange secret deactivate <name> \
  --workspace <workspace> \
  --project <project>
```

#### User and membership management

```sh
# List users in org
orange user list

# Assign user to workspace
orange workspace assign <user> \
  --workspace <workspace> \
  --project <project>

# Remove user from workspace
orange workspace unassign <user> \
  --workspace <workspace> \
  --project <project>

# Update user description
orange user set-description <user> --description <text>
```

#### Admin policy management

```sh
# Set a floor policy on a project
orange policy set \
  --scope project \
  --target <project> \
  --type floor \
  --rules '<rule-expression>' \
  [--description <text>]

# Set a floor policy on a workspace
orange policy set \
  --scope workspace \
  --target <workspace> \
  --project <project> \
  --type floor \
  --rules '<rule-expression>' \
  [--description <text>]

# Set a flexible policy on a workspace
orange policy set \
  --scope workspace \
  --target <workspace> \
  --project <project> \
  --type flexible \
  --rules '<rule-expression>' \
  [--description <text>]

# List all policies for a resource
orange policy list --scope project --target <project>
orange policy list --scope workspace --target <workspace> --project <project>

# Delete a policy
orange policy delete <policy-id>

# Show effective (resolved) policy for a workspace
orange policy effective --workspace <workspace> --project <project>

# Force-revoke a user's key (admin override)
orange admin key-revoke <key> \
  --workspace <workspace> \
  --project <project> \
  [--reason <text>]
```

---

### 9.5 User commands

Available to any authenticated user. Scoped to their memberships.

#### Key management

```sh
# Create a key (paseto_v4.public)
orange key create <name> \
  --workspace <workspace> \
  --project <project> \
  --public-key <path-to-ed25519.pub> \
  [--description <text>]

# List own keys (optionally filter by workspace)
orange key list
orange key list --workspace <workspace> --project <project>

# Rotate a key (revoke old, issue new with same name and policy)
# Provide new public key to rotate
orange key rotate <name> \
  --workspace <workspace> \
  --project <project> \
  --public-key <path-to-new-ed25519.pub>

# Delete a key
orange key delete <name> \
  --workspace <workspace> \
  --project <project>

# Update key description
orange key set-description <name> \
  --workspace <workspace> \
  --project <project> \
  --description <text>
```

#### Key policy management

```sh
# Set policy on own key (must satisfy narrowing invariant)
orange policy set \
  --scope key \
  --target <key> \
  --workspace <workspace> \
  --project <project> \
  --type flexible \
  --rules '<rule-expression>' \
  [--description <text>]

# Show policy for own key
orange policy list --scope key --target <key> \
  --workspace <workspace> --project <project>

# Show effective (resolved) policy for own key
orange policy effective \
  --scope key \
  --target <key> \
  --workspace <workspace> \
  --project <project>

# Remove key policy (key reverts to workspace policy)
orange policy delete --scope key --target <key> \
  --workspace <workspace> --project <project>
```

#### Key-bound upstream credential management (BYOK)

Users may bind their own upstream credentials to individual keys.
Credential values are write-only: they are stored encrypted and never
returned by any list or describe command.

```sh
# Bind own upstream credential to a key (creates v1)
orange key-secret set <upstream> \
  --key <key> \
  --workspace <workspace> \
  --project <project> \
  --value <value> \
  [--description <text>]

# Rotate key-bound credential (creates next version, deactivates previous)
orange key-secret rotate <upstream> \
  --key <key> \
  --workspace <workspace> \
  --project <project> \
  --value <new-value>

# List key-bound credentials for a key (upstream targets only — values never returned)
orange key-secret list \
  --key <key> \
  --workspace <workspace> \
  --project <project>

# Show versions of a key-bound credential
orange key-secret versions <upstream> \
  --key <key> \
  --workspace <workspace> \
  --project <project>

# Remove key-bound credential (key reverts to workspace Secret for that upstream)
orange key-secret remove <upstream> \
  --key <key> \
  --workspace <workspace> \
  --project <project>
```

**Example output (`egress key-secret list`):**

```
KEY    UPSTREAM    ACTIVE_VERSION   DESCRIPTION
key3   upstream1   v2               "My personal OpenAI key"
key3   upstream3   v1               (none)
```

---

### 9.6 Describe commands

`describe` is a unified read command for inspecting the state of any resource.
Admins see the full view; users see a scoped view limited to their own resources.

#### `egress describe org`

```sh
egress describe org
```

```
# Admin output
organization
  projects:    project1, project2
  users:       admin, user1, user2
  floor policies:
    (none at org level)
  cp_validation_key:
    thumbprint: def456  algorithm: Ed25519  active: true

# User output
organization
  workspaces you have access to:
    workspace1@project1, workspace2@project1
```

#### `egress describe project <project>`

```sh
egress describe project project1
```

```
# Admin output
project:      project1@organization
description:  "Internal ML infrastructure"
workspaces:
  workspace1   egress: active    members: 2   keys: 4
  workspace2   egress: inactive  members: 0   keys: 0
floor policies:
  [floor] deny upstream3  — "block external inference APIs"
flexible policies:
  (none)
```

#### `egress describe workspace <workspace> --project <project>`

```sh
egress describe workspace workspace1 --project project1
```

```
# Admin output
workspace:    workspace1@project1.organization
description:  "Production egress for ML pipeline"
egress:       active
  identity:    CN=egress.workspace1... [expires: 2026-09-04, serial: abc...]
  keypair:     Ed25519 [active, rotated: 2026-06-01]
members:
  user1@organization  — "ML platform team"
  user2@organization  — "Data engineering"
secrets:
  secret1   upstream: upstream1   active_version: v2   description: "OpenAI key"
  secret2   upstream: upstream2   active_version: v1   description: "Internal model token"
policies:
  [floor]    project1:   deny upstream3
  [floor]    workspace1: max_req_per_min=1000
  [flexible] workspace1: allow upstream1, upstream2
keys:
  key1   owner: user1   format: paseto_v4.public      policy: allow upstream1         byok: —           effective: allow upstream1 @1000/min
  key2   owner: user1   format: paseto_v4.public      policy: max_req_per_min=100     byok: —           effective: allow upstream1+2 @100/min
  key3   owner: user1   format: paseto_v4.public   policy: (embedded)              byok: upstream1   effective: allow upstream1+2 @1000/min
  key4   owner: user2   format: paseto_v4.public      policy: (none)                  byok: —           effective: allow upstream1+2 @1000/min

# User output (user1)
workspace:    workspace1@project1.organization
description:  "Production egress for ML pipeline"
egress:       active
upstreams:    upstream1, upstream2  (workspace secrets configured — values hidden)
floor policies (read-only):
  [floor] project1:   deny upstream3
  [floor] workspace1: max_req_per_min=1000
workspace flexible policy:
  allow upstream1, upstream2
my keys:
  key1   format: paseto_v4.public      policy: allow upstream1       byok: —         effective: allow upstream1 @1000/min
  key2   format: paseto_v4.public      policy: max_req_per_min=100   byok: —         effective: allow upstream1+2 @100/min
  key3   format: paseto_v4.public   policy: (embedded)            byok: upstream1  effective: allow upstream1+2 @1000/min
```

#### `egress describe egress --workspace <workspace> --project <project>`

```sh
egress describe egress --workspace workspace1 --project project1
```

```
# Admin output
egress:       egress@workspace1.project1.organization
status:       active
description:  "Proxies ML API calls"
identity:
  certificate: CN=egress.workspace1.project1.organization
  serial:      550e8400-e29b-41d4-a716-446655440000
  issued:      2026-06-06
  expires:     2026-09-04
  algorithm:   Ed25519
  active:      true
keypair:
  algorithm:   Ed25519
  public_key:  MCowBQYDK2VwAyEA... (thumbprint: abc123)
  private_key: secret-service://vault/egress/ws1/key-v2 (not exposed)
  active:      true
  created:     2026-06-06
  rotated:     2026-06-01
cp_validation_key:
  thumbprint:  def456
  active:      true
secrets:
  secret1
    upstream:        upstream1
    active_version:  v2
    versions:        v1 [superseded], v2 [active]
    description:     "OpenAI API key"
  secret2
    upstream:        upstream2
    active_version:  v1
    versions:        v1 [active]
    description:     "Internal model service token"
effective_floor_policies:
  project floor: deny upstream3
  workspace floor: max_req_per_min=1000

# User output (user1)
egress:    active
upstreams: upstream1, upstream2  (credentials managed by admin)
format support: paseto_v4.public
```

#### `egress describe key <name> --workspace <workspace> --project <project>`

```sh
egress describe key key3 --workspace workspace1 --project project1
```

```
# Output is the same for admin and key owner (user1)
# A user cannot describe keys they do not own.

key:          key3@workspace1.project1.organization
owner:        user1@organization
description:  "BYOK key for my upstream1 account"
format:       paseto_v4.public
public_key_thumbprint: abc123...
stored policy:
  (none — inherits workspace)
bound credentials (BYOK):
  upstream1
    active_version: v2
    versions:       v1 [superseded], v2 [active]
    description:    "My personal OpenAI key"
effective policy (resolved):
  project floor:    deny upstream3
  workspace floor:  max_req_per_min=1000
  workspace flex:   allow upstream1, upstream2
  key flex:         (none)
  ─────────────────────────────────────────────
  resolved:         allow upstream1, upstream2, max 1000 req/min
credential resolution:
  upstream1 → KeySecret v2  (personal — user1's own account)
  upstream2 → workspace Secret v1  (shared)
verification: stateless (PASETO signature verified locally)
```

#### `egress describe user <user>`

```sh
egress describe user user1@organization  # admin only for full view
egress describe user me                  # any user, own info
```

```
# Admin output
user:         user1@organization
description:  "ML platform team"
workspaces:
  workspace1@project1.organization
  workspace2@project1.organization
keys:
  key1@workspace1  — "Used by training pipeline"     format: paseto_v4.public      byok: —
  key2@workspace1  — (no description)                format: paseto_v4.public      byok: —
  key3@workspace1  — "BYOK key for my upstream1"     format: paseto_v4.public   byok: upstream1
  key5@workspace2  — "Batch job key"                 format: paseto_v4.public      byok: —

# Self output (user1 running: egress describe user me)
user:        user1@organization
description: "ML platform team"
workspaces:  workspace1, workspace2
my keys:
  key1@workspace1 — format: paseto_v4.public      byok: —
  key2@workspace1 — format: paseto_v4.public      byok: —
  key3@workspace1 — format: paseto_v4.public   byok: upstream1
  key5@workspace2 — format: paseto_v4.public      byok: —
```

#### `egress describe policy --scope <scope> --target <target>`

```sh
egress describe policy --scope workspace --target workspace1 --project project1
```

```
# Admin output — all policies for workspace1
scope: workspace1@project1.organization

[floor] set by admin  — "Org-wide rate cap"
  rule: max_req_per_min = 1000

[flexible] set by admin  — "Default upstream allowlist"
  rule: allow upstream1, upstream2

inherited from project1:
[floor]  — "Block external inference APIs"
  rule: deny upstream3
```

---

### 9.7 Output formats

Every command accepts `--output` / `-o`:

```sh
egress describe workspace workspace1 --project project1 --output json
egress describe workspace workspace1 --project project1 --output yaml
egress key list --output json | jq '.[] | select(.format == "paseto_v4.public")'
```

**JSON output example (`egress key list -o json`):**

```json
[
  {
    "name": "key1",
    "workspace": "workspace1",
    "project": "project1",
    "owner": "user1@organization",
    "description": "Used by training pipeline",
    "key_format": "paseto_v4.public",
    "public_key_thumbprint": null,
    "stored_policy": { "allow": ["upstream1"] },
    "effective_policy": {
      "allow": ["upstream1"],
      "max_req_per_min": 1000
    },
    "key_secrets": []
  },
  {
    "name": "key2",
    "workspace": "workspace1",
    "project": "project1",
    "owner": "user1@organization",
    "description": null,
    "key_format": "paseto_v4.public",
    "public_key_thumbprint": null,
    "stored_policy": { "max_req_per_min": 100 },
    "effective_policy": {
      "allow": ["upstream1", "upstream2"],
      "max_req_per_min": 100
    },
    "key_secrets": []
  },
  {
    "name": "key3",
    "workspace": "workspace1",
    "project": "project1",
    "owner": "user1@organization",
    "description": "BYOK key for my upstream1 account",
    "key_format": "paseto_v4.public",
    "public_key_thumbprint": "abc123...",
    "stored_policy": null,
    "effective_policy": {
      "allow": ["upstream1", "upstream2"],
      "max_req_per_min": 1000
    },
    "key_secrets": [
      {
        "upstream": "upstream1",
        "active_version": "v2",
        "description": "My personal OpenAI key"
      }
    ]
  }
]
```

> `key_secrets` entries never include the credential value — only the upstream
> target, active version, and description are returned.
> `public_key_thumbprint` is only set for PASETO-format keys.

---

### REPL walkthrough — admin onboarding

```
$ orange repl

orange v0.1.0  |  org: organization  |  user: admin@organization
Type 'help' for commands, 'exit' to quit.

orange> project create project1 --description "Internal ML infrastructure"
  created project1@organization

orange> workspace create workspace1 --project project1 \
          --description "Production egress for ML pipeline"
  created workspace1@project1.organization

orange> use workspace workspace1 --project project1
  context: workspace → workspace1@project1.organization

orange> egress deploy
  deployed egress@workspace1.project1.organization
  auto-generated:
    - EgressIdentity (X.509, CN=egress.workspace1..., expires: 2026-09-04)
    - EgressKeyPair (Ed25519, private key stored in secret service)
    - CPValidationKey distributed

orange> identity describe
  identity:
    certificate: CN=egress.workspace1.project1.organization
    expires:     2026-09-04
    serial:      550e8400-e29b-41d4-a716-446655440000
  keypair:
    algorithm:   Ed25519
    thumbprint:  abc123...
    active:      true

orange> secret set secret1 --upstream upstream1 \
          --value $OPENAI_KEY --description "OpenAI API key"
  created secret1 v1  [active]

orange> secret set secret2 --upstream upstream2 \
          --value $INTERNAL_TOKEN --description "Internal model token"
  created secret2 v1  [active]

orange> policy set --scope workspace --type floor \
          --rules 'max_req_per_min=1000' \
          --description "Org-wide rate cap"
  created floor policy on workspace1

orange> policy set --scope workspace --type flexible \
          --rules 'allow upstream1, upstream2' \
          --description "Default upstream allowlist"
  created flexible policy on workspace1

orange> workspace assign user1@organization
  assigned user1@organization → workspace1

orange> workspace assign user2@organization
  assigned user2@organization → workspace1

orange> describe workspace
  workspace:    workspace1@project1.organization
  description:  "Production egress for ML pipeline"
  egress:       active
    identity:   CN=egress.workspace1... [expires: 2026-09-04]
    keypair:    Ed25519 [active]
  members:      user1@organization, user2@organization
  secrets:      secret1 (upstream1, v1), secret2 (upstream2, v1)
  policies:
    [floor]    max_req_per_min=1000
    [flexible] allow upstream1, upstream2
  keys:         (none yet)

orange> exit
  goodbye
```

### REPL walkthrough — user key management, PASETO, and BYOK

```
$ orange repl

orange v0.1.0  |  org: organization  |  user: user1@organization
Type 'help' for commands, 'exit' to quit.

orange> use workspace workspace1 --project project1
  context: workspace → workspace1@project1.organization

# --- key1: PASETO key ---

orange> key create key1 --description "Used by training pipeline"
  created key1@workspace1.project1.organization
  format: paseto_v4.public
  public_key_thumbprint: def456...
  (private key securely stored by platform)

orange> policy set --scope key --target key1 --type flexible \
          --rules 'allow upstream1'
  created policy on key1
  validation: upstream1 ⊆ {upstream1, upstream2}  ✓

# --- key3: PASETO key with BYOK ---

orange> key create key3 --description "PASETO key for local signing"
  created key3@workspace1.project1.organization
  format: paseto_v4.public
  public_key_thumbprint: abc123...
  (private key securely stored by platform)

orange> describe key key3
  key:           key3
  description:   "PASETO key for local signing"
  format:        paseto_v4.public
  thumbprint:    abc123...
  stored policy: (none — inherits workspace)
  effective:     allow upstream1+2, max 1000 req/min
  verification:  stateless (local signature verify)
  private_key:   (managed by platform — never exposed)

# --- BYOK on PASETO key ---

orange> key-secret set upstream1 --key key3 \
          --value $MY_OWN_OPENAI_KEY \
          --description "My personal OpenAI key"
  created KeySecret for upstream1 on key3 — v1 [active]
  requests via key3 to upstream1 will use your personal credential

orange> describe key key3
  key:           key3
  description:   "PASETO key with BYOK"
  format:        paseto_v4.public
  bound credentials (BYOK):
    upstream1  active_version: v1  description: "My personal OpenAI key"
  effective:     allow upstream1+2, max 1000 req/min
  credential resolution:
    upstream1 → KeySecret v1 (personal)
    upstream2 → workspace Secret v1 (shared)
  verification: stateless

orange> key list
  NAME   WORKSPACE    FORMAT       POLICY                    BYOK        EFFECTIVE
  key1   workspace1   paseto_v4.public  allow upstream1           —           allow upstream1 @1000/min
  key3   workspace1   paseto_v4.public    (none)                    upstream1   allow upstream1+2 @1000/min

orange> key-secret rotate upstream1 --key key3 \
          --value $MY_NEW_OPENAI_KEY
  rotated KeySecret for upstream1 on key3: v1 → v2 [active]
  in-flight requests will drain on v1; new requests use v2

orange> key-secret list --key key3
  KEY    UPSTREAM    ACTIVE_VERSION   DESCRIPTION
  key3   upstream1   v2               "My personal OpenAI key"

# --- Signing a PASETO request (user-side, not CLI) ---
# The CLI does not sign requests — the user's application does.
# Example with a PASETO library:
#
#   token = paseto.create(
#     key_id: "key3@workspace1.project1.organization",
#     (CLI can generate tokens directly; no local private key needed)
#     claims: { "sub": "user1@org", "wsk": "workspace1@project1.org", ... }
#   )
#   headers = { "Authorization": "Paseto " + token }
#   response = HTTP.post("https://egress.example.com/v1/proxy", headers: headers)

orange> exit
  goodbye
```

---

## 10. Summary: invariants and rules

```
┌─────────────────────────────────────────────────────────────────┐
│  INVARIANT                                                       │
│  key_policy ⊆ workspace_policy ⊆ project_floor                 │
│  enforced at write time for user-set key policies               │
│  workspace tightening uses lazy invalidation (eval-time clamp)  │
├─────────────────────────────────────────────────────────────────┤
│  CRYPTOGRAPHIC INVARIANTS                                        │
│  1. EgressIdentity binds exactly one egress to one workspace   │
│     (X.509 certificate, org-CA signed, 90-day validity)        │
│  2. EgressKeyPair private key NEVER leaves secret service      │
│     (opaque reference only in CP; retrieved at runtime)         │
│  3. PASETO private keys are user-local NEVER sent to CP       │
│     (user generates Ed25519 keypair, provides public key only) │
│  4. CPValidationKey is org-wide; rotates to all egress         │
│     (grace window for seamless transition)                     │
│  5. Narrowing invariant applies to PASETO embedded "pol"       │
│     (floor policies always override embedded claims)           │
├─────────────────────────────────────────────────────────────────┤
│  RULES                                                           │
│  1.  deny always wins across all levels                         │
│  2.  floor policies: admin-write only, never overrideable       │
│  3.  users can only restrict, never widen                       │
│  4.  Key.workspace_id is immutable after creation               │
│  5.  keys never hold workspace Secret values directly           │
│  6.  workspace Secret rotation is zero-downtime                 │
│  7.  all workspace members have equal rights                    │
│  8.  removing a user cascades to invalidate keys + KeySecrets   │
│  9.  describe scoping: admins see all, users see own resources  │
│  10. description fields are free-text, set by owner/admin       │
│  11. users may bind personal upstream credentials (KeySecrets)  │
│      to their own keys (BYOK); one KeySecret per upstream       │
│      per key                                                     │
│  12. credential resolution at dispatch:                         │
│        KeySecret (active) → workspace Secret (active) → DENIED  │
│  13. BYOK affects which credential is used and which account    │
│      is metered — policy evaluation is identical either way     │
│  14. KeySecret values are write-only; never returned by API     │
│  15. KeySecret rotation is zero-downtime; user-managed;         │
│      independent of workspace Secret lifecycle                  │
│  16. a KeySecret may be configured for an upstream with no      │
│      workspace Secret; that upstream is then accessible via     │
│      that key only (other keys without a KeySecret cannot       │
│      reach it unless the admin adds a workspace Secret)         │
│  17. all tokens are PASETO v4.public; verified statelessly      │
│  18. PASETO v4.public tokens are verified statelessly at egress │
│      (Ed25519 signature + revocation list cache check)          │
│  19. EgressKeyPair rotation has a grace window (default 5min)  │
│  20. CPValidationKey rotation has a grace window (default 24h) │
│  21. emergency revocation bypasses grace window (immediate)     │
│  22. EgressIdentity auto-renews before expiry if egress healthy │
│  23. PASETO "wsk" claim prevents cross-workspace token replay │
│  24. PASETO "exp" claim checked before policy evaluation      │
└─────────────────────────────────────────────────────────────────┘
```

---

*Document version: 2.0 — includes cryptographic identity & attestation model*
