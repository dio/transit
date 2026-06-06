# Orange Admin Quickstart

This guide walks through starting the management plane server for the first time,
bootstrapping the initial admin credentials, and running the core admin operations
(A1–A6) using the `orange` CLI.

## Prerequisites

- Go 1.22+
- PostgreSQL 15+ **or** use `--local` to start embedded Postgres automatically

## 1. Build

```bash
# from examples/
go build -o .bin/orange ./orange/cmd/orange
export PATH="$PWD/.bin:$PATH"
```

## 2. Bootstrap (one-time setup)

`--local` uses embedded Postgres (data persisted at `~/.orange/data/`) and
auto-generates a master KEK at `~/.orange/kek`. No manual key setup needed.

```bash
orange server --local --bootstrap=acme
```

Output (stdout — eval-friendly):

```
# org: acme  user: admin@acme.com
export ORANGE_API_KEY=sk-org-...
```

**Copy and run the `export` line.** The key is shown only once and never stored
in plaintext.

To reset everything and start fresh:

```bash
orange server --local --purge --bootstrap=acme
```

## 3. Save credentials

```bash
export ORANGE_API_KEY=sk-org-...   # from the bootstrap output

orange auth login --org acme
# prompts for the API key, saves to ~/.orange/config
```

After login, `ORANGE_API_KEY` is no longer required on every command — the CLI
reads it from `~/.orange/config`.

## 4. Start the server

```bash
orange server --local
```

The server listens on `:8080` by default. Change the port with `--port`.

## 5. Core admin operations

### A1 — Create org, project, and workspace

The bootstrap already created the `acme` org. Retrieve it:

```bash
orange org list
```

Create a project:

```bash
orange project create --org-id <org_id> --name platform
```

Create a workspace:

```bash
orange workspace create --project-id <project_id> --name production
```

### A2 — Create users

```bash
orange user create --org-id <org_id> --email alice@acme.com
orange user create --org-id <org_id> --email bob@acme.com
```

### A5 — Add workspace members

```bash
orange member add --workspace-id <workspace_id> --user-id <alice_user_id>
orange member add --workspace-id <workspace_id> --user-id <bob_user_id>
```

### A6 — Remove a workspace member

```bash
orange member remove --workspace-id <workspace_id> --user-id <bob_user_id>
```

### Inspect resources

```bash
orange org        get  --org-id        <id>
orange project    get  --project-id    <id>
orange workspace  get  --workspace-id  <id>
orange user       get  --user-id       <id>

orange org       list
orange project   list  --org-id      <id>
orange workspace list  --project-id  <id>
orange user      list  --org-id      <id>
orange member    list  --workspace-id <id>
```

All output is JSON by default. Pass `--output yaml` for YAML.

## 6. Inspect local data with psql

```bash
# In one terminal — starts embedded Postgres and prints the DSN:
orange localdata

# In another terminal:
psql "$(orange localdata 2>/dev/null)"
```

## 7. External Postgres (production)

```bash
export STORE_DSN="postgres://user:pass@host:5432/orange?sslmode=require"
export MASTER_KEK_URI="env://MASTER_KEK_B64"        # or gcp-kms://…
export MASTER_KEK_B64="$(openssl rand -base64 32)"  # generate once, store safely

orange server --bootstrap=acme   # bootstrap once
orange server                    # run the server
```

## Environment variable reference

| Variable                  | Default                  | Description                              |
|---------------------------|--------------------------|------------------------------------------|
| `ORANGE_SERVER`           | `http://localhost:8080`  | Server base URL (CLI)                    |
| `ORANGE_API_KEY`          | —                        | Admin API key (CLI); falls back to config|
| `ORANGE_ORG`              | —                        | Active org override (CLI)                |
| `PORT`                    | `8080`                   | Listen port (server)                     |
| `STORE_DSN`               | —                        | Postgres DSN; embedded PG used if unset  |
| `MASTER_KEK_URI`          | —                        | Master key URI, **required** unless `--local` |
| `ORANGE_BOOTSTRAP_ORG`    | —                        | Org slug for bootstrap (alt to `--bootstrap`) |
| `ORANGE_BOOTSTRAP_EMAIL`  | —                        | Admin email for bootstrap (default: `admin@<org>`) |
