package local

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// CleanupTemps removes only files with this backend's reserved temporary name
// inside valid shard directories. Active writes in this Store are skipped.
// Callers must not share a storage root with another StillFS process.
func (s *Store) CleanupTemps(ctx context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	shards, err := os.ReadDir(s.root)
	if err != nil {
		return 0, fmt.Errorf("read storage root during cleanup: %w", err)
	}
	removed := 0
	for _, shardEntry := range shards {
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		if !validShardName(shardEntry.Name()) {
			continue
		}
		shardPath := filepath.Join(s.root, shardEntry.Name())
		shardInfo, err := os.Lstat(shardPath)
		if err != nil {
			return removed, fmt.Errorf("inspect storage shard during cleanup: %w", err)
		}
		if !shardInfo.IsDir() || shardInfo.Mode()&os.ModeSymlink != 0 {
			continue
		}
		entries, err := os.ReadDir(shardPath)
		if err != nil {
			return removed, fmt.Errorf("read storage shard during cleanup: %w", err)
		}
		shardChanged := false
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return removed, err
			}
			name := entry.Name()
			if !strings.HasPrefix(name, tempPrefix) || !strings.HasSuffix(name, ".part") {
				continue
			}
			path := filepath.Join(shardPath, name)
			if _, active := s.open[path]; active {
				continue
			}
			info, err := os.Lstat(path)
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return removed, fmt.Errorf("inspect temporary object: %w", err)
			}
			if info.IsDir() {
				continue
			}
			// Remove acts on a symlink itself and never follows its target.
			if err := os.Remove(path); err != nil {
				return removed, fmt.Errorf("remove abandoned temporary object: %w", err)
			}
			removed++
			shardChanged = true
		}
		if shardChanged {
			if err := syncDirectory(shardPath); err != nil {
				return removed, fmt.Errorf("sync storage shard after cleanup: %w", err)
			}
		}
	}
	return removed, nil
}

func validShardName(name string) bool {
	if len(name) != 2 {
		return false
	}
	for _, char := range name {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return false
		}
	}
	return true
}
