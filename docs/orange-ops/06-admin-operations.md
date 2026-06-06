# Admin Operational Scenarios

Complete reference for all administrative tasks and operations.

## A1 — Create a Project

```
Precondition: org exists
Actor: admin

admin creates project
  project.org_id = organization.id
  project.name   = "project1"

Result: project1@organization exists
```

## A2 — Create a Workspace Within a Project

```
Precondition: project exists
Actor: admin

admin creates workspace
  workspace.project_id = project1.id
  workspace.name       = "workspace1"

Result: workspace1@project1.organization exists
```

## A3 — Onboard Egress to a Workspace

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

See **04-cryptographic-identity.md** for cryptographic details.

## A4 — Set a Secret on an Egress

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

## A5 — Assign a User to a Workspace

```
Precondition: user exists in org, workspace exists
Actor: admin

admin creates WorkspaceMember
  workspace_id = workspace1.id
  user_id      = user1.id

Result: user1 can now create keys and send requests
        via workspace1
```

## A6 — Remove a User From a Workspace

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

## A7 — Set a Floor Policy on a Project

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

## A8 — Set a Floor Policy on a Workspace

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

## A9 — Tighten a Workspace Flexible Policy (Lazy Invalidation)

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

See **02-policy-system.md** for lazy invalidation details.

## A10 — Rotate a Secret (Zero-Downtime)

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

See **09-secret-rotation.md** for detailed rotation scenarios.

## A11 — Add a Second Workspace to a Project

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

## A12 — Remove a Workspace

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

## A13 — Describe a Workspace (Admin View)

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

## A14 — Describe a Project (Admin View)

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

## A15 — Describe an Egress (Admin View)

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

## A16 — Describe a User (Admin View)

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

## A17 — Rotate Egress Key Pair

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

See **04-cryptographic-identity.md** for key rotation details.

## A18 — Rotate Control Plane Validation Key

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

See **04-cryptographic-identity.md** for details.

## Related Topics

- **02-policy-system.md** — Policy model and evaluation
- **03-credential-and-key-management.md** — Secret management
- **04-cryptographic-identity.md** — Key and certificate management
- **11-cli-reference.md** — Admin CLI commands reference
