package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/jmicoltatapias/stillfs/internal/storage"
	"github.com/lib/pq"
)

const (
	defaultPageSize = 100
	maxPageSize     = 1000
)

// BucketRepository persists bucket identity and names.
type BucketRepository struct{ db *sql.DB }

// NewBucketRepository creates a bucket repository backed by db.
func NewBucketRepository(db *sql.DB) *BucketRepository { return &BucketRepository{db: db} }

// CreateBucket inserts a bucket. Duplicate names map to storage.ErrAlreadyExists.
func (r *BucketRepository) CreateBucket(ctx context.Context, ownerID int64, name string) (storage.Bucket, error) {
	if name == "" || ownerID <= 0 {
		return storage.Bucket{}, storage.ErrInvalidBucketName
	}
	var bucket storage.Bucket
	err := r.db.QueryRowContext(ctx, `
		INSERT INTO buckets(owner_id, name) VALUES ($1, $2)
		RETURNING id, owner_id, name, created_at`, ownerID, name).Scan(&bucket.ID, &bucket.OwnerID, &bucket.Name, &bucket.CreatedAt)
	if err != nil {
		return storage.Bucket{}, mapDBError(err)
	}
	return bucket, nil
}

// GetBucket finds a bucket by stable internal ID.
func (r *BucketRepository) GetBucket(ctx context.Context, id int64) (storage.Bucket, error) {
	return scanBucket(r.db.QueryRowContext(ctx, `SELECT id, COALESCE(owner_id, 0), name, created_at FROM buckets WHERE id = $1`, id))
}

// GetBucketByName finds a bucket by its unique name.
func (r *BucketRepository) GetBucketByName(ctx context.Context, name string) (storage.Bucket, error) {
	return scanBucket(r.db.QueryRowContext(ctx, `SELECT id, COALESCE(owner_id, 0), name, created_at FROM buckets WHERE name = $1`, name))
}

// ListBuckets returns a keyset-paginated page ordered by C-collated name.
func (r *BucketRepository) ListBuckets(ctx context.Context, request storage.BucketPageRequest) (storage.BucketPage, error) {
	limit, err := pageLimit(request.Limit)
	if err != nil {
		return storage.BucketPage{}, err
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, owner_id, name, created_at FROM buckets
		WHERE owner_id = $1 AND name > $2
		ORDER BY name
		LIMIT $3`, request.OwnerID, request.After, limit+1)
	if err != nil {
		return storage.BucketPage{}, mapDBError(err)
	}
	defer rows.Close()

	page := storage.BucketPage{Items: make([]storage.Bucket, 0, limit)}
	for rows.Next() {
		var bucket storage.Bucket
		if err := rows.Scan(&bucket.ID, &bucket.OwnerID, &bucket.Name, &bucket.CreatedAt); err != nil {
			return storage.BucketPage{}, mapDBError(err)
		}
		page.Items = append(page.Items, bucket)
	}
	if err := rows.Err(); err != nil {
		return storage.BucketPage{}, mapDBError(err)
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.NextAfter = page.Items[len(page.Items)-1].Name
	}
	return page, nil
}

// DeleteBucket removes an empty bucket only. The row lock and FK provide
// serialization against object registration; no object bytes are touched.
func (r *BucketRepository) DeleteBucket(ctx context.Context, ownerID int64, name string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return mapDBError(err)
	}
	defer tx.Rollback()
	var id int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM buckets WHERE name = $1 AND owner_id = $2 FOR UPDATE`, name, ownerID).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return storage.ErrBucketNotFound
		}
		return mapDBError(err)
	}
	var hasObjects bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM objects WHERE bucket_id = $1)`, id).Scan(&hasObjects); err != nil {
		return mapDBError(err)
	}
	if hasObjects {
		return storage.ErrBucketNotEmpty
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM buckets WHERE id = $1`, id); err != nil {
		var pgErr *pq.Error
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			return storage.ErrBucketNotEmpty
		}
		return mapDBError(err)
	}
	if err := tx.Commit(); err != nil {
		return mapDBError(err)
	}
	return nil
}

// MetadataRepository persists object metadata without storing object bytes.
type MetadataRepository struct{ db *sql.DB }

// NewMetadataRepository creates an object metadata repository backed by db.
func NewMetadataRepository(db *sql.DB) *MetadataRepository {
	return &MetadataRepository{db: db}
}

// Get finds metadata using bucket name and object key.
func (r *MetadataRepository) Get(ctx context.Context, id storage.ObjectID) (storage.ObjectMetadata, error) {
	return scanObject(r.db.QueryRowContext(ctx, objectSelect+`
		WHERE b.name = $1 AND o.object_key = $2`, id.Bucket, id.Key))
}

// Put creates object metadata. A repeated bucket/key returns ErrAlreadyExists.
func (r *MetadataRepository) Put(ctx context.Context, id storage.ObjectID, metadata storage.ObjectMetadata) error {
	if err := validateMetadata(metadata); err != nil {
		return err
	}
	result, err := r.db.ExecContext(ctx, `
		INSERT INTO objects(bucket_id, object_key, storage_ref, size_bytes, content_type,
		                    etag, checksum_algorithm, checksum_value)
		SELECT b.id, $2, $3, $4, NULLIF($5, ''), NULLIF($6, ''), NULLIF($7, ''), NULLIF($8, '')
		FROM buckets b WHERE b.name = $1`,
		id.Bucket, id.Key, metadata.StorageRef, metadata.ContentLength,
		metadata.ContentType, metadata.ETag, metadata.ChecksumAlgorithm, metadata.Checksum)
	if err != nil {
		return mapDBError(err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return mapDBError(err)
	}
	if inserted == 0 {
		return storage.ErrBucketNotFound
	}
	return nil
}

// Update changes stored metadata for an existing bucket/key pair and returns
// its stable ID and database timestamps.
func (r *MetadataRepository) Update(ctx context.Context, id storage.ObjectID, metadata storage.ObjectMetadata) (storage.ObjectMetadata, error) {
	if err := validateMetadata(metadata); err != nil {
		return storage.ObjectMetadata{}, err
	}
	updated, err := scanObject(r.db.QueryRowContext(ctx, `
		UPDATE objects o SET storage_ref = $3, size_bytes = $4,
		    content_type = NULLIF($5, ''), etag = NULLIF($6, ''),
		    checksum_algorithm = NULLIF($7, ''), checksum_value = NULLIF($8, ''),
		    updated_at = CURRENT_TIMESTAMP
		FROM buckets b
		WHERE o.bucket_id = b.id AND b.name = $1 AND o.object_key = $2
		RETURNING o.id, b.name, o.object_key, o.storage_ref, o.size_bytes,
		          COALESCE(o.content_type, ''), COALESCE(o.etag, ''),
		          COALESCE(o.checksum_algorithm, ''), COALESCE(o.checksum_value, ''),
		          o.created_at, o.updated_at`,
		id.Bucket, id.Key, metadata.StorageRef, metadata.ContentLength,
		metadata.ContentType, metadata.ETag, metadata.ChecksumAlgorithm, metadata.Checksum))
	if err != nil {
		return storage.ObjectMetadata{}, err
	}
	return updated, nil
}

// List returns metadata for a bucket/prefix in stable C-collated key order.
func (r *MetadataRepository) List(ctx context.Context, bucket string, request storage.ObjectPageRequest) (storage.ObjectPage, error) {
	limit, err := pageLimit(request.Limit)
	if err != nil {
		return storage.ObjectPage{}, err
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return storage.ObjectPage{}, mapDBError(err)
	}
	defer tx.Rollback()
	var bucketID int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM buckets WHERE name = $1`, bucket).Scan(&bucketID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return storage.ObjectPage{}, storage.ErrBucketNotFound
		}
		return storage.ObjectPage{}, mapDBError(err)
	}
	rows, err := tx.QueryContext(ctx, objectColumns+`
		FROM objects o JOIN buckets b ON b.id = o.bucket_id
		WHERE o.bucket_id = $1 AND o.object_key LIKE $2 ESCAPE E'\\'
		  AND o.object_key > $3
		ORDER BY o.object_key
		LIMIT $4`, bucketID, prefixPattern(request.Prefix), request.After, limit+1)
	if err != nil {
		return storage.ObjectPage{}, mapDBError(err)
	}
	page := storage.ObjectPage{Items: make([]storage.ObjectMetadata, 0, limit)}
	for rows.Next() {
		item, err := scanObjectRow(rows)
		if err != nil {
			_ = rows.Close()
			return storage.ObjectPage{}, err
		}
		page.Items = append(page.Items, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return storage.ObjectPage{}, mapDBError(err)
	}
	if err := rows.Close(); err != nil {
		return storage.ObjectPage{}, mapDBError(err)
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.NextAfter = page.Items[len(page.Items)-1].Key
	}
	if err := tx.Commit(); err != nil {
		return storage.ObjectPage{}, mapDBError(err)
	}
	return page, nil
}

// Delete removes only metadata. The associated storage reference is not acted on.
func (r *MetadataRepository) Delete(ctx context.Context, id storage.ObjectID) error {
	result, err := r.db.ExecContext(ctx, `
		DELETE FROM objects o USING buckets b
		WHERE o.bucket_id = b.id AND b.name = $1 AND o.object_key = $2`, id.Bucket, id.Key)
	if err != nil {
		return mapDBError(err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return mapDBError(err)
	}
	if count == 0 {
		return storage.ErrObjectNotFound
	}
	return nil
}

// IsStorageRefReferenced reports whether any object metadata currently points
// at reference. A coordinator must retain content when this check fails.
func (r *MetadataRepository) IsStorageRefReferenced(ctx context.Context, reference string) (bool, error) {
	var exists bool
	err := r.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM objects WHERE storage_ref = $1)`, reference).Scan(&exists)
	if err != nil {
		return false, mapDBError(err)
	}
	return exists, nil
}

// ListStorageReferences returns distinct published references in C-collated
// key order. It is intended for read-only consistency diagnostics; callers
// must not infer that an unreferenced result is safe to delete during writes.
func (r *MetadataRepository) ListStorageReferences(ctx context.Context, after string, requestedLimit int) ([]string, string, error) {
	limit, err := pageLimit(requestedLimit)
	if err != nil {
		return nil, "", err
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, "", mapDBError(err)
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `
		SELECT storage_ref FROM (
			SELECT DISTINCT storage_ref COLLATE "C" AS storage_ref
			FROM objects
			WHERE storage_ref COLLATE "C" > $1
		) refs
		ORDER BY storage_ref COLLATE "C"
		LIMIT $2`, after, limit+1)
	if err != nil {
		return nil, "", mapDBError(err)
	}
	defer rows.Close()
	references := make([]string, 0, limit)
	for rows.Next() {
		var reference string
		if err := rows.Scan(&reference); err != nil {
			return nil, "", mapDBError(err)
		}
		references = append(references, reference)
	}
	if err := rows.Err(); err != nil {
		return nil, "", mapDBError(err)
	}
	if err := rows.Close(); err != nil {
		return nil, "", mapDBError(err)
	}
	if len(references) <= limit {
		if err := tx.Commit(); err != nil {
			return nil, "", mapDBError(err)
		}
		return references, "", nil
	}
	references = references[:limit]
	next := references[len(references)-1]
	if err := tx.Commit(); err != nil {
		return nil, "", mapDBError(err)
	}
	return references, next, nil
}

const objectColumns = `
	SELECT o.id, b.name, o.object_key, o.storage_ref, o.size_bytes,
	       COALESCE(o.content_type, ''), COALESCE(o.etag, ''),
	       COALESCE(o.checksum_algorithm, ''), COALESCE(o.checksum_value, ''),
	       o.created_at, o.updated_at`

const objectSelect = objectColumns + `
	FROM objects o JOIN buckets b ON b.id = o.bucket_id`

type rowScanner interface{ Scan(...interface{}) error }

func scanBucket(row rowScanner) (storage.Bucket, error) {
	var bucket storage.Bucket
	if err := row.Scan(&bucket.ID, &bucket.OwnerID, &bucket.Name, &bucket.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return storage.Bucket{}, storage.ErrBucketNotFound
		}
		return storage.Bucket{}, mapDBError(err)
	}
	return bucket, nil
}

func scanObject(row rowScanner) (storage.ObjectMetadata, error) {
	object, err := scanObjectFields(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return storage.ObjectMetadata{}, storage.ErrObjectNotFound
		}
		return storage.ObjectMetadata{}, mapDBError(err)
	}
	return object, nil
}

func scanObjectRow(row rowScanner) (storage.ObjectMetadata, error) {
	object, err := scanObjectFields(row)
	if err != nil {
		return storage.ObjectMetadata{}, mapDBError(err)
	}
	return object, nil
}

func scanObjectFields(row rowScanner) (storage.ObjectMetadata, error) {
	var object storage.ObjectMetadata
	err := row.Scan(&object.ID, &object.Bucket, &object.Key, &object.StorageRef,
		&object.ContentLength, &object.ContentType, &object.ETag,
		&object.ChecksumAlgorithm, &object.Checksum, &object.CreatedAt, &object.UpdatedAt)
	return object, err
}

func validateMetadata(metadata storage.ObjectMetadata) error {
	if metadata.StorageRef == "" || metadata.ContentLength < 0 {
		return storage.ErrInvalidMetadata
	}
	if (metadata.ChecksumAlgorithm == "") != (metadata.Checksum == "") {
		return storage.ErrInvalidMetadata
	}
	return nil
}

func pageLimit(limit int) (int, error) {
	if limit == 0 {
		return defaultPageSize, nil
	}
	if limit < 1 || limit > maxPageSize {
		return 0, storage.ErrInvalidPageLimit
	}
	return limit, nil
}

func prefixPattern(prefix string) string {
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(prefix)
	return escaped + "%"
}

func mapDBError(err error) error {
	var pgErr *pq.Error
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505":
			return storage.ErrAlreadyExists
		case "23503":
			return storage.ErrConflict
		case "23514", "23502", "22001":
			return storage.ErrInvalidMetadata
		}
	}
	return fmt.Errorf("postgres metadata operation: %w", err)
}

var (
	_ storage.BucketStore   = (*BucketRepository)(nil)
	_ storage.MetadataStore = (*MetadataRepository)(nil)
)
