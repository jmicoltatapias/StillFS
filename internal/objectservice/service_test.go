package objectservice

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/jmicoltatapias/stillfs/internal/logging"
	"github.com/jmicoltatapias/stillfs/internal/storage"
	"github.com/jmicoltatapias/stillfs/internal/storage/local"
)

func TestPutAndOpenCoordinatesBytesAndMetadata(t *testing.T) {
	objects, metadata, logs := newTestService(t)
	id := storage.ObjectID{Bucket: "bucket", Key: "path/key"}
	content := "stored outside PostgreSQL"
	if err := objects.Put(context.Background(), id, "text/plain", strings.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	stored := metadata.get(id)
	if stored.StorageRef == "" || stored.ContentLength != int64(len(content)) || stored.ContentType != "text/plain" || stored.ChecksumAlgorithm != "sha256" || stored.Checksum == "" || stored.ETag != `"sha256-`+stored.Checksum+`"` {
		t.Fatalf("unexpected persisted metadata: %#v", stored)
	}
	if referenced, err := metadata.IsStorageRefReferenced(context.Background(), stored.StorageRef); err != nil || !referenced {
		t.Fatalf("reference lookup = (%v, %v)", referenced, err)
	}
	gotMetadata, reader, err := objects.Open(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read/close errors: %v / %v", readErr, closeErr)
	}
	if gotMetadata.StorageRef != stored.StorageRef || string(data) != content {
		t.Fatalf("opened metadata/content mismatch: %#v / %q", gotMetadata, data)
	}
	if logs.Len() != 0 {
		t.Fatalf("unexpected error logs: %s", logs.String())
	}
}

func TestDeleteRemovesMetadataAndUnreferencedBytes(t *testing.T) {
	objects, metadata, _ := newTestService(t)
	id := storage.ObjectID{Bucket: "bucket", Key: "delete"}
	if err := objects.Put(context.Background(), id, "", strings.NewReader("delete me")); err != nil {
		t.Fatal(err)
	}
	reference := metadata.get(id).StorageRef
	if err := objects.Delete(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.Get(context.Background(), id); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Fatalf("metadata remains after Delete: %v", err)
	}
	if _, err := objects.bytes.Stat(context.Background(), reference); !errors.Is(err, storage.ErrContentNotFound) {
		t.Fatalf("content remains after Delete: %v", err)
	}
}

func TestPutOrReplaceAllowsS3StyleRepeatedPut(t *testing.T) {
	objects, metadata, _ := newTestService(t)
	id := storage.ObjectID{Bucket: "bucket", Key: "upsert"}
	for _, content := range []string{"first", "second"} {
		if err := objects.PutOrReplace(context.Background(), id, "text/plain", strings.NewReader(content)); err != nil {
			t.Fatalf("PutOrReplace(%q): %v", content, err)
		}
	}
	stored := metadata.get(id)
	if stored.ContentLength != int64(len("second")) {
		t.Fatalf("replacement metadata = %#v", stored)
	}
	_, reader, err := objects.Open(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || string(data) != "second" {
		t.Fatalf("replacement content = %q, errors=%v/%v", data, readErr, closeErr)
	}
}

func TestFailedMetadataInsertCompensatesOnlyUnreferencedContent(t *testing.T) {
	objects, metadata, logs := newTestService(t)
	metadata.putErr = errors.New("injected PostgreSQL insert failure")
	id := storage.ObjectID{Bucket: "bucket", Key: "key"}
	err := objects.Put(context.Background(), id, "", strings.NewReader("orphan candidate"))
	if !errors.Is(err, metadata.putErr) {
		t.Fatalf("Put() error = %v", err)
	}
	refs := objects.bytes.(*observedStore).references()
	if len(refs) != 1 {
		t.Fatalf("written refs = %d, want 1", len(refs))
	}
	if _, err := objects.bytes.Stat(context.Background(), refs[0]); !errors.Is(err, storage.ErrContentNotFound) {
		t.Fatalf("unreferenced blob was not compensated: %v", err)
	}
	if logs.Len() != 0 {
		t.Fatalf("successful compensation logged an error: %s", logs.String())
	}
}

func TestUncertainOrReferencedCompensationRetainsContentAndLogsReference(t *testing.T) {
	for _, tt := range []struct {
		name       string
		checkError error
		referenced bool
	}{
		{name: "reference check failed", checkError: errors.New("database unavailable")},
		{name: "reference still exists", referenced: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			objects, metadata, logs := newTestService(t)
			metadata.putErr = errors.New("ambiguous metadata result")
			metadata.checkErr = tt.checkError
			metadata.forceReference = tt.referenced
			err := objects.Put(context.Background(), storage.ObjectID{Bucket: "b", Key: "k"}, "", strings.NewReader("keep"))
			if !errors.Is(err, metadata.putErr) {
				t.Fatalf("Put() error = %v", err)
			}
			refs := objects.bytes.(*observedStore).references()
			if len(refs) != 1 {
				t.Fatalf("references = %v", refs)
			}
			if _, err := objects.bytes.Stat(context.Background(), refs[0]); err != nil {
				t.Fatalf("possibly referenced content was deleted: %v", err)
			}
			if !strings.Contains(logs.String(), refs[0]) {
				t.Fatalf("diagnostic did not include storage reference: %s", logs.String())
			}
		})
	}
}

func TestReplacementLeavesOldBlobForSafeReconciliation(t *testing.T) {
	objects, metadata, _ := newTestService(t)
	id := storage.ObjectID{Bucket: "bucket", Key: "key"}
	if err := objects.Put(context.Background(), id, "text/plain", strings.NewReader("before")); err != nil {
		t.Fatal(err)
	}
	old := metadata.get(id).StorageRef
	if err := objects.Replace(context.Background(), id, "text/plain", strings.NewReader("after")); err != nil {
		t.Fatal(err)
	}
	updated := metadata.get(id)
	if updated.StorageRef == old || updated.ContentLength != int64(len("after")) {
		t.Fatalf("replacement metadata = %#v, old ref %q", updated, old)
	}
	if _, err := objects.bytes.Stat(context.Background(), old); err != nil {
		t.Fatalf("old blob was deleted by replacement: %v", err)
	}
}

func TestReaderRemainsValidAcrossReplaceAndDelete(t *testing.T) {
	objects, _, _ := newTestService(t)
	id := storage.ObjectID{Bucket: "bucket", Key: "open-during-mutation"}
	if err := objects.Put(context.Background(), id, "text/plain", strings.NewReader("before")); err != nil {
		t.Fatal(err)
	}
	_, beforeReader, err := objects.Open(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if err := objects.Replace(context.Background(), id, "text/plain", strings.NewReader("after replacement")); err != nil {
		t.Fatal(err)
	}
	before, err := io.ReadAll(beforeReader)
	closeErr := beforeReader.Close()
	if err != nil || closeErr != nil || string(before) != "before" {
		t.Fatalf("reader opened before replacement returned %q; errors=%v/%v", before, err, closeErr)
	}

	_, currentReader, err := objects.Open(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if err := objects.Delete(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	current, err := io.ReadAll(currentReader)
	closeErr = currentReader.Close()
	if err != nil || closeErr != nil || string(current) != "after replacement" {
		t.Fatalf("reader opened before delete returned %q; errors=%v/%v", current, err, closeErr)
	}
}

func TestFailedMetadataReplacementRetainsOldAndCompensatesNewBytes(t *testing.T) {
	objects, metadata, _ := newTestService(t)
	id := storage.ObjectID{Bucket: "bucket", Key: "failed-replace"}
	if err := objects.Put(context.Background(), id, "text/plain", strings.NewReader("old")); err != nil {
		t.Fatal(err)
	}
	old := metadata.get(id).StorageRef
	metadata.updateErr = errors.New("injected metadata update failure")
	if err := objects.Replace(context.Background(), id, "text/plain", strings.NewReader("new")); !errors.Is(err, metadata.updateErr) {
		t.Fatalf("Replace() error = %v", err)
	}
	if got := metadata.get(id).StorageRef; got != old {
		t.Fatalf("failed update changed reference from %q to %q", old, got)
	}
	if _, err := objects.bytes.Stat(context.Background(), old); err != nil {
		t.Fatalf("failed update removed valid old bytes: %v", err)
	}
	refs := objects.bytes.(*observedStore).references()
	if len(refs) != 2 {
		t.Fatalf("published references = %d, want old plus attempted replacement", len(refs))
	}
	for _, reference := range refs {
		_, err := objects.bytes.Stat(context.Background(), reference)
		if reference == old && err != nil {
			t.Fatalf("old bytes missing: %v", err)
		}
		if reference != old && !errors.Is(err, storage.ErrContentNotFound) {
			t.Fatalf("failed replacement bytes were not compensated: %v", err)
		}
	}
}

func TestConcurrentCreateForSameObjectKeepsWinnerAndCompensatesLoser(t *testing.T) {
	objects, metadata, _ := newTestService(t)
	id := storage.ObjectID{Bucket: "bucket", Key: "race"}
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- objects.Put(context.Background(), id, "", strings.NewReader("same key"))
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	successes, duplicates := 0, 0
	for err := range errs {
		if err == nil {
			successes++
		} else if errors.Is(err, storage.ErrAlreadyExists) {
			duplicates++
		} else {
			t.Fatalf("unexpected create error: %v", err)
		}
	}
	if successes != 1 || duplicates != 1 {
		t.Fatalf("results successes=%d duplicates=%d", successes, duplicates)
	}
	kept := metadata.get(id).StorageRef
	refs := objects.bytes.(*observedStore).references()
	if len(refs) != 2 {
		t.Fatalf("generated refs = %d, want 2", len(refs))
	}
	for _, reference := range refs {
		_, err := objects.bytes.Stat(context.Background(), reference)
		if reference == kept && err != nil {
			t.Fatalf("winner content removed: %v", err)
		}
		if reference != kept && !errors.Is(err, storage.ErrContentNotFound) {
			t.Fatalf("loser content not compensated: %v", err)
		}
	}
}

func newTestService(t *testing.T) (*Service, *metadataStub, *bytes.Buffer) {
	t.Helper()
	store, err := local.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	observed := &observedStore{ObjectStore: store}
	metadata := &metadataStub{records: make(map[storage.ObjectID]storage.ObjectMetadata)}
	var logs bytes.Buffer
	return New(observed, metadata, logging.New(&logs)), metadata, &logs
}

type observedStore struct {
	storage.ObjectStore
	mu   sync.Mutex
	refs []string
}

func (s *observedStore) Put(ctx context.Context, content io.Reader) (storage.ObjectInfo, error) {
	info, err := s.ObjectStore.Put(ctx, content)
	if info.Reference != "" {
		s.mu.Lock()
		s.refs = append(s.refs, info.Reference)
		s.mu.Unlock()
	}
	return info, err
}

func (s *observedStore) references() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.refs...)
}

type metadataStub struct {
	storage.MetadataStore
	mu             sync.Mutex
	records        map[storage.ObjectID]storage.ObjectMetadata
	putErr         error
	updateErr      error
	checkErr       error
	forceReference bool
}

func (m *metadataStub) Get(_ context.Context, id storage.ObjectID) (storage.ObjectMetadata, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	metadata, ok := m.records[id]
	if !ok {
		return storage.ObjectMetadata{}, storage.ErrObjectNotFound
	}
	return metadata, nil
}

func (m *metadataStub) Put(_ context.Context, id storage.ObjectID, metadata storage.ObjectMetadata) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.putErr != nil {
		return m.putErr
	}
	if _, ok := m.records[id]; ok {
		return storage.ErrAlreadyExists
	}
	m.records[id] = metadata
	return nil
}

func (m *metadataStub) Update(_ context.Context, id storage.ObjectID, metadata storage.ObjectMetadata) (storage.ObjectMetadata, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.updateErr != nil {
		return storage.ObjectMetadata{}, m.updateErr
	}
	if _, ok := m.records[id]; !ok {
		return storage.ObjectMetadata{}, storage.ErrObjectNotFound
	}
	m.records[id] = metadata
	return metadata, nil
}

func (m *metadataStub) Delete(_ context.Context, id storage.ObjectID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.records[id]; !ok {
		return storage.ErrObjectNotFound
	}
	delete(m.records, id)
	return nil
}

func (m *metadataStub) IsStorageRefReferenced(_ context.Context, reference string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.checkErr != nil {
		return false, m.checkErr
	}
	if m.forceReference {
		return true, nil
	}
	for _, metadata := range m.records {
		if metadata.StorageRef == reference {
			return true, nil
		}
	}
	return false, nil
}

func (m *metadataStub) get(id storage.ObjectID) storage.ObjectMetadata {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.records[id]
}
