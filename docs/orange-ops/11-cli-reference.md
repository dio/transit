# CLI Reference

Complete reference for the Orange CLI: installation, authentication, commands, and output formats.

## Installation and Authentication

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

## Non-Interactive Mode

One command, one result, exits. Suitable for scripts and CI pipelines.

```sh
orange <resource> <verb> [target] [flags]
```

**Global flags available on every command:**

```
--org <org>          Override org from config
--output, -o         Output format: table (default) | json | yaml
--quiet, -q          Suppress headers and decorations
--no-color           Disable ANSI color output
```

## REPL Mode

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

### Context Commands (REPL Only)

```
use project <name>      Set active project (scopes subsequent commands)
use workspace <name>    Set active workspace
use key <name>          Set active key (user mode)
ctx                     Show current context
ctx clear               Clear all context
```

### REPL Navigation

```
Up/Down arrows    Command history
Tab               Autocomplete resource names, flags, and values
Ctrl+C            Cancel current input
Ctrl+D / exit     Exit REPL
history           Show command history
history clear     Clear history
```

> REPL output is always human-readable by default. Pipe to `--output json`
for machine output even in REPL: `describe workspace1 --output json`

## Admin Commands

All commands below require admin identity (`orange auth whoami` shows `role: admin`).

### Project Management

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

### Workspace Management

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

### Egress Management

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

### Egress Identity Management

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

### Control Plane Validation Key Management

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

### Secret Management

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

### User and Membership Management

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

### Admin Policy Management

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

## User Commands

Available to any authenticated user. Scoped to their memberships.

### Key Management

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

### Key Policy Management

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

### Key-Bound Upstream Credential Management (BYOK)

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

**Example output (`orange key-secret list`):**

```
KEY    UPSTREAM    ACTIVE_VERSION   DESCRIPTION
key3   upstream1   v2               "My personal OpenAI key"
key3   upstream3   v1               (none)
```

## Describe Commands

`describe` is a unified read command for inspecting the state of any resource.
Admins see the full view; users see a scoped view limited to their own resources.

See **06-admin-operations.md** and **07-user-operations.md** for detailed describe output examples.

## Output Formats

Every command accepts `--output` / `-o`:

```sh
orange describe workspace workspace1 --project project1 --output json
orange describe workspace workspace1 --project project1 --output yaml
orange key list --output json | jq '.[] | select(.format == "paseto_v4.public")'
```

**JSON output example (`orange key list -o json`):**

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
  }
]
```

> `key_secrets` entries never include the credential value — only the upstream
> target, active version, and description are returned.
> `public_key_thumbprint` is only set for PASETO-format keys.

## REPL Walkthroughs

### Admin Onboarding Example

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

orange> secret set secret1 --upstream upstream1 \
          --value $OPENAI_KEY --description "OpenAI API key"
  created secret1 v1  [active]

orange> workspace assign user1@organization
  assigned user1@organization → workspace1

orange> describe workspace
  workspace:    workspace1@project1.organization
  egress:       active
  members:      user1@organization
  secrets:      secret1 (upstream1, v1)

orange> exit
  goodbye
```

### User Key and BYOK Example

```
$ orange repl

orange v0.1.0  |  org: organization  |  user: user1@organization
Type 'help' for commands, 'exit' to quit.

orange> use workspace workspace1 --project project1
  context: workspace → workspace1@project1.organization

orange> key create key1 --description "Used by training pipeline"
  created key1@workspace1.project1.organization
  format: paseto_v4.public

orange> key create key3 --description "BYOK key"
  created key3@workspace1.project1.organization
  format: paseto_v4.public

orange> key-secret set upstream1 --key key3 \
          --value $MY_OWN_OPENAI_KEY \
          --description "My personal OpenAI key"
  created KeySecret for upstream1 on key3 — v1 [active]

orange> describe key key3
  key:           key3
  bound credentials (BYOK):
    upstream1  active_version: v1
  credential resolution:
    upstream1 → KeySecret v1 (personal)

orange> key list
  NAME   WORKSPACE    FORMAT           BYOK        EFFECTIVE
  key1   workspace1   paseto_v4.public —           allow upstream1+2 @1000/min
  key3   workspace1   paseto_v4.public upstream1   allow upstream1+2 @1000/min

orange> exit
  goodbye
```

## Related Topics

- **05-onboarding-workflow.md** — Step-by-step walkthrough
- **06-admin-operations.md** — Detailed admin scenarios
- **07-user-operations.md** — Detailed user scenarios
