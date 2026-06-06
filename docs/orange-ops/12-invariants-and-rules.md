# Summary: Invariants and Rules

Complete reference for all system invariants, cryptographic constraints, and operational rules.

## Policy Invariant

```
key_policy ⊆ workspace_policy ⊆ project_floor
```

- Enforced at write time for user-set key policies
- Workspace tightening uses lazy invalidation (eval-time clamp)
- No stored policy is ever rewritten; effective policy is computed at request time

See **02-policy-system.md** for details.

## Cryptographic Invariants

```
1. EgressIdentity binds exactly one egress to one workspace
   (X.509 certificate, org-CA signed, 90-day validity)

2. EgressKeyPair private key NEVER leaves secret service
   (opaque reference only in CP; retrieved at runtime)

3. PASETO private keys are user-local NEVER sent to CP
   (user generates Ed25519 keypair, provides public key only)

4. CPValidationKey is org-wide; rotates to all egress
   (grace window for seamless transition)

5. Narrowing invariant applies to PASETO embedded "pol"
   (floor policies always override embedded claims)
```

See **04-cryptographic-identity.md** for details.

## Operational Rules

1. **Deny always wins across all levels**
   - A deny at any policy level blocks the request

2. **Floor policies: admin-write only, never overrideable**
   - No lower scope can widen a floor policy

3. **Users can only restrict, never widen**
   - Key policies must remain ⊆ workspace policies

4. **Key.workspace_id is immutable after creation**
   - A key cannot be moved to another workspace

5. **Keys never hold workspace Secret values directly**
   - Secrets are resolved at dispatch time

6. **Workspace Secret rotation is zero-downtime**
   - In-flight requests drain naturally; new requests use new version

7. **All workspace members have equal rights**
   - No role distinction within a workspace

8. **Removing a user cascades to invalidate keys + KeySecrets**
   - Purges keys, KeySecrets, and token records

9. **Describe scoping: admins see all, users see own resources**
   - Users cannot describe other users' keys or secret values

10. **Description fields are free-text, set by owner/admin**
    - No validation; surface in audit logs

11. **Users may bind personal upstream credentials (KeySecrets) to their own keys (BYOK)**
    - One KeySecret per upstream per key
    - User-managed; independent of workspace Secret lifecycle

12. **Credential resolution at dispatch**
    ```
    KeySecret (active) → workspace Secret (active) → DENIED
    ```

13. **BYOK affects which credential is used and which account is metered**
    - Policy evaluation is identical either way

14. **KeySecret values are write-only; never returned by API**
    - Only existence, upstream target, and active version are surfaced

15. **KeySecret rotation is zero-downtime; user-managed; independent of workspace Secret lifecycle**
    - User controls rotation timing

16. **A KeySecret may be configured for an upstream with no workspace Secret**
    - That upstream is then accessible via that key only
    - Other keys without a KeySecret cannot reach it unless admin adds workspace Secret

17. **All tokens are PASETO v4.public; verified statelessly**
    - Ed25519 signature verified at egress

18. **PASETO v4.public tokens are verified statelessly at egress**
    - Ed25519 sig verify + revocation list cache check
    - Zero CP round-trips for verification

19. **EgressKeyPair rotation has a grace window (default 5min)**
    - Old signatures accepted during grace; rejected after

20. **CPValidationKey rotation has a grace window (default 24h)**
    - All egress instances updated; old key deactivated after grace

21. **Emergency revocation bypasses grace window (immediate)**
    - Compromised keys deactivated immediately

22. **EgressIdentity auto-renews before expiry if egress healthy**
    - Pushed via secure channel; hot-reloaded

23. **PASETO "wsk" claim prevents cross-workspace token replay**
    - Token workspace must match egress workspace

24. **PASETO "exp" claim checked before policy evaluation**
    - Expired tokens rejected at Step 0

## Related Topics

- **02-policy-system.md** — Policy model
- **04-cryptographic-identity.md** — Cryptographic artefacts
- **05-onboarding-workflow.md** — Onboarding walkthrough
