package configapply

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/installer/generation"
)

func TestGenerationManifestRoundTrip(t *testing.T) {
	root := t.TempDir()
	want := baseManifest()
	want.Node.ControlPlaneEndpoint = managedEndpoint("192.0.2.1")
	if err := WriteGenerationManifest(root, "generation-1", want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadGenerationManifest(root, "generation-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Node.ControlPlaneEndpoint == nil || got.Node.ControlPlaneEndpoint.Advertisement == nil || got.Node.ControlPlaneEndpoint.Advertisement.BGP.Peers[0].Address != "192.0.2.1" {
		t.Fatalf("generation manifest endpoint = %#v", got.Node.ControlPlaneEndpoint)
	}
}

func TestReadGenerationManifestPreservesNotExist(t *testing.T) {
	_, err := ReadGenerationManifest(t.TempDir(), "generation-0")
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReadGenerationManifest() error = %v, want os.ErrNotExist", err)
	}
}

func TestReadEffectiveGenerationManifestInheritsRuntimeUpgradeConfiguration(t *testing.T) {
	root := t.TempDir()
	want := baseManifest()
	if err := WriteGenerationManifest(root, "generation-config", want); err != nil {
		t.Fatal(err)
	}
	createdAt := time.Date(2026, 7, 24, 18, 0, 0, 0, time.UTC)
	spec := generation.GenerationSpec{
		APIVersion: generation.APIVersion, Kind: generation.SpecKind,
		GenerationID: "generation-upgrade", PreviousGenerationID: "generation-config",
		RuntimeVersion: "2026.7.0", CreatedAt: createdAt,
		Root: generation.RootSelection{
			Slot: "root-b", PartitionUUID: "aaaaaaaa-1111-2222-3333-444444444444",
			RuntimeVersion: "2026.7.0", RuntimeInterface: "katl-runtime-1",
			Architecture: "x86_64", RuntimeArtifactSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
		Boot: generation.BootSelection{
			UKIPath: "/efi/EFI/Linux/katl_2026.7.0.efi", LoaderEntryPath: "loader/entries/katl-generation-upgrade.conf",
		},
	}
	status, err := generation.NewGenerationStatus(spec, generation.CommitStateCommitted, generation.BootStateTrying, generation.HealthStateUnknown, createdAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := generation.WriteGeneration(root, spec, status); err != nil {
		t.Fatal(err)
	}

	got, source, err := ReadEffectiveGenerationManifest(root, spec.GenerationID)
	if err != nil {
		t.Fatal(err)
	}
	if source != "generation-config" || got.Node.Identity.Hostname != want.Node.Identity.Hostname {
		t.Fatalf("effective manifest = source %q node %#v, want source generation-config node %#v", source, got.Node, want.Node)
	}
}
