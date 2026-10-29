.PHONY: build run fmt vet test test-integration test-s3-compat check frontend-install frontend-check frontend-build

build:
	mkdir -p bin
	go build -o bin/stillfs ./cmd/stillfs
	go build -o bin/stillfsctl ./cmd/stillfsctl

run:
	go run ./cmd/stillfs

fmt:
	gofmt -w $$(find . -name '*.go' -not -path './vendor/*')

vet:
	go vet ./...

test:
	go test ./...

test-integration:
	@test -n "$$STILLFS_TEST_DATABASE_URL" || (echo "STILLFS_TEST_DATABASE_URL must point to a disposable PostgreSQL database" >&2; exit 1)
	go test -count=1 -v ./internal/postgres -run '^Test(PostgresMetadataRepositories|ObjectServiceWithPostgresAndLocalStore|S3APIWithPostgresAndLocalStore|CredentialMasterKeyRotationIsTransactionalAndPreservesState|IdentityMigrationPreservesLegacyBuckets|StorageReferenceListingIsDistinctAndKeysetPaginated)$$'

test-s3-compat:
	@test -n "$$STILLFS_TEST_DATABASE_URL" || (echo "STILLFS_TEST_DATABASE_URL must point to a disposable PostgreSQL database" >&2; exit 1)
	cd tests/s3compat && go test -count=1 -v ./...

frontend-install:
	cd web && npm ci

frontend-check:
	cd web && npm run check

frontend-build:
	cd web && npm run build

check: fmt vet test frontend-check frontend-build
