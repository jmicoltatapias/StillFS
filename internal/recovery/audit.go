// Package recovery contains read-only consistency diagnostics. It never
// deletes object content or metadata.
package recovery

import (
	"context"
	"fmt"
	"sort"

	"github.com/jmicoltatapias/stillfs/internal/storage/local"
)

const referencePageSize = 500

type referenceLister interface {
	ListStorageReferences(context.Context, string, int) ([]string, string, error)
}

// Report compares metadata references with the filesystem namespace. A
// reference mismatch is diagnostic evidence, not authorization to delete.
type Report struct {
	MetadataReferences int               `json:"metadata_references"`
	PublishedFiles     int               `json:"published_files"`
	UnreferencedFiles  []string          `json:"unreferenced_file_candidates"`
	MissingFiles       []string          `json:"missing_file_references"`
	TemporaryFiles     []local.Temporary `json:"temporary_files_for_review"`
	UnsafePaths        []string          `json:"unsafe_paths_for_review,omitempty"`
}

// Audit enumerates metadata references and a read-only local inventory. It
// does not hash payloads and does not remove any files.
func Audit(ctx context.Context, references referenceLister, root string) (Report, error) {
	report := Report{}
	inventory, err := local.Inspect(root)
	if err != nil {
		return report, err
	}
	files := make(map[string]struct{}, len(inventory.References))
	for _, reference := range inventory.References {
		files[reference] = struct{}{}
	}
	metadata := make(map[string]struct{})
	after := ""
	for {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		page, next, err := references.ListStorageReferences(ctx, after, referencePageSize)
		if err != nil {
			return report, fmt.Errorf("list metadata storage references: %w", err)
		}
		for _, reference := range page {
			metadata[reference] = struct{}{}
			if _, exists := files[reference]; !exists {
				report.MissingFiles = append(report.MissingFiles, reference)
			}
		}
		if next == "" {
			break
		}
		if next <= after || len(page) == 0 {
			return report, fmt.Errorf("metadata reference pagination did not advance")
		}
		after = next
	}
	for _, reference := range inventory.References {
		if _, exists := metadata[reference]; !exists {
			report.UnreferencedFiles = append(report.UnreferencedFiles, reference)
		}
	}
	report.MetadataReferences = len(metadata)
	report.PublishedFiles = len(inventory.References)
	report.TemporaryFiles = inventory.Temporaries
	report.UnsafePaths = inventory.UnsafePaths
	sort.Strings(report.MissingFiles)
	sort.Strings(report.UnreferencedFiles)
	sort.Strings(report.UnsafePaths)
	return report, nil
}
