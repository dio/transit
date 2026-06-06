# Policy Conflicts and Edge Case Scenarios

Detailed reference for policy conflict handling and edge cases that can occur during operations.

## Policy Conflict Scenarios

### P1 — Key Policy More Permissive Than Workspace (Write-Time Rejection)

```
workspace1 allows: upstream1
user1 sets key1 policy: allow upstream1 + upstream2

→ REJECTED at write time
  stored key policies always satisfy the narrowing invariant
```

### P2 — Two Keys, Different Policies, Same Workspace

```
workspace1 allows: upstream1 + upstream2

key1 policy (user1): allow upstream1 ONLY
key2 policy (user1): allow upstream2 ONLY

Request via key1 to upstream1:  ALLOWED
Request via key1 to upstream2:  DENIED  (key1 policy)
Request via key2 to upstream1:  DENIED  (key2 policy)
Request via key2 to upstream2:  ALLOWED
```

### P3 — Floor Policy Blocks What Workspace Flexible Allows

```
project floor: deny upstream2

workspace1 flexible: allow upstream1 + upstream2

Request via any key to upstream2:
  project floor check: upstream2 denied  ✗
  → BLOCKED immediately
  workspace flexible policy is irrelevant
  KeySecret for upstream2 (if any) is irrelevant
```

### P4 — No Key Policy Set (Inherits Workspace)

```
workspace1 policy: allow upstream1 + upstream2, max 500 req/min
key1: no policy attached

Effective policy for key1 = workspace policy
  allow upstream1 + upstream2, max 500 req/min
```

### P5 — Workspace Tightened After Key Policy Set (Lazy Invalidation)

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

See **02-policy-system.md** for lazy invalidation details.

### P6 — Multiple Floor Policies Stacked (Project + Workspace)

```
project floor:   deny upstream3
workspace floor: deny upstream2

workspace1 flexible: allow upstream1 + upstream2

Effective policy for all keys in workspace1:
  can reach: upstream1 only
  upstream2 blocked by workspace floor
  upstream3 blocked by project floor
```

### P7 — PASETO Embedded Policy vs Workspace Floor (Narrowing Still Applies)

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

## Edge Cases and Error Scenarios

### E1 — Attempt to Create Key in Unassigned Workspace

```
Actor: user1 (member of workspace1 only)

user1 attempts to create key in workspace2

Result: DENIED
        user1 is not a WorkspaceMember of workspace2
```

### E2 — Attempt to Access Egress Without a Key

```
Actor: user1

user1 sends request with no Authorization header

Result: DENIED immediately
        no key = no identity = no policy evaluation
```

### E3 — Expired or Revoked Key

```
Precondition: key1 deleted

user1 sends request with key1

Result: DENIED
        key not found in active key store
```

### E4 — Request to Upstream With No Credential Configured

```
Precondition:
  key1 has no KeySecret for upstream2
  workspace has no Secret for upstream2

user1 sends request targeting upstream2

Result: DENIED at egress dispatch
        error: "no credential configured for this upstream"
```

### E5 — Workspace Has No Egress Deployed

```
Precondition: workspace2 exists, no egress deployed

user1 (member of workspace2) sends request

Result: DENIED — workspace2 has no egress to proxy through
```

### E6 — All Secrets for an Upstream Deactivated

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

### E7 — Policy Write While Request In Flight

```
Precondition: request R mid-flight, key1 active

user1 deletes key1 policy

Result:
  R completes (policy snapshot taken at dispatch)
  subsequent requests via key1 inherit workspace policy
```

### E8 — User Removed From Workspace Mid-Session

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

### E9 — Two Users, Same Workspace, Isolated Key Policies and KeySecrets

```
user1: key1 (allow upstream1), key3 (byok: upstream1, paseto_v4.public)
user2: key4 (allow upstream1 + upstream2)

user1 cannot see or modify key4
user2 cannot see or modify key1 or key3

Policy isolation is per-key, per-owner.
KeySecret isolation is per-key, per-owner.
PASETO key isolation is per-key — user2 cannot derive user1's private key.
```

### E10 — Admin Sets Floor Policy More Restrictive Than Existing Keys

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

### E11 — User Attempts to Bind KeySecret for Upstream Denied by Floor

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

## PASETO-Specific Edge Cases

### E12 — PASETO Token With Invalid Signature

```
Precondition: key3 is PASETO format, backed by EgressPASETOKeyPair

Attacker forges PASETO token (wrong private key)

Result: DENIED at Step 0 (PASETO verification)
  Signature verification fails against registered public key
  Request rejected before policy evaluation
  No CP lookup needed — purely local rejection
```

### E13 — PASETO Token Expired But Signature Valid

```
Precondition: key3 is PASETO format, valid EgressPASETOKeyPair backing

user1 sends request with expired PASETO token (exp < now)

Result: DENIED at Step 0 (PASETO verification)
  "Token has expired" — exp claim checked before any policy eval
  User must generate a fresh signed token
```

### E14 — PASETO Token for Wrong Workspace

```
Precondition: key3 is bound to workspace1 (wsk claim)

user1 sends request to workspace2's egress with key3 token

Result: DENIED at Step 0
  Egress detects wsk == "workspace1" but this is workspace2
  "Token workspace mismatch"
  Prevents cross-workspace token replay
```

## Related Topics

- **02-policy-system.md** — Policy model and evaluation rules
- **03-credential-and-key-management.md** — Credential resolution interaction with policy
- **04-cryptographic-identity.md** — PASETO verification details
- **09-secret-rotation.md** — Rotation scenarios and edge cases
