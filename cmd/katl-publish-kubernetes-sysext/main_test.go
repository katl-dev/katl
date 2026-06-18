package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zariel/katl/internal/installer/artifact"
)

func TestRunRequiresMetadata(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	err := run([]string{"--metadata", "/missing", "--output-dir", t.TempDir()}, &stdout, &stderr)
	if err == nil {
		t.Fatal("run() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "read Kubernetes sysext metadata") {
		t.Fatalf("run() error = %v", err)
	}
}

func TestRunPrintsBundleOutputs(t *testing.T) {
	dir := t.TempDir()
	outputDir := filepath.Join(dir, "publish")
	_, metadataPath := writeTestSysextArtifact(t, dir)
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	err := run([]string{"--metadata", metadataPath, "--output-dir", outputDir}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run() error = %v, stderr %s", err, stderr.String())
	}
	for _, want := range []string{
		"artifact: " + filepath.Join(outputDir, "katl-kubernetes-v1.36.0-x86_64.sysext.raw"),
		"bundle: " + filepath.Join(outputDir, "bundles", "v1.36.0", "x86_64", "bundle.json"),
		"bundle-manifest-digest: sha256:",
		"bundle-index: " + filepath.Join(outputDir, "index.json"),
		"bundle-catalog: " + filepath.Join(outputDir, "catalog", "v1.36.json"),
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("stdout missing %q:\n%s", want, stdout.String())
		}
	}
}

func writeTestSysextArtifact(t *testing.T, dir string) (string, string) {
	t.Helper()
	rawPath := filepath.Join(dir, "katl-kubernetes.raw")
	payload := []byte("kubernetes sysext")
	if err := os.WriteFile(rawPath, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	meta := artifact.LocalMeta{
		Name:           "kubernetes",
		Kind:           artifact.ArtifactSysext,
		Format:         "sysext",
		Path:           filepath.Base(rawPath),
		SizeBytes:      int64(len(payload)),
		SHA256:         hex.EncodeToString(sum[:]),
		Version:        "v1.36.0-build.1",
		PayloadVersion: "v1.36.0",
		Architecture:   "x86_64",
		SourceRepo: &artifact.SourceRepo{
			ID:      "kubernetes",
			BaseURL: "https://pkgs.k8s.io/core:/stable:/v1.36/rpm/",
			Minor:   "v1.36",
		},
		PackageVersions: map[string]string{
			"kubeadm": "0:1.36.0-150500.1.1",
			"kubectl": "0:1.36.0-150500.1.1",
			"kubelet": "0:1.36.0-150500.1.1",
		},
		RuntimeInterface: "katl-runtime-1",
		CompatibleRuntime: &artifact.Compat{
			Interface: "katl-runtime-1",
		},
		Created: "2026-06-18T12:00:00Z",
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	metadataPath := rawPath + ".json"
	if err := os.WriteFile(metadataPath, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	return rawPath, metadataPath
}
