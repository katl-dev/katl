package agent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/configapply"
	"github.com/katl-dev/katl/internal/installer/kubeadmconfig"
)

func TestUpgradeSourceConfigurationFromFrozenSnapshot(t *testing.T) {
	host, snapshot := frozenConfigurationSnapshot(t)
	if err := completeUpgradeSourceConfiguration(snapshot, host, "bootstrap"); err != nil {
		t.Fatal(err)
	}
	configuration, err := configapply.ReadLocalGenerationConfiguration(snapshot, "bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	if configuration.Manifest.Node.Identity.Hostname != "configured-node" {
		t.Fatalf("configuration reverted to installation: %q", configuration.Manifest.Node.Identity.Hostname)
	}
	ref := configuration.Manifest.Node.Kubernetes.Kubeadm.ConfigRef
	if !strings.Contains(string(configuration.KubeadmConfigs[ref].Config.Content), "maxPods: 117") {
		t.Fatalf("desired kubeadm input reverted: %s", configuration.KubeadmConfigs[ref].Config.Content)
	}
	if _, err := configapply.ReadGenerationManifest(host, "bootstrap"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("live source was modified: %v", err)
	}

	// Complete snapshots do not depend on the live host or generation ancestry.
	if err := completeUpgradeSourceConfiguration(snapshot, t.TempDir(), "bootstrap"); err != nil {
		t.Fatalf("complete offline snapshot: %v", err)
	}
}

func TestUpgradeSourceConfigurationRejectsUnmatchedHost(t *testing.T) {
	for _, scenario := range []string{"machine", "selection", "spec", "missing-parent", "corrupt-local"} {
		t.Run(scenario, func(t *testing.T) {
			host, snapshot := frozenConfigurationSnapshot(t)
			switch scenario {
			case "machine":
				if err := os.WriteFile(filepath.Join(host, "etc/machine-id"), []byte("another-host\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "selection":
				writeBootSelection(t, host, "generation-0")
			case "spec":
				spec, status, err := generation.ReadGeneration(host, "bootstrap")
				if err != nil {
					t.Fatal(err)
				}
				spec.ConfiguredKernelCommandLine = []string{"quiet"}
				spec.KernelCommandLine = append(spec.KernelCommandLine, "quiet")
				dir, _ := generation.GenerationDir(host, "bootstrap")
				if err := os.RemoveAll(dir); err != nil {
					t.Fatal(err)
				}
				status, err = generation.NewGenerationStatus(spec, status.CommitState, status.BootState, status.HealthState, status.UpdatedAt)
				if err != nil {
					t.Fatal(err)
				}
				if err := generation.WriteGeneration(host, spec, status); err != nil {
					t.Fatal(err)
				}
			case "missing-parent":
				dir, _ := generation.GenerationDir(host, "generation-0")
				if err := os.RemoveAll(dir); err != nil {
					t.Fatal(err)
				}
			case "corrupt-local":
				path, _ := configapply.GenerationManifestPath(snapshot, "bootstrap")
				if err := os.WriteFile(path, []byte("corrupt"), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			if err := completeUpgradeSourceConfiguration(snapshot, host, "bootstrap"); err == nil {
				t.Fatal("accepted invalid upgrade source")
			}
		})
	}
}

func TestUpgradeSnapshotCarriesEffectiveConfiguration(t *testing.T) {
	host, _ := frozenConfigurationSnapshot(t)
	snapshot := filepath.Join(t.TempDir(), "snapshot")
	if err := snapshotUpgradeSource(host, snapshot, generation.UpgradeHandoff{SourceGenerationID: "bootstrap"}); err != nil {
		t.Fatal(err)
	}
	configuration, err := configapply.ReadLocalGenerationConfiguration(snapshot, "bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	if configuration.Manifest.Node.Identity.Hostname != "configured-node" {
		t.Fatalf("snapshot lost effective host configuration: %q", configuration.Manifest.Node.Identity.Hostname)
	}
	ref := configuration.Manifest.Node.Kubernetes.Kubeadm.ConfigRef
	if !strings.Contains(string(configuration.KubeadmConfigs[ref].Config.Content), "maxPods: 117") {
		t.Fatal("snapshot lost desired kubeadm input")
	}
	if _, err := os.Stat(filepath.Join(snapshot, "var/lib/katl/generations/generation-0/spec.json")); !os.IsNotExist(err) {
		t.Fatalf("snapshot copied configuration ancestry: %v", err)
	}
}

func TestUpgradeSourceConfigurationReservesStorage(t *testing.T) {
	for _, limit := range []string{"input", "free-space"} {
		t.Run(limit, func(t *testing.T) {
			host, snapshot := frozenConfigurationSnapshot(t)
			configuration, err := configapply.ReadGenerationConfiguration(host, "bootstrap")
			if err != nil {
				t.Fatal(err)
			}
			remaining, free := maxHostUpgradePreparationInput, hostUpgradeStorageReserve
			if limit == "input" {
				remaining, free = 1, maxHostUpgradePreparationInput+hostUpgradeStorageReserve
			}
			err = materializeUpgradeSourceConfiguration(snapshot, "bootstrap", configuration, remaining, func(string) (uint64, error) { return free, nil })
			if err == nil {
				t.Fatal("materialized configuration without available preparation storage")
			}
			if _, err := configapply.ReadGenerationManifest(snapshot, "bootstrap"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("storage rejection wrote private configuration: %v", err)
			}
		})
	}
}

func frozenConfigurationSnapshot(t *testing.T) (string, string) {
	t.Helper()
	host := t.TempDir()
	writeConfigApplyBaseState(t, host)
	writeConfigApplyManifestWithKubeadmRef(t, host, "control-plane")
	writeStoredKubeadmConfig(t, host, "control-plane")
	configuration, err := configapply.ReadGenerationConfiguration(host, "generation-0")
	if err != nil {
		t.Fatal(err)
	}
	configuration.Manifest.Node.Identity.Hostname = "configured-node"
	ref := configuration.Manifest.Node.Kubernetes.Kubeadm.ConfigRef
	plan, err := kubeadmconfig.PlanFromRenderedFiles(ref, []kubeadmconfig.File{{
		RenderPath: "/etc/katl/kubeadm/" + ref + "/config.yaml",
		Content:    []byte("apiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\nmaxPods: 117\n"), Mode: 0o644,
	}})
	if err != nil {
		t.Fatal(err)
	}
	configuration.KubeadmConfigs = map[string]kubeadmconfig.Plan{ref: plan}
	if err := configapply.WriteGenerationConfiguration(host, "generation-0", configuration); err != nil {
		t.Fatal(err)
	}
	spec, _, err := generation.ReadGeneration(host, "generation-0")
	if err != nil {
		t.Fatal(err)
	}
	spec.GenerationID, spec.PreviousGenerationID = "bootstrap", "generation-0"
	for i := range spec.Sysexts {
		spec.Sysexts[i].Path = strings.Replace(spec.Sysexts[i].Path, "/generation-0/", "/bootstrap/", 1)
	}
	for i := range spec.Confexts {
		spec.Confexts[i].Path = strings.Replace(spec.Confexts[i].Path, "/generation-0/", "/bootstrap/", 1)
	}
	status, err := generation.NewGenerationStatus(spec, generation.CommitStateCommitted, generation.BootStateGood, generation.HealthStateHealthy, spec.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := generation.WriteGeneration(host, spec, status); err != nil {
		t.Fatal(err)
	}
	if err := copyUpgradeTree(filepath.Join(host, "var/lib/katl/generations/generation-0/sysext"), filepath.Join(host, "var/lib/katl/generations/bootstrap/sysext")); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(host, "var/lib/katl/generations/bootstrap/confext/etc/hostname"), "configured-node\n")
	writeBootSelection(t, host, "bootstrap")
	if err := os.MkdirAll(filepath.Join(host, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(host, "etc/machine-id"), []byte("5d2a5f4fd2553ba8c8bee87d90245c79\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	snapshot := t.TempDir()
	// Reproduce the published source exporter: selected generation, identity,
	// install manifest and cluster input, without configuration-only ancestors.
	for _, relative := range []string{"var/lib/katl/generations/bootstrap", "var/lib/katl/boot", "var/lib/katl/install", "var/lib/katl/cluster", "etc"} {
		if err := copyUpgradeTree(filepath.Join(host, relative), filepath.Join(snapshot, relative)); err != nil {
			t.Fatal(err)
		}
	}
	return host, snapshot
}
