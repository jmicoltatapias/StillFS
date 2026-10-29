# Go toolchain notes

The project module retains `go 1.18`. CI builds the server with the standard
Go toolchain selected by `actions/setup-go` (`1.27.x`); the S3 SDK compatibility
test is an isolated Go 1.24 module and uses that CI toolchain.

In the local task environment, `/usr/bin/go` resolved to Debian GCCGo 14.4 and
reported `go version go1.18 gccgo`. Its `go env GOROOT` was `/usr`, and `go
tool compile` returned `no such tool "compile"`; it is not the standard Go
compiler/tool distribution. Running the race suite with that executable
previously failed with `package testmain: cannot find package`. This failure
comes from the local GCCGo test/race toolchain, not from a race report.

The same source was then tested using the official standard Go 1.27.3 Linux
amd64 archive, downloaded from go.dev and verified against its published
SHA-256 checksum. Both `go test -race -count=1 ./...` and the isolated AWS SDK
compatibility test with `-race` passed. No project Go version or CI toolchain
change was made for this diagnosis.

To check which compiler a local `go` command uses, run:

```sh
go version
go env GOROOT GOVERSION CC
go tool compile -V
```

For race testing, use a standard Go distribution for the host platform. A
GCCGo wrapper may compile ordinary packages but still lack the standard test
and race tooling.
