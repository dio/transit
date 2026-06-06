# Orange Operations Documentation

Complete, sliced reference for the egress proxy onboarding model — organized into self-contained task documents.

## Document Structure

This documentation is organized into 12 self-contained task documents, each covering a specific aspect of the system:

### Foundational Concepts
- **01-entity-model.md** — Core entities, database schema, hierarchy, containment, key scoping
  - Entity definitions and relationships
  - Database schema and naming conventions
  - Workspace and key scoping rules

- **02-policy-system.md** — Policy model, scopes, types, narrowing invariant, evaluation
  - Floor vs. flexible policies
  - Narrowing invariant (key ⊆ workspace ⊆ project)
  - Lazy invalidation and request-time evaluation
  - Policy conflict resolution

### Credentials and Secrets
- **03-credential-and-key-management.md** — Workspace secrets, BYOK, credential resolution
  - Workspace secrets and versioning
  - User-bound credentials (KeySecrets)
  - Credential resolution at dispatch
  - Zero-downtime rotation

- **04-cryptographic-identity.md** — Egress identity, key pairs, PASETO, certificates
  - Five cryptographic artefacts
  - EgressIdentity (X.509), EgressKeyPair, CPValidationKey
  - PASETO tokens and stateless verification
  - Certificate and key lifecycle
  - Rotation and revocation procedures

### Workflows and Operations
- **05-onboarding-workflow.md** — Step-by-step onboarding from project creation to first request
  - Complete end-to-end walkthrough
  - Sequence diagrams
  - 12-step onboarding process

- **06-admin-operations.md** — All administrative tasks (18 scenarios)
  - Project and workspace management
  - Egress deployment and management
  - Secret management
  - User assignment and removal
  - Policy management (floor and flexible)
  - Key rotation and revocation

- **07-user-operations.md** — All user tasks (16 scenarios + BYOK)
  - Key creation and management
  - Policy attachment (narrowing)
  - Sending requests (allowed and denied)
  - Multi-user scenarios
  - BYOK credential binding and rotation
  - Describing resources

- **08-policy-conflicts.md** — Policy conflict scenarios and edge cases (14+ scenarios)
  - Policy conflicts and their resolution
  - Write-time validation
  - Lazy invalidation examples
  - PASETO embedded policy interactions
  - Edge cases: missing credentials, workspace deletion, user removal
  - PASETO-specific edge cases

- **09-secret-rotation.md** — Workspace and KeySecret rotation scenarios
  - Standard rotation (no in-flight requests)
  - Rotation with in-flight requests
  - Impact on keys and BYOK
  - Emergency revocation procedures

- **10-cryptographic-scenarios.md** — Cryptographic identity operations (6 scenarios)
  - Egress boot with identity certificate
  - Key pair rotation and emergency revocation
  - Certificate expiry and renewal
  - CPValidationKey rotation
  - Multiple PASETO keys

### Reference
- **11-cli-reference.md** — Complete CLI documentation
  - Installation and authentication
  - Non-interactive and REPL modes
  - Admin commands (projects, workspaces, egress, identity, secrets, policies)
  - User commands (keys, policies, BYOK)
  - Describe commands
  - Output formats (table, JSON, YAML)
  - REPL walkthroughs

- **12-invariants-and-rules.md** — Summary of all system invariants and rules
  - Policy invariant
  - Cryptographic invariants
  - 24 operational rules
  - Key references to other documents

## Reading Order

**For Quick Understanding:**
1. 01-entity-model.md
2. 02-policy-system.md
3. 03-credential-and-key-management.md
4. 05-onboarding-workflow.md
5. 12-invariants-and-rules.md

**For Implementing Admin Features:**
1. 01-entity-model.md
2. 02-policy-system.md
3. 03-credential-and-key-management.md
4. 04-cryptographic-identity.md
5. 06-admin-operations.md
6. 11-cli-reference.md

**For Implementing User Features:**
1. 01-entity-model.md
2. 02-policy-system.md
3. 03-credential-and-key-management.md
4. 04-cryptographic-identity.md
5. 07-user-operations.md
6. 11-cli-reference.md

**For Understanding Edge Cases:**
1. 08-policy-conflicts.md
2. 09-secret-rotation.md
3. 10-cryptographic-scenarios.md
4. 12-invariants-and-rules.md

**For Complete Picture:**
Read all documents in numerical order.

## Cross-References

Each document includes "Related Topics" sections pointing to relevant documents. For example:

- **02-policy-system.md** references **03-credential-and-key-management.md** for how policies interact with credential resolution
- **07-user-operations.md** references **04-cryptographic-identity.md** for PASETO details
- **06-admin-operations.md** references **04-cryptographic-identity.md** for key rotation specifics

## Key Concepts

### Policy Narrowing Invariant
```
key_policy ⊆ workspace_policy ⊆ project_floor
```
Enforced at write time. Lazy invalidation when workspace is tightened.
See **02-policy-system.md** and **12-invariants-and-rules.md**.

### Credential Resolution
```
Request → [Policy evaluation] → Credential resolution
   ↓
   [KeySecret active?] → YES → use KeySecret
   ↓
   [Workspace Secret active?] → YES → use workspace Secret
   ↓
   NO → DENIED
```
See **03-credential-and-key-management.md**.

### PASETO Verification (Stateless)
```
Request → [Extract PASETO] → [Local Ed25519 verify] → [Revocation check] → [Policy eval]
```
Zero CP round-trips for verification. See **04-cryptographic-identity.md**.

## Document Statistics

- **12 documents** covering all aspects
- **50+ detailed scenarios** from admin and user operations
- **14+ edge case scenarios** with complete resolution
- **24 system rules** summarized in one place
- **Complete CLI reference** with examples and walkthroughs
- **Cross-referenced** throughout for easy navigation

## Scope

These documents cover:
- ✅ Entity model and relationships
- ✅ Policy system (floor, flexible, narrowing)
- ✅ Workspace secrets and BYOK
- ✅ Cryptographic identity and attestation
- ✅ PASETO token verification
- ✅ Request dispatch and credential resolution
- ✅ Key lifecycle and rotation
- ✅ Onboarding and operational workflows
- ✅ Admin and user scenarios
- ✅ Edge cases and error handling
- ✅ Complete CLI reference

## Original Document

This sliced documentation replaces the original monolithic `orange-ops.md` document. The original remains available as reference.

---

**Document version:** 2.0 — includes cryptographic identity & attestation model
**Last updated:** 2026-06-06
