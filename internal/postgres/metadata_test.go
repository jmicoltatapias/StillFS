package postgres

import (
	"errors"
	"testing"

	"github.com/jmicoltatapias/stillfs/internal/storage"
	"github.com/lib/pq"
)

func TestPageLimit(t *testing.T) {
	for _, tt := range []struct {
		name string
		in   int
		want int
		bad  bool
	}{{"default", 0, defaultPageSize, false}, {"explicit", 20, 20, false}, {"negative", -1, 0, true}, {"too large", maxPageSize + 1, 0, true}} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := pageLimit(tt.in)
			if (err != nil) != tt.bad || got != tt.want {
				t.Fatalf("pageLimit(%d) = (%d, %v)", tt.in, got, err)
			}
		})
	}
}

func TestPrefixPatternEscapesLikeWildcards(t *testing.T) {
	if got, want := prefixPattern(`a%_\z`), `a\%\_\\z%`; got != want {
		t.Fatalf("prefixPattern() = %q, want %q", got, want)
	}
}

func TestValidateMetadata(t *testing.T) {
	valid := storage.ObjectMetadata{StorageRef: "opaque:abc", ContentLength: 4}
	if err := validateMetadata(valid); err != nil {
		t.Fatalf("valid metadata rejected: %v", err)
	}
	for _, metadata := range []storage.ObjectMetadata{
		{StorageRef: "", ContentLength: 0},
		{StorageRef: "opaque:abc", ContentLength: -1},
		{StorageRef: "opaque:abc", ContentLength: 0, ChecksumAlgorithm: "sha256"},
	} {
		if err := validateMetadata(metadata); !errors.Is(err, storage.ErrInvalidMetadata) {
			t.Fatalf("validateMetadata(%#v) = %v, want ErrInvalidMetadata", metadata, err)
		}
	}
}

func TestMapDBError(t *testing.T) {
	for _, tt := range []struct {
		code string
		want error
	}{{"23505", storage.ErrAlreadyExists}, {"23503", storage.ErrConflict}, {"23514", storage.ErrInvalidMetadata}} {
		if got := mapDBError(&pq.Error{Code: pq.ErrorCode(tt.code)}); !errors.Is(got, tt.want) {
			t.Errorf("mapDBError(%s) = %v, want %v", tt.code, got, tt.want)
		}
	}
}
