# ADR 0006: Read-only storage audit and S3 client compatibility test

- Status: accepted
- Date: 2026-10-29

## Context

PostgreSQL and the local filesystem do not share a transaction. A crash or
ambiguous database result can leave published files without metadata, metadata
references without files, or reserved temporary files. The single-process
object locks do not coordinate a second process sharing the root. The current
S3 subset also had protocol-only tests but no test using an external S3 SDK.

## Decision

Add `stillfsctl storage audit` as a read-only diagnostic. It compares paginated,
distinct PostgreSQL storage references with generated local filenames, reports
missing and unreferenced entries, lists reserved temporary files and unsafe
paths, and never removes or repairs anything. The diagnostic is an inventory,
not a common snapshot, and candidate findings do not authorize deletion. Run it
while object writes are quiesced. Memory use grows with the number of
references.

Keep one process per local root as a documented operational restriction. Do
not add automatic blob deletion until an inter-process coordination protocol
and safe quiescence proof exist. Existing startup cleanup remains limited to
reserved temporary names under that restriction.

Add a separate test module under `tests/s3compat` using AWS SDK for Go v2. Keep
its Go version and dependencies isolated from the server's Go 1.18 module.
Tests use a disposable PostgreSQL schema, local object storage, and ephemeral
credentials. CI runs this client test with the repository's standard Go 1.27
toolchain. The test establishes compatibility only for the exercised API
subset and this SDK version.

## Consequences and limits

Audit output can contain false mismatches when writes occur during the scan; it
cannot recover missing object bytes and does not validate payload checksums.
The report and manual remediation require an operator. AWS CLI, other SDK
versions, multipart, IAM policies, and non-documented S3 operations remain
unverified or unsupported. No compatibility or distributed-storage guarantee
is implied beyond the tests and documented single-process behavior.
