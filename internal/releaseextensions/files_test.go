package releaseextensions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCopyRegular(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "source")
	if err := os.WriteFile(source, []byte("verified driver bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(directory, "payload", "driver")
	if err := CopyRegular(source, destination, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(destination)
	if err != nil || string(data) != "verified driver bytes" {
		t.Fatalf("copied payload = %q, %v", data, err)
	}
	info, err := os.Stat(destination)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("copied mode = %v", info.Mode())
	}
	if err := CopyRegular(source, destination, 0o755); err == nil {
		t.Fatal("copy overwrote an existing payload")
	}
	link := filepath.Join(directory, "source-link")
	if err := os.Symlink(source, link); err != nil {
		t.Fatal(err)
	}
	if err := CopyRegular(link, filepath.Join(directory, "linked"), 0o644); err == nil {
		t.Fatal("copy followed a source symlink")
	}
}

func TestSHA256(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	digest, err := SHA256(path)
	if err != nil {
		t.Fatal(err)
	}
	if digest != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Fatalf("SHA-256 of empty file = %s", digest)
	}
	if _, err := SHA256(strings.Repeat("missing", 10)); err == nil {
		t.Fatal("missing file has a digest")
	}
}
