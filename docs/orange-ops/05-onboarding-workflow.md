# Onboarding Workflow (Step-by-Step)

Complete walkthrough of the full onboarding process from project creation through first request.

## Actors

```
admin  →  admin@organization
user1  →  user1@organization
user2  →  user2@organization
```

## Step 1 — Admin Creates Project

```
admin@organization
  creates project1@organization
```

## Step 2 — Admin Creates Workspace

```
admin@organization
  creates workspace1@project1.organization
```

## Step 3 — Admin Onboards Egress (with Identity and Key Pair)

```
admin@organization
  deploys egress@workspace1.project1.organization
        │
        ├──► System automatically:
        │    1. Generates EgressIdentity (X.509)
        │       CN=egress.workspace1.project1.organization
        │       Validity: 90 days
        │
        │    2. Generates EgressKeyPair (Ed25519)
        │       Private key → secret service (vault path)
        │       Public key  → registered with CP
        │
        │    3. Distributes CPValidationKey to egress
        │       (CP public key for telemetry verification)
        │
        │    4. Generates EgressPASETOKeyPair #1 (Ed25519)
        │       Private key → secret service
        │       Public key  → held by Egress for token verification
        │
        │    5. Generates EgressPASETOKeyPair #2 (Ed25519)
        │       (second pair for rotation / redundancy)
        │
        └──► Egress instance boots with:
             - Identity certificate (mTLS to CP)
             - Private key reference (resolves from secret service)
             - CP validation key (verifies CP-signed config)
             - Two EgressPASETOKeyPairs (for signing/verifying user tokens)
```

See **04-cryptographic-identity.md** for details on each artefact.

## Step 4 — Admin Sets Workspace Secrets

```
admin@organization
  sets secret1:v1  →  upstream1
  sets secret2:v1  →  upstream2
  on egress@workspace1.project1.organization
```

See **03-credential-and-key-management.md** for secret management details.

## Step 5 — Admin Assigns Users

```
admin@organization
  assigns user1@organization  →  workspace1@project1.organization
  assigns user2@organization  →  workspace1@project1.organization
```

## Steps 6–7 — Users Create Keys and Attach Policies

```
user1@organization
  runs: orange key create key1 --workspace workspace1 --project project1
    → Platform registers Key (signing done via EgressPASETOKeyPair)
  runs: orange key create key2 --workspace workspace1 --project project1
    → Platform registers second Key
  attaches policy-A  →  key1  (restricts to upstream1 only)
  attaches policy-B  →  key2  (restricts rate limit to 100 req/min)
```

See **07-user-operations.md** for user key and policy management.

## Step 8 — User Sends Request (PASETO Token)

```
user1@organization
  obtains a signed PASETO token from the platform
    (via orange token generate --key key1 ...)
  sends request via key1@workspace1.project1.organization (PASETO)
    Authorization: Paseto v4.public.<payload>.<signature>
        │
        ▼
  Egress evaluates policies (project floor → workspace → key1)
        │ all pass
        ▼
  Credential resolution: key1 has no KeySecret → workspace Secret
  Egress proxies to upstream1 using active secret: secret1:v1
        │
        ▼
  upstream1 responds → user1 receives response
```

## Step 9 — User Sends Request with BYOK (Optional)

```
user1@organization
  obtains a signed PASETO token from the platform
    (via orange token generate --key key3 ...)
  sends request via key3@workspace1.project1.organization (PASETO)
    Authorization: Paseto v4.public.<payload>.<signature>
        │
        ▼
  Egress receives request:
    1. Validates PASETO signature locally against cached EgressPASETOKeyPair
       (no CP round-trip for verification)
    2. Checks token not on revocation list (cached result)
    3. Evaluates policies (project floor → workspace → embedded "pol")
    4. Resolves credentials → workspace Secret
    5. Proxies to upstream
        │
        ▼
  upstream1 responds → user1 receives response
```

## Step 10 — Admin Rotates Secret (Zero-Downtime)

```
admin@organization
  rotates secret1:v1 → secret1:v2
  on egress@workspace1.project1.organization
        │
        ▼
  Egress now resolves secret1:v2 for all new requests to upstream1
  Existing keys: unaffected
  In-flight requests: drain on v1, new requests use v2
```

See **09-secret-rotation.md** for detailed rotation scenarios.

## Step 11 (Optional) — User Binds Personal Upstream Credential (BYOK)

```
user1@organization
  creates a new key for BYOK:
    orange key create key3 --workspace workspace1 --project project1
  binds KeySecret:v1 for upstream1 on key3
    (user1's own upstream1 API key)
        │
        ▼
  Requests via key3 to upstream1 now use user1's personal credential
  Metering: against user1's upstream1 account
  key1 and key2 continue using workspace Secret (unaffected)
```

See **03-credential-and-key-management.md** and **07-user-operations.md** for BYOK details.

## Step 12 (Optional) — Admin Rotates Egress Key Pair

```
admin@organization
  rotates EgressKeyPair for egress@workspace1.project1.organization
        │
        ▼
  Secret service generates new Ed25519 key pair
  New public key registered with CP
  Egress retrieves new private key, begins signing with it
  CP accepts both old and new signatures during 5-min grace window
  After grace window: old signatures rejected
```

See **04-cryptographic-identity.md** for key rotation details.

## Full Sequence Diagram

```
admin          egress/system     user1           upstream1
  │                 │               │                │
  ├─create proj────►│               │                │
  ├─create ws───────►               │                │
  ├─deploy egress──►│               │                │
  │  (auto-gen      │               │                │
  │   identity +    │               │                │
  │   keypair +     │               │                │
  │   2 PASETO pairs)│              │                │
  ├─set secrets─────►               │                │
  ├─assign user1────►               │                │
  │                 │◄──────────────┤ create key1    │
  │                 │◄──────────────┤ attach policy  │
  │                 │◄──────────────┤ create key3    │
  │                 │                │  (new Key)     │
  │                 │◄──────────────┤ send request   │
  │                 │  (PASETO token)│                │
  │                 │   (signed by   │                │
  │                 │    EgressPASETOKeyPair)         │
  │                 ├─eval policies─┤                │
  │                 ├─resolve cred (workspace Secret)│
  │                 ├───proxy req─────────────────────►
  │                 │◄──────────────────────response─┤
  │                 ├───────────────►response        │
  ├─rotate secret──►│               │                │
  │                 │ (v2 now active for workspace)   │
  │                 │◄──────────────┤ bind KeySecret │  (BYOK)
  │                 │◄──────────────┤ send request   │
  │                 ├─eval policies─┤                │
  │                 ├─resolve cred (KeySecret v1)────┤
  │                 ├───proxy req─────────────────────►
  │                 │◄──────────────────────response─┤
  │                 ├───────────────►response        │
  ├─rotate keypair─►│               │                │
  │                 │ (new Ed25519 pair active)       │
```

## Related Documents

- **01-entity-model.md** — Entity definitions and relationships
- **02-policy-system.md** — Policy model and evaluation
- **03-credential-and-key-management.md** — Workspace secrets and BYOK
- **04-cryptographic-identity.md** — Cryptographic artefacts
- **06-admin-operations.md** — Detailed admin scenarios
- **07-user-operations.md** — Detailed user scenarios
- **11-cli-reference.md** — Complete CLI walkthrough examples
