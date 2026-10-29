// Package local implements a streaming, single-process local object store.
package local

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/jmicoltatapias/stillfs/internal/storage"
)

const (
	referencePrefix = "local:v1:"
	objectIDSize    = 32
	tempPrefix      = ".stillfs-tmp-"
)

var ErrInvalidReference = errors.New("invalid local storage reference")

// Store keeps content under a private root using generated references only.
// A root must not be shared with another StillFS process.
type Store struct {
	root string
	mu   sync.Mutex
	open map[string]struct{}
}

// New creates a private root, resolves it to a stable absolute path and cleans
// abandoned temporary files before the caller begins accepting new operations.
func New(root string) (*Store, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("storage root must not be empty")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve storage root: %w", err)
	}
	if err := os.MkdirAll(abs, 0700); err != nil {
		return nil, fmt.Errorf("create storage root: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("resolve storage root symlinks: %w", err)
	}
	info, err := os.Lstat(resolved)
	if err != nil {
		return nil, fmt.Errorf("inspect storage root: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("storage root is not a directory")
	}
	if err := os.Chmod(resolved, 0700); err != nil {
		return nil, fmt.Errorf("restrict storage root permissions: %w", err)
	}
	store := &Store{root: resolved, open: make(map[string]struct{})}
	if _, err := store.CleanupTemps(context.Background()); err != nil {
		return nil, fmt.Errorf("clean abandoned storage temporaries: %w", err)
	}
	return store, nil
}

// Root returns the canonical absolute root path used by this store.
func (s *Store) Root() string { return s.root }

// Put streams content to an exclusive temporary file, computes SHA-256 while
// copying, syncs the file, then atomically publishes a no-overwrite hard link.
// If publication succeeds but a later step fails, info.Reference is returned
// when cleanup could not prove that the published file was removed.
func (s *Store) Put(ctx context.Context, content io.Reader) (storage.ObjectInfo, error) {
	if content == nil {
		return storage.ObjectInfo{}, fmt.Errorf("content reader must not be nil")
	}
	token := make([]byte, objectIDSize)
	if _, err := rand.Read(token); err != nil {
		return storage.ObjectInfo{}, fmt.Errorf("generate storage reference: %w", err)
	}
	encoded := hex.EncodeToString(token)
	shard := filepath.Join(s.root, encoded[:2])
	if err := s.ensureShard(shard); err != nil {
		return storage.ObjectInfo{}, err
	}
	finalPath := filepath.Join(shard, encoded[2:])
	temp, err := s.createTemp(shard)
	if err != nil {
		return storage.ObjectInfo{}, fmt.Errorf("create temporary object: %w", err)
	}
	tempPath := temp.Name()
	hasher := sha256.New()
	size, copyErr := io.Copy(io.MultiWriter(temp, hasher), contextReader{ctx: ctx, reader: content})
	if copyErr != nil {
		return storage.ObjectInfo{}, s.discardTemp(temp, tempPath, fmt.Errorf("stream object content: %w", copyErr))
	}
	if err := temp.Sync(); err != nil {
		return storage.ObjectInfo{}, s.discardTemp(temp, tempPath, fmt.Errorf("sync object content: %w", err))
	}
	if err := ctx.Err(); err != nil {
		return storage.ObjectInfo{}, s.discardTemp(temp, tempPath, err)
	}
	if err := temp.Close(); err != nil {
		s.unregisterTemp(tempPath)
		return storage.ObjectInfo{}, s.discardClosedTemp(tempPath, fmt.Errorf("close temporary object: %w", err))
	}
	info := storage.ObjectInfo{
		Reference:         referencePrefix + encoded,
		Size:              size,
		ChecksumAlgorithm: "sha256",
		Checksum:          hex.EncodeToString(hasher.Sum(nil)),
	}
	if err := os.Link(tempPath, finalPath); err != nil {
		return storage.ObjectInfo{}, s.discardClosedTemp(tempPath, fmt.Errorf("publish complete object: %w", err))
	}
	if err := os.Remove(tempPath); err != nil {
		return s.rollbackPublished(info, finalPath, tempPath, fmt.Errorf("remove temporary object name: %w", err))
	}
	if err := syncDirectory(shard); err != nil {
		return s.rollbackPublished(info, finalPath, tempPath, fmt.Errorf("sync object directory: %w", err))
	}
	s.unregisterTemp(tempPath)
	return info, nil
}

// Open returns a context-aware streaming reader for a generated reference.
func (s *Store) Open(ctx context.Context, reference string) (io.ReadCloser, error) {
	path, err := s.objectPath(reference)
	if err != nil {
		return nil, err
	}
	before, err := os.Lstat(path)
	if err != nil {
		return nil, mapFileError("open object", err)
	}
	if !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 {
		return nil, ErrInvalidReference
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, mapFileError("open object", err)
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) {
		_ = file.Close()
		if err != nil {
			return nil, fmt.Errorf("verify opened object: %w", err)
		}
		return nil, ErrInvalidReference
	}
	return &contextFile{ctx: ctx, file: file}, nil
}

// Stat streams the object to recompute its checksum without buffering it.
func (s *Store) Stat(ctx context.Context, reference string) (storage.ObjectInfo, error) {
	reader, err := s.Open(ctx, reference)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	hasher := sha256.New()
	size, readErr := io.Copy(hasher, reader)
	closeErr := reader.Close()
	if readErr != nil {
		return storage.ObjectInfo{}, fmt.Errorf("read object for stat: %w", readErr)
	}
	if closeErr != nil {
		return storage.ObjectInfo{}, fmt.Errorf("close object after stat: %w", closeErr)
	}
	return storage.ObjectInfo{
		Reference:         reference,
		Size:              size,
		ChecksumAlgorithm: "sha256",
		Checksum:          hex.EncodeToString(hasher.Sum(nil)),
	}, nil
}

// Delete removes one explicitly named content reference and syncs its shard
// directory. It never accepts bucket names or object keys as paths.
func (s *Store) Delete(ctx context.Context, reference string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := s.objectPath(reference)
	if err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return mapFileError("delete object", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return ErrInvalidReference
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("delete object content: %w", err)
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return fmt.Errorf("object was removed but shard directory sync failed: %w", err)
	}
	return nil
}

func (s *Store) ensureShard(path string) error {
	created := false
	if err := os.Mkdir(path, 0700); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("create storage shard: %w", err)
		}
	} else {
		created = true
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect storage shard: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ErrInvalidReference
	}
	if err := os.Chmod(path, 0700); err != nil {
		return fmt.Errorf("restrict storage shard permissions: %w", err)
	}
	if created {
		if err := syncDirectory(s.root); err != nil {
			return fmt.Errorf("sync storage root after creating shard: %w", err)
		}
	}
	return nil
}

func (s *Store) createTemp(shard string) (*os.File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	file, err := os.CreateTemp(shard, tempPrefix+"*.part")
	if err != nil {
		return nil, err
	}
	s.open[file.Name()] = struct{}{}
	return file, nil
}

func (s *Store) objectPath(reference string) (string, error) {
	if !strings.HasPrefix(reference, referencePrefix) {
		return "", ErrInvalidReference
	}
	encoded := strings.TrimPrefix(reference, referencePrefix)
	if len(encoded) != objectIDSize*2 {
		return "", ErrInvalidReference
	}
	for _, char := range encoded {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return "", ErrInvalidReference
		}
	}
	shard := filepath.Join(s.root, encoded[:2])
	info, err := os.Lstat(shard)
	if err != nil {
		return "", mapFileError("resolve object shard", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", ErrInvalidReference
	}
	return filepath.Join(shard, encoded[2:]), nil
}

func (s *Store) discardTemp(file *os.File, path string, cause error) error {
	closeErr := file.Close()
	return s.discardClosedTemp(path, appendError(cause, "close temporary object", closeErr))
}

func (s *Store) discardClosedTemp(path string, cause error) error {
	removeErr := os.Remove(path)
	if errors.Is(removeErr, os.ErrNotExist) {
		removeErr = nil
	}
	s.unregisterTemp(path)
	return appendError(cause, "remove temporary object", removeErr)
}

func (s *Store) rollbackPublished(info storage.ObjectInfo, finalPath, tempPath string, cause error) (storage.ObjectInfo, error) {
	removeErr := os.Remove(finalPath)
	if errors.Is(removeErr, os.ErrNotExist) {
		removeErr = nil
	}
	if removeErr == nil {
		if syncErr := syncDirectory(filepath.Dir(finalPath)); syncErr != nil {
			removeErr = syncErr
		}
	}
	if tempErr := s.discardClosedTemp(tempPath, nil); tempErr != nil {
		removeErr = appendError(removeErr, "remove temporary object", tempErr)
	}
	if removeErr != nil {
		return info, fmt.Errorf("%w; published content may remain: %v", cause, removeErr)
	}
	return storage.ObjectInfo{}, cause
}

func (s *Store) unregisterTemp(path string) {
	s.mu.Lock()
	delete(s.open, path)
	s.mu.Unlock()
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	syncErr := dir.Sync()
	closeErr := dir.Close()
	return appendError(syncErr, "close directory after sync", closeErr)
}

func mapFileError(operation string, err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s: %w", operation, storage.ErrContentNotFound)
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func appendError(primary error, label string, secondary error) error {
	if secondary == nil {
		return primary
	}
	if primary == nil {
		return fmt.Errorf("%s: %w", label, secondary)
	}
	return fmt.Errorf("%w (also %s: %v)", primary, label, secondary)
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(buffer []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	default:
		return r.reader.Read(buffer)
	}
}

type contextFile struct {
	ctx  context.Context
	file *os.File
}

func (f *contextFile) Read(buffer []byte) (int, error) {
	select {
	case <-f.ctx.Done():
		return 0, f.ctx.Err()
	default:
		return f.file.Read(buffer)
	}
}

func (f *contextFile) Close() error { return f.file.Close() }

var _ storage.ObjectStore = (*Store)(nil)
