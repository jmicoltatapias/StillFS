package local

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Inventory is a read-only snapshot of names found in a local storage root.
// References are generated local:v1 locators; no object bytes are opened.
type Inventory struct {
	References  []string    `json:"references"`
	Temporaries []Temporary `json:"temporaries"`
	UnsafePaths []string    `json:"unsafe_paths,omitempty"`
}

// Temporary describes a reserved temporary name. An inventory cannot prove
// whether another process is actively writing it, so it is never removed here.
type Temporary struct {
	Path       string    `json:"path"`
	Size       int64     `json:"size,omitempty"`
	ModifiedAt time.Time `json:"modified_at,omitempty"`
}

// Inspect enumerates valid published references and reserved temporary files
// without creating, chmod'ing, opening, or deleting anything in root.
func Inspect(root string) (Inventory, error) {
	var result Inventory
	abs, err := filepath.Abs(root)
	if err != nil {
		return result, fmt.Errorf("resolve storage root: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return result, fmt.Errorf("resolve storage root symlinks: %w", err)
	}
	info, err := os.Lstat(resolved)
	if err != nil {
		return result, fmt.Errorf("inspect storage root: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return result, fmt.Errorf("storage root is not a directory")
	}
	shards, err := os.ReadDir(resolved)
	if err != nil {
		return result, fmt.Errorf("read storage root: %w", err)
	}
	for _, shard := range shards {
		if !validShardName(shard.Name()) {
			result.UnsafePaths = append(result.UnsafePaths, shard.Name())
			continue
		}
		shardPath := filepath.Join(resolved, shard.Name())
		shardInfo, err := os.Lstat(shardPath)
		if err != nil {
			return result, fmt.Errorf("inspect shard %q: %w", shard.Name(), err)
		}
		if !shardInfo.IsDir() || shardInfo.Mode()&os.ModeSymlink != 0 {
			result.UnsafePaths = append(result.UnsafePaths, filepath.Join(shard.Name()))
			continue
		}
		entries, err := os.ReadDir(shardPath)
		if err != nil {
			return result, fmt.Errorf("read shard %q: %w", shard.Name(), err)
		}
		for _, entry := range entries {
			name := entry.Name()
			path := filepath.Join(shard.Name(), name)
			entryPath := filepath.Join(shardPath, name)
			if strings.HasPrefix(name, tempPrefix) && strings.HasSuffix(name, ".part") {
				fileInfo, err := os.Lstat(entryPath)
				if err != nil {
					return result, fmt.Errorf("inspect temporary %q: %w", path, err)
				}
				temporary := Temporary{Path: path, ModifiedAt: fileInfo.ModTime()}
				if fileInfo.Mode().IsRegular() {
					temporary.Size = fileInfo.Size()
				} else {
					result.UnsafePaths = append(result.UnsafePaths, path)
				}
				result.Temporaries = append(result.Temporaries, temporary)
				continue
			}
			if !validObjectName(name) {
				result.UnsafePaths = append(result.UnsafePaths, path)
				continue
			}
			fileInfo, err := os.Lstat(entryPath)
			if err != nil {
				return result, fmt.Errorf("inspect object entry %q: %w", path, err)
			}
			if !fileInfo.Mode().IsRegular() || fileInfo.Mode()&os.ModeSymlink != 0 {
				result.UnsafePaths = append(result.UnsafePaths, path)
				continue
			}
			result.References = append(result.References, referencePrefix+shard.Name()+name)
		}
	}
	return result, nil
}

func validObjectName(name string) bool {
	if len(name) != objectIDSize*2-2 {
		return false
	}
	for _, char := range name {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return false
		}
	}
	return true
}
