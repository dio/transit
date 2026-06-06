# `orange` CLI

A single CLI that runs orange ecosystem

1. API server

```bash
# Run orange API server bootstrap routine, to generate:
# 1. Organization with ORG_NAME
# 2. First user in that org with password (probably not needed (?))
# 3. API key, stored in api_keys table, with scope=admin
# 4. Also some MASTER_KEK-related, hence the default service KEKs are created
orange server --local --bootstrap=<ORG_NAME>
# The first (plain) API key will be printed in the screen.
ORANGE_ADMIN_API_KEY=sk-org-<ORANGE_ADMIN_API_KEY>
```

``` bash
# Runs orange API server with embedded PG, with standard data dir in ~/.orange/
# no need to set MASTER_KEK since service KEKs are generated
orange server --local
```

2. API server client

```bash
export ORANGE_ADMIN_API_KEY=sk-org-<ORANGE_ADMIN_API_KEY>
```

There are two behaviors:

1. interactive
2. non-interactive

Also two scope:

1. Admin API client
2. (Emulated) API client as an Egress Proxy (Config API client)

```bash
# Runs orange client as Admin API client. ORANGE_SERVER_URL defaults to http://localhost:8080
orange admin --server=<ORANGE_SERVER_URL> --repl # or --interactive? or -r or -i? For interactive
orange admin --server=<ORANGE_SERVER_URL> <resource> # for on-off single command
```

```bash
# Runs orange client as Config API client
ORANGE_CLIENT_IDENTITY="<path/to/ORANGE_CLIENT_IDENTITY_FILE>" orange --server=<ORANGE_SERVER_URL> --repl
orange --server=<ORANGE_SERVER_URL> <resource> # for on-off single command
```

```bash
# runs orange proxy, an envoy runner, that starts envoy proxy (with liborange.so module)
orange proxy # later later later, this is similar to make run
```

Later we can have, subcommands to run something similar like in examples/orange/demos/{llm,mcp,images}

```bash
orange llm # run llm client test
orange mcp # run mcp client tests
orange img # run demos/images
```
