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

The object coordinator serializes `PutOrReplace`, `Replace`, and `Delete` by
object key using process-local striped locks. Delete removes the metadata row,
then checks whether any row still refers to the storage reference, and removes
bytes only when the check succeeds and reports no references. A failed check
leaves content in place and returns an error. These locks depend on the local
backend's one-process root restriction and do not coordinate another process
writing directly to PostgreSQL.

A process crash after publishing but before metadata commit can leave an
orphan. A crash or ambiguous DB outcome can also leave stale references or
bytes. A replacement crash after the metadata update leaves the new reference
valid and the former bytes orphaned; a delete crash after metadata removal can
leave bytes without a reference. If the metadata commit result is uncertain,
the coordinator retains content unless PostgreSQL confirms that it is
unreferenced. A metadata reference whose file is missing cannot be reconstructed
by this backend; restore its bytes from a backup or recover it through a future
operator procedure.

There is no automatic object-file reconciler. `stillfsctl storage audit` is a
read-only comparison of distinct PostgreSQL references and local published
filenames. It reports unreferenced-file candidates, missing-file references,
reserved temporary files, and unsafe entries. It does not hash payloads, repair
metadata, or delete anything. Its inventories are not a shared snapshot; run it
while object writes/deletes are quiesced for the most useful report. Even then,
an unreferenced candidate is not authorization for automatic deletion. The
diagnostic currently holds reference names in memory, so its memory use grows
with object count.

The audit command does not call `local.New` and therefore does not trigger
startup temporary cleanup. It can inspect temporary files before StillFS is
restarted. StillFS startup removes only reserved `.stillfs-tmp-*.part` files;
this is safe only under the documented single-process-per-root restriction.
Never delete content based only on age, an audit candidate, or a failed
metadata query. A second StillFS process or another writer sharing the root
invalidates the process-local lock and cleanup assumptions. That restriction is
documented but not enforced with an operating-system lock yet.

At initialization the backend removes only files named
`.stillfs-tmp-*.part` in valid, non-symlink shard directories. Within one
process, active temporary paths are protected by a mutex. A crash-abandoned
temporary is safe to remove because that reserved name is never used for a
published object. Cleanup is not safe against another process sharing the
root, which is why shared roots are unsupported.

On Linux, `Open` opens the file before releasing the object lock. Replacing an
object retains its old file, and unlinking a file during `Delete` does not
invalidate an already-open file descriptor. The reader therefore continues
reading the version it opened. This behavior is covered by local tests on
Linux; other filesystem/platform semantics are not claimed.

The implementation does not inject OS-level write, close, fsync, or directory
sync failures in tests. Reader failure is deterministic and covered. Those
hardware/filesystem failure modes and crash recovery need deployment-level
testing before stronger durability claims.
