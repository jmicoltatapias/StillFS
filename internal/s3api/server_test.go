package s3api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmicoltatapias/stillfs/internal/logging"
	"github.com/jmicoltatapias/stillfs/internal/objectservice"
	"github.com/jmicoltatapias/stillfs/internal/sigv4"
	"github.com/jmicoltatapias/stillfs/internal/storage"
	"github.com/jmicoltatapias/stillfs/internal/storage/local"
)

const (
	testAccessKey = "SFS-api-test-access"
	testSecret    = "test-secret-never-returned"
)

func TestSignedS3BucketAndObjectFlow(t *testing.T) {
	server, data := newTestServer(t)
	create := signedRequest(t, http.MethodPut, "http://stillfs.test/test-bucket", nil, nil)
	created := httptest.NewRecorder()
	server.ServeHTTP(created, create)
	if created.Code != http.StatusOK {
		t.Fatalf("create bucket = %d %s", created.Code, created.Body.String())
	}
	duplicate := httptest.NewRecorder()
	server.ServeHTTP(duplicate, signedRequest(t, http.MethodPut, "http://stillfs.test/test-bucket", nil, nil))
	if duplicate.Code != http.StatusConflict || !strings.Contains(duplicate.Body.String(), "BucketAlreadyExists") {
		t.Fatalf("duplicate bucket = %d %s", duplicate.Code, duplicate.Body.String())
	}
	headBucket := httptest.NewRecorder()
	server.ServeHTTP(headBucket, signedRequest(t, http.MethodHead, "http://stillfs.test/test-bucket", nil, nil))
	if headBucket.Code != http.StatusOK {
		t.Fatalf("HEAD bucket = %d %s", headBucket.Code, headBucket.Body.String())
	}
	root := httptest.NewRecorder()
	server.ServeHTTP(root, signedRequest(t, http.MethodGet, "http://stillfs.test/", nil, nil))
	if root.Code != http.StatusOK || !strings.Contains(root.Body.String(), "<Name>test-bucket</Name>") {
		t.Fatalf("list buckets = %d %s", root.Code, root.Body.String())
	}

	putObject := func(key, content string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		request := signedRequest(t, http.MethodPut, "http://stillfs.test/test-bucket/"+key, []byte(content), http.Header{"Content-Type": []string{"text/plain"}})
		server.ServeHTTP(recorder, request)
		return recorder
	}
	put := putObject("dir/a/file.txt", "first payload")
	if put.Code != http.StatusOK || put.Header().Get("ETag") == "" {
		t.Fatalf("PUT object = %d %#v %s", put.Code, put.Header(), put.Body.String())
	}
	if second := putObject("dir/a/file.txt", "replacement payload"); second.Code != http.StatusOK {
		t.Fatalf("replace object = %d %s", second.Code, second.Body.String())
	}
	if second := putObject("dir/b.txt", "other"); second.Code != http.StatusOK {
		t.Fatalf("PUT second object = %d %s", second.Code, second.Body.String())
	}
	if doubleSlash := putObject("a//b", "slashes"); doubleSlash.Code != http.StatusOK {
		t.Fatalf("PUT key with repeated slash = %d %s", doubleSlash.Code, doubleSlash.Body.String())
	}
	doubleSlashGet := httptest.NewRecorder()
	server.ServeHTTP(doubleSlashGet, signedRequest(t, http.MethodGet, "http://stillfs.test/test-bucket/a//b", nil, nil))
	if doubleSlashGet.Code != http.StatusOK || doubleSlashGet.Body.String() != "slashes" {
		t.Fatalf("GET key with repeated slash = %d %q", doubleSlashGet.Code, doubleSlashGet.Body.String())
	}

	get := httptest.NewRecorder()
	server.ServeHTTP(get, signedRequest(t, http.MethodGet, "http://stillfs.test/test-bucket/dir/a/file.txt", nil, nil))
	if get.Code != http.StatusOK || get.Body.String() != "replacement payload" || get.Header().Get("Content-Length") != "19" {
		t.Fatalf("GET object = %d headers=%#v content=%q", get.Code, get.Header(), get.Body.String())
	}
	head := httptest.NewRecorder()
	server.ServeHTTP(head, signedRequest(t, http.MethodHead, "http://stillfs.test/test-bucket/dir/a/file.txt", nil, nil))
	if head.Code != http.StatusOK || head.Header().Get("Content-Type") != "text/plain" || head.Header().Get("ETag") == "" || head.Header().Get("x-amz-checksum-sha256") == "" || head.Header().Get("Content-Length") != "19" {
		t.Fatalf("HEAD object = %d headers=%#v", head.Code, head.Header())
	}

	listURL := "http://stillfs.test/test-bucket?list-type=2&prefix=dir%2F&delimiter=%2F&max-keys=1"
	page1 := httptest.NewRecorder()
	server.ServeHTTP(page1, signedRequest(t, http.MethodGet, listURL, nil, nil))
	if page1.Code != http.StatusOK || !strings.Contains(page1.Body.String(), "<CommonPrefixes><Prefix>dir/a/</Prefix></CommonPrefixes>") || !strings.Contains(page1.Body.String(), "<IsTruncated>true</IsTruncated>") {
		t.Fatalf("ListObjectsV2 first page = %d %s", page1.Code, page1.Body.String())
	}
	var pageOne struct {
		Next string `xml:"NextContinuationToken"`
	}
	if err := xmlUnmarshal(page1.Body.Bytes(), &pageOne); err != nil || pageOne.Next == "" {
		t.Fatalf("decode next token = %q, %v", pageOne.Next, err)
	}
	page2URL := listURL + "&continuation-token=" + urlQueryEscape(pageOne.Next)
	page2 := httptest.NewRecorder()
	server.ServeHTTP(page2, signedRequest(t, http.MethodGet, page2URL, nil, nil))
	if page2.Code != http.StatusOK || !strings.Contains(page2.Body.String(), "<Key>dir/b.txt</Key>") || strings.Contains(page2.Body.String(), "dir/a/") {
		t.Fatalf("ListObjectsV2 second page = %d %s", page2.Code, page2.Body.String())
	}

	nonempty := httptest.NewRecorder()
	server.ServeHTTP(nonempty, signedRequest(t, http.MethodDelete, "http://stillfs.test/test-bucket", nil, nil))
	if nonempty.Code != http.StatusConflict || !strings.Contains(nonempty.Body.String(), "BucketNotEmpty") {
		t.Fatalf("delete nonempty bucket = %d %s", nonempty.Code, nonempty.Body.String())
	}
	for _, key := range []string{"dir/a/file.txt", "dir/b.txt", "a//b"} {
		deleted := httptest.NewRecorder()
		server.ServeHTTP(deleted, signedRequest(t, http.MethodDelete, "http://stillfs.test/test-bucket/"+key, nil, nil))
		if deleted.Code != http.StatusNoContent {
			t.Fatalf("delete object %q = %d %s", key, deleted.Code, deleted.Body.String())
		}
	}
	deleteBucket := httptest.NewRecorder()
	server.ServeHTTP(deleteBucket, signedRequest(t, http.MethodDelete, "http://stillfs.test/test-bucket", nil, nil))
	if deleteBucket.Code != http.StatusNoContent {
		t.Fatalf("delete empty bucket = %d %s", deleteBucket.Code, deleteBucket.Body.String())
	}
	if len(data.objects) != 0 {
		t.Fatalf("metadata rows remain after deletes: %d", len(data.objects))
	}
}

func TestS3AuthenticationErrorsAndOwnershipIsolation(t *testing.T) {
	server, _ := newTestServer(t)
	request := httptest.NewRequest(http.MethodGet, "http://stillfs.test/", nil)
	missing := httptest.NewRecorder()
	server.ServeHTTP(missing, request)
	if missing.Code != http.StatusForbidden || !strings.Contains(missing.Body.String(), "AccessDenied") {
		t.Fatalf("unsigned request = %d %s", missing.Code, missing.Body.String())
	}
	if strings.Contains(missing.Body.String(), testSecret) {
		t.Fatal("error response exposed credential secret")
	}
	wrong := signedRequest(t, http.MethodGet, "http://stillfs.test/", nil, nil)
	wrongAuthorization := wrong.Header.Get("Authorization")
	wrong.Header.Set("Authorization", wrongAuthorization[:len(wrongAuthorization)-1]+"0")
	badSignature := httptest.NewRecorder()
	server.ServeHTTP(badSignature, wrong)
	if badSignature.Code != http.StatusForbidden || !strings.Contains(badSignature.Body.String(), "SignatureDoesNotMatch") {
		t.Fatalf("bad signature = %d %s", badSignature.Code, badSignature.Body.String())
	}

	create := httptest.NewRecorder()
	server.ServeHTTP(create, signedRequest(t, http.MethodPut, "http://stillfs.test/private-bucket", nil, nil))
	if create.Code != http.StatusOK {
		t.Fatal(create.Body.String())
	}
	privateObject := httptest.NewRecorder()
	server.ServeHTTP(privateObject, signedRequest(t, http.MethodPut, "http://stillfs.test/private-bucket/secret-key", []byte("private"), nil))
	if privateObject.Code != http.StatusOK {
		t.Fatalf("create private object = %d %s", privateObject.Code, privateObject.Body.String())
	}
	other := signedRequestWithAccessKey(t, http.MethodHead, "http://stillfs.test/private-bucket", nil, nil, "SFS-other", testSecret)
	otherRecorder := httptest.NewRecorder()
	server.ServeHTTP(otherRecorder, other)
	if otherRecorder.Code != http.StatusNotFound || otherRecorder.Body.Len() != 0 || otherRecorder.Header().Get("Content-Type") != "application/xml" {
		t.Fatalf("foreign bucket HEAD = %d headers=%#v body=%s", otherRecorder.Code, otherRecorder.Header(), otherRecorder.Body.String())
	}
	if strings.Contains(otherRecorder.Body.String(), testSecret) {
		t.Fatal("error response exposed credential secret")
	}
	foreignObject := httptest.NewRecorder()
	server.ServeHTTP(foreignObject, signedRequestWithAccessKey(t, http.MethodGet, "http://stillfs.test/private-bucket/secret-key", nil, nil, "SFS-other", testSecret))
	if foreignObject.Code != http.StatusNotFound || !strings.Contains(foreignObject.Body.String(), "NoSuchBucket") || strings.Contains(foreignObject.Body.String(), "<Message>private</Message>") {
		t.Fatalf("foreign object GET = %d %s", foreignObject.Code, foreignObject.Body.String())
	}
}

func TestS3RejectsInvalidBucketAndPayloadMismatchWithoutPublishing(t *testing.T) {
	server, data := newTestServer(t)
	badBucket := httptest.NewRecorder()
	server.ServeHTTP(badBucket, signedRequest(t, http.MethodPut, "http://stillfs.test/ab", nil, nil))
	if badBucket.Code != http.StatusBadRequest || !strings.Contains(badBucket.Body.String(), "InvalidBucketName") {
		t.Fatalf("invalid bucket = %d %s", badBucket.Code, badBucket.Body.String())
	}
	server.ServeHTTP(httptest.NewRecorder(), signedRequest(t, http.MethodPut, "http://stillfs.test/test-bucket", nil, nil))
	body := []byte("actual payload")
	request := signedRequest(t, http.MethodPut, "http://stillfs.test/test-bucket/key", []byte("different"), nil)
	request.Body = io.NopCloser(bytes.NewReader(body))
	request.ContentLength = int64(len(body))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "XAmzContentSHA256Mismatch") {
		t.Fatalf("mismatched payload = %d %s", response.Code, response.Body.String())
	}
	if len(data.objects) != 0 {
		t.Fatalf("payload mismatch registered metadata: %#v", data.objects)
	}
}

func TestS3EmptyLargeAndOversizedObjects(t *testing.T) {
	server, data := newTestServer(t)
	server.ServeHTTP(httptest.NewRecorder(), signedRequest(t, http.MethodPut, "http://stillfs.test/test-bucket", nil, nil))
	emptyPut := httptest.NewRecorder()
	server.ServeHTTP(emptyPut, signedRequest(t, http.MethodPut, "http://stillfs.test/test-bucket/empty", nil, nil))
	if emptyPut.Code != http.StatusOK {
		t.Fatalf("empty PUT = %d %s", emptyPut.Code, emptyPut.Body.String())
	}
	emptyGet := httptest.NewRecorder()
	server.ServeHTTP(emptyGet, signedRequest(t, http.MethodGet, "http://stillfs.test/test-bucket/empty", nil, nil))
	if emptyGet.Code != http.StatusOK || emptyGet.Body.Len() != 0 || emptyGet.Header().Get("Content-Length") != "0" {
		t.Fatalf("empty GET = %d headers=%#v body=%q", emptyGet.Code, emptyGet.Header(), emptyGet.Body.String())
	}
	large := bytes.Repeat([]byte{0x5a}, 3<<20)
	largePut := httptest.NewRecorder()
	server.ServeHTTP(largePut, signedRequest(t, http.MethodPut, "http://stillfs.test/test-bucket/large", large, nil))
	if largePut.Code != http.StatusOK {
		t.Fatalf("large PUT = %d %s", largePut.Code, largePut.Body.String())
	}
	largeGet := httptest.NewRecorder()
	server.ServeHTTP(largeGet, signedRequest(t, http.MethodGet, "http://stillfs.test/test-bucket/large", nil, nil))
	if largeGet.Code != http.StatusOK || !bytes.Equal(largeGet.Body.Bytes(), large) {
		t.Fatalf("large GET = %d body length=%d", largeGet.Code, largeGet.Body.Len())
	}

	server.maxObjectBytes = 5
	tooLarge := httptest.NewRecorder()
	server.ServeHTTP(tooLarge, signedRequest(t, http.MethodPut, "http://stillfs.test/test-bucket/too-large", []byte("123456"), nil))
	if tooLarge.Code != http.StatusBadRequest || !strings.Contains(tooLarge.Body.String(), "EntityTooLarge") {
		t.Fatalf("oversized PUT = %d %s", tooLarge.Code, tooLarge.Body.String())
	}
	if _, err := data.Get(context.Background(), storage.ObjectID{Bucket: "test-bucket", Key: "too-large"}); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Fatalf("oversized body registered metadata: %v", err)
	}
}

func TestS3ConcurrentReplacementsRemainReadable(t *testing.T) {
	server, data := newTestServer(t)
	server.ServeHTTP(httptest.NewRecorder(), signedRequest(t, http.MethodPut, "http://stillfs.test/test-bucket", nil, nil))
	const writers = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	errCh := make(chan int, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			content := []byte(fmt.Sprintf("payload-%d", i))
			response := httptest.NewRecorder()
			server.ServeHTTP(response, signedRequest(t, http.MethodPut, "http://stillfs.test/test-bucket/key", content, nil))
			errCh <- response.Code
		}(i)
	}
	close(start)
	wg.Wait()
	close(errCh)
	for status := range errCh {
		if status != http.StatusOK {
			t.Fatalf("concurrent PUT status = %d", status)
		}
	}
	metadata, err := data.Get(context.Background(), storage.ObjectID{Bucket: "test-bucket", Key: "key"})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, signedRequest(t, http.MethodGet, "http://stillfs.test/test-bucket/key", nil, nil))
	if response.Code != http.StatusOK || int64(response.Body.Len()) != metadata.ContentLength {
		t.Fatalf("read final concurrent content status=%d len=%d metadata=%d", response.Code, response.Body.Len(), metadata.ContentLength)
	}
}

func newTestServer(t *testing.T) (*Server, *memoryStore) {
	t.Helper()
	data := &memoryStore{buckets: make(map[string]storage.Bucket), objects: make(map[storage.ObjectID]storage.ObjectMetadata), now: time.Now}
	store, err := local.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	logger := logging.New(io.Discard)
	objects := objectservice.New(store, data, logger)
	server := New(sigv4.Authenticator{
		Region: "us-east-1", MaxClockSkew: 5 * time.Minute, Resolver: apiResolver{},
	}, data, data, objects, 8<<20, 8<<10, logger)
	return server, data
}

type apiResolver struct{}

func (apiResolver) Resolve(_ context.Context, accessKeyID string) (sigv4.Credential, error) {
	if accessKeyID == "SFS-other" {
		return sigv4.Credential{Secret: testSecret, PrincipalID: 22, Active: true}, nil
	}
	if accessKeyID != testAccessKey {
		return sigv4.Credential{}, sigv4.ErrUnknownCredential
	}
	return sigv4.Credential{Secret: testSecret, PrincipalID: 11, Active: true}, nil
}

func signedRequest(t *testing.T, method, target string, body []byte, headers http.Header) *http.Request {
	return signedRequestWithAccessKey(t, method, target, body, headers, testAccessKey, testSecret)
}

func signedRequestWithAccessKey(t *testing.T, method, target string, body []byte, headers http.Header, accessKey, secret string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(method, target, bytes.NewReader(body))
	if body == nil {
		request.Body = http.NoBody
		request.ContentLength = 0
	}
	request.Host = "stillfs.test"
	for name, values := range headers {
		request.Header[name] = append([]string(nil), values...)
	}
	payload := sha256.Sum256(body)
	payloadHash := hex.EncodeToString(payload[:])
	request.Header.Set("X-Amz-Date", time.Now().UTC().Format("20060102T150405Z"))
	request.Header.Set("X-Amz-Content-Sha256", payloadHash)
	signed := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	if request.Header.Get("Content-Type") != "" {
		signed = append(signed, "content-type")
		sort.Strings(signed)
	}
	canonical, err := sigv4.CanonicalRequest(request, signed, payloadHash)
	if err != nil {
		t.Fatal(err)
	}
	timestamp := request.Header.Get("X-Amz-Date")
	signature := sigv4.Signature(secret, timestamp, "us-east-1", "s3", canonical)
	request.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s/us-east-1/s3/aws4_request,SignedHeaders=%s,Signature=%s", accessKey, timestamp[:8], strings.Join(signed, ";"), signature))
	return request
}

type memoryStore struct {
	storage.BucketStore
	storage.MetadataStore
	mu      sync.Mutex
	buckets map[string]storage.Bucket
	objects map[storage.ObjectID]storage.ObjectMetadata
	nextID  int64
	now     func() time.Time
}

func (m *memoryStore) CreateBucket(_ context.Context, ownerID int64, name string) (storage.Bucket, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.buckets[name]; exists {
		return storage.Bucket{}, storage.ErrAlreadyExists
	}
	if !validBucketName(name) {
		return storage.Bucket{}, storage.ErrInvalidBucketName
	}
	m.nextID++
	bucket := storage.Bucket{ID: m.nextID, OwnerID: ownerID, Name: name, CreatedAt: m.now()}
	m.buckets[name] = bucket
	return bucket, nil
}

func (m *memoryStore) GetBucket(ctx context.Context, id int64) (storage.Bucket, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, bucket := range m.buckets {
		if bucket.ID == id {
			return bucket, nil
		}
	}
	return storage.Bucket{}, storage.ErrBucketNotFound
}

func (m *memoryStore) GetBucketByName(_ context.Context, name string) (storage.Bucket, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	bucket, ok := m.buckets[name]
	if !ok {
		return storage.Bucket{}, storage.ErrBucketNotFound
	}
	return bucket, nil
}

func (m *memoryStore) ListBuckets(_ context.Context, req storage.BucketPageRequest) (storage.BucketPage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	all := make([]storage.Bucket, 0)
	for _, bucket := range m.buckets {
		if bucket.OwnerID == req.OwnerID && bucket.Name > req.After {
			all = append(all, bucket)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Name < all[j].Name })
	if len(all) > req.Limit {
		page := storage.BucketPage{Items: all[:req.Limit], NextAfter: all[req.Limit-1].Name}
		return page, nil
	}
	return storage.BucketPage{Items: all}, nil
}

func (m *memoryStore) DeleteBucket(_ context.Context, ownerID int64, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	bucket, ok := m.buckets[name]
	if !ok || bucket.OwnerID != ownerID {
		return storage.ErrBucketNotFound
	}
	for id := range m.objects {
		if id.Bucket == name {
			return storage.ErrBucketNotEmpty
		}
	}
	delete(m.buckets, name)
	return nil
}

func (m *memoryStore) Get(_ context.Context, id storage.ObjectID) (storage.ObjectMetadata, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	item, ok := m.objects[id]
	if !ok {
		return storage.ObjectMetadata{}, storage.ErrObjectNotFound
	}
	return item, nil
}

func (m *memoryStore) Put(_ context.Context, id storage.ObjectID, item storage.ObjectMetadata) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.buckets[id.Bucket]; !ok {
		return storage.ErrBucketNotFound
	}
	if _, ok := m.objects[id]; ok {
		return storage.ErrAlreadyExists
	}
	m.nextID++
	item.ID, item.Bucket, item.Key = m.nextID, id.Bucket, id.Key
	item.CreatedAt, item.UpdatedAt = m.now(), m.now()
	m.objects[id] = item
	return nil
}

func (m *memoryStore) Update(_ context.Context, id storage.ObjectID, item storage.ObjectMetadata) (storage.ObjectMetadata, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	old, ok := m.objects[id]
	if !ok {
		return storage.ObjectMetadata{}, storage.ErrObjectNotFound
	}
	item.ID, item.Bucket, item.Key, item.CreatedAt = old.ID, id.Bucket, id.Key, old.CreatedAt
	item.UpdatedAt = m.now()
	m.objects[id] = item
	return item, nil
}

func (m *memoryStore) List(_ context.Context, bucket string, req storage.ObjectPageRequest) (storage.ObjectPage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	all := make([]storage.ObjectMetadata, 0)
	for id, item := range m.objects {
		if id.Bucket == bucket && strings.HasPrefix(id.Key, req.Prefix) && id.Key > req.After {
			all = append(all, item)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Key < all[j].Key })
	if len(all) > req.Limit {
		return storage.ObjectPage{Items: all[:req.Limit], NextAfter: all[req.Limit-1].Key}, nil
	}
	return storage.ObjectPage{Items: all}, nil
}

func (m *memoryStore) Delete(_ context.Context, id storage.ObjectID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.objects[id]; !ok {
		return storage.ErrObjectNotFound
	}
	delete(m.objects, id)
	return nil
}

func (m *memoryStore) IsStorageRefReferenced(_ context.Context, reference string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, item := range m.objects {
		if item.StorageRef == reference {
			return true, nil
		}
	}
	return false, nil
}

func xmlUnmarshal(data []byte, value interface{}) error { return xml.Unmarshal(data, value) }

func urlQueryEscape(value string) string { return url.QueryEscape(value) }
