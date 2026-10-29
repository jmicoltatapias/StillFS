package securefile

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestReadMasterKeyRequiresProtectedRaw32ByteRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.key")
	want := bytes.Repeat([]byte{0x5a}, 32)
	if err := os.WriteFile(path, want, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadMasterKey(path)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("ReadMasterKey() mismatch, err=%v", err)
	}
	for _, size := range []int{31, 33} {
		if err := os.WriteFile(path, bytes.Repeat([]byte{1}, size), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadMasterKey(path); err == nil {
			t.Fatalf("accepted %d-byte key file", size)
		}
	}
	if err := os.WriteFile(path, want, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadMasterKey(path); err == nil {
		t.Fatal("accepted group/world-readable key file")
	}
}

func TestReadMasterKeyRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	link := filepath.Join(dir, "link")
	if err := os.WriteFile(target, bytes.Repeat([]byte{2}, 32), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := ReadMasterKey(link); err == nil {
		t.Fatal("accepted symlink key file")
	}
}
