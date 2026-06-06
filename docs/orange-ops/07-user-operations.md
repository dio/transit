# User Operational Scenarios

Complete reference for all user tasks including key management, policies, BYOK, and requests.

## Key Management

### U1 — Create a Key

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

### U2 — Create a PASETO-Format Key

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

See **04-cryptographic-identity.md** for PASETO details.

## Policy Management

### U3 — Attach a Policy to a Key (Valid — Narrowing)

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

### U4 — Attach a Policy to a Key (Invalid — Widening Attempt)

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

### U15 — Update a Key Policy (Further Restrict)

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

### U16 — Update a Key Policy (Attempt to Widen)

```
Precondition:
  key1 policy: allow upstream1 ONLY
  workspace policy: allow upstream1 ONLY

Actor: user1

user1 attempts: rules: [{ allow: upstream1, upstream2 }]

Validation: upstream2 ⊄ workspace policy  ✗

Result: REJECTED — existing key1 policy unchanged
```

See **02-policy-system.md** for policy model details.

## Sending Requests

### U5 — Send a Request (Allowed, Workspace Secret Used, PASETO Token)

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

### U6 — Send a Request (PASETO Token, Stateless Verification)

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

### U7 — Send a Request (Denied by Key Policy)

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

### U8 — Send a Request (Denied by Floor Policy)

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

## Multi-User Scenarios

### U9 — user2 Creates a Key Independently

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

### U10 — User Attempts to Use Another User's Key

```
Precondition: key1 owned by user1
Actor: user2

user2 sends request with key1

Result: DENIED
        key1.user_id ≠ user2.id
        auth check fails before policy evaluation
```

## Key Lifecycle

### U11 — Delete a Key

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

## Describing Resources

### U12 — Describe Own Key (User View)

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

### U13 — Describe Own PASETO Key (User View)

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

### U14 — Describe Own Workspace (User View)

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

## BYOK (Bring-Your-Own-Key / User-Bound Credentials)

### B1 — User Binds Own Upstream Credential to a Key (BYOK)

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

See **03-credential-and-key-management.md** for BYOK details.

### B2 — Request Dispatched Using BYOK Credential

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

### B3 — Request Falls Back to Workspace Secret (No KeySecret)

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

### B4 — User Binds Credential for Upstream Without Workspace Secret

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

### B5 — User Rotates Their Bound Credential (Zero-Downtime)

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

### B6 — User Removes Their Bound Credential (Fallback to Workspace Secret)

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

### B7 — BYOK Credential Set But Upstream Blocked by Policy

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

## Related Topics

- **02-policy-system.md** — Policy model and narrowing invariant
- **03-credential-and-key-management.md** — Workspace secrets and BYOK
- **04-cryptographic-identity.md** — PASETO and key generation
- **11-cli-reference.md** — User CLI commands reference
