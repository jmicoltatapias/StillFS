package s3compat

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/jmicoltatapias/stillfs/internal/authorization"
	"github.com/jmicoltatapias/stillfs/internal/credential"
	"github.com/jmicoltatapias/stillfs/internal/logging"
	"github.com/jmicoltatapias/stillfs/internal/objectservice"
	"github.com/jmicoltatapias/stillfs/internal/postgres"
	"github.com/jmicoltatapias/stillfs/internal/s3api"
	"github.com/jmicoltatapias/stillfs/internal/sigv4"
	"github.com/jmicoltatapias/stillfs/internal/storage/local"
	_ "github.com/lib/pq"
)

func TestAWSGoV2S3SubsetAgainstPostgresAndLocalStorage(t *testing.T) {
	dsn := os.Getenv("STILLFS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set STILLFS_TEST_DATABASE_URL to a disposable PostgreSQL database")
	}
	db, cleanup := openDatabase(t, dsn)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := postgres.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}

	credentialStore := postgres.NewCredentialRepository(db)
	masterKey := make([]byte, 32) // ephemeral test key; never sourced from user credentials
	manager := credential.Manager{Store: credentialStore, Key: masterKey}
	created, err := manager.Create(ctx, 0)
	if err != nil {
		t.Fatalf("create ephemeral test credential: %v", err)
	}
	if err := credentialStore.SetCredentialPermissions(ctx, created.AccessKeyID, []string{authorization.ListBuckets, authorization.ListObjects, authorization.CreateBucket, authorization.DeleteBucket, authorization.GetObject, authorization.PutObject, authorization.DeleteObject}); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = manager.Revoke(context.Background(), created.AccessKeyID)
	}()

	bytesStore, err := local.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	metadata := postgres.NewMetadataRepository(db)
	logger := logging.New(io.Discard)
	objects := objectservice.New(bytesStore, metadata, logger)
	auth := sigv4.Authenticator{
		Region: "us-east-1", MaxClockSkew: 5 * time.Minute,
		Resolver: credential.Resolver{Store: credentialStore, Key: masterKey},
	}
	api := s3api.New(auth, postgres.NewBucketRepository(db), metadata, objects, 16<<20, 8<<10, logger, authorization.Authorizer{Store: credentialStore})
	httpServer := httptest.NewServer(api)
	defer func() { httpServer.Close() }()

	provider := aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{AccessKeyID: created.AccessKeyID, SecretAccessKey: created.Secret, Source: "ephemeral StillFS integration test"}, nil
	})
	newClient := func(endpoint string) *s3.Client {
		return s3.NewFromConfig(aws.Config{
			Region: "us-east-1", Credentials: aws.NewCredentialsCache(provider),
			RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		}, func(options *s3.Options) { options.BaseEndpoint = aws.String(endpoint); options.UsePathStyle = true })
	}
	client := newClient(httpServer.URL)

	bucket := fmt.Sprintf("sdk-compat-%d", time.Now().UnixNano())
	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	if _, err := client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatalf("HeadBucket: %v", err)
	}
	listedBuckets, err := client.ListBuckets(ctx, &s3.ListBucketsInput{})
	if err != nil {
		t.Fatalf("ListBuckets: %v", err)
	}
	if !hasBucket(listedBuckets.Buckets, bucket) {
		t.Fatalf("ListBuckets omitted target bucket %q: %#v", bucket, listedBuckets.Buckets)
	}
	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); apiCode(err) != "BucketAlreadyExists" {
		t.Fatalf("duplicate CreateBucket error code=%q, err=%v", apiCode(err), err)
	}

	put := func(key, body, contentType string) {
		t.Helper()
		_, err := client.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String(bucket), Key: aws.String(key), Body: strings.NewReader(body),
			ContentLength: aws.Int64(int64(len(body))), ContentType: aws.String(contentType),
		})
		if err != nil {
			t.Fatalf("PutObject(%q): %v", key, err)
		}
	}
	put("empty", "", "application/octet-stream")
	largeBody := strings.Repeat("stream-check-", 1<<18) // 3.5 MiB, above the usual small transfer buffer
	put("large/object.bin", largeBody, "application/octet-stream")
	for _, key := range []string{"tree/a/one", "tree/a/two", "tree/b/one", "tree/root"} {
		put(key, "payload:"+key, "text/plain")
	}

	for key, want := range map[string]string{"empty": "", "large/object.bin": largeBody} {
		got, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
		if err != nil {
			t.Fatalf("GetObject(%q): %v", key, err)
		}
		body, readErr := io.ReadAll(got.Body)
		closeErr := got.Body.Close()
		if readErr != nil || closeErr != nil || string(body) != want {
			t.Fatalf("GetObject(%q) content mismatch: bytes=%d read=%v close=%v", key, len(body), readErr, closeErr)
		}
	}
	head, err := client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String("large/object.bin")})
	if err != nil || aws.ToInt64(head.ContentLength) != int64(len(largeBody)) || aws.ToString(head.ContentType) != "application/octet-stream" || aws.ToString(head.ETag) == "" {
		t.Fatalf("HeadObject metadata=%#v err=%v", head, err)
	}
	if _, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String("missing")}); apiCode(err) != "NoSuchKey" {
		t.Fatalf("missing GetObject error code=%q, err=%v", apiCode(err), err)
	}
	if _, err := client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(bucket)}); apiCode(err) != "BucketNotEmpty" {
		t.Fatalf("nonempty DeleteBucket error code=%q, err=%v", apiCode(err), err)
	}

	var keys []string
	var prefixes []string
	var token *string
	for page := 0; page < 10; page++ {
		output, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket: aws.String(bucket), Prefix: aws.String("tree/"), Delimiter: aws.String("/"),
			MaxKeys: aws.Int32(1), ContinuationToken: token,
		})
		if err != nil {
			t.Fatalf("ListObjectsV2 page %d: %v", page, err)
		}
		for _, object := range output.Contents {
			keys = append(keys, aws.ToString(object.Key))
		}
		for _, common := range output.CommonPrefixes {
			prefixes = append(prefixes, aws.ToString(common.Prefix))
		}
		if !aws.ToBool(output.IsTruncated) {
			break
		}
		if output.NextContinuationToken == nil || *output.NextContinuationToken == "" {
			t.Fatal("truncated ListObjectsV2 page omitted continuation token")
		}
		token = output.NextContinuationToken
	}
	if strings.Join(prefixes, ",") != "tree/a/,tree/b/" || strings.Join(keys, ",") != "tree/root" {
		t.Fatalf("paginated listing keys=%v commonPrefixes=%v", keys, prefixes)
	}

	// Rotation changes ciphertext and the active key verifier atomically. Start
	// a fresh server with the replacement key and exercise the SDK again.
	httpServer.Close()
	newMasterKey := make([]byte, 32)
	for i := range newMasterKey {
		newMasterKey[i] = 0x62
	}
	if count, err := credential.RotateMasterKey(ctx, credentialStore, masterKey, newMasterKey); err != nil || count != 1 {
		t.Fatalf("rotate master key count=%d err=%v", count, err)
	}
	newAuth := sigv4.Authenticator{Region: "us-east-1", MaxClockSkew: 5 * time.Minute, Resolver: credential.Resolver{Store: credentialStore, Key: newMasterKey}}
	api = s3api.New(newAuth, postgres.NewBucketRepository(db), metadata, objects, 16<<20, 8<<10, logger, authorization.Authorizer{Store: credentialStore})
	httpServer = httptest.NewServer(api)
	client = newClient(httpServer.URL)
	postRotation, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String("tree/root")})
	if err != nil {
		t.Fatalf("SDK GetObject after master-key rotation: %v", err)
	}
	postRotationBody, readErr := io.ReadAll(postRotation.Body)
	closeErr := postRotation.Body.Close()
	if readErr != nil || closeErr != nil || string(postRotationBody) != "payload:tree/root" {
		t.Fatalf("SDK response after rotation=%q read=%v close=%v", postRotationBody, readErr, closeErr)
	}

	for _, key := range []string{"empty", "large/object.bin", "tree/a/one", "tree/a/two", "tree/b/one", "tree/root"} {
		if _, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)}); err != nil {
			t.Fatalf("DeleteObject(%q): %v", key, err)
		}
	}
	if _, err := client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatalf("DeleteBucket: %v", err)
	}
	if err := manager.Revoke(ctx, created.AccessKeyID); err != nil {
		t.Fatalf("revoke ephemeral test credential: %v", err)
	}
	if _, err := client.ListBuckets(ctx, &s3.ListBucketsInput{}); apiCode(err) != "AccessDenied" {
		t.Fatalf("revoked ListBuckets error code=%q, err=%v", apiCode(err), err)
	}
}

func openDatabase(t *testing.T, dsn string) (*sql.DB, func()) {
	t.Helper()
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
	schema := fmt.Sprintf("stillfs_sdk_it_%d", time.Now().UnixNano())
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
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		_, _ = admin.ExecContext(context.Background(), `DROP SCHEMA "`+schema+`" CASCADE`)
		_ = admin.Close()
		t.Fatalf("connect to isolated test schema: %v", err)
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

func hasBucket(buckets []types.Bucket, name string) bool {
	for _, bucket := range buckets {
		if aws.ToString(bucket.Name) == name {
			return true
		}
	}
	return false
}

func apiCode(err error) string {
	var apiError smithy.APIError
	if errors.As(err, &apiError) {
		return apiError.ErrorCode()
	}
	return ""
}
