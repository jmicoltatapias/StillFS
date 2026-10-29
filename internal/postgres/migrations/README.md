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
