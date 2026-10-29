package recovery

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jmicoltatapias/stillfs/internal/storage/local"
)

func TestAuditReportsMismatchesWithoutDeletingFiles(t *testing.T) {
	root := t.TempDir()
	store, err := local.New(root)
	if err != nil {
		t.Fatal(err)
	}
	valid, err := store.Put(context.Background(), strings.NewReader("still referenced"))
	if err != nil {
		t.Fatal(err)
	}
	orphan, err := store.Put(context.Background(), strings.NewReader("candidate only"))
	if err != nil {
		t.Fatal(err)
	}
	missing := "local:v1:" + strings.Repeat("f", 64)
	if missing == valid.Reference || missing == orphan.Reference {
		t.Fatal("generated test reference unexpectedly matched missing reference")
	}
	encoded := strings.TrimPrefix(orphan.Reference, "local:v1:")
	tempPath := filepath.Join(root, encoded[:2], ".stillfs-tmp-abandoned.part")
	if err := os.WriteFile(tempPath, []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}

	report, err := Audit(context.Background(), refListerStub{references: []string{valid.Reference, missing}}, root)
	if err != nil {
		t.Fatal(err)
	}
	if report.MetadataReferences != 2 || report.PublishedFiles != 2 || !contains(report.MissingFiles, missing) || !contains(report.UnreferencedFiles, orphan.Reference) || len(report.TemporaryFiles) != 1 {
		t.Fatalf("unexpected audit report: %#v", report)
	}
	if _, err := store.Stat(context.Background(), valid.Reference); err != nil {
		t.Fatalf("audit damaged referenced content: %v", err)
	}
	if _, err := store.Stat(context.Background(), orphan.Reference); err != nil {
		t.Fatalf("audit deleted candidate content: %v", err)
	}
	if _, err := os.Stat(tempPath); err != nil {
		t.Fatalf("audit removed temporary file: %v", err)
	}
}

func TestAuditRejectsPaginationThatDoesNotAdvance(t *testing.T) {
	root := t.TempDir()
	if _, err := local.New(root); err != nil {
		t.Fatal(err)
	}
	_, err := Audit(context.Background(), refListerStub{references: []string{"first"}, repeatCursor: true}, root)
	if err == nil || !strings.Contains(err.Error(), "pagination did not advance") {
		t.Fatalf("Audit pagination error = %v", err)
	}
}

func TestAuditPropagatesReferenceQueryFailure(t *testing.T) {
	root := t.TempDir()
	if _, err := local.New(root); err != nil {
		t.Fatal(err)
	}
	want := errors.New("database unavailable")
	if _, err := Audit(context.Background(), refListerStub{err: want}, root); !errors.Is(err, want) {
		t.Fatalf("Audit error = %v, want %v", err, want)
	}
}

type refListerStub struct {
	references   []string
	repeatCursor bool
	err          error
}

func (s refListerStub) ListStorageReferences(_ context.Context, after string, limit int) ([]string, string, error) {
	if s.err != nil {
		return nil, "", s.err
	}
	if after != "" {
		if s.repeatCursor {
			return []string{"first"}, after, nil
		}
		return nil, "", nil
	}
	if s.repeatCursor {
		return []string{"first"}, "first", nil
	}
	if len(s.references) <= limit {
		return s.references, "", nil
	}
	return s.references[:limit], s.references[limit-1], nil
}

func contains(items []string, wanted string) bool {
	for _, item := range items {
		if item == wanted {
			return true
		}
	}
	return false
}
