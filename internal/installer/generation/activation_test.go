package generation

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestApplyActivationExposesOnlySelectedGeneration(t *testing.T) {
	root := t.TempDir()
	record := activationRecord(t, root, "2026.06.05-001", "selected sysext\n")
	inactive := filepath.Join(root, "var/lib/katl/generations/2026.06.04-001/sysext/kubernetes.raw")
	mustWrite(t, inactive, "inactive sysext\n", 0o644)
	mustWrite(t, filepath.Join(root, "run/extensions/stale.raw"), "stale\n", 0o644)
	mustWrite(t, filepath.Join(root, "run/confexts/stale/etc/hostname"), "stale\n", 0o644)

	plan, err := ApplyActivation(root, record)
	if err != nil {
		t.Fatalf("ApplyActivation() error = %v", err)
	}

	if plan.GenerationID != record.GenerationID || len(plan.Sysexts) != 1 || len(plan.Confexts) != 1 {
		t.Fatalf("activation plan = %#v", plan)
	}
	assertSymlink(t, filepath.Join(root, "run/extensions/kubernetes.raw"), record.Sysexts[0].Path)
	assertSymlink(t, filepath.Join(root, "run/confexts/katl-node"), record.Confexts[0].Path)
	assertMissing(t, filepath.Join(root, "run/extensions/stale.raw"))
	assertMissing(t, filepath.Join(root, "run/confexts/stale"))
	if entries, err := os.ReadDir(filepath.Join(root, "run/extensions")); err != nil || len(entries) != 1 {
		t.Fatalf("active sysext entries = %v, %v; want exactly selected generation", entries, err)
	}
}

func TestApplyActivationRejectsDigestMismatch(t *testing.T) {
	root := t.TempDir()
	record := activationRecord(t, root, "2026.06.05-001", "selected sysext\n")
	mustWrite(t, filepath.Join(root, "run/extensions/stale.raw"), "stale\n", 0o644)
	record.Sysexts[0].SHA256 = strings.Repeat("0", 64)

	_, err := ApplyActivation(root, record)
	if err == nil || !strings.Contains(err.Error(), "SHA-256 mismatch") {
		t.Fatalf("ApplyActivation() error = %v, want digest mismatch", err)
	}
	assertMissing(t, filepath.Join(root, "run/extensions/kubernetes.raw"))
	assertExists(t, filepath.Join(root, "run/extensions/stale.raw"))
}

func TestApplyActivationRejectsConfigApplyKubernetesSysextChangeBeforeReset(t *testing.T) {
	root := t.TempDir()
	previous := activationRecord(t, root, "2026.06.05-001", "current sysext\n")
	writeActivationGeneration(t, root, previous)
	record := activationRecord(t, root, "2026.06.05-002", "target sysext\n")
	record.ConfigApply = &ConfigApplyRecord{
		SourceDigest:       strings.Repeat("d", 64),
		ChangedDomains:     []string{"selected-kubernetes-sysext"},
		RequestedApplyMode: "next-boot",
		AcceptedApplyMode:  "next-boot",
		PreviousGeneration: previous.GenerationID,
	}
	mustWrite(t, filepath.Join(root, "run/extensions/stale.raw"), "stale\n", 0o644)
	mustWrite(t, filepath.Join(root, "run/confexts/stale/etc/hostname"), "stale\n", 0o644)

	_, err := ApplyActivation(root, record)
	if err == nil || !strings.Contains(err.Error(), "target kubeadm access mode") || !strings.Contains(err.Error(), "kubelet activation gate") {
		t.Fatalf("ApplyActivation() error = %v, want Kubernetes sysext gate refusal", err)
	}
	assertExists(t, filepath.Join(root, "run/extensions/stale.raw"))
	assertExists(t, filepath.Join(root, "run/confexts/stale/etc/hostname"))
	assertMissing(t, filepath.Join(root, "run/extensions/kubernetes.raw"))
}

func TestApplyActivationRejectsRawKubernetesSysextChangeFromSplitLineage(t *testing.T) {
	root := t.TempDir()
	previous := activationRecord(t, root, "2026.06.05-001", "current sysext\n")
	writeActivationGeneration(t, root, previous)
	record := activationRecord(t, root, "2026.06.05-002", "target sysext\n")
	record.PreviousGenerationID = previous.GenerationID
	writeActivationGeneration(t, root, record)
	metadataPath, err := MetadataPath(root, record.GenerationID)
	if err != nil {
		t.Fatal(err)
	}
	splitRecord, err := ReadRecord(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	if splitRecord.ConfigApply != nil || splitRecord.PreviousGenerationID != previous.GenerationID {
		t.Fatalf("split record lineage = configApply:%#v previous:%q", splitRecord.ConfigApply, splitRecord.PreviousGenerationID)
	}
	mustWrite(t, filepath.Join(root, "run/extensions/stale.raw"), "stale\n", 0o644)

	_, err = ApplyActivation(root, splitRecord)
	if err == nil || !strings.Contains(err.Error(), "target kubeadm access mode") || !strings.Contains(err.Error(), "kubelet activation gate") {
		t.Fatalf("ApplyActivation() error = %v, want raw Kubernetes sysext gate refusal", err)
	}
	assertExists(t, filepath.Join(root, "run/extensions/stale.raw"))
	assertMissing(t, filepath.Join(root, "run/extensions/kubernetes.raw"))
}

func TestKubernetesUpgradeActivationRecognizesSelectedGate(t *testing.T) {
	record := Record{
		GenerationID: "upgrade-v1361-cp",
		KubernetesUpgrade: &KubernetesUpgrade{
			OperationID:             "kubeadm-upgrade-1",
			TargetKubeadmAccessMode: "operation-private-sysext",
			KubeletActivationGate:   "operation-released-target-kubelet",
		},
	}
	err := authorizeKubernetesUpgradeActivation(t.TempDir(), record, ExtensionRef{PayloadVersion: "v1.36.1", SHA256: strings.Repeat("a", 64)})
	if err == nil || !strings.Contains(err.Error(), "read Kubernetes upgrade operation kubeadm-upgrade-1") {
		t.Fatalf("authorization error = %v, want operation evidence lookup", err)
	}
}

func TestPlanActivationRejectsPathsOutsideSelectedGeneration(t *testing.T) {
	root := t.TempDir()
	record := activationRecord(t, root, "2026.06.05-001", "selected sysext\n")
	record.Sysexts[0].Path = "/var/lib/katl/generations/2026.06.04-001/sysext/kubernetes.raw"

	_, err := PlanActivation(record)
	if err == nil || !strings.Contains(err.Error(), "must be under /var/lib/katl/generations/2026.06.05-001/sysext") {
		t.Fatalf("PlanActivation() error = %v, want selected generation path failure", err)
	}
}

func TestPlanActivationRejectsActivationOutsideRunSearchPath(t *testing.T) {
	root := t.TempDir()
	record := activationRecord(t, root, "2026.06.05-001", "selected sysext\n")
	record.Confexts[0].ActivationPath = "/etc/katl-node"

	_, err := PlanActivation(record)
	if err == nil || !strings.Contains(err.Error(), "must be under /run/confexts") {
		t.Fatalf("PlanActivation() error = %v, want activation path failure", err)
	}
}

func TestSelectedGenerationFromCommandLine(t *testing.T) {
	got, err := SelectedGenerationFromCommandLine("root=PARTUUID=abc katl.generation=2026.06.05-001 quiet")
	if err != nil {
		t.Fatalf("SelectedGenerationFromCommandLine() error = %v", err)
	}
	if got != "2026.06.05-001" {
		t.Fatalf("generation = %q, want 2026.06.05-001", got)
	}

	_, err = SelectedGenerationFromCommandLine("root=PARTUUID=abc")
	if err == nil || !strings.Contains(err.Error(), "katl.generation") {
		t.Fatalf("SelectedGenerationFromCommandLine() error = %v, want missing generation", err)
	}
}

func activationRecord(t *testing.T, root string, id string, sysextContent string) Record {
	t.Helper()
	sysextPath := filepath.ToSlash(filepath.Join(GenerationRecordsDir, id, "sysext", "kubernetes.raw"))
	confextPath := filepath.ToSlash(filepath.Join(GenerationRecordsDir, id, "confext"))
	sysextHostPath := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(sysextPath, "/")))
	confextHostPath := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(confextPath, "/")))
	mustWrite(t, sysextHostPath, sysextContent, 0o644)
	mustWrite(t, filepath.Join(confextHostPath, "etc/hostname"), "katl-node\n", 0o644)

	confextDigest, err := DigestDirectory(confextHostPath)
	if err != nil {
		t.Fatalf("DigestDirectory() error = %v", err)
	}
	sysextSum := sha256.Sum256([]byte(sysextContent))
	return Record{
		APIVersion:     APIVersion,
		Kind:           Kind,
		GenerationID:   id,
		RuntimeVersion: "0.1.0",
		Root: RootSelection{
			Slot:                  "root-a",
			PartitionUUID:         "11111111-2222-3333-4444-555555555555",
			RuntimeVersion:        "0.1.0",
			RuntimeInterface:      "katl-runtime-1",
			Architecture:          "x86_64",
			RuntimeArtifactSHA256: strings.Repeat("a", 64),
		},
		Boot: BootSelection{UKIPath: "/efi/EFI/Linux/katl-" + id + ".efi"},
		Sysexts: []ExtensionRef{
			{
				Name:            "kubernetes",
				Path:            sysextPath,
				ActivationPath:  "/run/extensions/kubernetes.raw",
				SHA256:          hex.EncodeToString(sysextSum[:]),
				ArtifactVersion: "k8s-v1.34.8",
				PayloadVersion:  "v1.34.8",
				Architecture:    "x86_64",
				Compatibility: ExtensionCompatibility{
					RuntimeInterfaces: []string{"katl-runtime-1"},
				},
			},
		},
		Confexts: []GeneratedConfext{
			{
				Name:           "katl-node",
				Path:           confextPath,
				ActivationPath: "/run/confexts/katl-node",
				SHA256:         confextDigest,
				Compatibility: ConfextCompatibility{
					ID:           "katl",
					VersionID:    "0.1.0",
					ConfextLevel: 1,
				},
			},
		},
		CreatedAt: time.Date(2026, 6, 5, 10, 0, 0, 0, time.UTC),
	}
}

func writeActivationGeneration(t *testing.T, root string, record Record) {
	t.Helper()
	spec := SpecFromRecord(record)
	digest, err := CanonicalSpecDigest(spec)
	if err != nil {
		t.Fatalf("CanonicalSpecDigest() error = %v", err)
	}
	if err := WriteGeneration(root, spec, StatusFromRecord(record, digest)); err != nil {
		t.Fatalf("WriteGeneration() error = %v", err)
	}
}

func assertExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("Lstat(%s) error = %v, want existing", path, err)
	}
}

func assertMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("Lstat(%s) error = %v, want missing", path, err)
	}
}
