# Orange Config Scope Hierarchy

The orange control plane supports a three-level configuration hierarchy: org, project, and workspace. Narrower scopes win on key collision. The egress always receives a fully materialized, workspace-scoped snapshot and never needs to understand the hierarchy itself.

## Three-level hierarchy

Configs are authored at three scopes:

- **Org** — shared across all projects and workspaces in the org. Typically contains `llm.providers`, `llm.models`, `mcp.servers`, and `profiles`.
- **Project** — inherits from org; can add or override any org-level record. Same record types as org.
- **Workspace** — inherits from project (which inherits from org); can add or override. The only level where `keys` and `rate_limit` are meaningful — these are workspace-specific by nature.

## What belongs at each scope

| Config section | Org | Project | Workspace |
|---|---|---|---|
| `llm.providers` | yes | override/add | override/add |
| `llm.models` | yes | override/add | override/add |
| `mcp.servers` | yes | override/add | override/add |
| `profiles` | yes | override/add | override/add |
| `keys` | — | — | workspace-only |
| `rate_limit` | — | — | workspace-only |

Data cannot be shared between orgs. The org boundary is enforced by the relational hierarchy — `ResolveWorkspaceHierarchy` only returns the org that the workspace's project belongs to.

## Scope ID convention

The `SnapshotStore` (table: `config_snapshots`, key column: `workspace_id TEXT`) is keyed by an opaque string. Scope IDs use a typed prefix on the existing key column — no schema changes were needed:

```
"org:{orgID}"      → org-level config snapshot
"proj:{projectID}" → project-level config snapshot
"{workspaceID}"    → workspace-level config snapshot (no prefix; backward-compatible)
```

There is no `scope_type` column in `config_snapshots`. The type is entirely implicit in the string prefix. To enumerate all org-level snapshots directly in SQL, use `WHERE workspace_id LIKE 'org:%'`.

Helper functions in `examples/orange/internal/config/config_hierarchy.go`:

- `OrgScopeID(orgID string) string` — returns `"org:" + orgID`
- `ProjectScopeID(projectID string) string` — returns `"proj:" + projectID`
- `IsScopeID(id string) bool` — true if the string starts with `"org:"` or `"proj:"`

## Relational hierarchy

Org/project/workspace membership is tracked in standard relational tables:

- `workspaces(workspace_id, project_id, ...)` — workspace belongs to a project
- `projects(project_id, org_id, ...)` — project belongs to an org

The `HierarchyResolver` interface resolves a workspace ID to its containing project and org:

```go
type HierarchyResolver interface {
    ResolveWorkspaceHierarchy(ctx context.Context, workspaceID string) (WorkspaceHierarchy, error)
}

type WorkspaceHierarchy struct { OrgID, ProjectID string }
```

`WorkspaceService` in `examples/orange/internal/server/resources/workspace_postgres.go` implements this with a single JOIN:

```sql
SELECT w.project_id, p.org_id
FROM workspaces w
JOIN projects p ON p.project_id = w.project_id
WHERE w.workspace_id = $1
```

## Merge semantics

`MergeRawConfigs(base, overlay *RawConfig) *RawConfig` in `config_hierarchy.go` does an additive map merge across all sections — overlay wins on key collision. Neither input is mutated; the result owns its own map headers.

Each map field is merged independently: `llm.providers`, `llm.models`, `mcp.servers`, `profiles`, `keys`, `rate_limit.tiers`, `rate_limit.policies`.

The full merge is:

```go
merged = MergeRawConfigs(MergeRawConfigs(orgRaw, projRaw), wsRaw)
```

## Fetch-time materialization

When an egress calls `Fetch()` (the data-plane snapshot RPC), `ConfigService.Fetch()` in `examples/orange/internal/server/resources/config_service.go` performs the following steps:

1. Calls `HierarchyResolver.ResolveWorkspaceHierarchy(ctx, wsID)` to get `orgID` and `projectID`.
2. Loads the org snapshot via `store.FetchLatest(ctx, OrgScopeID(orgID), 0)`.
3. Loads the project snapshot via `store.FetchLatest(ctx, ProjectScopeID(projectID), 0)`.
4. Loads the workspace snapshot via `store.FetchLatest(ctx, wsID, 0)`.
5. Merges: org → project → workspace (workspace wins).
6. Projects the merged config down to workspace scope via `ProjectForWorkspace(merged, wsID)` — this filters `keys`, `profiles`, and `rate_limit` to entries belonging to this workspace, and prunes providers and servers to only those referenced by the resulting records.
7. Marshals the projected config to YAML and computes a SHA-256 checksum.
8. Returns `Unchanged` if the projected checksum matches the client's `last_checksum`; otherwise returns the full projected YAML envelope.

The egress always receives a fully materialized, workspace-scoped snapshot. It never sees records from other workspaces.

## Staleness detection

Staleness is detected via the **projected SHA-256 checksum**, not the workspace version number. If org config changes but workspace config does not, the workspace version stays the same but the projected checksum changes — so the egress correctly receives an update on the next poll.

## Publishing configs at different scopes

Use the `PublishSnapshot` RPC with scope IDs as `workspace_id`. The same publish/list/get/rollback admin API serves all three scopes since they all go through the same `SnapshotStore`.

- Org config: `workspace_id = "org:{orgID}"`
- Project config: `workspace_id = "proj:{projectID}"`
- Workspace config: `workspace_id = "{workspaceID}"`

Example: to publish an org-level config that sets a shared LLM provider for all workspaces in org `acme`, set `workspace_id = "org:acme"` in the `PublishSnapshot` request. All workspaces whose project belongs to `acme` will incorporate the org config on their next `Fetch()` call.
