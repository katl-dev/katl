package installmedia

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoad(t *testing.T) {
	root := t.TempDir()
	image := []byte("katlos image")
	metadata := Metadata{
		APIVersion:       APIVersion,
		Kind:             Kind,
		ImageRole:        "install",
		Format:           "squashfs",
		Version:          "2026.7.0",
		Architecture:     "x86_64",
		RuntimeInterface: "katl-runtime-1",
		Path:             "katlos-install.squashfs",
		SizeBytes:        int64(len(image)),
		SHA256:           strings.Repeat("a", 64),
	}
	writeMedia(t, root, metadata, image)

	media, found, err := Load(root)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !found {
		t.Fatal("Load() found = false")
	}
	if media.Image.LocalRef != "images/katlos-install.squashfs" || media.Image.Version != metadata.Version || media.Image.SHA256 != metadata.SHA256 {
		t.Fatalf("Load() image = %#v", media.Image)
	}
}

func TestLoadRejectsMismatch(t *testing.T) {
	root := t.TempDir()
	metadata := Metadata{
		APIVersion: APIVersion, Kind: Kind, ImageRole: "install", Format: "squashfs",
		Version: "2026.7.0", Architecture: "x86_64", RuntimeInterface: "katl-runtime-1",
		Path: "katlos-install.squashfs", SizeBytes: 99, SHA256: strings.Repeat("a", 64),
	}
	writeMedia(t, root, metadata, []byte("short"))
	if _, _, err := Load(root); err == nil || !strings.Contains(err.Error(), "size") {
		t.Fatalf("Load() error = %v, want size mismatch", err)
	}
}

func TestLoadAbsent(t *testing.T) {
	if _, found, err := Load(t.TempDir()); err != nil || found {
		t.Fatalf("Load() found = %v, error = %v", found, err)
	}
}

func writeMedia(t *testing.T, root string, metadata Metadata, image []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "images"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "images", metadata.Path), image, 0o644); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "media.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}
