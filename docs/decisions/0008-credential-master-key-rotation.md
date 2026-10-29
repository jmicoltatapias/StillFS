# ADR 0008: Transactional credential master-key rotation

- Status: accepted
- Date: 2026-10-29

## Context

Credential secrets use AES-256-GCM under a configured master key. Replacing
the key directly would make existing credentials unreadable, and without a
database verifier a stale StillFS process could create a credential encrypted
with an old key after rotation.

## Decision

Preserve the existing ciphertext/nonce format. Add a singleton HMAC verifier
and generation row for the active key. Validate it during server startup and
read it together with each credential ciphertext. Require a key verifier when
creating credentials after initialization. Serialize credential creation,
permission changes, revocation, verification, and rotation with one
PostgreSQL transaction advisory lock.

The local `stillfsctl credential rotate-master-key` command reads old/new raw
32-byte keys from protected files. It verifies old decryption and new
decryption for every row before commit. All ciphertext/nonces, the active-key
verifier, generation, and successful audit event change in one transaction;
there is no startup rotation. Attempts and failures are separately audited
without secret material. `STILLFS_CREDENTIAL_ENCRYPTION_KEY_FILE` supports
reading the active raw key file at server startup; the legacy Base64
environment variable remains supported.

## Consequences and limits

Operators must stop StillFS and other administrative processes during
rotation, change the configured key source, verify it, then restart. The
advisory lock coordinates current StillFS code, not older binaries, direct SQL,
or other systems sharing the database. A failed transaction leaves old rows
and verifier intact. A lost key without a matching backup makes existing
credential secrets unrecoverable; no reset command exists. PostgreSQL commit
errors can be ambiguous, so recovery verifies each candidate key against all
stored credentials. Audit data remains operator-editable and failure events
cannot be persisted while PostgreSQL is unavailable.
