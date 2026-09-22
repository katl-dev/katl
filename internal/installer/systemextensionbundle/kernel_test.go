package systemextensionbundle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/kernelmodule"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestKernelBundleContract(t *testing.T) {
	file := filepath.Join(t.TempDir(), "drbd.raw")
	if err := os.WriteFile(file, []byte("fixture payload"), 0600); err != nil {
		t.Fatal(err)
	}
	built, err := Build(BuildRequest{
		Name:                       "drbd9",
		ArtifactVersion:            "2026.9.1",
		PayloadVersion:             "9.3.0",
		Architecture:               "x86_64",
		SupportedRuntimeInterfaces: []string{"katl-runtime-1"},
		CreatedAt:                  time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC),
		Payloads: []Input{{
			Path: file,
			Role: SysextRole,
		}},
		Kernel: &kernelmodule.Contract{
			Target: kernelmodule.Target{
				Release:       "6.12",
				RuntimeSHA256: strings.Repeat("a", 64),
			},
			Modules: []kernelmodule.Module{{
				Name:   "drbd",
				Path:   "usr/lib/modules/6.12/extra/drbd.ko",
				SHA256: strings.Repeat("b", 64),
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if built.Bundle.ArtifactKind != "katl.system-extension.v2" {
		t.Fatal("kernel bundle can be accepted by a v1 consumer")
	}
	manifest := ocispec.Manifest{Layers: []ocispec.Descriptor{ociDescriptor(built.Blobs[0].Descriptor)}}
	if err := validateBundle(built.Bundle, manifest, ResolveRequest{RuntimeSHA256: strings.Repeat("c", 64)}); err == nil {
		t.Fatal("accepted another kernel build")
	}
	if err := validateBundle(built.Bundle, manifest, ResolveRequest{RuntimeSHA256: strings.Repeat("a", 64)}); err != nil {
		t.Fatal(err)
	}

	built.Bundle.ArtifactKind = ArtifactKind
	if err := validateBundle(built.Bundle, manifest, ResolveRequest{}); err == nil {
		t.Fatal("accepted kernel constraints in a v1 artifact")
	}
}
