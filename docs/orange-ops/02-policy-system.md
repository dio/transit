# Policy System: Model and Evaluation

Complete reference for policy scopes, types, narrowing invariant, and request-time evaluation.

## Policy Model

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

### Policy Types

**Floor policies** are hard limits. No scope below them can override or widen them.
Only admins can write floor policies.

**Flexible policies** can be restricted (narrowed) by lower scopes, but never widened.

## Narrowing Invariant

```
key_policy ⊆ workspace_policy ⊆ project_floor
```

This invariant is **enforced at write time** for user-set key policies.
When a user attempts to create or update a key policy, the system validates
that the proposed rules do not exceed the workspace's current effective policy.
If they do, the write is rejected immediately.

## Lazy Invalidation: Workspace Policy Tightening

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

Request via key1 to upstream2: DENIED  (workspace clamps at eval time)
Request via key1 to upstream1: ALLOWED
```

The stored key policy is never rewritten. The evaluator always computes
the intersection at request time. No cascade writes are needed.

## Policy Evaluation at Request Time

### Evaluation Order

```
Incoming request (Key + payload)
        │
        ▼
┌───────────────────────────┐
│  0. Key format check      │  PASETO keys verified statelessly
│     (PASETO verify)       │  Stateless Ed25519 sig verify
└──────────┬────────────────┘
           │ pass
           ▼
┌───────────────────────────┐
│  1. Project floor         │  admin-set hard limits
│     policy                │  any deny here → BLOCKED
└──────────┬────────────────┘
           │ pass
           ▼
┌───────────────────────────┐
│  2. Workspace floor       │  admin-set workspace limits
│     policy                │  any deny here → BLOCKED
└──────────┬────────────────┘
           │ pass
           ▼
┌───────────────────────────┐
│  3. Workspace             │  flexible workspace defaults
│     flexible policy       │  any deny here → BLOCKED
└──────────┬────────────────┘
           │ pass
           ▼
┌───────────────────────────┐
│  4. Key flexible          │  user-set key restrictions
│     policy                │  any deny here → BLOCKED
└──────────┬────────────────┘
           │ all pass
           ▼
    POLICY PASSED
    → Credential resolution (see credential-and-key-management.md)
    → Egress proxies to upstream
```

### Evaluation Rules

- **Deny always wins.** A deny at any level blocks the request regardless
of what lower levels allow.

- **BYOK does not affect policy evaluation.** BYOK (user-bound KeySecrets) affect *which credential* is used
at dispatch — not *whether the request is allowed*. Policy evaluation is
identical regardless of whether a key has a KeySecret bound.

### What Users Can and Cannot Do with Policies

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

## Policy Conflict Examples

### Floor vs. Flexible: Floor Always Wins

```
project floor:   deny upstream2

workspace1 flexible: allow upstream1 + upstream2

Request via any key to upstream2:
  project floor check: upstream2 denied  ✗
  → BLOCKED immediately
  workspace flexible policy is irrelevant
  KeySecret for upstream2 (if any) is irrelevant
```

### No Key Policy: Inherit Workspace

```
workspace1 policy: allow upstream1 + upstream2, max 500 req/min
key1: no policy attached

Effective policy for key1 = workspace policy
  allow upstream1 + upstream2, max 500 req/min
```

### Multiple Floor Policies Stacked

```
project floor:   deny upstream3
workspace floor: deny upstream2

workspace1 flexible: allow upstream1 + upstream2

Effective policy for all keys in workspace1:
  can reach: upstream1 only
  upstream2 blocked by workspace floor
  upstream3 blocked by project floor
```

## Related Topics

- See **03-credential-and-key-management.md** for how policies interact with credential resolution at dispatch time
- See **07-user-operations.md** for user scenarios around policy creation and management
- See **08-policy-conflicts.md** for detailed edge case scenarios
