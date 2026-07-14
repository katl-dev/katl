package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/installer/generation"
)

func TestGenerationActivateUsesSelectedMetadata(t *testing.T) {
	root := t.TempDir()
	record := commandActivationRecord(t, root, "2026.06.05-001")
	metadataPath, err := generation.MetadataPath(root, record.GenerationID)
	if err != nil {
		t.Fatalf("MetadataPath() error = %v", err)
	}
	if err := generation.WriteRecord(metadataPath, record); err != nil {
		t.Fatalf("WriteRecord() error = %v", err)
	}
	cmdline := filepath.Join(root, "proc-cmdline")
	if err := os.WriteFile(cmdline, []byte("root=PARTUUID=abc katl.generation=2026.06.05-001\n"), 0o644); err != nil {
		t.Fatalf("write cmdline: %v", err)
	}

	var stdout bytes.Buffer
	if err := run(t.Context(), []string{"--root", root, "--cmdline", cmdline}, &stdout); err != nil {
		t.Fatalf("run() error = %v", err)
	}

	assertCommandSymlink(t, filepath.Join(root, "run/extensions/kubernetes.raw"), record.Sysexts[0].Path)
	assertCommandSymlink(t, filepath.Join(root, "run/confexts/katl-node"), record.Confexts[0].Path)
	if !strings.Contains(stdout.String(), "generation=2026.06.05-001") {
		t.Fatalf("stdout = %q, want selected generation", stdout.String())
	}
}

func TestGenerationActivateRejectsMismatchedMetadata(t *testing.T) {
	root := t.TempDir()
	record := commandActivationRecord(t, root, "2026.06.05-001")
	metadataPath, err := generation.MetadataPath(root, record.GenerationID)
	if err != nil {
		t.Fatalf("MetadataPath() error = %v", err)
	}
	record.GenerationID = "2026.06.05-002"
	if err := generation.WriteRecord(metadataPath, record); err != nil {
		t.Fatalf("WriteRecord() error = %v", err)
	}

	err = run(t.Context(), []string{"--root", root, "--generation", "2026.06.05-001"}, nil)
	if err == nil || !strings.Contains(err.Error(), "does not match selected generation") {
		t.Fatalf("run() error = %v, want metadata mismatch", err)
	}
}

func TestActivateHostnameUsesSelectedVerifiedConfext(t *testing.T) {
	root := t.TempDir()
	confext := "/var/lib/katl/generations/1/confext"
	writeCommandFile(t, filepath.Join(root, strings.TrimPrefix(confext, "/"), "etc/hostname"), "cp-1\n")
	var selected string
	hostname, err := activateHostname(root, generation.ActivationPlan{
		Confexts: []generation.ActivationLink{{Name: "katl-node", SourcePath: confext}},
	}, func(value []byte) error {
		selected = string(value)
		return nil
	})
	if err != nil {
		t.Fatalf("activateHostname() error = %v", err)
	}
	if hostname != "cp-1" || selected != "cp-1" {
		t.Fatalf("hostname = %q, selected = %q", hostname, selected)
	}
}

func TestActivateHostnameKeepsBaseHostnameForOlderGeneration(t *testing.T) {
	called := false
	hostname, err := activateHostname(t.TempDir(), generation.ActivationPlan{}, func([]byte) error {
		called = true
		return nil
	})
	if err != nil || hostname != "" || called {
		t.Fatalf("activateHostname() hostname = %q, called = %t, error = %v", hostname, called, err)
	}
}

func commandActivationRecord(t *testing.T, root string, id string) generation.Record {
	t.Helper()
	sysextContent := "selected sysext\n"
	sysextPath := filepath.ToSlash(filepath.Join(generation.GenerationRecordsDir, id, "sysext", "kubernetes.raw"))
	confextPath := filepath.ToSlash(filepath.Join(generation.GenerationRecordsDir, id, "confext"))
	writeCommandFile(t, filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(sysextPath, "/"))), sysextContent)
	writeCommandFile(t, filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(confextPath, "/")), "etc/hostname"), "katl-node\n")
	confextDigest, err := generation.DigestDirectory(filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(confextPath, "/"))))
	if err != nil {
		t.Fatalf("DigestDirectory() error = %v", err)
	}
	sum := sha256.Sum256([]byte(sysextContent))
	return generation.Record{
		APIVersion:   generation.APIVersion,
		Kind:         generation.Kind,
		GenerationID: id,
		Root: generation.RootSelection{
			RuntimeInterface: "katl-runtime-1",
			Architecture:     "x86_64",
		},
		Sysexts: []generation.ExtensionRef{
			{
				Name:            "kubernetes",
				Path:            sysextPath,
				ActivationPath:  "/run/extensions/kubernetes.raw",
				SHA256:          hex.EncodeToString(sum[:]),
				ArtifactVersion: "k8s-v1.34.8",
				PayloadVersion:  "v1.34.8",
				Architecture:    "x86_64",
				Compatibility: generation.ExtensionCompatibility{
					RuntimeInterfaces: []string{"katl-runtime-1"},
				},
			},
		},
		Confexts: []generation.GeneratedConfext{
			{
				Name:           "katl-node",
				Path:           confextPath,
				ActivationPath: "/run/confexts/katl-node",
				SHA256:         confextDigest,
				Compatibility: generation.ConfextCompatibility{
					ID:           "katl",
					VersionID:    "0.1.0",
					ConfextLevel: 1,
				},
			},
		},
	}
}

func writeCommandFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func assertCommandSymlink(t *testing.T, path string, want string) {
	t.Helper()
	got, err := os.Readlink(path)
	if err != nil {
		t.Fatalf("readlink %s: %v", path, err)
	}
	if got != want {
		t.Fatalf("%s -> %s, want %s", path, got, want)
	}
}
