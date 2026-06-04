package sysextcatalog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zariel/katl/internal/installer/artifact"
)

func TestStageKubernetesSysext(t *testing.T) {
	sourceDir := t.TempDir()
	outputDir := t.TempDir()
	rawPath, metadataPath := writeSysextArtifact(t, sourceDir, "sysext payload")

	staged, err := StageKubernetesSysext(StageRequest{
		MetadataPath: metadataPath,
		OutputDir:    outputDir,
	})
	if err != nil {
		t.Fatalf("StageKubernetesSysext() error = %v", err)
	}

	wantName := "katl-kubernetes-v1.36.1-x86_64.sysext.raw"
	if filepath.Base(staged.ArtifactPath) != wantName {
		t.Fatalf("artifact name = %q, want %q", filepath.Base(staged.ArtifactPath), wantName)
	}
	for _, path := range []string{staged.ArtifactPath, staged.ChecksumPath, staged.MetadataPath, staged.CatalogPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("stat staged output %s: %v", path, err)
		}
	}

	checksum := readText(t, staged.ChecksumPath)
	if !strings.HasSuffix(checksum, "  "+wantName+"\n") {
		t.Fatalf("checksum = %q", checksum)
	}

	var stagedMeta artifact.LocalMeta
	readJSON(t, staged.MetadataPath, &stagedMeta)
	if stagedMeta.Path != wantName {
		t.Fatalf("staged metadata path = %q, want %q", stagedMeta.Path, wantName)
	}
	if stagedMeta.CompatibleRuntime == nil || stagedMeta.CompatibleRuntime.ArtifactPath != "katl-runtime-root.squashfs" {
		t.Fatalf("staged compatible runtime = %#v", stagedMeta.CompatibleRuntime)
	}

	catalog := readCatalog(t, staged.CatalogPath)
	if len(catalog.Entries) != 1 {
		t.Fatalf("catalog entry count = %d, want 1", len(catalog.Entries))
	}
	entry := catalog.Entries[0]
	if entry.LocalPath != wantName || entry.URL != "" {
		t.Fatalf("catalog entry location = local %q URL %q", entry.LocalPath, entry.URL)
	}
	if entry.PayloadVersion != "v1.36.1" || entry.KubernetesMinor != "v1.36" || entry.Architecture != "x86_64" {
		t.Fatalf("catalog entry = %#v", entry)
	}

	for _, path := range []string{staged.MetadataPath, staged.CatalogPath, staged.ChecksumPath} {
		data := readText(t, path)
		if strings.Contains(data, sourceDir) || strings.Contains(data, outputDir) || strings.Contains(data, rawPath) {
			t.Fatalf("%s contains host path: %s", path, data)
		}
	}
}

func TestStageKubernetesSysextWithBaseURL(t *testing.T) {
	sourceDir := t.TempDir()
	outputDir := t.TempDir()
	_, metadataPath := writeSysextArtifact(t, sourceDir, "sysext payload")

	staged, err := StageKubernetesSysext(StageRequest{
		MetadataPath: metadataPath,
		OutputDir:    outputDir,
		BaseURL:      "https://artifacts.example.invalid/katl/kubernetes",
	})
	if err != nil {
		t.Fatalf("StageKubernetesSysext() error = %v", err)
	}

	entry := readCatalog(t, staged.CatalogPath).Entries[0]
	if entry.LocalPath != "" {
		t.Fatalf("catalog local path = %q, want empty", entry.LocalPath)
	}
	if entry.URL != "https://artifacts.example.invalid/katl/kubernetes/katl-kubernetes-v1.36.1-x86_64.sysext.raw" {
		t.Fatalf("catalog URL = %q", entry.URL)
	}
}

func TestStageKubernetesSysextRejectsDigestMismatch(t *testing.T) {
	sourceDir := t.TempDir()
	outputDir := t.TempDir()
	rawPath, metadataPath := writeSysextArtifact(t, sourceDir, "sysext payload")
	if err := os.WriteFile(rawPath, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := StageKubernetesSysext(StageRequest{
		MetadataPath: metadataPath,
		OutputDir:    outputDir,
	})
	if !errors.Is(err, ErrInvalidCatalog) {
		t.Fatalf("StageKubernetesSysext() error = %v, want ErrInvalidCatalog", err)
	}
}

func TestStageKubernetesSysextRejectsNonKubernetesSysext(t *testing.T) {
	sourceDir := t.TempDir()
	outputDir := t.TempDir()
	_, metadataPath := writeSysextArtifact(t, sourceDir, "sysext payload")

	var meta artifact.LocalMeta
	readJSON(t, metadataPath, &meta)
	meta.Name = "storage"
	metadata, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metadataPath, append(metadata, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = StageKubernetesSysext(StageRequest{
		MetadataPath: metadataPath,
		OutputDir:    outputDir,
	})
	if !errors.Is(err, ErrInvalidCatalog) {
		t.Fatalf("StageKubernetesSysext() error = %v, want ErrInvalidCatalog", err)
	}
}

func writeSysextArtifact(t *testing.T, dir string, payload string) (string, string) {
	t.Helper()

	rawPath := filepath.Join(dir, "katl-kubernetes.raw")
	if err := os.WriteFile(rawPath, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	digestBytes := sha256.Sum256([]byte(payload))
	digest := hex.EncodeToString(digestBytes[:])

	meta := artifact.LocalMeta{
		Name:           "kubernetes",
		Kind:           artifact.ArtifactSysext,
		Format:         "sysext",
		Path:           filepath.Base(rawPath),
		SizeBytes:      int64(len(payload)),
		SHA256:         digest,
		Version:        "build-001",
		PayloadVersion: "v1.36.1",
		Architecture:   "x86_64",
		SourceRepo: &artifact.SourceRepo{
			ID:      "kubernetes",
			BaseURL: "https://pkgs.k8s.io/core:/stable:/v1.36/rpm/",
			Minor:   "v1.36",
		},
		RuntimeInterface: "katl-runtime-1",
		CompatibleRuntime: &artifact.Compat{
			Interface:    "katl-runtime-1",
			ArtifactPath: filepath.Join(dir, "katl-runtime-root.squashfs"),
		},
		Created: "2026-06-04T20:00:00Z",
	}
	metadata, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	metadataPath := rawPath + ".json"
	if err := os.WriteFile(metadataPath, append(metadata, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	return rawPath, metadataPath
}

func readText(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func readJSON(t *testing.T, path string, dest any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, dest); err != nil {
		t.Fatal(err)
	}
}

func readCatalog(t *testing.T, path string) Catalog {
	t.Helper()
	catalog, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}
