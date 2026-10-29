# Initial PostgreSQL metadata schema

## Schema choices

- `buckets.id` and `objects.id` are stable PostgreSQL `BIGINT IDENTITY`
  identifiers. They are internal and are not S3 version IDs.
- Bucket names are unique under PostgreSQL `C` collation. This is exact,
  case-sensitive bytewise uniqueness; this layer does not implement S3 bucket
  naming validation or normalization.
- Object keys are `TEXT COLLATE "C"` and unique per bucket. Empty keys are not
  rejected by this metadata layer.
- `objects.storage_ref` is a nonempty opaque locator for the separate object
  byte store. There is no `BYTEA` or other payload column.
- `size_bytes` is nonnegative. `content_type` is nullable when unknown.
- `etag` is nullable and opaque; it is not defined as MD5. Checksum algorithm
  and value are separate nullable columns and must be both present or both
  absent. The schema does not prescribe a checksum encoding.
- Creation/update timestamps are `TIMESTAMPTZ` values assigned by PostgreSQL.
  Repository updates set `updated_at`; no database trigger is installed.
- An object has a restrictive foreign key to its bucket. Bucket deletion locks
  its row, checks for objects and deletes in one transaction. A database FK is
  the final guard against a concurrent object insert.
- Unique indexes enforce bucket name and `(bucket_id, object_key)` uniqueness.
  A second `(bucket_id, object_key text_pattern_ops)` index supports prefix
  listing.

## Migration behavior

Migration `0001_metadata.sql` creates the two metadata tables and index. The
tracking table is bootstrap infrastructure and is created before numbered
migrations run. A PostgreSQL transaction-scoped advisory lock serializes the
applied-version check and SQL execution. Each migration's DDL and version row
commit together. If one migration fails, that migration is rolled back, earlier
versions stay committed, and startup returns an error. Migration files are not
reversed or checksum-validated; applied files must be treated as immutable.

## Repository behavior and limits

- Duplicate bucket/object records return `storage.ErrAlreadyExists`; missing
  buckets/objects have distinct sentinels; FK conflicts are mapped to
  `storage.ErrConflict` (bucket deletion maps them to `ErrBucketNotEmpty`).
- Bucket pagination is ordered by name. Object pagination is ordered by key
  inside a bucket and filters prefixes with escaped SQL `LIKE` patterns. Both
  use keyset cursors instead of offsets.
- A single list request sees a repeatable-read snapshot. Multiple page calls do
  not share a snapshot: inserts beyond the cursor can appear on later calls,
  earlier inserts cannot, and deleted rows disappear.
- PostgreSQL cannot atomically commit an external object-store operation.
  `Put`/`Update` only persist or replace the opaque reference; `Delete` removes
  metadata only. The object coordinator supplies best-effort single-process
  compensation, but failed or reordered operations can still leave orphaned
  bytes or stale references. `ListStorageReferences` supports a read-only
  diagnostic; it is paginated and does not share a snapshot across pages.
  Diagnostic mismatches are not safe-delete decisions, and no automatic blob
  reconciler exists.
- Object versioning, lifecycle, IAM, bucket policies, replication, and S3
  compatibility rules remain outside this schema.

## Task 004 identity extension

Migration `0002_identity_ownership.sql` adds `principals`, encrypted
`credentials`, and nullable `buckets.owner_id`. Nullable ownership preserves
legacy rows without assigning them to a new principal; S3 queries filter by
owner and do not expose unowned buckets. New bucket creation requires a
principal ID. Credential ciphertext uses AES-256-GCM in `BYTEA`, with its
nonce stored separately and a required external master key. This is credential
material, not object payload data or an IAM policy model.

Migration `0003_authorization_audit.sql` adds a fixed permission set per access
key and a PostgreSQL audit table. No permission rows are backfilled, so
existing credentials are denied S3 operations until an operator grants
permissions with `stillfsctl`. Audit events for credential creation,
revocation, and permission replacement share a transaction with the change.
PostgreSQL operators can still alter audit rows; no immutable storage or
retention policy is configured. See [authorization and local administration](../api/authorization-admin.md).

Migration `0004_credential_master_key_state.sql` stores one HMAC verifier and
generation counter for the active credential-encryption key; encrypted
credential columns keep their existing AES-256-GCM format. Server startup
checks the configured key, and credential lookup reads the verifier and
ciphertext together. Rotation locks credential-management writes and updates
all encrypted secrets, nonce values, the verifier, and the success audit event
in one transaction. See the operational recovery procedure in
`docs/api/authorization-admin.md`.
