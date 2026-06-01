package artifact

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReadLocal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime-root.squashfs.json")
	data := `{
  "name": "runtime-root",
  "kind": "runtime-root",
  "format": "squashfs",
  "path": "runtime-root.squashfs",
  "sizeBytes": 4096,
  "sha256": "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824",
  "compression": "zstd",
  "generation": "abc123",
  "architecture": "x86_64",
  "created": "2026-06-01T00:00:00Z"
}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}

	meta, err := ReadLocal(path)
	if err != nil {
		t.Fatalf("ReadLocal() error = %v", err)
	}
	if meta.Format != "squashfs" || meta.Compression != "zstd" {
		t.Fatalf("meta = %#v", meta)
	}

	spec := meta.Spec("https://artifacts.example/katl")
	if spec.URL != "https://artifacts.example/katl/runtime-root.squashfs" {
		t.Fatalf("spec URL = %q", spec.URL)
	}
	if spec.SizeBytes != 4096 || spec.SHA256 != meta.SHA256 {
		t.Fatalf("spec = %#v", spec)
	}
}

func TestReadLocalBad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime-root.squashfs.json")
	if err := os.WriteFile(path, []byte(`{"name":"runtime-root"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := ReadLocal(path)
	if !errors.Is(err, ErrInvalidArtifactSpec) {
		t.Fatalf("ReadLocal() error = %v, want ErrInvalidArtifactSpec", err)
	}
}
