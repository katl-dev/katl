package persistedrecord_test

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/persistedrecord"
)

// A generation's content is materialized with ordinary writes and published
// by its spec, which later references (status, selection, loader entry)
// follow. Once the spec survives a crash, the content it names must too.
func TestWriteGenerationContentDurableBeforeSpec(t *testing.T) {
	root := t.TempDir()
	id := "candidate"
	created := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	generationPath := filepath.Join(generation.GenerationRecordsDir, id)
	record, err := generation.NewFirstInstallRecord(generation.FirstInstallRequest{
		Root: generation.RootSelection{
			RuntimeVersion:        "0.1.0",
			RuntimeInterface:      "katl-runtime-1",
			Architecture:          "x86_64",
			Slot:                  "root-a",
			PartitionUUID:         "11111111-2222-3333-4444-555555555555",
			RuntimeArtifactSHA256: strings.Repeat("a", 64),
		},
		GenerationID: id,
		UKIPath:      "/efi/EFI/Linux/katl-" + id + ".efi",
		GeneratedConfext: generation.GeneratedConfext{
			Name:           "katl-node",
			Path:           filepath.Join(generationPath, "confext"),
			ActivationPath: "/run/confexts/katl-node",
			SHA256:         strings.Repeat("c", 64),
			Compatibility: generation.ConfextCompatibility{
				ID:           "katlos",
				VersionID:    "0.1.0",
				ConfextLevel: 1,
			},
		},
		Sysexts: []generation.ExtensionRef{{
			Name:            "kubernetes",
			Path:            filepath.Join(generationPath, "sysext", "kubernetes.raw"),
			ActivationPath:  "/run/extensions/kubernetes.raw",
			SHA256:          strings.Repeat("b", 64),
			ArtifactVersion: "k8s-v1.36.1",
			PayloadVersion:  "v1.36.1",
			Architecture:    "x86_64",
			Compatibility: generation.ExtensionCompatibility{
				RuntimeInterfaces: []string{"katl-runtime-1"},
			},
		}},
		CreatedAt: created,
	})
	if err != nil {
		t.Fatalf("NewFirstInstallRecord() error = %v", err)
	}
	spec := generation.SpecFromRecord(record)
	status, err := generation.NewGenerationStatus(spec, generation.CommitStateCandidate, generation.BootStatePending, generation.HealthStateUnknown, created)
	if err != nil {
		t.Fatalf("NewGenerationStatus() error = %v", err)
	}

	dir, err := generation.GenerationDir(root, id)
	if err != nil {
		t.Fatal(err)
	}
	specPath := filepath.Join(dir, "spec.json")
	statusPath := filepath.Join(dir, "status.json")
	content := map[string][]byte{
		filepath.Join(root, spec.Confexts[0].Path, "etc", "sysctl.d", "katl.conf"): []byte("vm.swappiness = 1\n"),
		filepath.Join(root, spec.Sysexts[0].Path):                                  []byte("hsqs kubernetes\n"),
	}

	persistedrecord.CrashEachStep(t, func(fs *persistedrecord.CrashFS) {
		// The records directory exists; the materializer created the rest.
		for _, sub := range []string{"", "confext", "confext/etc", "confext/etc/sysctl.d", "sysext"} {
			fs.SeedVolatileDir(filepath.Join(dir, sub))
		}
		for path, data := range content {
			fs.SeedVolatile(path, data)
		}
	}, func() error {
		return generation.WriteGeneration(root, spec, status)
	}, func(t *testing.T, fs *persistedrecord.CrashFS, returned bool) {
		published, ok := fs.Read(specPath)
		if returned {
			if _, statusOK := fs.Read(statusPath); !ok || len(published) == 0 || !statusOK {
				t.Fatalf("after a successful return spec present %v (%d bytes), status present %v", ok, len(published), statusOK)
			}
		}
		if !ok || len(published) == 0 {
			return
		}
		for path, want := range content {
			if got, present := fs.Read(path); !bytes.Equal(got, want) {
				t.Errorf("spec is published but %s is %q (present %v), want %q", path, got, present, want)
			}
		}
	})
}
