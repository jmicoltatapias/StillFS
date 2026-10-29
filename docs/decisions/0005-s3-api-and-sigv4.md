# ADR 0005: Limited S3 API v1 and SigV4 identity

- Status: accepted
- Date: 2026-10-29

## Decision

Implement the documented path-style S3 subset under `/`, and move the existing
status UI to `/console/` to avoid a route collision. Preserve separate S3
handlers, object coordination, PostgreSQL repositories, and byte storage.

Use header-based `AWS4-HMAC-SHA256` authentication only, one configured region,
service `s3`, a bounded clock skew, and a signed hexadecimal SHA-256 payload
digest. Verify the digest as object bytes stream. Reject unsigned payloads,
chunked signing, presigned URLs, session tokens, and unsupported scope values.
Use standard-library HMAC/SHA-256 and constant-time signature comparison.

Introduce principals and encrypted credentials. Store secrets using AES-256-GCM
under a required external 32-byte master key; bind ciphertext to its access key
ID with associated data. A local-only `stillfsctl` command creates credentials
and revokes them; no default key or public credential endpoint is provided.
Buckets are owned by a principal. Existing buckets remain unowned after the
nullable ownership migration and are not exposed to S3 until explicitly
assigned.

Serialize same-object mutations inside the one supported StillFS process.
Create/replace publishes bytes before updating metadata. Delete removes
metadata first and deletes bytes only after a reference check confirms they are
unused. External bytes and PostgreSQL are not atomic, and failed or interrupted
operations may leave orphaned content or stale references.

## Consequences and limits

This increment does not provide general S3 compatibility, IAM policy
evaluation, bucket policies, user metadata, multipart, versioning, lifecycle,
or distributed coordination. The local filesystem backend is not supported as
a shared multi-process root. The credential master key has no rotation
workflow. TLS must be provided by a trusted front end. A read-only storage
audit and one pinned AWS SDK for Go v2 compatibility test are follow-up
decisions in ADR 0006; they do not add automatic recovery or general S3
compatibility.
