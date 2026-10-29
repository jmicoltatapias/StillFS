package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmicoltatapias/stillfs/internal/authorization"
	"github.com/jmicoltatapias/stillfs/internal/credential"
	"github.com/jmicoltatapias/stillfs/internal/logging"
	"github.com/jmicoltatapias/stillfs/internal/objectservice"
	"github.com/jmicoltatapias/stillfs/internal/s3api"
	"github.com/jmicoltatapias/stillfs/internal/sigv4"
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
	principalID := createTestPrincipal(t, ctx, db)
	bucketName := fmt.Sprintf("service-it-%d", time.Now().UnixNano())
	if _, err := NewBucketRepository(db).CreateBucket(ctx, principalID, bucketName); err != nil {
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

func TestS3APIWithPostgresAndLocalStore(t *testing.T) {
	db, cleanup := openIntegrationDB(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	credentialStore := NewCredentialRepository(db)
	masterKey := make([]byte, 32)
	manager := credential.Manager{Store: credentialStore, Key: masterKey}
	created, err := manager.Create(ctx, 0)
	if err != nil {
		t.Fatalf("create API test credential: %v", err)
	}
	if err := credentialStore.SetCredentialPermissions(ctx, created.AccessKeyID, testS3Permissions()); err != nil {
		t.Fatal(err)
	}
	restricted, err := manager.Create(ctx, created.PrincipalID)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Revoke(context.Background(), restricted.AccessKeyID)
	bytesStore, err := local.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	metadata := NewMetadataRepository(db)
	logger := logging.New(io.Discard)
	service := objectservice.New(bytesStore, metadata, logger)
	api := s3api.New(sigv4.Authenticator{
		Region: "us-east-1", MaxClockSkew: 5 * time.Minute,
		Resolver: credential.Resolver{Store: credentialStore, Key: masterKey},
	}, NewBucketRepository(db), metadata, service, 1<<20, 8<<10, logger, authorization.Authorizer{Store: credentialStore})
	call := func(method, target string, body []byte) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		api.ServeHTTP(recorder, signS3Request(t, method, target, body, created.AccessKeyID, created.Secret))
		return recorder
	}
	denied := httptest.NewRecorder()
	api.ServeHTTP(denied, signS3Request(t, http.MethodGet, "http://stillfs.test/not-visible/object", nil, restricted.AccessKeyID, restricted.Secret))
	if denied.Code != http.StatusForbidden || !strings.Contains(denied.Body.String(), "<Code>AccessDenied</Code>") || strings.Contains(denied.Body.String(), "NoSuchBucket") {
		t.Fatalf("credential without grant response = %d %s", denied.Code, denied.Body.String())
	}
	bucket := fmt.Sprintf("s3-it-%d", time.Now().UnixNano())
	if response := call(http.MethodPut, "http://stillfs.test/"+bucket, nil); response.Code != http.StatusOK {
		t.Fatalf("S3 create bucket = %d %s", response.Code, response.Body.String())
	}
	objectURL := "http://stillfs.test/" + bucket + "/dir/object"
	if response := call(http.MethodPut, objectURL, []byte("postgres and local bytes")); response.Code != http.StatusOK {
		t.Fatalf("S3 PUT object = %d %s", response.Code, response.Body.String())
	}
	if response := call(http.MethodGet, objectURL, nil); response.Code != http.StatusOK || response.Body.String() != "postgres and local bytes" {
		t.Fatalf("S3 GET object = %d %q", response.Code, response.Body.String())
	}
	if response := call(http.MethodHead, objectURL, nil); response.Code != http.StatusOK || response.Header().Get("Content-Length") != "24" {
		t.Fatalf("S3 HEAD object = %d headers=%#v", response.Code, response.Header())
	}
	listURL := "http://stillfs.test/" + bucket + "?list-type=2&prefix=dir%2F"
	if response := call(http.MethodGet, listURL, nil); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "<Key>dir/object</Key>") {
		t.Fatalf("S3 ListObjectsV2 = %d %s", response.Code, response.Body.String())
	}
	if response := call(http.MethodDelete, "http://stillfs.test/"+bucket, nil); response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "BucketNotEmpty") {
		t.Fatalf("S3 delete nonempty bucket = %d %s", response.Code, response.Body.String())
	}
	if response := call(http.MethodDelete, objectURL, nil); response.Code != http.StatusNoContent {
		t.Fatalf("S3 DELETE object = %d %s", response.Code, response.Body.String())
	}
	if response := call(http.MethodDelete, "http://stillfs.test/"+bucket, nil); response.Code != http.StatusNoContent {
		t.Fatalf("S3 DELETE bucket = %d %s", response.Code, response.Body.String())
	}
}

func TestCredentialMasterKeyRotationIsTransactionalAndPreservesState(t *testing.T) {
	db, cleanup := openIntegrationDB(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	repository := NewCredentialRepository(db)
	oldKey, newKey := bytes.Repeat([]byte{0x21}, 32), bytes.Repeat([]byte{0x42}, 32)
	manager := credential.Manager{Store: repository, Key: oldKey}
	first, err := manager.Create(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Create(ctx, first.PrincipalID)
	if err != nil {
		t.Fatal(err)
	}
	revoked, err := manager.Create(ctx, first.PrincipalID)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.SetCredentialPermissions(ctx, first.AccessKeyID, []string{authorization.ListBuckets, authorization.GetObject}); err != nil {
		t.Fatal(err)
	}
	if err := repository.SetCredentialPermissions(ctx, second.AccessKeyID, []string{authorization.PutObject}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Revoke(ctx, revoked.AccessKeyID); err != nil {
		t.Fatal(err)
	}

	localStore, err := local.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	metadata := NewMetadataRepository(db)
	bucket, err := NewBucketRepository(db).CreateBucket(ctx, first.PrincipalID, fmt.Sprintf("rotate-it-%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	objects := objectservice.New(localStore, metadata, logging.New(io.Discard))
	objectID := storage.ObjectID{Bucket: bucket.Name, Key: "keep/object"}
	const objectContent = "rotation must not touch object bytes"
	if err := objects.Put(ctx, objectID, "text/plain", strings.NewReader(objectContent)); err != nil {
		t.Fatal(err)
	}
	beforeMetadata, err := metadata.Get(ctx, objectID)
	if err != nil {
		t.Fatal(err)
	}
	beforeCipher, err := repository.GetCredential(ctx, first.AccessKeyID)
	if err != nil {
		t.Fatal(err)
	}

	wrong := bytes.Repeat([]byte{0x33}, 32)
	if _, err := credential.RotateMasterKey(ctx, repository, wrong, newKey); !errors.Is(err, credential.ErrMasterKeyMismatch) {
		t.Fatalf("wrong old key error = %v", err)
	}
	if _, err := credential.RotateMasterKey(ctx, repository, oldKey, newKey[:31]); !errors.Is(err, credential.ErrInvalidMasterKey) {
		t.Fatalf("invalid new key error = %v", err)
	}
	if _, err := credential.VerifyMasterKey(ctx, repository, oldKey); err != nil {
		t.Fatalf("old key failed after rejected attempts: %v", err)
	}

	corrupt := append([]byte(nil), beforeCipher.EncryptedSecret...)
	corrupt[len(corrupt)-1] ^= 0x80
	if _, err := db.ExecContext(ctx, `UPDATE credentials SET secret_ciphertext=$2 WHERE access_key_id=$1`, first.AccessKeyID, corrupt); err != nil {
		t.Fatal(err)
	}
	if _, err := credential.RotateMasterKey(ctx, repository, oldKey, newKey); !errors.Is(err, credential.ErrInvalidCredentialData) {
		t.Fatalf("corrupted ciphertext rotation error = %v", err)
	}
	afterFailure, err := repository.GetCredential(ctx, first.AccessKeyID)
	if err != nil || !bytes.Equal(afterFailure.EncryptedSecret, corrupt) {
		t.Fatalf("failed rotation changed ciphertext: err=%v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE credentials SET secret_ciphertext=$2 WHERE access_key_id=$1`, first.AccessKeyID, beforeCipher.EncryptedSecret); err != nil {
		t.Fatal(err)
	}

	if _, err := db.ExecContext(ctx, `CREATE FUNCTION stillfs_reject_credential_rotation() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected rotation failure'; END $$; CREATE TRIGGER stillfs_reject_credential_rotation BEFORE UPDATE OF secret_ciphertext ON credentials FOR EACH ROW EXECUTE FUNCTION stillfs_reject_credential_rotation()`); err != nil {
		t.Fatal(err)
	}
	if _, err := credential.RotateMasterKey(ctx, repository, oldKey, newKey); err == nil {
		t.Fatal("database-triggered rotation failure unexpectedly succeeded")
	}
	if _, err := db.ExecContext(ctx, `DROP TRIGGER stillfs_reject_credential_rotation ON credentials; DROP FUNCTION stillfs_reject_credential_rotation()`); err != nil {
		t.Fatal(err)
	}
	if _, err := credential.VerifyMasterKey(ctx, repository, oldKey); err != nil {
		t.Fatalf("old key failed after transaction rollback: %v", err)
	}

	oldCheck, _ := credential.MasterKeyCheck(oldKey)
	newCheck, _ := credential.MasterKeyCheck(newKey)
	started, release := make(chan struct{}), make(chan struct{})
	rotationDone := make(chan error, 1)
	var once sync.Once
	go func() {
		_, err := repository.RotateCredentialSecrets(ctx, oldCheck, newCheck, func(id string, ciphertext, nonce []byte) ([]byte, []byte, error) {
			plain, err := credential.Decrypt(oldKey, id, ciphertext, nonce)
			if err != nil {
				return nil, nil, err
			}
			newCipher, newNonce, err := credential.Encrypt(newKey, id, plain)
			if err != nil {
				return nil, nil, err
			}
			once.Do(func() { close(started); <-release })
			return newCipher, newNonce, nil
		})
		rotationDone <- err
	}()
	<-started
	staleCreateDone := make(chan error, 1)
	go func() { _, err := manager.Create(ctx, first.PrincipalID); staleCreateDone <- err }()
	select {
	case err := <-staleCreateDone:
		t.Fatalf("credential create did not wait for rotation lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-rotationDone; err != nil {
		t.Fatalf("rotate credentials: %v", err)
	}
	if err := <-staleCreateDone; !errors.Is(err, storage.ErrMasterKeyMismatch) {
		t.Fatalf("stale-key concurrent create error = %v", err)
	}

	if count, err := credential.VerifyMasterKey(ctx, repository, newKey); err != nil || count != 3 {
		t.Fatalf("verify rotated key count=%d err=%v", count, err)
	}
	if _, err := credential.VerifyMasterKey(ctx, repository, oldKey); !errors.Is(err, credential.ErrMasterKeyMismatch) {
		t.Fatalf("old key after rotation error = %v", err)
	}
	events, err := repository.ListAuditEvents(ctx, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	sawRotationSuccess := false
	for _, event := range events {
		if event.Action == "credential.master-key-rotation.succeeded" && event.Target == "credential-count:3" {
			sawRotationSuccess = true
		}
		if strings.Contains(event.Actor, first.Secret) || strings.Contains(event.Action, first.Secret) || strings.Contains(event.Target, first.Secret) {
			t.Fatal("audit event exposed plaintext credential secret")
		}
	}
	if !sawRotationSuccess {
		t.Fatal("successful rotation audit event was not committed with the transaction")
	}
	if err := repository.EnsureCredentialMasterKey(ctx, oldCheck); !errors.Is(err, storage.ErrMasterKeyMismatch) {
		t.Fatalf("startup accepted stale master-key verifier: %v", err)
	}
	for _, item := range []struct {
		access, secret string
		revoked        bool
	}{{first.AccessKeyID, first.Secret, false}, {second.AccessKeyID, second.Secret, false}, {revoked.AccessKeyID, revoked.Secret, true}} {
		resolved, err := (credential.Resolver{Store: repository, Key: newKey}).Resolve(ctx, item.access)
		if err != nil || resolved.Active == item.revoked || (!item.revoked && resolved.Secret != item.secret) {
			t.Fatalf("credential after rotation active=%v err=%v", resolved.Active, err)
		}
	}
	for _, grant := range []struct {
		access     string
		permission string
		want       bool
	}{{first.AccessKeyID, authorization.ListBuckets, true}, {first.AccessKeyID, authorization.PutObject, false}, {second.AccessKeyID, authorization.PutObject, true}, {revoked.AccessKeyID, authorization.PutObject, false}} {
		allowed, err := repository.HasCredentialPermission(ctx, grant.access, first.PrincipalID, grant.permission)
		if err != nil || allowed != grant.want {
			t.Fatalf("permission %s after rotation = %v err=%v want=%v", grant.permission, allowed, err, grant.want)
		}
	}
	newManager := credential.Manager{Store: repository, Key: newKey}
	replacement, err := newManager.Create(ctx, first.PrincipalID)
	if err != nil {
		t.Fatalf("create replacement credential under new key: %v", err)
	}
	if allowed, err := repository.HasCredentialPermission(ctx, replacement.AccessKeyID, first.PrincipalID, authorization.GetObject); err != nil || allowed {
		t.Fatalf("replacement credential was granted permissions automatically: allowed=%v err=%v", allowed, err)
	}
	if err := repository.SetCredentialPermissions(ctx, replacement.AccessKeyID, []string{authorization.GetObject}); err != nil {
		t.Fatal(err)
	}
	if allowed, err := repository.HasCredentialPermission(ctx, replacement.AccessKeyID, first.PrincipalID, authorization.GetObject); err != nil || !allowed {
		t.Fatalf("explicit replacement grant failed: allowed=%v err=%v", allowed, err)
	}
	afterMetadata, err := metadata.Get(ctx, objectID)
	if err != nil || afterMetadata.ID != beforeMetadata.ID || afterMetadata.Bucket != beforeMetadata.Bucket || afterMetadata.Key != beforeMetadata.Key || afterMetadata.StorageRef != beforeMetadata.StorageRef || afterMetadata.ContentLength != beforeMetadata.ContentLength || afterMetadata.ContentType != beforeMetadata.ContentType || afterMetadata.ETag != beforeMetadata.ETag || afterMetadata.ChecksumAlgorithm != beforeMetadata.ChecksumAlgorithm || afterMetadata.Checksum != beforeMetadata.Checksum || !afterMetadata.CreatedAt.Equal(beforeMetadata.CreatedAt) || !afterMetadata.UpdatedAt.Equal(beforeMetadata.UpdatedAt) {
		t.Fatalf("object metadata changed during rotation: before=%#v after=%#v err=%v", beforeMetadata, afterMetadata, err)
	}
	reader, err := localStore.Open(ctx, afterMetadata.StorageRef)
	if err != nil {
		t.Fatal(err)
	}
	content, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || string(content) != objectContent {
		t.Fatalf("object bytes changed: content=%q read=%v close=%v", content, readErr, closeErr)
	}
}

func testS3Permissions() []string {
	return []string{authorization.ListBuckets, authorization.ListObjects, authorization.CreateBucket, authorization.DeleteBucket, authorization.GetObject, authorization.PutObject, authorization.DeleteObject}
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
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM stillfs_schema_migrations WHERE version = 2`).Scan(&applied); err != nil || applied != 1 {
		t.Fatalf("version 2 rows = %d, err=%v; want 1 after repeated migration", applied, err)
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
	principalID := createTestPrincipal(t, ctx, db)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	credentials := NewCredentialRepository(db)
	credential, err := credentials.CreateCredential(ctx, storage.Credential{
		AccessKeyID: "cred-it-" + suffix, PrincipalID: principalID,
		EncryptedSecret: []byte("authenticated ciphertext"), Nonce: make([]byte, 12),
	})
	if err != nil || credential.PrincipalID != principalID {
		t.Fatalf("create credential for existing principal = (%#v, %v)", credential, err)
	}
	loadedCredential, err := credentials.GetCredential(ctx, credential.AccessKeyID)
	if err != nil || loadedCredential.RevokedAt != nil || string(loadedCredential.EncryptedSecret) != "authenticated ciphertext" {
		t.Fatalf("load active credential = (%#v, %v)", loadedCredential, err)
	}
	allowed, err := credentials.HasCredentialPermission(ctx, credential.AccessKeyID, principalID, authorization.GetObject)
	if err != nil || allowed {
		t.Fatalf("new credential permission = %v, %v; want deny by default", allowed, err)
	}
	if err := credentials.SetCredentialPermissions(ctx, credential.AccessKeyID, []string{authorization.GetObject}); err != nil {
		t.Fatal(err)
	}
	allowed, err = credentials.HasCredentialPermission(ctx, credential.AccessKeyID, principalID, authorization.GetObject)
	if err != nil || !allowed {
		t.Fatalf("granted permission = %v, %v", allowed, err)
	}
	allowed, err = credentials.HasCredentialPermission(ctx, credential.AccessKeyID, principalID, authorization.PutObject)
	if err != nil || allowed {
		t.Fatalf("ungranted permission = %v, %v", allowed, err)
	}
	secondKey, err := credentials.CreateCredential(ctx, storage.Credential{AccessKeyID: "other-key-" + suffix, PrincipalID: principalID, EncryptedSecret: []byte("other ciphertext"), Nonce: make([]byte, 12)})
	if err != nil {
		t.Fatal(err)
	}
	allowed, err = credentials.HasCredentialPermission(ctx, secondKey.AccessKeyID, principalID, authorization.GetObject)
	if err != nil || allowed {
		t.Fatalf("same-principal key inherited permissions: %v, %v", allowed, err)
	}
	allowed, err = credentials.HasCredentialPermission(ctx, credential.AccessKeyID, principalID+1, authorization.GetObject)
	if err != nil || allowed {
		t.Fatalf("credential permission crossed principal boundary: %v, %v", allowed, err)
	}
	events, err := credentials.ListAuditEvents(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var sawCreate, sawPermission bool
	for _, event := range events {
		if event.Target == credential.AccessKeyID && event.Action == "credential.create" {
			sawCreate = true
		}
		if event.Target == credential.AccessKeyID && event.Action == "credential.permissions.set" {
			sawPermission = true
		}
	}
	if !sawCreate || !sawPermission {
		t.Fatalf("credential audit events missing: create=%v permissions=%v", sawCreate, sawPermission)
	}
	if err := credentials.RevokeCredential(ctx, credential.AccessKeyID); err != nil {
		t.Fatalf("revoke credential: %v", err)
	}
	loadedCredential, err = credentials.GetCredential(ctx, credential.AccessKeyID)
	if err != nil || loadedCredential.RevokedAt == nil {
		t.Fatalf("load revoked credential = (%#v, %v)", loadedCredential, err)
	}
	allowed, err = credentials.HasCredentialPermission(ctx, credential.AccessKeyID, principalID, authorization.GetObject)
	if err != nil || allowed {
		t.Fatalf("revoked credential retained a grant: %v, %v", allowed, err)
	}
	if err := credentials.RevokeCredential(ctx, "missing-"+suffix); !errors.Is(err, storage.ErrCredentialNotFound) {
		t.Fatalf("revoke missing credential error = %v", err)
	}
	nameA, nameB, nameC := "it-"+suffix+"-a", "it-"+suffix+"-b", "it-"+suffix+"-c"
	if _, err := buckets.CreateBucket(ctx, principalID, ""); !errors.Is(err, storage.ErrInvalidBucketName) {
		t.Fatalf("empty bucket name error = %v", err)
	}
	bucket, err := buckets.CreateBucket(ctx, principalID, nameA)
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	if bucket.ID <= 0 || bucket.Name != nameA || bucket.CreatedAt.IsZero() {
		t.Fatalf("incomplete created bucket: %#v", bucket)
	}
	if _, err := buckets.CreateBucket(ctx, principalID, nameA); !errors.Is(err, storage.ErrAlreadyExists) {
		t.Fatalf("duplicate bucket error = %v", err)
	}
	if got, err := buckets.GetBucket(ctx, bucket.ID); err != nil || got.Name != nameA {
		t.Fatalf("get bucket by ID = (%#v, %v)", got, err)
	}
	if got, err := buckets.GetBucketByName(ctx, nameA); err != nil || got.ID != bucket.ID {
		t.Fatalf("get bucket by name = (%#v, %v)", got, err)
	}
	if got, err := buckets.GetBucketByName(ctx, nameA); err != nil || got.OwnerID != principalID {
		t.Fatalf("bucket owner = (%#v, %v), want %d", got, err, principalID)
	}
	if _, err := buckets.GetBucketByName(ctx, "missing-"+suffix); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Fatalf("missing bucket error = %v", err)
	}
	if _, err := buckets.CreateBucket(ctx, principalID, nameB); err != nil {
		t.Fatal(err)
	}
	if _, err := buckets.CreateBucket(ctx, principalID, nameC); err != nil {
		t.Fatal(err)
	}
	otherPrincipalID := createTestPrincipal(t, ctx, db)
	otherPage, err := buckets.ListBuckets(ctx, storage.BucketPageRequest{OwnerID: otherPrincipalID})
	if err != nil || len(otherPage.Items) != 0 {
		t.Fatalf("other principal bucket listing = (%#v, %v), want empty", otherPage, err)
	}
	firstBuckets, err := buckets.ListBuckets(ctx, storage.BucketPageRequest{OwnerID: principalID, After: "it-" + suffix, Limit: 1})
	if err != nil || len(firstBuckets.Items) != 1 || firstBuckets.NextAfter == "" {
		t.Fatalf("first bucket page = (%#v, %v)", firstBuckets, err)
	}
	secondBuckets, err := buckets.ListBuckets(ctx, storage.BucketPageRequest{OwnerID: principalID, After: firstBuckets.NextAfter, Limit: 2})
	if err != nil || len(secondBuckets.Items) != 2 {
		t.Fatalf("second bucket page = (%#v, %v)", secondBuckets, err)
	}
	if firstBuckets.Items[0].Name >= secondBuckets.Items[0].Name {
		t.Fatalf("bucket pages are not in stable order: %#v then %#v", firstBuckets, secondBuckets)
	}
	if _, err := buckets.ListBuckets(ctx, storage.BucketPageRequest{OwnerID: principalID, Limit: maxPageSize + 1}); !errors.Is(err, storage.ErrInvalidPageLimit) {
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
	if err := buckets.DeleteBucket(ctx, principalID, nameA); !errors.Is(err, storage.ErrBucketNotEmpty) {
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
	if err := buckets.DeleteBucket(ctx, principalID, nameA); err != nil {
		t.Fatalf("delete empty bucket: %v", err)
	}
	if err := buckets.DeleteBucket(ctx, principalID, nameA); !errors.Is(err, storage.ErrBucketNotFound) {
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
	raceBucket, err := buckets.CreateBucket(ctx, principalID, "it-"+suffix+"-race")
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
		deleteResult <- buckets.DeleteBucket(ctx, principalID, raceBucket.Name)
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
		if err := buckets.DeleteBucket(ctx, principalID, raceBucket.Name); err != nil {
			t.Fatal(err)
		}
	} else if deleteErr != nil {
		t.Fatalf("unexpected concurrent bucket delete error: %v", deleteErr)
	}
}

func TestIdentityMigrationPreservesLegacyBuckets(t *testing.T) {
	db, cleanup := openIntegrationDB(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, `CREATE TABLE stillfs_schema_migrations (
		version BIGINT PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		t.Fatalf("create migration tracking table: %v", err)
	}

	initial, err := migrationFiles.ReadFile("migrations/0001_metadata.sql")
	if err != nil {
		t.Fatal(err)
	}
	if err := applyMigration(ctx, db, 1, "0001_metadata.sql", string(initial)); err != nil {
		t.Fatalf("apply initial migration: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO buckets (name) VALUES ('legacy-bucket')`); err != nil {
		t.Fatalf("insert legacy bucket: %v", err)
	}
	identity, err := migrationFiles.ReadFile("migrations/0002_identity_ownership.sql")
	if err != nil {
		t.Fatal(err)
	}
	if err := applyMigration(ctx, db, 2, "0002_identity_ownership.sql", string(identity)); err != nil {
		t.Fatalf("apply identity migration: %v", err)
	}
	var ownerID sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT owner_id FROM buckets WHERE name = 'legacy-bucket'`).Scan(&ownerID); err != nil {
		t.Fatalf("read legacy bucket ownership: %v", err)
	}
	if ownerID.Valid {
		t.Fatalf("legacy bucket owner_id = %d, want NULL pending explicit assignment", ownerID.Int64)
	}
}

func TestStorageReferenceListingIsDistinctAndKeysetPaginated(t *testing.T) {
	db, cleanup := openIntegrationDB(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	principalID := createTestPrincipal(t, ctx, db)
	bucket, err := NewBucketRepository(db).CreateBucket(ctx, principalID, fmt.Sprintf("refs-it-%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	repository := NewMetadataRepository(db)
	for i, reference := range []string{"local:v1:01", "local:v1:02", "local:v1:01", "local:v1:03"} {
		if err := repository.Put(ctx, storage.ObjectID{Bucket: bucket.Name, Key: fmt.Sprintf("key-%d", i)}, storage.ObjectMetadata{
			StorageRef: reference, ContentLength: 0,
		}); err != nil {
			t.Fatalf("insert metadata reference %q: %v", reference, err)
		}
	}
	first, next, err := repository.ListStorageReferences(ctx, "", 2)
	if err != nil || len(first) != 2 || first[0] != "local:v1:01" || first[1] != "local:v1:02" || next != "local:v1:02" {
		t.Fatalf("first reference page = %v next=%q err=%v", first, next, err)
	}
	second, next, err := repository.ListStorageReferences(ctx, next, 2)
	if err != nil || len(second) != 1 || second[0] != "local:v1:03" || next != "" {
		t.Fatalf("second reference page = %v next=%q err=%v", second, next, err)
	}
}

func createTestPrincipal(t *testing.T, ctx context.Context, db *sql.DB) int64 {
	t.Helper()
	accessKeyID := fmt.Sprintf("principal-it-%d", time.Now().UnixNano())
	credential, err := NewCredentialRepository(db).CreateCredential(ctx, storage.Credential{
		AccessKeyID: accessKeyID, EncryptedSecret: []byte("valid-test-ciphertext"), Nonce: make([]byte, 12),
	})
	if err != nil {
		t.Fatalf("create test principal: %v", err)
	}
	return credential.PrincipalID
}

func signS3Request(t *testing.T, method, target string, body []byte, accessKeyID, secret string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(method, target, strings.NewReader(string(body)))
	if body == nil {
		request.Body = http.NoBody
		request.ContentLength = 0
	}
	request.Host = "stillfs.test"
	payload := sha256.Sum256(body)
	payloadHash := hex.EncodeToString(payload[:])
	timestamp := time.Now().UTC().Format("20060102T150405Z")
	request.Header.Set("X-Amz-Date", timestamp)
	request.Header.Set("X-Amz-Content-Sha256", payloadHash)
	signedHeaders := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	canonical, err := sigv4.CanonicalRequest(request, signedHeaders, payloadHash)
	if err != nil {
		t.Fatal(err)
	}
	signature := sigv4.Signature(secret, timestamp, "us-east-1", "s3", canonical)
	request.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s/us-east-1/s3/aws4_request,SignedHeaders=%s,Signature=%s", accessKeyID, timestamp[:8], strings.Join(signedHeaders, ";"), signature))
	return request
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
