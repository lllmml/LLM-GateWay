# ADR-007: Provider credential encryption

- Status: Accepted
- Date: 2026-08-30

## Context

Provider API credentials differ from virtual gateway keys. Virtual keys need only be authenticated and can remain non-recoverable, while provider credentials must be recovered by the Data Plane to authenticate upstream requests. Plaintext storage, reversible encoding, or returning stored secrets through the Control Plane would expose provider accounts and violate the gateway's security boundary.

The Week 2 control plane needs a mechanism that works in local development, preserves a path to managed key storage, supports future master-key rotation, and remains understandable without adding a cloud dependency.

## Options considered

1. Store provider credentials as plaintext in PostgreSQL.
2. Keep all project credentials in environment variables.
3. Encrypt each credential with an environment-held AES-256-GCM master key and persist a key version.
4. Introduce cloud KMS envelope encryption immediately.

## Decision

Use AES-256-GCM from the Go standard library for the MVP:

- `CREDENTIAL_MASTER_KEY` is standard Base64 that must decode to exactly 32 bytes.
- Each create or rotate operation generates a fresh 12-byte cryptographic nonce.
- PostgreSQL stores ciphertext, nonce, and `key_version`; the current version is `1`.
- AES-GCM Additional Authenticated Data binds every envelope to an immutable credential identity: the AAD domain and format version, credential ID, project ID, provider, and credential key version.
- AAD uses a deterministic binary encoding: fixed-width UUID bytes and integers plus a length-prefixed provider value. It never includes plaintext, master-key material, or another secret.
- The service creates the credential UUID before encryption, encrypts the submitted secret with that identity, and passes both the ID and encrypted envelope to the Store.
- Rotation reads ownership-scoped immutable credential metadata before encryption so it reconstructs the same identity; the PostgreSQL adapter never performs encryption.
- Create, list, rotate, and disable queries include project ownership authorization.
- List and Control Plane responses expose only credential metadata, never ciphertext, nonce, key version, or plaintext.
- Rotation atomically replaces the encrypted envelope and records `rotated_at`.
- The temporary mutable plaintext byte slice is cleared after encryption, and the duplicate decoded master-key slice is cleared after constructing the long-lived cipher.
- `CREDENTIAL_MASTER_KEY`, `SESSION_TOKEN_PEPPER`, and `VIRTUAL_KEY_PEPPER` must contain pairwise-distinct key material; configuration fails at startup when any pair is reused.

No live provider-validation call occurs in this slice. It would transmit the secret externally and needs explicit semantics for timeouts, provider errors, rate limits, and cost.

## Consequences

- A PostgreSQL leak alone does not reveal provider credentials without the master key.
- A random nonce prevents unsafe AES-GCM nonce reuse, and the authentication tag detects ciphertext/AAD changes, but those properties alone do not identify the database row for which an otherwise valid envelope was created. Identity-bound AAD makes copying a complete ciphertext/nonce envelope to another credential, project, or provider fail authentication during decryption.
- Losing the master key makes stored credentials unrecoverable; backup and rotation procedures are required before a long-lived public deployment.
- One environment-held key remains a process-level secret and is less isolated than managed KMS.
- `key_version` permits a future keyring and re-encryption process without changing the credential domain API or schema.
- Disabling a credential is reversible only through a future explicitly designed operation; Week 2 intentionally exposes no re-enable or delete endpoint.

## Verification

- Unit tests cover encrypt/decrypt, fresh nonces, wrong keys, wrong versions, ciphertext/nonce tampering, credential/project/provider context changes, cross-row envelope substitution, deterministic AAD, malformed nonces, and secret-free errors.
- Config tests reject missing, malformed, and non-32-byte master keys and every pairwise key-material reuse combination.
- Handler tests prove session/CSRF enforcement, strict JSON parsing, ownership-derived scope, and secret-free responses.
- PostgreSQL integration tests cover migration up/down, constraints, cross-owner create/list/rotate/disable, atomic rotation, idempotent disable, metadata-only list queries, and ciphertext recovery with the correct master key.
- The repository test, typecheck, lint, build, integration, and race commands pass.

## Revisit when

- The public deployment becomes long-lived or has multiple operators.
- Master-key rotation must occur without downtime.
- A cloud KMS/HSM provides justified access control, audit, and operational benefits.
- Provider credential validation is added with explicit external-call and redaction semantics.
