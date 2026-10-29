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
  metadata only. Failed or reordered object-store operations can leave orphaned
  bytes or stale references. No cleanup protocol is defined here.
- Object versioning, lifecycle, IAM, bucket policies, replication, and S3
  compatibility rules remain outside this schema.
