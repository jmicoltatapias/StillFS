# StillFS

StillFS is an object-storage server under development. It has a Go HTTP
process, PostgreSQL metadata persistence and migrations, a local streaming
object-byte backend, a limited path-style S3 API v1, and a small React status
UI. This is not general S3 compatibility.

## Requirements

- Go 1.18 or newer
- Node.js 24 and npm (for the frontend)
- Docker Compose v2 (for the local integrated setup)

The Go dependency `github.com/lib/pq` includes MIT-licensed code and BSD-3-Clause
SCRAM portions according to its upstream license file. The frontend's direct
packages use MIT or Apache-2.0 licenses based on their package metadata and
upstream license files. The full transitive dependency review is not complete;
see [license notes](docs/licensing.md).

## Local setup

1. Copy `.env.example` to `.env`, set a persistent credential-encryption key,
   and change the development database password before using a shared machine.
   Generate a key without printing it to the terminal with
   `umask 077; printf 'STILLFS_CREDENTIAL_ENCRYPTION_KEY=%s\n' "$(openssl rand -base64 32)" >> .env; chmod 600 .env`.
2. Start PostgreSQL and StillFS with `docker compose up --build`.
3. Open `http://localhost:9000/console/`. The status page checks
   `/health/ready`, which
   reports whether StillFS can reach PostgreSQL. `/health/live` checks only
   that the HTTP process is responding.
4. Create an S3 credential locally with explicit permissions, for example
   `docker compose exec stillfs /app/stillfsctl credential create --permission s3:ListBuckets --permission s3:CreateBucket --permission s3:PutObject --permission s3:GetObject --permission s3:DeleteObject`.
   Save the secret shown once. There is no default access key or HTTP credential API.

For frontend-only development, run `cd web && npm ci && npm run dev`; Vite
proxies health requests to `localhost:9000`. Run the backend separately with
`go run ./cmd/stillfs` after starting PostgreSQL and setting the database and
credential encryption key environment variables below. The frontend is at
`/console/`; `/` is reserved for the authenticated S3 service listing.

## Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `STILLFS_HTTP_ADDR` | `:9000` | HTTP listen address |
| `STILLFS_STORAGE_ROOT` | `./data/objects` | Persistent root for local object bytes |
| `STILLFS_DB_HOST` | `localhost` | PostgreSQL hostname |
| `STILLFS_DB_PORT` | `5432` | PostgreSQL port |
| `STILLFS_DB_NAME` | `stillfs` | Database name |
| `STILLFS_DB_USER` | `stillfs` | Database user |
| `STILLFS_DB_PASSWORD` | empty | Database password; never logged |
| `STILLFS_DB_SSLMODE` | `verify-full` | `disable`, `require`, `verify-ca`, or `verify-full` |
| `STILLFS_DB_SSLROOTCERT` | driver default | PostgreSQL CA certificate file |
| `STILLFS_DB_MAX_OPEN_CONNS` | `10` | Maximum open pool connections |
| `STILLFS_DB_MAX_IDLE_CONNS` | `5` | Maximum idle pool connections |
| `STILLFS_CREDENTIAL_ENCRYPTION_KEY` | required unless key file is set | Legacy Base64 encoding of exactly 32 random bytes |
| `STILLFS_CREDENTIAL_ENCRYPTION_KEY_FILE` | optional alternative | Path to protected raw 32-byte key file (mode `0600`); configure only one key source |
| `STILLFS_S3_REGION` | `us-east-1` | Only accepted SigV4 region |
| `STILLFS_S3_CLOCK_SKEW` | `5m` | Maximum absolute request timestamp difference |
| `STILLFS_MAX_OBJECT_BYTES` | `5368709120` | Maximum single PUT size in bytes |
| `STILLFS_MAX_HEADER_BYTES` | `1048576` | HTTP request header limit |
| `STILLFS_MAX_METADATA_BYTES` | `8192` | Maximum supported object/list metadata bytes |

The HTTP server applies a 15 second header-read timeout and the configured
header-size limit. It has no whole-request or response timeout because object
transfers stream; body size is bounded, but slow-body idle timeouts still need
an operational policy. Shutdown timeout, database pool lifetime, and connect
timeout have validated defaults. Development Compose explicitly disables
PostgreSQL TLS on its private local network. The standalone backend default
verifies the PostgreSQL server certificate and hostname. Put the S3 API behind
TLS in deployments; this server does not terminate TLS itself.

The local object store creates its root and shard directories with mode `0700`
and files with mode `0600`. Keep the root on a persistent filesystem. Compose
mounts the named `stillfs-objects` volume at `/app/data/objects`; deleting the
volume destroys all local object bytes. Do not share one storage root between
multiple StillFS processes. See [local storage guarantees and recovery limits](docs/architecture/local-storage.md).

## S3 API and credentials

The implemented path-style subset, SigV4 mode, errors, limits, and known
compatibility gaps are listed in [docs/api/s3-v1.md](docs/api/s3-v1.md).
Credential secrets are encrypted using AES-256-GCM under
`STILLFS_CREDENTIAL_ENCRYPTION_KEY`; back up this key and keep it stable or
existing credentials cannot be decrypted. New keys have no permissions unless
explicit `--permission` flags are provided. Change grants with
`stillfsctl credential permissions set ACCESS_KEY_ID`; revoke a key with
`stillfsctl credential revoke ACCESS_KEY_ID`. Revocation disables that key but
leaves its principal and buckets intact. See
[authorization and local administration](docs/api/authorization-admin.md) for
the fixed permission list, audit access, master-key rotation command, backup,
and recovery procedure.

The first credential command creates the principal. Use the generated Access
Key ID and secret with a path-style SigV4 client in region `us-east-1`. The AWS
CLI example has not been run in this environment; configure the CLI for
path-style requests and signed payloads before use:

```ini
[default]
s3 =
    addressing_style = path
    payload_signing_enabled = true
```

```sh
AWS_ACCESS_KEY_ID='the-access-key-id' AWS_SECRET_ACCESS_KEY='the-secret' \
  aws --endpoint-url http://localhost:9000 --region us-east-1 s3api create-bucket --bucket stillfs-example
```

## Build and tests

From the repository root:

```sh
make frontend-install
make check
make build
```

`make build` builds both `stillfs` and the local-only `stillfsctl` credential
tool. The frontend status page remains observational; it does not administer
buckets or credentials.

`go test ./...` runs unit tests and runs the PostgreSQL integration test when
`STILLFS_TEST_DATABASE_URL` is set. Run it explicitly with:

```sh
STILLFS_TEST_DATABASE_URL='postgres://user:password@localhost:5432/stillfs_test?sslmode=verify-full&sslrootcert=/path/to/ca.crt' make test-integration
```

The tests require a disposable database role that can create and drop schemas.
They use isolated temporary schemas and remove them at the end. Coverage
includes migration from an empty schema, repeated/concurrent migration
behavior, transaction rollback, repositories, constraints, pagination, races,
coordination of local bytes with PostgreSQL metadata, and signed S3 operations.
CI runs these tests against a temporary PostgreSQL service.

## Migrations

Numbered SQL migrations live in `internal/postgres/migrations/` and are embedded
in the server binary. On startup, StillFS creates
`stillfs_schema_migrations`, applies each pending migration in its own
transaction, and records its version. A transaction-scoped advisory lock
serializes concurrent migration runners. If a migration fails, its DDL and
version record roll back together; earlier successful versions remain applied
and startup stops. The initial schema migration creates the bucket/object
metadata tables. Migrations are not automatically reversed; treat applied SQL
files as immutable and add a new version for changes.

## Metadata model

The first schema has `buckets` and `objects`. IDs are PostgreSQL-generated
`BIGINT` values. Bucket names are unique and object keys are unique within a
bucket using bytewise `C` collation. Objects store only an opaque `storage_ref`
to content held elsewhere, its size and optional descriptive metadata. ETag is
opaque and independent from the optional `(checksum_algorithm, checksum_value)`
pair. PostgreSQL checks nonnegative size, nonempty references, checksum pairing,
and the bucket foreign key. A bucket cannot be deleted while object metadata
references it.

Listings use keyset cursors in bucket-name/object-key order with a default page
size of 100 and maximum of 1000. A cursor stabilizes ordering against offset
shifts, but pages are separate read snapshots: later inserts after the cursor
may appear, earlier inserts will not, and deleted rows disappear. Metadata
transactions do not include external object bytes. The object coordinator
publishes bytes before metadata and compensates failed metadata operations only
after confirming the reference is unused. Replacement keeps prior content.
There is no crash reconciler or cross-store atomic consistency; see the local
storage documentation and [ADR 0004](docs/decisions/0004-local-object-storage.md).

## License

The intended project license is GNU AGPL-3.0; see `LICENSE`. A dependency and
distribution compliance review has not been completed.
