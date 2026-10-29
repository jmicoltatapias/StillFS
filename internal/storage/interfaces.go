// Package storage defines the boundary between object bytes and metadata.
package storage

import (
	"context"
	"errors"
	"io"
	"time"
)

var (
	ErrBucketNotFound    = errors.New("bucket not found")
	ErrInvalidBucketName = errors.New("invalid bucket name")
	ErrObjectNotFound    = errors.New("object metadata not found")
	ErrContentNotFound   = errors.New("object content not found")
	ErrAlreadyExists     = errors.New("metadata record already exists")
	ErrBucketNotEmpty    = errors.New("bucket is not empty")
	ErrConflict          = errors.New("metadata integrity conflict")
	ErrInvalidMetadata   = errors.New("invalid object metadata")
	ErrInvalidPageLimit  = errors.New("invalid page limit")
)

// ObjectID identifies an object by bucket and key.
type ObjectID struct {
	Bucket string
	Key    string
}

// ObjectInfo describes content persisted by an ObjectStore. Reference is
// opaque and must not be derived from bucket names or object keys.
type ObjectInfo struct {
	Reference         string
	Size              int64
	ChecksumAlgorithm string
	Checksum          string
}

// ObjectStore persists object bytes independently from metadata storage.
// Implementations must stream data and must not buffer entire objects.
type ObjectStore interface {
	Put(ctx context.Context, content io.Reader) (ObjectInfo, error)
	Open(ctx context.Context, reference string) (io.ReadCloser, error)
	Stat(ctx context.Context, reference string) (ObjectInfo, error)
	Delete(ctx context.Context, reference string) error
}

// ObjectMetadata contains the initial metadata boundary; it is not a database
// schema and may evolve before object operations are implemented.
type ObjectMetadata struct {
	ID         int64
	Bucket     string
	Key        string
	StorageRef string
	// ContentLength is the number of stored object bytes.
	ContentLength int64
	// ContentType is empty when unknown.
	ContentType string
	// ETag is an opaque value; it is not assumed to be an MD5 digest.
	ETag string
	// ChecksumAlgorithm and Checksum are empty together when no checksum exists.
	ChecksumAlgorithm string
	Checksum          string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// Bucket is metadata about one logical namespace for objects.
type Bucket struct {
	ID        int64
	Name      string
	CreatedAt time.Time
}

// BucketPageRequest uses the last bucket name as a keyset cursor.
type BucketPageRequest struct {
	After string
	Limit int
}

// BucketPage is ordered by bucket name. NextAfter is empty on the last page.
type BucketPage struct {
	Items     []Bucket
	NextAfter string
}

// ObjectPageRequest uses the last object key as a keyset cursor within a
// bucket and prefix.
type ObjectPageRequest struct {
	Prefix string
	After  string
	Limit  int
}

// ObjectPage is ordered by object key. NextAfter is empty on the last page.
type ObjectPage struct {
	Items     []ObjectMetadata
	NextAfter string
}

// BucketStore persists bucket metadata.
type BucketStore interface {
	CreateBucket(ctx context.Context, name string) (Bucket, error)
	GetBucket(ctx context.Context, id int64) (Bucket, error)
	GetBucketByName(ctx context.Context, name string) (Bucket, error)
	ListBuckets(ctx context.Context, request BucketPageRequest) (BucketPage, error)
	DeleteBucket(ctx context.Context, name string) error
}

// MetadataStore persists object metadata, never object bytes. Put is
// create-only; Update requires an existing bucket/key pair.
type MetadataStore interface {
	Get(ctx context.Context, id ObjectID) (ObjectMetadata, error)
	Put(ctx context.Context, id ObjectID, metadata ObjectMetadata) error
	Update(ctx context.Context, id ObjectID, metadata ObjectMetadata) (ObjectMetadata, error)
	List(ctx context.Context, bucket string, request ObjectPageRequest) (ObjectPage, error)
	Delete(ctx context.Context, id ObjectID) error
	IsStorageRefReferenced(ctx context.Context, reference string) (bool, error)
}
