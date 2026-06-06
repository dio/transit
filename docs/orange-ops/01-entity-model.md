````markdown
# Entity Model

Complete reference for core entities, schema, hierarchy, containment, resource naming, and key scoping.

## Entities and Ownership

| Entity | Owned by | Description |
|----------|----------|-------------|
| `Organization` | — | Top-level tenant |
| `Project` | Organization | Groups workspaces under a common administrative scope |
| `Workspace` | Project | Isolates users, keys, secrets, policies, and one Egress instance |
| `Egress` | Workspace | Proxy instance responsible for request dispatch and policy enforcement |
| `EgressIdentity` | Egress | X.509 identity certificate attesting workspace ownership |
| `EgressKeyPair` | Egress | Asymmetric key pair used for egress → control plane authentication |
| `CPValidationKey` | Control Plane | Control plane public key distributed to egress instances for signature verification |
| `Secret` | Workspace | Shared versioned upstream credential managed at workspace scope |
| `KeySecret` | Key | User-supplied versioned upstream credential bound to a specific key (BYOK) |
| `User` | Organization | Member of the organization; may belong to multiple workspaces |
| `Key` | User × Workspace | Authentication key permanently bound to a workspace |
| `PASETOToken` | User | Metadata and hash of an issued PASETO token; the token itself is never stored |
| `Policy` | Admin or User | Rules attached at Project, Workspace, or Key scope |

---

## Database Schema

```text
Organization {
  id,
  name
}

Project {
  id,
  org_id FK,
  name,
  description?
}

Workspace {
  id,
  project_id FK,
  name,
  description?
}

Egress {
  id,
  workspace_id FK,
  status: "active" | "inactive",
  identity_id FK,
  keypair_id FK,
  description?
}

EgressIdentity {
  id,
  egress_id FK,
  certificate PEM,
  issued_at,
  expires_at,
  serial_number,
  active BOOL
}

EgressKeyPair {
  id,
  egress_id FK,
  algorithm: "Ed25519" | "ECDSA_P256" | "RSA-2048",
  public_key PEM,
  private_key_ref,
  created_at,
  rotated_at,
  active BOOL
}

CPValidationKey {
  id,
  algorithm: "Ed25519" | "ECDSA_P256",
  public_key PEM,
  purpose: "telemetry" | "request_validation" | "both",
  created_at,
  expires_at,
  active BOOL
}

Secret {
  id,
  workspace_id FK,
  upstream_target,
  version,
  value (encrypted),
  active BOOL,
  description?
}

KeySecret {
  id,
  key_id FK,
  upstream_target,
  version,
  value (encrypted),
  active BOOL,
  description?
}

User {
  id,
  org_id FK,
  email,
  description?
}

WorkspaceMember {
  workspace_id FK,
  user_id FK
}

Key {
  id,
  workspace_id FK,
  user_id FK,
  name,
  key_format: "paseto_v4.public",
  description?
}

PASETOToken {
  id,
  key_id FK,
  jti,
  iat,
  exp,
  pol,
  token_hash,
  revoked BOOL,
  created_at
}

Policy {
  id,
  scope_type: "project" | "workspace" | "key",
  scope_id,
  type: "floor" | "flexible",
  description?,
  rules[]
}
```

> `description` fields are free-text annotations provided by users and administrators. They are surfaced by `describe` operations and audit logs.

---

## Hierarchy and Containment

```text
Organization
├── User
└── Project
    └── Workspace
        ├── Egress
        │   ├── EgressIdentity
        │   └── EgressKeyPair
        │
        ├── Secret
        │
        ├── WorkspaceMember
        │   └── Key
        │       └── KeySecret
        │
        └── Policy
```

### Containment Rules

- An Organization owns many Projects and Users.
- A Project owns many Workspaces.
- A Workspace owns exactly one Egress.
- A Workspace owns many Secrets.
- A User may belong to many Workspaces.
- A Key belongs to exactly one User and exactly one Workspace.
- A Key may own multiple KeySecrets.
- Policies may be attached to Projects, Workspaces, or Keys.

---

## Resource Naming Convention

Resources are identified using canonical paths.

Paths are ordered from the organization root to the target resource.

### Canonical Resource Paths

```text
/organizations/acme

/organizations/acme/projects/project1

/organizations/acme/projects/project1/workspaces/workspace1

/organizations/acme/projects/project1/workspaces/workspace1/egress

/organizations/acme/projects/project1/workspaces/workspace1/secrets/openai

/organizations/acme/projects/project1/workspaces/workspace1/users/user1

/organizations/acme/projects/project1/workspaces/workspace1/users/user1/keys/key1

/organizations/acme/projects/project1/workspaces/workspace1/users/user1/keys/key3
```

### Principal Names

Principals use:

```text
<name>@<organization>
```

Examples:

```text
admin@acme
user1@acme
egress@acme
```

Principal names identify the actor.

Resource paths identify the resource.

### Ownership Metadata

Ownership is not encoded into resource names.

Example:

```text
Resource:
/organizations/acme/projects/project1/workspaces/workspace1/users/user1/keys/key3

Metadata:
owner: user1@acme
binding: BYOK
```

### General Rules

1. Resource paths are canonical.
2. Paths are ordered from root to leaf.
3. Resource names are unique within their parent scope.
4. Ownership, bindings, policies, and key formats are metadata.
5. The full resource path uniquely identifies a resource.

---

## Key Scoping Model

A `Key` is permanently bound to a single Workspace.

The `workspace_id` foreign key is immutable after creation.

```text
User
 │
 ├── Key
 │     │
 │     ├── Workspace (exactly one)
 │     │
 │     ├── KeySecret (optional, per upstream)
 │     │
 │     └── PASETO Tokens
 │
 └── Key
```

### Invariants

- A Key belongs to exactly one Workspace.
- A Key belongs to exactly one User.
- A Key cannot be transferred to another Workspace.
- A Key cannot be reassigned to another User.
- A Key may optionally define BYOK credentials through KeySecrets.
- KeySecrets override Workspace Secrets during request dispatch.

---

## Workspace Member Scoping

```text
Workspace:
/organizations/acme/projects/project1/workspaces/workspace1

Members:
  user1@acme
  user2@acme

Keys:
  key1
    owner: user1@acme
    format: paseto_v4.public

  key2
    owner: user1@acme
    format: paseto_v4.public

  key3
    owner: user1@acme
    BYOK: upstream1
```

### Workspace Rules

- Keys created within a Workspace remain bound to that Workspace forever.
- Keys cannot be moved between Workspaces.
- Keys cannot be reassigned to another User.
- Workspace membership grants identical capabilities to all members.
- There are no workspace-specific roles.

Members may:

- Create keys.
- Delete their own keys.
- Attach policies to their own keys.
- Send requests using their own keys.
- Configure BYOK credentials on their own keys.

---

## PASETO Authentication Model

All user-facing authentication keys use:

```text
paseto_v4.public
```

Properties:

- Ed25519 signatures.
- Stateless verification at Egress.
- No control-plane lookup required for normal request validation.
- Revocation enforced through cached revocation data.
- Token payload may contain embedded policy information.

### Token Storage

The signed token itself is never stored.

Only the following metadata is retained:

```text
jti
iat
exp
pol
token_hash
revoked
created_at
```

This enables:

- Auditability
- Revocation
- Token lifecycle tracking

without retaining bearer credentials.

---

## Secret Resolution Order

When dispatching a request to an upstream target:

```text
1. KeySecret
2. Workspace Secret
3. No credential available
```

Example:

```text
Workspace Secret:
  openai -> workspace-api-key

KeySecret:
  openai -> personal-api-key

Result:
  personal-api-key is used
```

This allows users to bring their own credentials without affecting other workspace members.

---

## Policy Scope Hierarchy

Policies may be attached at:

```text
Project
└── Workspace
    └── Key
```

### Evaluation Order

```text
Project Policy
       ↓
Workspace Policy
       ↓
Key Policy
```

Project-level policies establish organizational guardrails.

Workspace and Key policies may further restrict behavior but cannot bypass enforced Project-level constraints.
````
