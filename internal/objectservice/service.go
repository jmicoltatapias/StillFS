// Package objectservice coordinates object bytes and PostgreSQL metadata.
package objectservice

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"sync"
	"time"

	"github.com/jmicoltatapias/stillfs/internal/storage"
)

const compensationTimeout = 5 * time.Second

// ErrorLogger is the structured logging surface required for recovery errors.
type ErrorLogger interface {
	Error(message string, fields map[string]interface{})
}

// Service coordinates local bytes with object metadata. It does not create a
// distributed transaction between PostgreSQL and the filesystem.
type Service struct {
	bytes    storage.ObjectStore
	metadata storage.MetadataStore
	logger   ErrorLogger
	locks    [128]sync.Mutex
}

// New creates a coordinator over the existing storage interfaces.
func New(bytes storage.ObjectStore, metadata storage.MetadataStore, logger ErrorLogger) *Service {
	return &Service{bytes: bytes, metadata: metadata, logger: logger}
}

// Put stores bytes first, then inserts metadata. Duplicate metadata does not
// overwrite an existing object. A newly published blob is compensated when
// PostgreSQL confirms that no row references it.
func (s *Service) Put(ctx context.Context, id storage.ObjectID, contentType string, content io.Reader) error {
	lock := s.objectLock(id)
	lock.Lock()
	defer lock.Unlock()
	return s.putLocked(ctx, id, contentType, content)
}

// PutOrReplace serializes create/replace decisions for a key within this
// process. Local storage roots are not supported across multiple processes.
func (s *Service) PutOrReplace(ctx context.Context, id storage.ObjectID, contentType string, content io.Reader) error {
	lock := s.objectLock(id)
	lock.Lock()
	defer lock.Unlock()
	_, err := s.metadata.Get(ctx, id)
	if err == nil {
		return s.replaceLocked(ctx, id, contentType, content)
	}
	if !errors.Is(err, storage.ErrObjectNotFound) {
		return fmt.Errorf("find object metadata before create or replace: %w", err)
	}
	return s.putLocked(ctx, id, contentType, content)
}

func (s *Service) putLocked(ctx context.Context, id storage.ObjectID, contentType string, content io.Reader) error {
	info, err := s.bytes.Put(ctx, content)
	if err != nil {
		if info.Reference != "" {
			return s.compensate(info.Reference, "write object content", err)
		}
		return fmt.Errorf("write object content: %w", err)
	}
	if err := validateInfo(info); err != nil {
		return s.compensate(info.Reference, "validate object storage result", err)
	}
	metadata := metadataFromInfo(info, contentType)
	if err := s.metadata.Put(ctx, id, metadata); err != nil {
		return s.compensate(info.Reference, "register object metadata", err)
	}
	return nil
}

// Replace writes a new blob, then updates metadata for an existing object.
// After a successful update the previous blob is deliberately retained; a
// future reconciler must prove it is unreferenced before deleting it.
func (s *Service) Replace(ctx context.Context, id storage.ObjectID, contentType string, content io.Reader) error {
	lock := s.objectLock(id)
	lock.Lock()
	defer lock.Unlock()
	return s.replaceLocked(ctx, id, contentType, content)
}

func (s *Service) replaceLocked(ctx context.Context, id storage.ObjectID, contentType string, content io.Reader) error {
	if _, err := s.metadata.Get(ctx, id); err != nil {
		return fmt.Errorf("find object metadata before replacement: %w", err)
	}
	info, err := s.bytes.Put(ctx, content)
	if err != nil {
		if info.Reference != "" {
			return s.compensate(info.Reference, "write replacement content", err)
		}
		return fmt.Errorf("write replacement content: %w", err)
	}
	if err := validateInfo(info); err != nil {
		return s.compensate(info.Reference, "validate replacement storage result", err)
	}
	if _, err := s.metadata.Update(ctx, id, metadataFromInfo(info, contentType)); err != nil {
		return s.compensate(info.Reference, "update object metadata", err)
	}
	return nil
}

// Open resolves metadata first and streams the referenced content. It never
// buffers the complete object in memory.
func (s *Service) Open(ctx context.Context, id storage.ObjectID) (storage.ObjectMetadata, io.ReadCloser, error) {
	lock := s.objectLock(id)
	lock.Lock()
	defer lock.Unlock()
	metadata, err := s.metadata.Get(ctx, id)
	if err != nil {
		return storage.ObjectMetadata{}, nil, err
	}
	reader, err := s.bytes.Open(ctx, metadata.StorageRef)
	if err != nil {
		s.logger.Error("object metadata references unavailable content", map[string]interface{}{
			"storage_ref": metadata.StorageRef,
			"error":       err.Error(),
		})
		return storage.ObjectMetadata{}, nil, fmt.Errorf("open referenced object content: %w", err)
	}
	return metadata, reader, nil
}

// Delete removes metadata first, then deletes the content only after a
// successful reference check. If the reference check is unavailable the
// bytes remain and the caller receives an error for operational diagnosis.
func (s *Service) Delete(ctx context.Context, id storage.ObjectID) error {
	lock := s.objectLock(id)
	lock.Lock()
	defer lock.Unlock()
	metadata, err := s.metadata.Get(ctx, id)
	if err != nil {
		return err
	}
	if err := s.metadata.Delete(ctx, id); err != nil {
		return fmt.Errorf("delete object metadata: %w", err)
	}
	referenced, err := s.metadata.IsStorageRefReferenced(ctx, metadata.StorageRef)
	if err != nil {
		s.logger.Error("deleted object metadata but retained content because reference state is unknown", map[string]interface{}{
			"storage_ref": metadata.StorageRef,
			"error":       err.Error(),
		})
		return fmt.Errorf("object metadata deleted; could not safely remove object content: %w", err)
	}
	if referenced {
		return nil
	}
	if err := s.bytes.Delete(ctx, metadata.StorageRef); err != nil {
		s.logger.Error("deleted object metadata but object content cleanup failed", map[string]interface{}{
			"storage_ref": metadata.StorageRef,
			"error":       err.Error(),
		})
		return fmt.Errorf("object metadata deleted; remove object content: %w", err)
	}
	return nil
}

func (s *Service) objectLock(id storage.ObjectID) *sync.Mutex {
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(id.Bucket))
	_, _ = hasher.Write([]byte{0})
	_, _ = hasher.Write([]byte(id.Key))
	return &s.locks[hasher.Sum32()%uint32(len(s.locks))]
}

func (s *Service) compensate(reference, operation string, cause error) error {
	if reference == "" {
		return fmt.Errorf("%s: %w", operation, cause)
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), compensationTimeout)
	defer cancel()
	referenced, err := s.metadata.IsStorageRefReferenced(cleanupCtx, reference)
	if err != nil {
		s.logger.Error("object content retained because reference state is unknown", map[string]interface{}{
			"operation":   operation,
			"storage_ref": reference,
			"cause":       cause.Error(),
			"check_error": err.Error(),
		})
		return fmt.Errorf("%s: %w; content retained because reference state could not be checked: %v", operation, cause, err)
	}
	if referenced {
		s.logger.Error("object content retained because metadata references it", map[string]interface{}{
			"operation":   operation,
			"storage_ref": reference,
			"cause":       cause.Error(),
		})
		return fmt.Errorf("%s: %w; content retained because metadata references it", operation, cause)
	}
	if err := s.bytes.Delete(cleanupCtx, reference); err != nil {
		s.logger.Error("orphan object content cleanup failed", map[string]interface{}{
			"operation":     operation,
			"storage_ref":   reference,
			"cause":         cause.Error(),
			"cleanup_error": err.Error(),
		})
		return fmt.Errorf("%s: %w; content cleanup failed: %v", operation, cause, err)
	}
	return fmt.Errorf("%s: %w", operation, cause)
}

func metadataFromInfo(info storage.ObjectInfo, contentType string) storage.ObjectMetadata {
	etag := ""
	if info.ChecksumAlgorithm == "sha256" && info.Checksum != "" {
		etag = `"sha256-` + info.Checksum + `"`
	}
	return storage.ObjectMetadata{
		StorageRef:        info.Reference,
		ContentLength:     info.Size,
		ContentType:       contentType,
		ETag:              etag,
		ChecksumAlgorithm: info.ChecksumAlgorithm,
		Checksum:          info.Checksum,
	}
}

func validateInfo(info storage.ObjectInfo) error {
	if info.Reference == "" || info.Size < 0 {
		return storage.ErrInvalidMetadata
	}
	if (info.ChecksumAlgorithm == "") != (info.Checksum == "") {
		return storage.ErrInvalidMetadata
	}
	return nil
}
