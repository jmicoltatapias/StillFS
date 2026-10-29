# Local object storage (initial backend)

StillFS stores payload bytes outside PostgreSQL through `storage.ObjectStore`.
The first implementation is `internal/storage/local`; metadata keeps only the
opaque `storage_ref`, byte length, and SHA-256 checksum fields.

## Reference and file layout

The backend generates 256 random bits for every write and returns a
`local:v1:<lowercase-hex>` reference. Bucket names and object keys are never
used as filesystem paths. The reference is a locator, not an authorization
token. SHA-256 is an integrity checksum, not an authentication mechanism, and
the metadata ETag remains independent and opaque.

The root is configured by `STILLFS_STORAGE_ROOT` (default
`./data/objects`). It must be persistent and writable by the StillFS process.
Initialization creates/restricts the root and shard directories to `0700` and
files to `0600`. Development Compose mounts a named volume. A root must not be
shared by multiple StillFS processes: temporary-file cleanup and coordination
are process-local, and shared-root multi-process operation has not been
designed or tested.

## Write and read behavior

Writes create an exclusive temporary file inside a shard, stream input through
a bounded copy while counting bytes and computing SHA-256, sync and close the
file, then publish it with a same-filesystem hard link. The hard link publishes
the complete inode atomically and refuses to overwrite an existing path. The
temporary name is removed and the shard directory is synced. A returned
reference is only successful after these steps complete.

This provides atomic visibility for one published file on filesystems that
support hard links with these semantics. File sync requests data durability;
directory sync requests persistence of the name changes where supported. It
does not by itself guarantee all filesystem, device, or PostgreSQL state is
durable, and it is not a transaction spanning PostgreSQL. Filesystem behavior
may vary by platform; the implementation targets the Linux development and
deployment environment.

`Stat` reads the object to recompute its size and SHA-256; it does not trust
metadata or cache a digest. `Open` returns a streaming reader. Explicit
`Delete` accepts only a validated generated reference and removes bytes; the
metadata coordinator calls it only after confirming no metadata row refers to
the reference.

## PostgreSQL coordination and recovery limits

Create order is: publish complete bytes, then insert metadata. If metadata
insert fails, the coordinator queries PostgreSQL for references and deletes
the content only when the query succeeds and confirms no row exists. If that
check fails, or reports a reference, it retains the bytes and emits a
structured error containing the opaque reference for diagnosis. Replacement
order is: publish new bytes, update the existing metadata row, and retain old
bytes. A failed update uses the same safe compensation check for the new
reference. PostgreSQL and the filesystem do not share an atomic transaction.

A process crash after publishing but before metadata commit can leave an
orphan. A crash or ambiguous DB outcome can also leave stale references or
bytes. There is no reconciler yet; no automatic object deletion is performed
at startup. Recovery requires an operator to compare references and remove
verified orphans using a future maintenance procedure. Never delete content
based only on age or a failed metadata query.

At initialization the backend removes only files named
`.stillfs-tmp-*.part` in valid, non-symlink shard directories. Within one
process, active temporary paths are protected by a mutex. A crash-abandoned
temporary is safe to remove because that reserved name is never used for a
published object. Cleanup is not safe against another process sharing the
root, which is why shared roots are unsupported.

The implementation does not inject OS-level write, close, fsync, or directory
sync failures in tests. Reader failure is deterministic and covered. Those
hardware/filesystem failure modes and crash recovery need deployment-level
testing before stronger durability claims.
