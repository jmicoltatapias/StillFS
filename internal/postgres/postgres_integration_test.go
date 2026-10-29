package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmicoltatapias/stillfs/internal/logging"
	"github.com/jmicoltatapias/stillfs/internal/objectservice"
	"github.com/jmicoltatapias/stillfs/internal/storage"
	"github.com/jmicoltatapias/stillfs/internal/storage/local"
)

func TestObjectServiceWithPostgresAndLocalStore(t *testing.T) {
	db, cleanup := openIntegrationDB(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	bucketName := fmt.Sprintf("service-it-%d", time.Now().UnixNano())
	if _, err := NewBucketRepository(db).CreateBucket(ctx, bucketName); err != nil {
		t.Fatal(err)
	}
	localStore, err := local.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repository := NewMetadataRepository(db)
	service := objectservice.New(localStore, repository, logging.New(io.Discard))
	id := storage.ObjectID{Bucket: bucketName, Key: "nested/object"}
	if err := service.Put(ctx, id, "application/octet-stream", strings.NewReader("first payload")); err != nil {
		t.Fatalf("coordinated create: %v", err)
	}
	first, err := repository.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	readerMetadata, reader, err := service.Open(ctx, id)
	if err != nil {
		t.Fatalf("coordinated open: %v", err)
	}
	content, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || string(content) != "first payload" || readerMetadata.StorageRef != first.StorageRef {
		t.Fatalf("read coordinated content = %q, metadata=%#v, errors=%v/%v", content, readerMetadata, readErr, closeErr)
	}
	if err := service.Put(ctx, id, "", strings.NewReader("losing duplicate")); !errors.Is(err, storage.ErrAlreadyExists) {
		t.Fatalf("duplicate coordinated create error = %v", err)
	}
	if _, err := localStore.Stat(ctx, first.StorageRef); err != nil {
		t.Fatalf("existing referenced content removed during compensation: %v", err)
	}
	if err := service.Replace(ctx, id, "text/plain", strings.NewReader("replacement")); err != nil {
		t.Fatalf("coordinated replace: %v", err)
	}
	second, err := repository.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if second.StorageRef == first.StorageRef || second.ContentLength != int64(len("replacement")) {
		t.Fatalf("replacement metadata = %#v; first = %#v", second, first)
	}
	if _, err := localStore.Stat(ctx, first.StorageRef); err != nil {
		t.Fatalf("replacement unexpectedly removed old bytes: %v", err)
	}
}

// This test uses a fresh per-test schema in an explicitly supplied disposable DB.
func TestPostgresMetadataRepositories(t *testing.T) {
	db, cleanup := openIntegrationDB(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var before sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT to_regclass('buckets')`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before.Valid {
		t.Fatal("isolated schema unexpectedly contains application tables before migration")
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("migrate empty schema: %v", err)
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("repeat migrations: %v", err)
	}
	var migrationWG sync.WaitGroup
	migrationErrors := make(chan error, 2)
	for i := 0; i < 2; i++ {
		migrationWG.Add(1)
		go func() {
			defer migrationWG.Done()
			migrationErrors <- Migrate(ctx, db)
		}()
	}
	migrationWG.Wait()
	close(migrationErrors)
	for err := range migrationErrors {
		if err != nil {
			t.Fatalf("concurrent migration: %v", err)
		}
	}
	var applied int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM stillfs_schema_migrations WHERE version = 1`).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if applied != 1 {
		t.Fatalf("version 1 rows = %d, want 1 after repeated migration", applied)
	}
	var byteColumns int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name IN ('buckets', 'objects') AND data_type = 'bytea'`).Scan(&byteColumns); err != nil {
		t.Fatal(err)
	}
	if byteColumns != 0 {
		t.Fatalf("metadata tables contain %d bytea columns", byteColumns)
	}
	if err := HealthCheck(ctx, db); err != nil {
		t.Fatalf("database health check: %v", err)
	}

	// A migration that fails after DDL must leave neither the DDL nor version row.
	if err := applyMigration(ctx, db, 9000001, "9000001_test_rollback.sql",
		`CREATE TABLE rollback_probe (id integer); SELECT 1 / 0;`); err == nil {
		t.Fatal("failing migration unexpectedly succeeded")
	}
	var probe sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT to_regclass('rollback_probe')`).Scan(&probe); err != nil {
		t.Fatal(err)
	}
	if probe.Valid {
		t.Fatal("DDL from failed migration was not rolled back")
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM stillfs_schema_migrations WHERE version = 9000001`).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if applied != 0 {
		t.Fatalf("failed migration version rows = %d, want 0", applied)
	}

	buckets := NewBucketRepository(db)
	objects := NewMetadataRepository(db)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	nameA, nameB, nameC := "it-"+suffix+"-a", "it-"+suffix+"-b", "it-"+suffix+"-c"
	if _, err := buckets.CreateBucket(ctx, ""); !errors.Is(err, storage.ErrInvalidBucketName) {
		t.Fatalf("empty bucket name error = %v", err)
	}
	bucket, err := buckets.CreateBucket(ctx, nameA)
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	if bucket.ID <= 0 || bucket.Name != nameA || bucket.CreatedAt.IsZero() {
		t.Fatalf("incomplete created bucket: %#v", bucket)
	}
	if _, err := buckets.CreateBucket(ctx, nameA); !errors.Is(err, storage.ErrAlreadyExists) {
		t.Fatalf("duplicate bucket error = %v", err)
	}
	if got, err := buckets.GetBucket(ctx, bucket.ID); err != nil || got.Name != nameA {
		t.Fatalf("get bucket by ID = (%#v, %v)", got, err)
	}
	if got, err := buckets.GetBucketByName(ctx, nameA); err != nil || got.ID != bucket.ID {
		t.Fatalf("get bucket by name = (%#v, %v)", got, err)
	}
	if _, err := buckets.GetBucketByName(ctx, "missing-"+suffix); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Fatalf("missing bucket error = %v", err)
	}
	if _, err := buckets.CreateBucket(ctx, nameB); err != nil {
		t.Fatal(err)
	}
	if _, err := buckets.CreateBucket(ctx, nameC); err != nil {
		t.Fatal(err)
	}
	firstBuckets, err := buckets.ListBuckets(ctx, storage.BucketPageRequest{After: "it-" + suffix, Limit: 1})
	if err != nil || len(firstBuckets.Items) != 1 || firstBuckets.NextAfter == "" {
		t.Fatalf("first bucket page = (%#v, %v)", firstBuckets, err)
	}
	secondBuckets, err := buckets.ListBuckets(ctx, storage.BucketPageRequest{After: firstBuckets.NextAfter, Limit: 2})
	if err != nil || len(secondBuckets.Items) != 2 {
		t.Fatalf("second bucket page = (%#v, %v)", secondBuckets, err)
	}
	if firstBuckets.Items[0].Name >= secondBuckets.Items[0].Name {
		t.Fatalf("bucket pages are not in stable order: %#v then %#v", firstBuckets, secondBuckets)
	}
	if _, err := buckets.ListBuckets(ctx, storage.BucketPageRequest{Limit: maxPageSize + 1}); !errors.Is(err, storage.ErrInvalidPageLimit) {
		t.Fatalf("invalid page limit error = %v", err)
	}

	objectA := storage.ObjectID{Bucket: nameA, Key: "dir/a%_literal"}
	objectB := storage.ObjectID{Bucket: nameA, Key: "dir/b"}
	objectC := storage.ObjectID{Bucket: nameA, Key: "other/item"}
	metadata := storage.ObjectMetadata{
		StorageRef: "opaque-backend-ref-1", ContentLength: 17,
		ContentType: "application/octet-stream", ETag: "multipart-etag-value",
		ChecksumAlgorithm: "sha256", Checksum: "digest-value",
	}
	for _, item := range []struct {
		id storage.ObjectID
		m  storage.ObjectMetadata
	}{{objectA, metadata}, {objectB, storage.ObjectMetadata{StorageRef: "opaque-ref-2", ContentLength: 5}}, {objectC, storage.ObjectMetadata{StorageRef: "opaque-ref-3", ContentLength: 9}}} {
		if err := objects.Put(ctx, item.id, item.m); err != nil {
			t.Fatalf("put metadata for %q: %v", item.id.Key, err)
		}
	}
	if err := objects.Put(ctx, storage.ObjectID{Bucket: "missing-" + suffix, Key: "x"}, metadata); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Fatalf("put with missing bucket error = %v", err)
	}
	if err := objects.Put(ctx, objectA, metadata); !errors.Is(err, storage.ErrAlreadyExists) {
		t.Fatalf("duplicate object error = %v", err)
	}
	got, err := objects.Get(ctx, objectA)
	if err != nil {
		t.Fatalf("get object metadata: %v", err)
	}
	if got.ID <= 0 || got.Bucket != objectA.Bucket || got.Key != objectA.Key || got.StorageRef != metadata.StorageRef || got.ContentLength != metadata.ContentLength || got.ETag != metadata.ETag || got.ChecksumAlgorithm != "sha256" || got.Checksum != "digest-value" || got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Fatalf("unexpected object metadata: %#v", got)
	}
	if _, err := objects.Get(ctx, storage.ObjectID{Bucket: nameA, Key: "missing"}); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Fatalf("missing object error = %v", err)
	}

	updated, err := objects.Update(ctx, objectA, storage.ObjectMetadata{
		StorageRef: "opaque-backend-ref-2", ContentLength: 19,
		ContentType: "application/custom", ETag: "still-not-assumed-md5",
	})
	if err != nil {
		t.Fatalf("update object metadata: %v", err)
	}
	if updated.ID != got.ID || !updated.CreatedAt.Equal(got.CreatedAt) || updated.StorageRef != "opaque-backend-ref-2" || updated.ContentLength != 19 || updated.ETag != "still-not-assumed-md5" {
		t.Fatalf("unexpected updated metadata: %#v", updated)
	}
	if _, err := objects.Update(ctx, storage.ObjectID{Bucket: nameA, Key: "missing"}, metadata); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Fatalf("update missing object error = %v", err)
	}

	page1, err := objects.List(ctx, nameA, storage.ObjectPageRequest{Prefix: "dir/", Limit: 1})
	if err != nil || len(page1.Items) != 1 || page1.NextAfter == "" || page1.Items[0].Key != objectA.Key {
		t.Fatalf("first object page = (%#v, %v)", page1, err)
	}
	literalPrefix, err := objects.List(ctx, nameA, storage.ObjectPageRequest{Prefix: "dir/a%_", Limit: 10})
	if err != nil || len(literalPrefix.Items) != 1 || literalPrefix.Items[0].Key != objectA.Key {
		t.Fatalf("literal prefix page = (%#v, %v)", literalPrefix, err)
	}
	// Removing the cursor row and adding a later key between requests must not
	// shift the next page as OFFSET pagination would.
	if err := objects.Delete(ctx, objectA); err != nil {
		t.Fatalf("delete first-page row: %v", err)
	}
	insertedDuringPaging := storage.ObjectID{Bucket: nameA, Key: "dir/aa"}
	if err := objects.Put(ctx, insertedDuringPaging, storage.ObjectMetadata{StorageRef: "opaque-during-page", ContentLength: 3}); err != nil {
		t.Fatalf("insert during pagination: %v", err)
	}
	page2, err := objects.List(ctx, nameA, storage.ObjectPageRequest{Prefix: "dir/", After: page1.NextAfter, Limit: 1})
	if err != nil || len(page2.Items) != 1 || page2.NextAfter == "" || page2.Items[0].Key != insertedDuringPaging.Key || page1.Items[0].Key >= page2.Items[0].Key {
		t.Fatalf("second object page = (%#v, %v)", page2, err)
	}
	page3, err := objects.List(ctx, nameA, storage.ObjectPageRequest{Prefix: "dir/", After: page2.NextAfter, Limit: 1})
	if err != nil || len(page3.Items) != 1 || page3.Items[0].Key != objectB.Key || page3.NextAfter != "" {
		t.Fatalf("third object page = (%#v, %v)", page3, err)
	}
	if _, err := objects.List(ctx, "missing-"+suffix, storage.ObjectPageRequest{}); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Fatalf("list missing bucket error = %v", err)
	}
	if err := buckets.DeleteBucket(ctx, nameA); !errors.Is(err, storage.ErrBucketNotEmpty) {
		t.Fatalf("delete nonempty bucket error = %v", err)
	}
	if _, err := buckets.GetBucketByName(ctx, nameA); err != nil {
		t.Fatalf("bucket should remain after rejected delete: %v", err)
	}
	if _, err := objects.Get(ctx, objectB); err != nil {
		t.Fatalf("object metadata should remain after rejected bucket delete: %v", err)
	}
	if err := objects.Delete(ctx, objectA); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Fatalf("delete missing object error = %v", err)
	}
	if err := objects.Delete(ctx, objectB); err != nil {
		t.Fatal(err)
	}
	if err := objects.Delete(ctx, objectC); err != nil {
		t.Fatal(err)
	}
	if err := objects.Delete(ctx, insertedDuringPaging); err != nil {
		t.Fatal(err)
	}
	if err := buckets.DeleteBucket(ctx, nameA); err != nil {
		t.Fatalf("delete empty bucket: %v", err)
	}
	if err := buckets.DeleteBucket(ctx, nameA); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Fatalf("delete missing bucket error = %v", err)
	}

	// The unique constraint is the arbiter for concurrent creates of one key.
	raceID := storage.ObjectID{Bucket: nameB, Key: "race/key"}
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- objects.Put(ctx, raceID, storage.ObjectMetadata{StorageRef: "opaque-race", ContentLength: 1})
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	successes, duplicates := 0, 0
	for err := range errs {
		if err == nil {
			successes++
		} else if errors.Is(err, storage.ErrAlreadyExists) {
			duplicates++
		} else {
			t.Fatalf("unexpected concurrent create error: %v", err)
		}
	}
	if successes != 1 || duplicates != 1 {
		t.Fatalf("concurrent create results: successes=%d duplicates=%d", successes, duplicates)
	}
	if err := objects.Delete(ctx, raceID); err != nil {
		t.Fatal(err)
	}

	// Bucket row locking and the FK prevent an insert and bucket deletion from
	// both succeeding.
	raceBucket, err := buckets.CreateBucket(ctx, "it-"+suffix+"-race")
	if err != nil {
		t.Fatal(err)
	}
	start = make(chan struct{})
	putResult := make(chan error, 1)
	deleteResult := make(chan error, 1)
	go func() {
		<-start
		putResult <- objects.Put(ctx, storage.ObjectID{Bucket: raceBucket.Name, Key: "one"}, storage.ObjectMetadata{StorageRef: "opaque", ContentLength: 0})
	}()
	go func() {
		<-start
		deleteResult <- buckets.DeleteBucket(ctx, raceBucket.Name)
	}()
	close(start)
	putErr, deleteErr := <-putResult, <-deleteResult
	if putErr == nil && !errors.Is(deleteErr, storage.ErrBucketNotEmpty) {
		t.Fatalf("successful object registration with bucket deletion result: put=%v delete=%v", putErr, deleteErr)
	}
	if deleteErr == nil && putErr == nil {
		t.Fatal("object registration and bucket deletion both succeeded")
	}
	if putErr != nil && !errors.Is(putErr, storage.ErrBucketNotFound) && !errors.Is(putErr, storage.ErrConflict) {
		t.Fatalf("unexpected concurrent insert error: %v", putErr)
	}
	if errors.Is(deleteErr, storage.ErrBucketNotEmpty) {
		if err := objects.Delete(ctx, storage.ObjectID{Bucket: raceBucket.Name, Key: "one"}); err != nil {
			t.Fatal(err)
		}
		if err := buckets.DeleteBucket(ctx, raceBucket.Name); err != nil {
			t.Fatal(err)
		}
	} else if deleteErr != nil {
		t.Fatalf("unexpected concurrent bucket delete error: %v", deleteErr)
	}
}

func openIntegrationDB(t *testing.T) (*sql.DB, func()) {
	t.Helper()
	dsn := os.Getenv("STILLFS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set STILLFS_TEST_DATABASE_URL to a disposable PostgreSQL database")
	}
	parsed, err := url.Parse(dsn)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") {
		t.Fatalf("STILLFS_TEST_DATABASE_URL must be a postgres:// URL")
	}
	admin, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := admin.PingContext(ctx); err != nil {
		_ = admin.Close()
		t.Fatalf("connect to test PostgreSQL: %v", err)
	}
	schema := fmt.Sprintf("stillfs_it_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(ctx, `CREATE SCHEMA "`+schema+`"`); err != nil {
		_ = admin.Close()
		t.Fatalf("create isolated test schema: %v", err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	db, err := sql.Open("postgres", parsed.String())
	if err != nil {
		_, _ = admin.ExecContext(context.Background(), `DROP SCHEMA "`+schema+`" CASCADE`)
		_ = admin.Close()
		t.Fatal(err)
	}
	db.SetMaxOpenConns(8)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		_, _ = admin.ExecContext(context.Background(), `DROP SCHEMA "`+schema+`" CASCADE`)
		_ = admin.Close()
		t.Fatalf("connect to isolated schema: %v", err)
	}
	cleanup := func() {
		_ = db.Close()
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = admin.ExecContext(cleanupCtx, `DROP SCHEMA "`+schema+`" CASCADE`)
		_ = admin.Close()
	}
	return db, cleanup
}
