# ADR 0004: Local object bytes and metadata coordination

- Status: accepted
- Date: 2026-10-29

## Decision

Implement `storage.ObjectStore` with a filesystem backend that uses generated
256-bit opaque references, sharded paths, streaming writes, SHA-256 checksums,
exclusive temporary files, file sync, and atomic no-overwrite hard-link
publication. Payload bytes remain outside PostgreSQL. The configured root is
private (`0700` directories, `0600` files) and persistent; one root is owned by
one StillFS process.

For create, publish bytes before inserting metadata. On metadata failure, query
all object metadata for the storage reference and delete only if the query
succeeds and proves it unreferenced. Retain and log the reference when its
state is uncertain. Replacement publishes new bytes then updates metadata and
retains the former bytes for later reconciliation. ETag remains opaque and
independent of the SHA-256 checksum.

## Guarantees and limits

Hard-link publication gives atomic visibility of a complete file on supported
same-filesystem deployments and does not overwrite a prior reference. File
sync and directory sync request durability for file data and directory name
changes, but do not establish a transaction with PostgreSQL or guarantee
hardware-level persistence. A crash between filesystem publication and the
metadata commit can leave an orphan; ambiguous failures can leave stale bytes.
No reconciler or background cleanup process is included. Startup cleanup
removes only reserved abandoned temporary names and is process-local.

There is no proof of atomic consistency across PostgreSQL and object storage.
Before the S3 API task, StillFS still needs a reviewed orphan/stale-reference
reconciliation procedure and operational policy. Multi-process use of one
local root, crash injection, and filesystem-specific durability need further
design and testing.
