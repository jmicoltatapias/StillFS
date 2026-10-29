// Package s3api implements the documented path-style S3 API subset.
package s3api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jmicoltatapias/stillfs/internal/authorization"
	"github.com/jmicoltatapias/stillfs/internal/objectservice"
	"github.com/jmicoltatapias/stillfs/internal/sigv4"
	"github.com/jmicoltatapias/stillfs/internal/storage"
)

const xmlNamespace = "http://s3.amazonaws.com/doc/2006-03-01/"

var bucketNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

type Logger interface {
	Error(message string, fields map[string]interface{})
}

type Server struct {
	auth       sigv4.Authenticator
	authorizer interface {
		Authorize(context.Context, sigv4.Authenticated, string) error
	}
	buckets        storage.BucketStore
	metadata       storage.MetadataStore
	objects        *objectservice.Service
	maxObjectBytes int64
	maxMetadata    int
	logger         Logger
}

func New(auth sigv4.Authenticator, buckets storage.BucketStore, metadata storage.MetadataStore, objects *objectservice.Service, maxObjectBytes int64, maxMetadataBytes int, logger Logger, authorizer interface {
	Authorize(context.Context, sigv4.Authenticated, string) error
}) *Server {
	return &Server{auth: auth, authorizer: authorizer, buckets: buckets, metadata: metadata, objects: objects, maxObjectBytes: maxObjectBytes, maxMetadata: maxMetadataBytes, logger: logger}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestID(w)
	identity, err := s.auth.Authenticate(r.Context(), r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	path, err := url.PathUnescape(r.URL.EscapedPath())
	if err != nil || path == "" || path[0] != '/' {
		writeS3Error(w, r, http.StatusBadRequest, "InvalidURI", "The request URI is invalid.")
		return
	}
	if path == "/" {
		if !s.authorize(w, r, identity, authorization.ListBuckets) {
			return
		}
		s.handleService(w, r, identity.PrincipalID)
		return
	}
	path = path[1:]
	bucketName, key, hasKey := splitResource(path)
	if !validBucketName(bucketName) {
		writeS3Error(w, r, http.StatusBadRequest, "InvalidBucketName", "The specified bucket is not valid.")
		return
	}
	if hasKey {
		if !validObjectKey(key) {
			writeS3Error(w, r, http.StatusBadRequest, "InvalidArgument", "The object key is not supported by this API subset.")
			return
		}
		permission := map[string]string{http.MethodGet: authorization.GetObject, http.MethodHead: authorization.GetObject, http.MethodPut: authorization.PutObject, http.MethodDelete: authorization.DeleteObject}[r.Method]
		if permission == "" {
			methodNotAllowed(w, r, http.MethodGet+", "+http.MethodHead+", "+http.MethodPut+", "+http.MethodDelete)
			return
		}
		if !s.authorize(w, r, identity, permission) {
			return
		}
		s.handleObject(w, r, identity.PrincipalID, bucketName, key)
		return
	}
	permission := map[string]string{http.MethodGet: authorization.ListObjects, http.MethodHead: authorization.ListObjects, http.MethodPut: authorization.CreateBucket, http.MethodDelete: authorization.DeleteBucket}[r.Method]
	if permission == "" {
		methodNotAllowed(w, r, http.MethodGet+", "+http.MethodHead+", "+http.MethodPut+", "+http.MethodDelete)
		return
	}
	if !s.authorize(w, r, identity, permission) {
		return
	}
	s.handleBucket(w, r, identity.PrincipalID, bucketName)
}

func (s *Server) authorize(w http.ResponseWriter, r *http.Request, identity sigv4.Authenticated, permission string) bool {
	if s.authorizer == nil {
		writeS3Error(w, r, http.StatusForbidden, "AccessDenied", "The request is not authorized.")
		return false
	}
	if err := s.authorizer.Authorize(r.Context(), identity, permission); err != nil {
		if errors.Is(err, authorization.ErrDenied) {
			writeS3Error(w, r, http.StatusForbidden, "AccessDenied", "The request is not authorized.")
		} else {
			writeError(w, r, err)
		}
		return false
	}
	return true
}

func (s *Server) handleService(w http.ResponseWriter, r *http.Request, principalID int64) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, r, http.MethodGet)
		return
	}
	if !validOperationQuery(r, "ListBuckets") || requestHasBody(r) {
		writeS3Error(w, r, http.StatusBadRequest, "InvalidRequest", "Service listing does not accept query parameters or a request body.")
		return
	}
	items := make([]storage.Bucket, 0)
	after := ""
	for {
		page, err := s.buckets.ListBuckets(r.Context(), storage.BucketPageRequest{OwnerID: principalID, After: after, Limit: 1000})
		if err != nil {
			writeError(w, r, err)
			return
		}
		items = append(items, page.Items...)
		if page.NextAfter == "" {
			break
		}
		after = page.NextAfter
	}
	listed := make([]listedBucket, 0, len(items))
	for _, bucket := range items {
		listed = append(listed, listedBucket{Name: bucket.Name, CreationDate: bucket.CreatedAt.UTC().Format(time.RFC3339)})
	}
	writeXML(w, r, http.StatusOK, listBucketsResult{XMLNS: xmlNamespace, Owner: ownerResult{ID: strconv.FormatInt(principalID, 10)}, Buckets: bucketsResult{Items: listed}})
}

func (s *Server) handleBucket(w http.ResponseWriter, r *http.Request, principalID int64, name string) {
	if r.Method == http.MethodGet {
		query, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil || query.Get("list-type") != "2" || len(query["list-type"]) != 1 {
			writeS3Error(w, r, http.StatusBadRequest, "InvalidRequest", "Bucket GET requires list-type=2.")
			return
		}
		if requestHasBody(r) {
			writeS3Error(w, r, http.StatusBadRequest, "InvalidRequest", "Bucket listing does not accept a request body.")
			return
		}
		page, err := s.listObjects(r.Context(), principalID, name, query)
		if err != nil {
			writeError(w, r, err)
			return
		}
		writeXML(w, r, http.StatusOK, page)
		return
	}
	operation := map[string]string{
		http.MethodPut: "CreateBucket", http.MethodHead: "HeadBucket", http.MethodDelete: "DeleteBucket",
	}[r.Method]
	if operation == "" {
		methodNotAllowed(w, r, http.MethodPut+", "+http.MethodHead+", "+http.MethodDelete)
		return
	}
	if !validOperationQuery(r, operation) || requestHasBody(r) {
		writeS3Error(w, r, http.StatusBadRequest, "InvalidRequest", "The operation does not accept these query parameters or a request body.")
		return
	}
	switch r.Method {
	case http.MethodPut:
		_, err := s.buckets.CreateBucket(r.Context(), principalID, name)
		if err != nil {
			writeError(w, r, err)
			return
		}
		w.Header().Set("Location", "/"+name)
		w.WriteHeader(http.StatusOK)
	case http.MethodHead:
		if _, err := s.ownedBucket(r.Context(), principalID, name); err != nil {
			writeError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusOK)
	case http.MethodDelete:
		if err := s.buckets.DeleteBucket(r.Context(), principalID, name); err != nil {
			writeError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) handleObject(w http.ResponseWriter, r *http.Request, principalID int64, bucket, key string) {
	if _, err := s.ownedBucket(r.Context(), principalID, bucket); err != nil {
		writeError(w, r, err)
		return
	}
	operation := map[string]string{
		http.MethodPut: "PutObject", http.MethodGet: "GetObject", http.MethodHead: "HeadObject", http.MethodDelete: "DeleteObject",
	}[r.Method]
	if operation == "" {
		methodNotAllowed(w, r, http.MethodPut+", "+http.MethodGet+", "+http.MethodHead+", "+http.MethodDelete)
		return
	}
	if !validOperationQuery(r, operation) {
		writeS3Error(w, r, http.StatusBadRequest, "InvalidRequest", "Object subresources and query parameters are not implemented.")
		return
	}
	id := storage.ObjectID{Bucket: bucket, Key: key}
	switch r.Method {
	case http.MethodPut:
		s.handlePutObject(w, r, id)
	case http.MethodGet:
		if requestHasBody(r) {
			writeS3Error(w, r, http.StatusBadRequest, "InvalidRequest", "GET does not accept a request body.")
			return
		}
		metadata, reader, err := s.objects.Open(r.Context(), id)
		if err != nil {
			writeError(w, r, err)
			return
		}
		defer reader.Close()
		setObjectHeaders(w.Header(), metadata)
		w.Header().Set("Content-Length", strconv.FormatInt(metadata.ContentLength, 10))
		w.WriteHeader(http.StatusOK)
		if err := streamResponse(w, reader, metadata); err != nil {
			s.logger.Error("S3 object stream failed", map[string]interface{}{"error": err.Error()})
			panic(http.ErrAbortHandler)
		}
	case http.MethodHead:
		if requestHasBody(r) {
			writeS3Error(w, r, http.StatusBadRequest, "InvalidRequest", "HEAD does not accept a request body.")
			return
		}
		metadata, reader, err := s.objects.Open(r.Context(), id)
		if err != nil {
			writeError(w, r, err)
			return
		}
		if err := reader.Close(); err != nil {
			writeError(w, r, err)
			return
		}
		setObjectHeaders(w.Header(), metadata)
		w.Header().Set("Content-Length", strconv.FormatInt(metadata.ContentLength, 10))
		w.WriteHeader(http.StatusOK)
	case http.MethodDelete:
		if requestHasBody(r) {
			writeS3Error(w, r, http.StatusBadRequest, "InvalidRequest", "DELETE does not accept a request body.")
			return
		}
		err := s.objects.Delete(r.Context(), id)
		if errors.Is(err, storage.ErrObjectNotFound) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if err != nil {
			writeError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) handlePutObject(w http.ResponseWriter, r *http.Request, id storage.ObjectID) {
	if unsupportedObjectMetadata(r.Header) {
		writeS3Error(w, r, http.StatusBadRequest, "InvalidRequest", "One or more object metadata headers are not supported.")
		return
	}
	contentType := r.Header.Get("Content-Type")
	checksumHeader := r.Header.Get("x-amz-checksum-sha256")
	if len(contentType)+len(checksumHeader) > s.maxMetadata {
		writeS3Error(w, r, http.StatusBadRequest, "MetadataTooLarge", "Object metadata exceeds the configured limit.")
		return
	}
	if checksumHeader != "" {
		provided, err := base64.StdEncoding.DecodeString(checksumHeader)
		expected, _ := hex.DecodeString(r.Header.Get("X-Amz-Content-Sha256"))
		if err != nil || len(provided) != sha256.Size || !bytes.Equal(provided, expected) {
			writeS3Error(w, r, http.StatusBadRequest, "BadDigest", "The checksum header does not match the signed payload digest.")
			return
		}
	}
	if r.Header.Get("Content-MD5") != "" {
		writeS3Error(w, r, http.StatusBadRequest, "InvalidRequest", "Content-MD5 is not supported; use the SigV4 SHA-256 payload hash.")
		return
	}
	if r.ContentLength > s.maxObjectBytes {
		writeS3Error(w, r, http.StatusBadRequest, "EntityTooLarge", "The object exceeds the configured size limit.")
		return
	}
	limited := &limitReadCloser{ReadCloser: r.Body, remaining: s.maxObjectBytes}
	verified, err := sigv4.NewVerifiedBody(limited, r.Header.Get("X-Amz-Content-Sha256"))
	if err != nil {
		writeS3Error(w, r, http.StatusBadRequest, "InvalidRequest", "The payload hash is invalid.")
		return
	}
	defer verified.Close()
	if err := s.objects.PutOrReplace(r.Context(), id, contentType, verified); err != nil {
		if errors.Is(err, errEntityTooLarge) {
			writeS3Error(w, r, http.StatusBadRequest, "EntityTooLarge", "The object exceeds the configured size limit.")
			return
		}
		if errors.Is(err, sigv4.ErrPayloadHashMismatch) {
			writeS3Error(w, r, http.StatusBadRequest, "XAmzContentSHA256Mismatch", "The payload does not match x-amz-content-sha256.")
			return
		}
		writeError(w, r, err)
		return
	}
	metadata, err := s.metadata.Get(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	w.Header().Set("ETag", metadata.ETag)
	w.WriteHeader(http.StatusOK)
}

var errEntityTooLarge = errors.New("request body exceeds configured object size limit")

type limitReadCloser struct {
	io.ReadCloser
	remaining int64
}

func (r *limitReadCloser) Read(buffer []byte) (int, error) {
	if r.remaining <= 0 {
		var extra [1]byte
		n, err := r.ReadCloser.Read(extra[:])
		if n > 0 {
			return 0, errEntityTooLarge
		}
		return 0, err
	}
	if int64(len(buffer)) > r.remaining {
		buffer = buffer[:r.remaining]
	}
	n, err := r.ReadCloser.Read(buffer)
	r.remaining -= int64(n)
	return n, err
}

func (s *Server) ownedBucket(ctx context.Context, principalID int64, name string) (storage.Bucket, error) {
	bucket, err := s.buckets.GetBucketByName(ctx, name)
	if err != nil || bucket.OwnerID != principalID {
		return storage.Bucket{}, storage.ErrBucketNotFound
	}
	return bucket, nil
}

func splitResource(path string) (bucket, key string, hasKey bool) {
	index := strings.IndexByte(path, '/')
	if index < 0 {
		return path, "", false
	}
	return path[:index], path[index+1:], true
}

func validBucketName(name string) bool {
	if !bucketNamePattern.MatchString(name) || strings.Contains(name, "..") || strings.Contains(name, ".-") || strings.Contains(name, "-.") || net.ParseIP(name) != nil {
		return false
	}
	return true
}

func validObjectKey(key string) bool {
	if key == "" || len(key) > 1024 || !utf8.ValidString(key) {
		return false
	}
	for _, value := range key {
		if (value >= 0 && value <= 8) || value == 11 || value == 12 || (value >= 14 && value <= 31) || value == 0xfffe || value == 0xffff {
			return false
		}
	}
	return true
}

func requestHasBody(r *http.Request) bool {
	return r.ContentLength > 0 || len(r.TransferEncoding) > 0
}

// validOperationQuery accepts the operation marker emitted by AWS SDK for Go
// v2. It is only a transport marker; it cannot select a different operation.
func validOperationQuery(r *http.Request, expected string) bool {
	if r.URL.RawQuery == "" {
		return true
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(query) != 1 || len(query["x-id"]) != 1 {
		return false
	}
	return query.Get("x-id") == expected
}

func unsupportedObjectMetadata(header http.Header) bool {
	for name := range header {
		lower := strings.ToLower(name)
		unsupportedAmz := strings.HasPrefix(lower, "x-amz-") && lower != "x-amz-content-sha256" && lower != "x-amz-date" && lower != "x-amz-checksum-sha256"
		if unsupportedAmz || lower == "cache-control" || lower == "content-disposition" || lower == "content-encoding" || lower == "expires" {
			return true
		}
	}
	return false
}

func setObjectHeaders(header http.Header, metadata storage.ObjectMetadata) {
	if metadata.ContentType != "" {
		header.Set("Content-Type", metadata.ContentType)
	}
	if metadata.ETag != "" {
		header.Set("ETag", metadata.ETag)
	}
	if metadata.ChecksumAlgorithm == "sha256" {
		checksum, err := hex.DecodeString(metadata.Checksum)
		if err == nil {
			header.Set("x-amz-checksum-sha256", base64.StdEncoding.EncodeToString(checksum))
		}
	}
}

func streamResponse(w io.Writer, reader io.Reader, metadata storage.ObjectMetadata) error {
	hasher := sha256.New()
	count, err := io.Copy(io.MultiWriter(w, hasher), reader)
	if err != nil {
		return err
	}
	if count != metadata.ContentLength {
		return fmt.Errorf("stored object length does not match metadata")
	}
	if metadata.ChecksumAlgorithm == "sha256" && hex.EncodeToString(hasher.Sum(nil)) != metadata.Checksum {
		return fmt.Errorf("stored object checksum does not match metadata")
	}
	return nil
}
