package sysextcatalog

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/katl-dev/katl/internal/generation"
)

func TestSelectDefault(t *testing.T) {
	ref, err := Select(SelectionRequest{
		Catalog:          validCatalog(t),
		Runtime:          Runtime{Interface: "katl-runtime-1", Architecture: "x86_64"},
		ArtifactBasePath: "/var/lib/katl/generations/2026.06.04-001/sysext",
	})
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}

	if ref.PayloadVersion != "v1.36.1" {
		t.Fatalf("payload version = %q, want latest compatible v1.36.1", ref.PayloadVersion)
	}
	if ref.Path != "/var/lib/katl/generations/2026.06.04-001/sysext/katl-kubernetes.raw" {
		t.Fatalf("path = %q", ref.Path)
	}
	if ref.ActivationPath != "/run/extensions/katl-kubernetes.raw" {
		t.Fatalf("activation path = %q", ref.ActivationPath)
	}
	assertKubernetesRef(t, ref)
}

func TestSelectLatestPatchWithinMinor(t *testing.T) {
	ref, err := Select(SelectionRequest{
		Catalog: validCatalog(t),
		Version: "v1.36",
		Runtime: Runtime{Interface: "katl-runtime-1", Architecture: "x86_64"},
	})
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	if ref.PayloadVersion != "v1.36.1" {
		t.Fatalf("payload version = %q, want v1.36.1", ref.PayloadVersion)
	}
}

func TestSelectExplicitDefault(t *testing.T) {
	ref, err := Select(SelectionRequest{
		Catalog: validCatalog(t),
		Version: "default",
		Runtime: Runtime{Interface: "katl-runtime-1", Architecture: "x86_64"},
	})
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	if ref.PayloadVersion != "v1.36.1" {
		t.Fatalf("payload version = %q, want v1.36.1", ref.PayloadVersion)
	}
}

func TestSelectExplicitDefaultOverridesExisting(t *testing.T) {
	existing := validExistingRef()

	ref, err := Select(SelectionRequest{
		Catalog:  validCatalog(t),
		Version:  "default",
		Runtime:  Runtime{Interface: "katl-runtime-1", Architecture: "x86_64"},
		Existing: &existing,
	})
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	if ref.PayloadVersion != "v1.36.1" {
		t.Fatalf("payload version = %q, want default v1.36.1", ref.PayloadVersion)
	}
	if ref.ArtifactVersion == existing.ArtifactVersion {
		t.Fatalf("explicit default preserved existing ref: %#v", ref)
	}
}

func TestSelectExactVersion(t *testing.T) {
	ref, err := Select(SelectionRequest{
		Catalog:        validCatalog(t),
		Version:        "v1.36.0",
		Runtime:        Runtime{Interface: "katl-runtime-1", Architecture: "x86_64"},
		ActivationPath: "/run/extensions/kubernetes",
	})
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	if ref.PayloadVersion != "v1.36.0" {
		t.Fatalf("payload version = %q, want v1.36.0", ref.PayloadVersion)
	}
	if ref.Path != "https://artifacts.example.invalid/katl/kubernetes/v1.36.0/x86_64/katl-kubernetes.raw" {
		t.Fatalf("path = %q", ref.Path)
	}
	if ref.ActivationPath != "/run/extensions/kubernetes" {
		t.Fatalf("activation path = %q", ref.ActivationPath)
	}
}

func TestSelectRejectsUnsupportedVersion(t *testing.T) {
	_, err := Select(SelectionRequest{
		Catalog: validCatalog(t),
		Version: "v1.37",
		Runtime: Runtime{Interface: "katl-runtime-1", Architecture: "x86_64"},
	})
	if !errors.Is(err, ErrInvalidCatalog) {
		t.Fatalf("Select() error = %v, want ErrInvalidCatalog", err)
	}
}

func TestSelectRejectsMalformedVersion(t *testing.T) {
	_, err := Select(SelectionRequest{
		Catalog: validCatalog(t),
		Version: "1.36",
		Runtime: Runtime{Interface: "katl-runtime-1", Architecture: "x86_64"},
	})
	if !errors.Is(err, ErrInvalidCatalog) {
		t.Fatalf("Select() error = %v, want ErrInvalidCatalog", err)
	}
}

func TestSelectRejectsArchitectureMismatch(t *testing.T) {
	_, err := Select(SelectionRequest{
		Catalog: validCatalog(t),
		Version: "v1.36",
		Runtime: Runtime{Interface: "katl-runtime-1", Architecture: "aarch64"},
	})
	if !errors.Is(err, ErrInvalidCatalog) {
		t.Fatalf("Select() error = %v, want ErrInvalidCatalog", err)
	}
}

func TestSelectRejectsRuntimeMismatch(t *testing.T) {
	_, err := Select(SelectionRequest{
		Catalog: validCatalog(t),
		Version: "v1.36",
		Runtime: Runtime{Interface: "katl-runtime-2", Architecture: "x86_64"},
	})
	if !errors.Is(err, ErrInvalidCatalog) {
		t.Fatalf("Select() error = %v, want ErrInvalidCatalog", err)
	}
}

func TestSelectRejectsUnsafeLocalPath(t *testing.T) {
	catalog := validCatalog(t)
	catalog.Entries[0].LocalPath = "../katl-kubernetes.raw"

	_, err := Select(SelectionRequest{
		Catalog:          catalog,
		Version:          "v1.36.1",
		Runtime:          Runtime{Interface: "katl-runtime-1", Architecture: "x86_64"},
		ArtifactBasePath: "/var/lib/katl/generations/2026.06.04-001/sysext",
	})
	if !errors.Is(err, ErrInvalidCatalog) {
		t.Fatalf("Select() error = %v, want ErrInvalidCatalog", err)
	}
}

func TestSelectPreservesExistingWhenNoChangeRequested(t *testing.T) {
	existing := validExistingRef()

	ref, err := Select(SelectionRequest{
		Catalog:  validCatalog(t),
		Runtime:  Runtime{Interface: "katl-runtime-1", Architecture: "x86_64"},
		Existing: &existing,
	})
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	if ref.Name != existing.Name || ref.Path != existing.Path || ref.PayloadVersion != existing.PayloadVersion || ref.ArtifactVersion != existing.ArtifactVersion {
		t.Fatalf("ref = %#v, want existing %#v", ref, existing)
	}
}

func TestSelectRejectsIncompatibleExisting(t *testing.T) {
	existing := validExistingRef()

	_, err := Select(SelectionRequest{
		Catalog:  validCatalog(t),
		Runtime:  Runtime{Interface: "katl-runtime-2", Architecture: "x86_64"},
		Existing: &existing,
	})
	if !errors.Is(err, ErrInvalidCatalog) {
		t.Fatalf("Select() error = %v, want ErrInvalidCatalog", err)
	}
}

func TestSelectRejectsIncompleteExisting(t *testing.T) {
	existing := validExistingRef()
	existing.SHA256 = ""

	_, err := Select(SelectionRequest{
		Catalog:  validCatalog(t),
		Runtime:  Runtime{Interface: "katl-runtime-1", Architecture: "x86_64"},
		Existing: &existing,
	})
	if !errors.Is(err, ErrInvalidCatalog) {
		t.Fatalf("Select() error = %v, want ErrInvalidCatalog", err)
	}
}

func assertKubernetesRef(t *testing.T, ref generation.ExtensionRef) {
	t.Helper()
	if ref.Name != "kubernetes" {
		t.Fatalf("name = %q", ref.Name)
	}
	if ref.SHA256 == "" || ref.ArtifactVersion == "" || ref.Architecture != "x86_64" {
		t.Fatalf("ref missing artifact metadata: %#v", ref)
	}
	if len(ref.Compatibility.RuntimeInterfaces) != 1 || ref.Compatibility.RuntimeInterfaces[0] != "katl-runtime-1" {
		t.Fatalf("runtime interfaces = %#v", ref.Compatibility.RuntimeInterfaces)
	}
}

func validExistingRef() generation.ExtensionRef {
	return generation.ExtensionRef{
		Name:            "kubernetes",
		Path:            filepath.Join("/var/lib/katl/generations", "previous", "sysext", "kubernetes.raw"),
		ActivationPath:  "/run/extensions/kubernetes.raw",
		SHA256:          "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ArtifactVersion: "previous-build",
		PayloadVersion:  "v1.35.7",
		Architecture:    "x86_64",
		Compatibility: generation.ExtensionCompatibility{
			RuntimeInterfaces: []string{"katl-runtime-1"},
		},
	}
}
