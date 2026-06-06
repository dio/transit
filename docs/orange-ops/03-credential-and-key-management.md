# Credential and Key Management

Complete reference for workspace secrets, user-bound credentials (BYOK), and credential resolution at dispatch.

## Workspace Secrets (Admin-Managed, Shared)

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

### Zero-Downtime Rotation

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

## User-Bound Upstream Credentials (BYOK)

A user may supply their own upstream credential for a specific upstream and
bind it to one of their keys as a `KeySecret`. When egress dispatches a
request via that key to that upstream, the `KeySecret` is used instead of
the workspace-level `Secret`.

### Why BYOK Matters

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

### KeySecret Properties

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

### KeySecret Versioning and Zero-Downtime Rotation

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

## Credential Resolution at Dispatch

When egress is ready to proxy a request (all policy checks have passed)
it resolves which credential to inject for the target upstream:

```
Request via Key K to upstream U
        │
        ▼
┌───────────────────────────────────────────┐
│  Does K have an active KeySecret          │
│  for upstream U?                          │
│                                           │
│  YES → use KeySecret (BYOK)               │
│         metering → user's account         │
│                                           │
│  NO  → does workspace have an active      │
│        Secret for upstream U?             │
│                                           │
│        YES → use workspace Secret         │
│               (shared)                    │
│               metering → workspace        │
│               account                     │
│                                           │
│        NO  → DENIED                       │
│              "no credential               │
│               configured for              │
│               this upstream"              │
└───────────────────────────────────────────┘
```

### Credential Resolution Summary Table

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

## Important Notes

- **Policy is evaluated BEFORE credential resolution.** A policy denial blocks the request before credential resolution is reached. Having a KeySecret for an upstream does not grant access to that upstream if policy denies it.

- **BYOK credential values are write-only.** Users can set, rotate, and remove KeySecrets, but the actual credential values are never returned by any read command (list, describe, get). Only existence, upstream target, and active version are surfaced.

- **Workspace Secret rotation is independent of BYOK.** When an admin rotates a workspace Secret, keys with KeySecrets for that upstream are unaffected — they continue using their own KeySecret.

- **BYOK does not affect other keys.** If user1 configures a KeySecret for upstream1 on key3, it only affects requests via key3. User1's other keys (key1, key2) continue using the workspace Secret.

## Related Topics

- See **02-policy-system.md** for how policies interact with credential resolution
- See **04-cryptographic-identity.md** for PASETO key generation and verification
- See **06-admin-operations.md** for admin secret management commands
- See **07-user-operations.md** for user BYOK scenarios
- See **09-secret-rotation.md** for detailed rotation scenarios
