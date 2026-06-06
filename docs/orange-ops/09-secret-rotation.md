# Secret Rotation Scenarios

Complete reference for workspace secret rotation and KeySecret rotation patterns and edge cases.

## Standard Rotation (No In-Flight Requests)

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

## Rotation With In-Flight Requests

```
State: 5 requests mid-flight using secret1:v1

admin rotates secret1: v1 → v2

In-flight requests: complete using v1 (already dispatched)
New requests:       egress resolves v2

Result: zero dropped requests
```

## Rotation Does Not Affect Keys or KeySecrets

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

## Rotate Secret for One Upstream, Other Unaffected

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

## Emergency Revocation

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

## KeySecret Rotation (User-Managed, Zero-Downtime)

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

## Related Topics

- **03-credential-and-key-management.md** — Workspace secrets and BYOK
- **06-admin-operations.md** — Admin secret management operations
- **07-user-operations.md** — User BYOK credential management
- **11-cli-reference.md** — CLI commands for secret rotation
