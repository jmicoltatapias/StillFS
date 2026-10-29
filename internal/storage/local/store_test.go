package local

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jmicoltatapias/stillfs/internal/storage"
)

func TestPutOpenStatDeleteAndRestart(t *testing.T) {
	root := filepath.Join(t.TempDir(), "objects")
	store, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("streamed object payload")
	info, err := store.Put(context.Background(), bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(info.Reference, referencePrefix) || info.Size != int64(len(content)) || info.ChecksumAlgorithm != "sha256" {
		t.Fatalf("unexpected object info: %#v", info)
	}
	wantSum := sha256.Sum256(content)
	if info.Checksum != fmt.Sprintf("%x", wantSum) {
		t.Fatalf("checksum = %s, want %x", info.Checksum, wantSum)
	}

	restarted, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := restarted.Open(context.Background(), info.Reference)
	if err != nil {
		t.Fatal(err)
	}
	readBack, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read/close errors: %v / %v", readErr, closeErr)
	}
	if !bytes.Equal(readBack, content) {
		t.Fatalf("read content = %q, want %q", readBack, content)
	}
	stat, err := restarted.Stat(context.Background(), info.Reference)
	if err != nil || stat.Size != info.Size || stat.Checksum != info.Checksum {
		t.Fatalf("Stat() = (%#v, %v)", stat, err)
	}
	if err := restarted.Delete(context.Background(), info.Reference); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Open(context.Background(), info.Reference); !errors.Is(err, storage.ErrContentNotFound) {
		t.Fatalf("Open deleted reference error = %v", err)
	}
	if err := restarted.Delete(context.Background(), info.Reference); !errors.Is(err, storage.ErrContentNotFound) {
		t.Fatalf("Delete missing reference error = %v", err)
	}
}

func TestEmptyObject(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	info, err := store.Put(context.Background(), bytes.NewReader(nil))
	if err != nil {
		t.Fatal(err)
	}
	emptySum := sha256.Sum256(nil)
	if info.Size != 0 || info.Checksum != fmt.Sprintf("%x", emptySum) {
		t.Fatalf("empty object info = %#v", info)
	}
}

func TestLargeContentStreamsThroughBoundedReads(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const total = 32 << 20
	reader := &generatedReader{remaining: total}
	info, err := store.Put(context.Background(), reader)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size != total {
		t.Fatalf("size = %d, want %d", info.Size, total)
	}
	if reader.maxBuffer > 128<<10 {
		t.Fatalf("reader was asked for %d bytes at once", reader.maxBuffer)
	}
	stat, err := store.Stat(context.Background(), info.Reference)
	if err != nil || stat.Checksum != info.Checksum {
		t.Fatalf("large content stat = (%#v, %v)", stat, err)
	}
}

func TestReadFailureDoesNotPublishObject(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("injected source failure")
	info, err := store.Put(context.Background(), &errorReader{err: wantErr})
	if !errors.Is(err, wantErr) || info.Reference != "" {
		t.Fatalf("Put() = (%#v, %v), want source error and no reference", info, err)
	}
	files, err := filepath.Glob(filepath.Join(store.Root(), "*", "*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("failed write left files published: %v", files)
	}
}

func TestInvalidReferencesAndSymlinkShardAreRejected(t *testing.T) {
	root := t.TempDir()
	store, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, reference := range []string{"", "../../etc/passwd", referencePrefix + "../" + strings.Repeat("a", 64), referencePrefix + strings.Repeat("A", 64), referencePrefix + "abcd"} {
		if _, err := store.Open(context.Background(), reference); !errors.Is(err, ErrInvalidReference) {
			t.Errorf("Open(%q) error = %v, want invalid reference", reference, err)
		}
	}
	external := t.TempDir()
	if err := os.Symlink(external, filepath.Join(root, "ab")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	reference := referencePrefix + "ab" + strings.Repeat("0", 62)
	if _, err := store.Open(context.Background(), reference); !errors.Is(err, ErrInvalidReference) {
		t.Fatalf("Open through symlink shard error = %v", err)
	}
	if err := store.ensureShard(filepath.Join(root, "ab")); !errors.Is(err, ErrInvalidReference) {
		t.Fatalf("ensure symlink shard error = %v", err)
	}
	entries, err := os.ReadDir(external)
	if err != nil || len(entries) != 0 {
		t.Fatalf("symlink target was modified: entries=%v err=%v", entries, err)
	}
}

func TestCleanupRemovesOnlyStaleTemporaryNamesAndSkipsActiveWrites(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	shard := filepath.Join(store.Root(), "ab")
	if err := os.Mkdir(shard, 0700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(shard, tempPrefix+"abandoned.part")
	if err := os.WriteFile(stale, []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	valid := filepath.Join(shard, strings.Repeat("c", 62))
	if err := os.WriteFile(valid, []byte("complete"), 0600); err != nil {
		t.Fatal(err)
	}
	removed, err := store.CleanupTemps(context.Background())
	if err != nil || removed != 1 {
		t.Fatalf("CleanupTemps() = (%d, %v), want one removal", removed, err)
	}
	if _, err := os.Stat(valid); err != nil {
		t.Fatalf("valid object removed by cleanup: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("temporary file still exists: %v", err)
	}

	entered := make(chan struct{})
	resume := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		_, err := store.Put(context.Background(), &blockingReader{entered: entered, resume: resume})
		result <- err
	}()
	<-entered
	removed, err = store.CleanupTemps(context.Background())
	if err != nil || removed != 0 {
		t.Fatalf("cleanup removed active temp: (%d, %v)", removed, err)
	}
	close(resume)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentIndependentWrites(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const count = 12
	var wg sync.WaitGroup
	infos := make(chan storage.ObjectInfo, count)
	errs := make(chan error, count)
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			info, err := store.Put(context.Background(), strings.NewReader(fmt.Sprintf("object-%d", i)))
			infos <- info
			errs <- err
		}(i)
	}
	wg.Wait()
	close(infos)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	seen := make(map[string]struct{})
	for info := range infos {
		if _, ok := seen[info.Reference]; ok {
			t.Fatalf("duplicate generated reference %q", info.Reference)
		}
		seen[info.Reference] = struct{}{}
	}
	if len(seen) != count {
		t.Fatalf("references = %d, want %d", len(seen), count)
	}
}

type errorReader struct {
	err  error
	sent bool
}

func (r *errorReader) Read(buffer []byte) (int, error) {
	if r.sent {
		return 0, io.EOF
	}
	r.sent = true
	return copy(buffer, "partial payload"), r.err
}

type generatedReader struct {
	remaining int
	maxBuffer int
}

func (r *generatedReader) Read(buffer []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	if len(buffer) > r.maxBuffer {
		r.maxBuffer = len(buffer)
	}
	count := len(buffer)
	if count > r.remaining {
		count = r.remaining
	}
	for i := 0; i < count; i++ {
		buffer[i] = byte((r.remaining + i) % 251)
	}
	r.remaining -= count
	return count, nil
}

type blockingReader struct {
	entered chan struct{}
	resume  chan struct{}
	started bool
}

func (r *blockingReader) Read(buffer []byte) (int, error) {
	if r.started {
		return 0, io.EOF
	}
	r.started = true
	close(r.entered)
	<-r.resume
	buffer[0] = 'x'
	return 1, nil
}
