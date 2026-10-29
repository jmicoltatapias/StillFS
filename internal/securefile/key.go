// Package securefile reads fixed-size secrets from protected regular files.
package securefile

import (
	"errors"
	"fmt"
	"io"
	"os"
)

var ErrInvalidKeyFile = errors.New("invalid protected key file")

// ReadMasterKey reads exactly 32 raw bytes from a regular file with no
// permissions for group or other users. Symlinks are rejected.
func ReadMasterKey(path string) ([]byte, error) {
	if path == "" {
		return nil, ErrInvalidKeyFile
	}
	before, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot inspect file", ErrInvalidKeyFile)
	}
	if !before.Mode().IsRegular() || before.Mode().Perm()&0077 != 0 {
		return nil, ErrInvalidKeyFile
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot open file", ErrInvalidKeyFile)
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return nil, ErrInvalidKeyFile
	}
	key := make([]byte, 33)
	n, err := io.ReadFull(file, key)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return nil, fmt.Errorf("%w: cannot read file", ErrInvalidKeyFile)
	}
	if n != 32 {
		return nil, ErrInvalidKeyFile
	}
	return key[:32], nil
}
