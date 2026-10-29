# PostgreSQL migrations

StillFS embeds numbered SQL files and applies each unapplied version in its own
transaction. Applied files must be treated as immutable; make later changes in
a new version. Keep object payload bytes out of PostgreSQL. See
`docs/architecture/metadata-layer.md` for schema and failure semantics.

`0001_metadata.sql` creates buckets and object metadata. `0002_identity_ownership.sql`
adds principals, encrypted S3 credential records, and nullable bucket ownership.
Legacy buckets remain unowned and are not visible through the S3 API until an
operator explicitly assigns a principal. Credential secrets are AES-256-GCM
ciphertext and require the external `STILLFS_CREDENTIAL_ENCRYPTION_KEY` to be
retained.

`0003_authorization_audit.sql` adds fixed per-credential S3 data permissions
and an application audit table. Existing and new credentials receive no grants
automatically. Credential creation, revocation, and permission replacement
write audit events in their PostgreSQL transaction. The audit table is
operator-editable and has no configured retention policy.

`0004_credential_master_key_state.sql` adds the singleton HMAC verifier and
generation for the active credential-encryption key. It does not rewrite
existing ciphertext. The first server startup or managed credential creation
initializes it with the configured key. Rotation updates the verifier and all
credential ciphertext/nonces in one transaction; see
`docs/api/authorization-admin.md` for shutdown, backup, and recovery steps.
