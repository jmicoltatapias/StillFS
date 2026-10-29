// Package objectservice coordinates object bytes and PostgreSQL metadata.
package objectservice

import (
	"context"
	"fmt"
	"io"
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
}

// New creates a coordinator over the existing storage interfaces.
func New(bytes storage.ObjectStore, metadata storage.MetadataStore, logger ErrorLogger) *Service {
	return &Service{bytes: bytes, metadata: metadata, logger: logger}
}

// Put stores bytes first, then inserts metadata. Duplicate metadata does not
// overwrite an existing object. A newly published blob is compensated when
// PostgreSQL confirms that no row references it.
func (s *Service) Put(ctx context.Context, id storage.ObjectID, contentType string, content io.Reader) error {
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
	return storage.ObjectMetadata{
		StorageRef:        info.Reference,
		ContentLength:     info.Size,
		ContentType:       contentType,
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
