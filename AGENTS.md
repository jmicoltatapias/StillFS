
# StillFS — Engineering Guidelines

## Project
StillFS is an object-storage server written in Go.
Its goals are S3 API compatibility, secure object storage,
operational simplicity, and future distributed storage.

## Architecture
- Keep HTTP/S3 handlers separate from domain logic.
- Separate object data storage from metadata persistence.
- Define explicit interfaces at real architectural boundaries.
- Keep the single-node implementation correct before adding clustering.
- Do not introduce distributed-storage behavior without a design document.
- Document architectural decisions in docs/decisions/.

## Engineering
- Follow idiomatic Go and keep dependencies minimal.
- Validate configuration at startup.
- Stream large objects; avoid buffering entire objects in memory.
- Handle cancellation, partial writes, cleanup, and errors explicitly.
- Never report success before the required persistence operation completes.
- Never log secret keys, credentials, or authorization headers.
- Use context.Context for cancellation and request-scoped operations.

## Testing
- Add unit tests for domain logic and error paths.
- Add integration tests for persistence and HTTP behavior.
- Add S3 compatibility tests for every implemented operation.
- Test concurrent requests and interrupted operations.
- Run gofmt, go vet, and go test ./... before completing a task.
- Add race detection where practical.

## Security
- Treat all external input as untrusted.
- Prevent path traversal and unauthorized bucket access.
- Never disable authentication or TLS verification to make tests pass.
- Do not execute downloaded binaries or untrusted scripts without review.
- Document security-sensitive design choices.

## Development workflow
- Read relevant code before proposing changes.
- Explain the plan before substantial modifications.
- Make small, reviewable changes.
- Do not claim a test passed unless it was actually executed.
- Do not introduce placeholder implementations that appear production-ready.
- Update documentation when behavior or configuration changes.
- Report files changed, tests run, results, and known limitations.

## Scope
Implement only explicitly requested features.
Do not claim complete S3 compatibility or distributed availability
unless supported by tests and documented guarantees.
