package bootstrapruntime

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zariel/katl/internal/installer"
	"github.com/zariel/katl/internal/installer/bootstrapplan"
	"github.com/zariel/katl/internal/installer/generation"
	"github.com/zariel/katl/internal/installer/operation"
)

func TestPrepareMaterializesCandidateRuntimeWithoutBootDefault(t *testing.T) {
	root := t.TempDir()
	previous, previousStatus := writeGenerationZero(t, root)
	if err := generation.WriteBootSelection(root, generation.BootSelectionRecord{
		APIVersion:          generation.APIVersion,
		Kind:                generation.BootSelectionKind,
		DefaultGenerationID: "0",
		BootedGenerationID:  "0",
		DefaultBootEntry:    "loader/entries/katl-0.conf",
		UpdatedAt:           time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	sysextPayload := []byte("kubernetes-sysext-payload")
	writeFile(t, filepath.Join(root, "var/lib/katl/artifacts/katlos-image/katl-kubernetes.raw"), string(sysextPayload))
	inputDir, err := installer.StoredKubeadmInputDir(root, "control-plane")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(inputDir, "config.yaml"), "apiVersion: kubeadm.k8s.io/v1beta4\nkind: InitConfiguration\n")
	kubeadmInput := bootstrapplan.KubeadmInput{
		ConfigRef: "control-plane",
		Intent:    "control-plane",
		Path:      "/etc/katl/kubeadm/control-plane/config.yaml",
	}
	kubeadmInput.Digest = storedInputDigest(t, root, kubeadmInput)

	result, err := Prepare(root, bootstrapplan.Plan{
		Operation: operation.OperationRecord{
			OperationID:           "bootstrap-init-01",
			OperationKind:         bootstrapplan.OperationKindInit,
			RequestDigest:         strings.Repeat("1", 64),
			CandidateGenerationID: "1",
			BootstrapRequest: &operation.BootstrapRequest{
				InventoryNodeName:        "cp-1",
				SystemRole:               "control-plane",
				KubernetesPayloadVersion: "v1.36.1",
				BootstrapProfileRef:      "control-plane",
				CandidateGenerationID:    "1",
			},
		},
		Previous:      previous,
		PreviousState: previousStatus,
		RuntimeInputs: bootstrapplan.RuntimeInputs{
			SelectedKubernetesSysext: bootstrapplan.SelectedKubernetesSysext{
				Path:             "/var/lib/katl/artifacts/katlos-image/katl-kubernetes.raw",
				SHA256:           digest(sysextPayload),
				SizeBytes:        uint64(len(sysextPayload)),
				PayloadVersion:   "v1.36.1",
				ActivationPath:   "/run/extensions/katl-kubernetes.raw",
				Architecture:     "x86_64",
				RuntimeInterface: "katl-runtime-1",
			},
			HostConfig: bootstrapplan.HostConfig{
				NodeName:             "cp-1",
				SystemRole:           "control-plane",
				ControlPlaneEndpoint: "api.katl.test:6443",
				NodeMetadataPath:     "/etc/katl/node.json",
			},
			KubeadmInput: kubeadmInput,
			KubernetesProjection: bootstrapplan.KubernetesProjection{
				What:  generation.KubernetesSource,
				Where: generation.KubernetesTarget,
			},
		},
	}, time.Date(2026, 6, 16, 12, 5, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if result.Record.GenerationID != "1" || len(result.Spec.Sysexts) != 1 || result.Status.CommitState != generation.CommitStateCandidate {
		t.Fatalf("result = %#v status %#v", result.Record, result.Status)
	}
	if result.Spec.Boot.LoaderEntryPath != "loader/entries/katl-1.conf" {
		t.Fatalf("candidate loader entry path = %q", result.Spec.Boot.LoaderEntryPath)
	}
	assertContains(t, filepath.Join(root, "var/lib/katl/generations/1/confext/etc/katl/kubeadm/control-plane/config.yaml"), "InitConfiguration")
	assertContains(t, filepath.Join(root, "var/lib/katl/generations/1/confext/etc/katl/bootstrap-runtime.json"), `"controlPlaneEndpoint": "api.katl.test:6443"`)
	assertContains(t, filepath.Join(root, "var/lib/katl/generations/1/confext/etc/extension-release.d/extension-release.katl-node"), "ID=fedora")
	assertContains(t, filepath.Join(root, "etc/systemd/system/etc-kubernetes.mount"), "What=/var/lib/katl/kubernetes/etc-kubernetes")
	assertContains(t, filepath.Join(root, "etc/systemd/system/katl-kubeadm-ready.target"), "Requires=systemd-sysext.service systemd-confext.service containerd.service etc-kubernetes.mount")
	assertContains(t, filepath.Join(root, "etc/systemd/system/containerd.service.d/10-katl-runtime.conf"), "RequiresMountsFor=/var/lib/containerd")
	assertContains(t, filepath.Join(root, "etc/systemd/system/kubelet.service.d/10-katl-runtime.conf"), "Requires=containerd.service etc-kubernetes.mount")
	assertContains(t, filepath.Join(root, "run/systemd/system/katl-generation-activate.service.d/10-katl-live-generation.conf"), "--generation 1")
	assertSymlink(t, filepath.Join(root, "run/extensions/katl-kubernetes.raw"), "/var/lib/katl/generations/1/sysext/katl-kubernetes.raw")
	selection, err := generation.ReadBootSelection(root)
	if err != nil {
		t.Fatal(err)
	}
	if selection.DefaultGenerationID != "0" || selection.TargetBootGenerationID != "" || selection.TrialGenerationID != "" {
		t.Fatalf("boot selection changed = %#v", selection)
	}
	if _, err := os.Lstat(filepath.Join(root, "etc/systemd/system/multi-user.target.wants/katl-kubeadm-ready.target")); !os.IsNotExist(err) {
		t.Fatalf("katl-kubeadm-ready.target was enabled: %v", err)
	}
}

func TestPrepareRejectsTamperedKubeadmInput(t *testing.T) {
	root := t.TempDir()
	plan := preparePlan(t, root)
	inputDir, err := installer.StoredKubeadmInputDir(root, "control-plane")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(inputDir, "config.yaml"), "apiVersion: kubeadm.k8s.io/v1beta4\nkind: InitConfiguration\n")
	plan.RuntimeInputs.KubeadmInput.Digest = storedInputDigest(t, root, plan.RuntimeInputs.KubeadmInput)
	writeFile(t, filepath.Join(inputDir, "config.yaml"), "apiVersion: kubeadm.k8s.io/v1beta4\nkind: JoinConfiguration\n")

	_, err = Prepare(root, plan, time.Date(2026, 6, 16, 12, 5, 0, 0, time.UTC))
	if err == nil || !strings.Contains(err.Error(), "stored kubeadm input digest") {
		t.Fatalf("Prepare() error = %v, want stored input digest refusal", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "var/lib/katl/generations/1/status.json")); !os.IsNotExist(statErr) {
		t.Fatalf("candidate status exists after refusal: %v", statErr)
	}
}

func TestPrepareRejectsStaleStoredKubeadmInputFile(t *testing.T) {
	root := t.TempDir()
	plan := preparePlan(t, root)
	inputDir, err := installer.StoredKubeadmInputDir(root, "control-plane")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(inputDir, "config.yaml"), "apiVersion: kubeadm.k8s.io/v1beta4\nkind: InitConfiguration\n")
	plan.RuntimeInputs.KubeadmInput.Digest = storedInputDigest(t, root, plan.RuntimeInputs.KubeadmInput)
	writeFile(t, filepath.Join(inputDir, "patches/stale.yaml"), "stale: true\n")

	_, err = Prepare(root, plan, time.Date(2026, 6, 16, 12, 5, 0, 0, time.UTC))
	if err == nil || !strings.Contains(err.Error(), "stored kubeadm input digest") {
		t.Fatalf("Prepare() error = %v, want stored input digest refusal", err)
	}
}

func preparePlan(t *testing.T, root string) bootstrapplan.Plan {
	t.Helper()
	previous, previousStatus := writeGenerationZero(t, root)
	if err := generation.WriteBootSelection(root, generation.BootSelectionRecord{
		APIVersion:          generation.APIVersion,
		Kind:                generation.BootSelectionKind,
		DefaultGenerationID: "0",
		BootedGenerationID:  "0",
		DefaultBootEntry:    "loader/entries/katl-0.conf",
		UpdatedAt:           time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	sysextPayload := []byte("kubernetes-sysext-payload")
	writeFile(t, filepath.Join(root, "var/lib/katl/artifacts/katlos-image/katl-kubernetes.raw"), string(sysextPayload))
	return bootstrapplan.Plan{
		Operation: operation.OperationRecord{
			OperationID:           "bootstrap-init-01",
			OperationKind:         bootstrapplan.OperationKindInit,
			RequestDigest:         strings.Repeat("1", 64),
			CandidateGenerationID: "1",
			BootstrapRequest: &operation.BootstrapRequest{
				InventoryNodeName:        "cp-1",
				SystemRole:               "control-plane",
				KubernetesPayloadVersion: "v1.36.1",
				BootstrapProfileRef:      "control-plane",
				CandidateGenerationID:    "1",
			},
		},
		Previous:      previous,
		PreviousState: previousStatus,
		RuntimeInputs: bootstrapplan.RuntimeInputs{
			SelectedKubernetesSysext: bootstrapplan.SelectedKubernetesSysext{
				Path:             "/var/lib/katl/artifacts/katlos-image/katl-kubernetes.raw",
				SHA256:           digest(sysextPayload),
				SizeBytes:        uint64(len(sysextPayload)),
				PayloadVersion:   "v1.36.1",
				ActivationPath:   "/run/extensions/katl-kubernetes.raw",
				Architecture:     "x86_64",
				RuntimeInterface: "katl-runtime-1",
			},
			HostConfig: bootstrapplan.HostConfig{
				NodeName:             "cp-1",
				SystemRole:           "control-plane",
				ControlPlaneEndpoint: "api.katl.test:6443",
				NodeMetadataPath:     "/etc/katl/node.json",
			},
			KubeadmInput: bootstrapplan.KubeadmInput{
				ConfigRef: "control-plane",
				Intent:    "control-plane",
				Path:      "/etc/katl/kubeadm/control-plane/config.yaml",
			},
			KubernetesProjection: bootstrapplan.KubernetesProjection{
				What:  generation.KubernetesSource,
				Where: generation.KubernetesTarget,
			},
		},
	}
}

func storedInputDigest(t *testing.T, root string, input bootstrapplan.KubeadmInput) string {
	t.Helper()
	_, digest, err := readStoredKubeadmInput(root, input)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func writeGenerationZero(t *testing.T, root string) (generation.GenerationSpec, generation.GenerationStatus) {
	t.Helper()
	record, err := generation.NewFirstInstallRecord(generation.FirstInstallRequest{
		GenerationID:          "0",
		RuntimeVersion:        "0.1.0",
		RuntimeInterface:      "katl-runtime-1",
		RuntimeArchitecture:   "x86_64",
		RootSlot:              "root-a",
		RootPartitionUUID:     "11111111-2222-3333-4444-555555555555",
		RuntimeArtifactSHA256: strings.Repeat("a", 64),
		UKIPath:               "/efi/EFI/Linux/katl-0.efi",
		GeneratedConfext: generation.GeneratedConfext{
			Name:           "katl-node",
			Path:           "/var/lib/katl/generations/0/confext",
			ActivationPath: "/run/confexts/katl-node",
			SHA256:         strings.Repeat("b", 64),
			Compatibility: generation.ConfextCompatibility{
				ID:           "fedora",
				VersionID:    "0.1.0",
				ConfextLevel: 1,
			},
		},
		CreatedAt: time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	spec := generation.SpecFromRecord(record)
	specDigest, err := generation.CanonicalSpecDigest(spec)
	if err != nil {
		t.Fatal(err)
	}
	status := generation.StatusFromRecord(record, specDigest)
	if err := generation.WriteGeneration(root, spec, status); err != nil {
		t.Fatal(err)
	}
	return spec, status
}

func writeFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertContains(t *testing.T, path string, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), want) {
		t.Fatalf("%s = %q, want %q", path, data, want)
	}
}

func assertSymlink(t *testing.T, path string, want string) {
	t.Helper()
	got, err := os.Readlink(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("symlink %s -> %s, want %s", path, got, want)
	}
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
