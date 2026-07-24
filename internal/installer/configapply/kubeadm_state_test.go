package configapply

import (
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/installer/generation"
	"github.com/katl-dev/katl/internal/installer/kubeadmconfig"
)

func TestGenerationKubeadmConfigFollowsGenerationAncestry(t *testing.T) {
	root := t.TempDir()
	writeKubeadmStateGeneration(t, root, "generation-bootstrap", "")
	writeKubeadmStateGeneration(t, root, "generation-host-config", "generation-bootstrap")
	want := "apiVersion: kubeadm.k8s.io/v1beta4\nkind: InitConfiguration\n"
	plan, err := kubeadmconfig.PlanFromRenderedFiles("control-plane", []kubeadmconfig.File{{
		RenderPath: "/etc/katl/kubeadm/control-plane/config.yaml",
		Content:    []byte(want),
		Mode:       0o644,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteGenerationKubeadmConfig(root, "generation-bootstrap", "control-plane", plan); err != nil {
		t.Fatal(err)
	}

	got, source, err := ReadEffectiveGenerationKubeadmConfig(root, "generation-host-config", "control-plane")
	if err != nil {
		t.Fatal(err)
	}
	if source != "generation-bootstrap" || string(got.Config.Content) != want {
		t.Fatalf("effective kubeadm input = source %q content %q", source, got.Config.Content)
	}
}

func TestGenerationKubeadmConfigRejectsAncestryCycle(t *testing.T) {
	root := t.TempDir()
	writeKubeadmStateGeneration(t, root, "generation-a", "generation-b")
	writeKubeadmStateGeneration(t, root, "generation-b", "generation-a")

	_, _, err := ReadEffectiveGenerationKubeadmConfig(root, "generation-a", "control-plane")
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("ReadEffectiveGenerationKubeadmConfig() error = %v, want cycle", err)
	}
}

func TestGenerationKubeadmConfigRejectsUnsafeRef(t *testing.T) {
	_, _, err := ReadEffectiveGenerationKubeadmConfig(t.TempDir(), "generation-a", "../control-plane")
	if err == nil || !strings.Contains(err.Error(), "single path segment") {
		t.Fatalf("ReadEffectiveGenerationKubeadmConfig() error = %v, want unsafe ref", err)
	}
}

func writeKubeadmStateGeneration(t *testing.T, root, id, previous string) {
	t.Helper()
	createdAt := time.Date(2026, 7, 24, 19, 0, 0, 0, time.UTC)
	spec := generation.GenerationSpec{
		APIVersion: generation.APIVersion, Kind: generation.SpecKind,
		GenerationID: id, PreviousGenerationID: previous,
		RuntimeVersion: "2026.7.0", CreatedAt: createdAt,
		Root: generation.RootSelection{
			Slot: "root-a", PartitionUUID: "aaaaaaaa-1111-2222-3333-444444444444",
			RuntimeVersion: "2026.7.0", RuntimeInterface: "katl-runtime-1",
			Architecture: "x86_64", RuntimeArtifactSHA256: strings.Repeat("a", 64),
		},
		Boot: generation.BootSelection{
			UKIPath: "/efi/EFI/Linux/katl_2026.7.0.efi",
		},
	}
	status, err := generation.NewGenerationStatus(spec, generation.CommitStateCommitted, generation.BootStateGood, generation.HealthStateHealthy, createdAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := generation.WriteGeneration(root, spec, status); err != nil {
		t.Fatal(err)
	}
}
