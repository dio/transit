# Scope Binding Authorization Design

## Overview

This document defines the scope-based authorization system for Orange API keys. The design embeds workspace context directly into scopes, allowing a single API key to safely access multiple workspaces with graduated permissions while maintaining a clean audit trail.

## Rationale

Five core decisions underpin this design. Each one has a naive alternative that was explicitly considered and rejected.

### 1. Context Embedded in Scopes, Not a Separate ACL
**Naive approach:** Store resource permissions in a separate ACL table; join on every authorization check.
**Why it fails:** Multi-table queries on the hot path. Debugging a denial requires reconstructing state across tables. Schema changes cascade unpredictably.
**This design:** The scope string is self-describing. `secret:read[workspace1]` encodes *what* and *where* in one token — a single key lookup returns everything needed to authorize the request. Authorization is a string match, not a query.

### 2. Same Key Material, New Key ID
**Naive approach:** Issue a new secret value whenever workspace membership changes; require clients to rotate.
**Why it fails:** Any rotation demands out-of-band coordination. Automated integrations — CI/CD pipelines, background workers, third-party services — routinely miss rotation events, causing silent production outages.
**This design:** The plaintext secret the client stores never changes. Only the server-side metadata (`key_id`, `scopes`) is updated atomically. Zero client disruption.

### 3. All Keys Updated Atomically on Membership Change
**Naive approach:** Let each key have independent scope management; update only the key used in the current session.
**Why it fails:** Keys for the same user drift out of sync. An admin removing a user from a workspace may not revoke access if the user has multiple keys — a silent security gap.
**This design:** All keys mirror the user's authoritative workspace memberships. Access revocation is uniform and immediate across every API client the user has configured.

### 4. Immutable Key IDs (Append-Only Audit Trail)
**Naive approach:** Update the `scopes` column in place on the existing row when membership changes.
**Why it fails:** In-place mutation destroys history. "What permissions did key `k_003` have on 2025-11-01?" becomes unanswerable. Compliance audits and incident investigations hit a dead end.
**This design:** Every scope change produces a new `key_id` that `supersedes` the previous one. The full chain (`k_001 → k_002 → k_003 → k_004`) is queryable for compliance and forensics.

### 5. New Keys Inherit Current Workspace Memberships
**Naive approach:** New keys start with minimal scopes (`user:read`) and require explicit configuration per workspace.
**Why it fails:** Users creating a second key for a new application must manually configure workspace access — friction that leads to misconfigured keys and support burden.
**This design:** A new key immediately reflects the user's current workspace memberships, consistent with all other active keys. The user's membership is the source of truth; keys are vehicles for it.

---

## Key Principles

- **Multiple keys per user**: A user can create multiple API keys (e.g., for different applications or integrations)
- **Context in scopes**: Workspace context is embedded in the scope string itself
- **Same material, new ID**: When updating scopes (add/remove workspace), revoke the old key ID but issue a new key with the same hashed material (secret), just with updated scopes. **All active keys** for the user are updated
- **No client disruption**: Since each key's material stays the same, API clients don't need to rotate their secrets
- **Full audit trail**: Key ID evolution + scope history allows tracking permission changes over time
- **Inherited scopes**: When a new key is created, it inherits the user's current workspace memberships in its initial scopes

## Scope Format

### Naming Convention

Scopes follow the format: `<resource>:<action>[context]`

- `<resource>`: The API resource (secret, token, workspace, org, user, egress-bundle, etc.)
- `<action>`: The operation (read, write, delete, issue, admin, etc.)
- `[context]`: Optional workspace/resource context, only present when scoped to a specific resource

### Context Rules

- Context is **only included when needed**
- Most scopes will have context (workspace-specific actions)
- Org/user-level scopes typically have no context: `user:read`, `org:admin`
- Multi-workspace users get separate scope entries per workspace: `secret:read[workspace1],secret:read[workspace2]`

### Wildcard Scopes

For bulk permissions within a context (e.g., workspace admin):
- Format: `<resource>:*[context]`
- Example: `ws:*[workspace1]` grants all actions on a workspace

## Common Scopes

### User Scopes
- `user:read` - Read own user info (no context needed)

### Workspace Scopes
- `ws:*[workspace1]` - Full workspace admin access
- `secret:read[workspace1]` - Read secrets in workspace1
- `secret:write[workspace1]` - Create/update secrets in workspace1
- `secret:delete[workspace1]` - Delete secrets in workspace1
- `token:issue[workspace1]` - Issue API keys within workspace1 context
- `egress-bundle:download[workspace1]` - Download egress bundles for workspace1

### Org Scopes
- `org:admin` - Org-level admin (no context)
- `user:create` - Create new users in org

## Key Lifecycle

> **Atomicity requirement:** All multi-key updates (steps 3–5 below) must execute in a single database transaction. Partial updates — where some keys have the new scopes and others still carry old scopes — are a security violation. If the transaction fails, roll back entirely and surface an error.

### 1. User Creation
```
CreateUser(email, org_id)
  ↓
Automatically issue one API key:
  - key_id: k_001
  - material_hash: <sha256(secret_aaa)>
  - scopes: [user:read]
  - created_at: now
```

Client receives the plaintext secret `secret_aaa` (once). They only store this.

### 2. User Creates Additional Keys
```
User creates a second key for a different application:
  - key_id: k_002
  - material_hash: <sha256(secret_bbb)>
  - scopes: [user:read]  ← same initial scopes as all keys
  - created_at: now
```

User now has two active keys with different secrets.

### 3. Adding User to Workspace (Updates ALL Keys)
```
AddWorkspaceMember(user_id, workspace_id)
  ↓
FOR EACH active key for user:
  1. Get key details (key_id, material_hash, scopes)
  2. Append workspace-specific scopes:
     - secret:read[workspace1]
     - secret:write[workspace1]
     - token:issue[workspace1]
  3. Revoke old key_id
  4. Issue new key with:
     - New key_id
     - SAME material_hash (unchanged)
     - Updated scopes

Result for key1:
  - Revoke k_001
  - Issue k_001b:
    - material_hash: <sha256(secret_aaa)>  ← SAME
    - scopes: [user:read, secret:read[workspace1], secret:write[workspace1], token:issue[workspace1]]
    - supersedes: k_001

Result for key2:
  - Revoke k_002
  - Issue k_002b:
    - material_hash: <sha256(secret_bbb)>  ← SAME
    - scopes: [user:read, secret:read[workspace1], secret:write[workspace1], token:issue[workspace1]]
    - supersedes: k_002
```

Both clients continue using their original secrets (`secret_aaa`, `secret_bbb`)—no change needed.

### 4. Adding User to Second Workspace (Updates ALL Keys Again)
```
AddWorkspaceMember(user_id, workspace2)
  ↓
FOR EACH active key for user:
  Update with workspace2 scopes

Result:
  k_001c: scopes=[user:read, secret:read[workspace1], secret:write[workspace1], token:issue[workspace1], secret:read[workspace2], secret:write[workspace2], token:issue[workspace2]]
  k_002c: scopes=[user:read, secret:read[workspace1], secret:write[workspace1], token:issue[workspace1], secret:read[workspace2], secret:write[workspace2], token:issue[workspace2]]
```

### 5. Removing User from Workspace (Updates ALL Keys)
```
RemoveWorkspaceMember(user_id, workspace1)
  ↓
FOR EACH active key for user:
  Remove all scopes for [workspace1]

Result:
  k_001d: scopes=[user:read, secret:read[workspace2], secret:write[workspace2], token:issue[workspace2]]
  k_002d: scopes=[user:read, secret:read[workspace2], secret:write[workspace2], token:issue[workspace2]]
```

### 6. User Creates New Key (Inherits Current Scopes)
```
User creates a third key after being added to workspace1 and workspace2:
  - key_id: k_003
  - material_hash: <sha256(secret_ccc)>
  - scopes: [user:read, secret:read[workspace1], secret:write[workspace1], token:issue[workspace1], secret:read[workspace2], secret:write[workspace2], token:issue[workspace2]]
  ↑ Inherits current workspace memberships
```

## Authorization Checks

### Lookup
1. Client provides API key (plaintext secret)
2. Server hashes it: `hash(secret)` → `<material_hash>`
3. Query key store: `SELECT key_id, scopes FROM keys WHERE material_hash = ?`
4. Get **current** key_id and scopes (only active keys)

### Enforcement
1. Extract required action: `<resource>:<action>`
2. Extract context from request (e.g., workspace_id from URL path or headers)
3. Check if any scope in the key's scope list matches:
   - Exact match: `secret:read[workspace1]` matches request to `workspace1`
   - Wildcard match: `ws:*[workspace1]` matches any action on `workspace1`
4. If match found, grant access and propagate context downstream

### Scope Matching Algorithm
```
For each scope in key.scopes:
  1. Parse scope: extract resource, action, context
  2. Check if resource:action matches requested operation
  3. If context present:
     - Extract request context (e.g., workspace_id)
     - Check if request context matches scope context
  4. If all checks pass, grant access
```

### Example: Secret Read Request
```
Request: GET /secret/s-123 (with workspace_id=workspace1 in header)
Key scopes: [user:read, secret:read[workspace1], secret:write[workspace1], token:issue[workspace1]]

Match process:
  1. Required action: secret:read
  2. Request context: workspace1
  3. Check scope "user:read" → no match (resource mismatch)
  4. Check scope "secret:read[workspace1]" → MATCH
     - Resource: secret ✓
     - Action: read ✓
     - Context: workspace1 == workspace1 ✓
  5. Grant access, propagate context=workspace1 downstream
```

## Scope Parsing

Scope format is straightforward to parse:

```
Pattern: ^([a-z-]+):([a-z*]+)(?:\[([^\]]+)\])?$

Examples:
  user:read → resource=user, action=read, context=null
  secret:read[workspace1] → resource=secret, action=read, context=workspace1
  ws:*[workspace1] → resource=ws, action=*, context=workspace1
  org:admin → resource=org, action=admin, context=null
```

## Storage

### Keys Table
```sql
CREATE TABLE api_keys (
  key_id              TEXT PRIMARY KEY,
  user_id             TEXT NOT NULL,
  org_id              TEXT NOT NULL,
  material_hash       TEXT NOT NULL,      -- SHA256(secret)
  scopes              TEXT NOT NULL,      -- comma-separated: "user:read,secret:read[ws1],..."
  is_active           BOOL NOT NULL,
  created_at          TIMESTAMP NOT NULL,
  revoked_at          TIMESTAMP,
  supersedes_key_id   TEXT,               -- reference to previous key_id (for audit trail)
  
  FOREIGN KEY (user_id) REFERENCES users(user_id),
  FOREIGN KEY (org_id) REFERENCES orgs(org_id),
  INDEX (material_hash),                  -- for fast lookup by secret hash
  INDEX (user_id),
  INDEX (is_active, material_hash)
);
```

## Example: Complete User Journey

```
1. Create user alice@example.com in org-1
   → key_id=k_001, scopes=[user:read]

2. Admin adds alice to workspace-1
   → Revoke k_001
   → key_id=k_002, scopes=[user:read, secret:read[workspace-1], secret:write[workspace-1], token:issue[workspace-1]]

3. Admin adds alice to workspace-2
   → Revoke k_002
   → key_id=k_003, scopes=[user:read, secret:read[workspace-1], secret:write[workspace-1], token:issue[workspace-1], secret:read[workspace-2], secret:write[workspace-2], token:issue[workspace-2]]

4. Alice requests GET /workspace/workspace-1/secret
   → API hashes alice's key material
   → Finds k_003 (active)
   → Parses scopes, finds secret:read[workspace-1] matches
   → Grants access, propagates context=workspace-1

5. Admin removes alice from workspace-1
   → Revoke k_003
   → key_id=k_004, scopes=[user:read, secret:read[workspace-2], secret:write[workspace-2], token:issue[workspace-2]]

6. Alice tries GET /workspace/workspace-1/secret
   → Finds k_004 (active)
   → No scope matches secret:read for workspace-1
   → Denies access (403)
```

## Migration & Backwards Compatibility

- This design applies to new keys going forward.
- **Security note:** Existing keys without recorded scopes must be treated carefully. Granting them implicit `*` (all permissions) may over-privilege legacy clients. Prefer treating them as `user:read` only and requiring users to re-issue keys with explicit scopes during a migration window.
- When updating an existing user's workspace membership, issue a new key with this design; the old key becomes inactive.
- Legacy keys should be flagged in the keys table (e.g., `is_legacy = true`) and subjected to a separate, more conservative authorization path until fully migrated.

## Security Considerations

- **Timing-safe hash comparison:** The `material_hash` lookup must use a constant-time string comparison to prevent timing attacks that could infer valid key material.
- **Secret transmission:** The plaintext secret is returned to the client exactly once on key creation. It is never stored server-side in plaintext and must be transmitted over TLS only.
- **Scope escalation:** The `token:issue[workspace1]` scope allows a key to issue new tokens scoped to `workspace1`. Issued tokens must carry a subset of the issuing key's scopes — they must never be allowed to escalate beyond the issuing key's permissions.
- **Key ID predictability:** Key IDs (`k_001`, `k_001b`, etc.) are sequential in the examples but should be random/unpredictable in production (e.g., `k_<uuid>`) to prevent enumeration.
- **Concurrency:** Concurrent `AddWorkspaceMember` calls for the same user must be serialized (advisory lock on `user_id`) to prevent race conditions where two transactions each see the same active keys and independently attempt to supersede them.

## Future Enhancements

- **Scope wildcards for resources**: `secret:*` (all actions on secrets)
- **Expiring scopes**: `secret:read[workspace1]:expires[2026-12-31]`
- **Conditional scopes**: `secret:read[workspace1]:if[ip=10.0.0.0/8]`
- **Scope delegation**: Allow tokens issued with `token:issue[workspace1]` to themselves issue tokens with reduced scopes
