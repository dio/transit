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

All management plane operations live under `orange admin`.
Aliases work at every level: `org`=`organization`, `proj`=`project`, `ws`=`workspace`, `sec`=`secret`.

### A1 — Create org, project, and workspace

The bootstrap already created the `acme` org. Retrieve it:

```bash
orange admin org list
```

Create a project (set `ORANGE_ORG_ID` once to avoid repeating the flag):

```bash
export ORANGE_ORG_ID=<org_id>
orange admin project create --name platform
```

Create a workspace:

```bash
export ORANGE_PROJ_ID=<project_id>
orange admin workspace create --name production
```

### A2 — Create users

```bash
orange admin user create --email alice@acme.com
orange admin user create --email bob@acme.com
```

### A5 — Add workspace members

```bash
export ORANGE_WS_ID=<workspace_id>
orange admin member add --user-id <alice_user_id>
orange admin member add --user-id <bob_user_id>
```

### A6 — Remove a workspace member

```bash
orange admin member remove --user-id <bob_user_id>
```

### Inspect resources

```bash
orange admin org        get  --org-id        <id>
orange admin project    get  --project-id    <id>
orange admin workspace  get  --workspace-id  <id>
orange admin user       get  --user-id       <id>

orange admin org       list
orange admin project   list
orange admin workspace list
orange admin user      list
orange admin member    list
```

All output is table format by default. Pass `-o json` or `-o yaml` for structured output.

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
