# StillFS

StillFS is an object-storage server under development. It has a Go HTTP
process, PostgreSQL metadata persistence and migrations, a local streaming
object-byte backend, and a small React status UI. S3 operations are not
implemented.

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

1. Copy `.env.example` to `.env` and change the development database password
   before using a shared machine.
2. Start PostgreSQL and StillFS with `docker compose up --build`.
3. Open `http://localhost:9000`. The status page checks `/health/ready`, which
   reports whether StillFS can reach PostgreSQL. `/health/live` checks only
   that the HTTP process is responding.

For frontend-only development, run `cd web && npm ci && npm run dev`; Vite
proxies health requests to `localhost:9000`. Run the backend separately with
`go run ./cmd/stillfs` after starting a PostgreSQL instance and setting the
database environment variables below.

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

HTTP read/write timeouts, shutdown timeout, connection lifetime, and connect
timeout have validated code defaults. Development Compose explicitly disables
PostgreSQL TLS on its private local network. The standalone backend default
verifies the PostgreSQL server certificate and hostname. Configure secrets
appropriately for other environments.

The local object store creates its root and shard directories with mode `0700`
and files with mode `0600`. Keep the root on a persistent filesystem. Compose
mounts the named `stillfs-objects` volume at `/app/data/objects`; deleting the
volume destroys all local object bytes. Do not share one storage root between
multiple StillFS processes. See [local storage guarantees and recovery limits](docs/architecture/local-storage.md).

## Build and tests

From the repository root:

```sh
make frontend-install
make check
make build
```

`go test ./...` runs unit tests and runs the PostgreSQL integration test when
`STILLFS_TEST_DATABASE_URL` is set. Run it explicitly with:

```sh
STILLFS_TEST_DATABASE_URL='postgres://user:password@localhost:5432/stillfs_test?sslmode=verify-full&sslrootcert=/path/to/ca.crt' make test-integration
```

The tests require a disposable database role that can create and drop schemas.
They use isolated temporary schemas and remove them at the end. Coverage
includes migration from an empty schema, repeated/concurrent migration
behavior, transaction rollback, repositories, constraints, pagination, races,
and coordination of local bytes with PostgreSQL metadata. CI runs these tests
against a temporary PostgreSQL service.

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
