# Orange Admin Quickstart

This guide walks through starting the management plane server for the first time,
bootstrapping the initial admin credentials, and running the core admin operations
(A1–A6) using the `orange` CLI.

## Prerequisites

- Go 1.22+
- PostgreSQL 15+ **or** let the server start its embedded Postgres automatically

## 1. Build the binaries

```bash
# from examples/
go build -o .bin/orange-server ./orange/cmd/server
go build -o .bin/orange       ./orange/cmd/orange
```

Add `.bin/` to your `PATH` for the rest of this guide:

```bash
export PATH="$PWD/.bin:$PATH"
```

## 2. Generate a master key

The server encrypts all secrets with a three-tier KEK hierarchy. For local
development, a random 32-byte key stored in an environment variable is enough:

```bash
export MASTER_KEK_B64="$(openssl rand -base64 32)"
export MASTER_KEK_URI="env://MASTER_KEK_B64"
```

For production, use a cloud KMS URI (e.g. `gcp-kms://projects/…/keyRings/…`).

## 3. Start the server with bootstrap

Set the bootstrap variables **before** the first run. The server creates the
initial org and admin user only when no orgs exist yet.

```bash
export ORANGE_BOOTSTRAP_ORG=acme
export ORANGE_BOOTSTRAP_EMAIL=admin@acme.com

orange-server
```

On first boot you will see a bordered block on stderr:

```
╔══════════════════════════════════════════════════════════╗
║              ORANGE BOOTSTRAP COMPLETE                  ║
╠══════════════════════════════════════════════════════════╣
║  org_id  : 01936c1a-…                                   ║
║  user_id : 01936c1b-…                                   ║
║  key_id  : 01936c1c-…                                   ║
║                                                          ║
║  ADMIN API KEY (save this — shown only once):           ║
║  osk_…                                                  ║
╚══════════════════════════════════════════════════════════╝
```

**Save the `osk_…` key.** It is never stored in plaintext and cannot be
recovered after this point.

On subsequent restarts the bootstrap block is skipped; the server logs
`bootstrap: orgs already exist, skipping`.

## 4. Configure the CLI

```bash
export ORANGE_API_KEY="osk_…"          # paste the key from step 3
export ORANGE_SERVER="http://localhost:8080"  # default, can omit
```

Both variables can also be passed per-command with `--api-key` and `--server`.

## 5. Core admin operations

### A1 — Create an org, project, and workspace

The bootstrap already created the `acme` org. Retrieve it:

```bash
orange org list
```

Create a project inside it:

```bash
orange project create \
  --org-id <org_id from above> \
  --name platform
```

Create a workspace inside the project:

```bash
orange workspace create \
  --project-id <project_id> \
  --name production
```

### A2 — Create users

```bash
orange user create \
  --org-id <org_id> \
  --email alice@acme.com

orange user create \
  --org-id <org_id> \
  --email bob@acme.com
```

### A5 — Add workspace members

```bash
orange member add \
  --workspace-id <workspace_id> \
  --user-id <alice_user_id>

orange member add \
  --workspace-id <workspace_id> \
  --user-id <bob_user_id>
```

### A6 — Remove a workspace member

```bash
orange member remove \
  --workspace-id <workspace_id> \
  --user-id <bob_user_id>
```

### Inspect members

```bash
orange member list --workspace-id <workspace_id>
```

## 6. Get / list any resource

Every resource supports `get` and `list`:

```bash
orange org        get  --org-id        <id>
orange project    get  --project-id    <id>
orange workspace  get  --workspace-id  <id>
orange user       get  --user-id       <id>

orange org       list
orange project   list  --org-id       <id>
orange workspace list  --project-id   <id>
orange user      list  --org-id       <id>
```

All responses are printed as pretty-printed JSON.

## 7. External Postgres (production)

To use an external database instead of the embedded Postgres:

```bash
export STORE_DSN="postgres://user:pass@host:5432/orange?sslmode=require"
orange-server
```

## Environment variable reference

| Variable                  | Default                  | Description                              |
|---------------------------|--------------------------|------------------------------------------|
| `ORANGE_SERVER`           | `http://localhost:8080`  | Server base URL (CLI)                    |
| `ORANGE_API_KEY`          | —                        | Admin API key (CLI)                      |
| `PORT`                    | `8080`                   | Listen port (server)                     |
| `STORE_DSN`               | —                        | Postgres DSN; embedded PG used if unset  |
| `MASTER_KEK_URI`          | —                        | Master key URI, **required** (server)    |
| `ORANGE_BOOTSTRAP_ORG`    | —                        | Org slug for first-run bootstrap         |
| `ORANGE_BOOTSTRAP_EMAIL`  | —                        | Admin email for first-run bootstrap      |
