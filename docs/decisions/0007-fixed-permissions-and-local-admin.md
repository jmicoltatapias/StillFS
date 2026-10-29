# ADR 0007: Fixed per-key permissions and local administration boundary

- Status: accepted
- Date: 2026-10-29

## Context

SigV4 establishes which credential signed an S3 request, while ownership
isolates buckets by principal. Previously every active key of a principal
inherited every operation, and the project had no auditable permission model.
No remote administrator identity exists to safely protect an administrative
HTTP API.

## Decision

Separate SigV4 authentication from backend authorization. Store an explicit
fixed set of seven S3 data-plane capabilities per access key and deny by
default. A permission check runs before bucket/object metadata lookup; existing
principal ownership checks remain additional controls. Credential create,
revoke, and permission replacement are recorded in the same PostgreSQL
transaction as the state change. The database session role identifies the
local operator context. Local audit reads are also recorded.

Keep credential/permission management and audit querying in `stillfsctl`,
under the OS/PostgreSQL trust boundary. Do not expose an administrative HTTP
route until there is an independently authenticated administrator model and
authorization requirements for each operation.

## Consequences and limits

This is not AWS IAM. There are no policies, roles, resource conditions,
delegation, credential self-management, audit-read grants, or global admin
permissions. Audit records are database rows, can be changed by a database
owner, and have no retention policy. Existing credentials receive no grants
on migration and must be explicitly configured. Master-key rotation is
specified separately in [ADR 0008](0008-credential-master-key-rotation.md).
