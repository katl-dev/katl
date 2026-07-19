package status

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/installer/generation"
	"github.com/katl-dev/katl/internal/installer/operation"
	"github.com/katl-dev/katl/internal/installer/persistedrecord"
)

func TestRedactSourceRemovesCredentialsAndQuery(t *testing.T) {
	got := RedactSource("https://user:secret@example.invalid/path/katlos.squashfs?token=secret#frag")
	want := "https://example.invalid/path/katlos.squashfs"
	if got != want {
		t.Fatalf("RedactSource() = %q, want %q", got, want)
	}
}

func TestRedactErrorRemovesEmbeddedURLSecrets(t *testing.T) {
	got := RedactError(errors.New("download failed: https://user:secret@example.invalid/path?token=secret"))
	want := "download failed: https://example.invalid/path"
	if got != want {
		t.Fatalf("RedactError() = %q, want %q", got, want)
	}
}

func TestWriteRuntimeHandoff(t *testing.T) {
	root := t.TempDir()
	writeCleanGenerationZero(t, root)
	record := New(StateKubeadmReady, time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC))
	record.InputMode = InputModePXEPreseed
	record.InputSource = "https://example.invalid/install.json"
	record.RequestDigest = strings.Repeat("a", 64)
	record.KatlosImage = Image{
		URL:              "https://example.invalid/katlos.squashfs",
		SHA256:           strings.Repeat("b", 64),
		Version:          "2026.06.04",
		Architecture:     "x86_64",
		RuntimeInterface: "katl-runtime-1",
		Role:             "install",
	}
	record.TargetDiskStableID = "/dev/disk/by-id/ata-root"
	record.SelectedRootSlot = "root-a"
	record.InstalledGeneration = "0"

	if err := WriteRuntimeHandoff(root, record); err != nil {
		t.Fatalf("WriteRuntimeHandoff() error = %v", err)
	}

	data, err := os.ReadFile(filepath.Join(root, "var/lib/katl/install/status.json"))
	if err != nil {
		t.Fatalf("read runtime status: %v", err)
	}
	if !strings.Contains(string(data), `"recordType": "katl.install.status"`) || !strings.Contains(string(data), `"payload": {`) {
		t.Fatalf("status is not enveloped:\n%s", data)
	}
	decoded, err := ReadFile(filepath.Join(root, "var/lib/katl/install/status.json"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if decoded.State != StateWaitingForClusterBootstrap || decoded.FinalHandoff != StateWaitingForClusterBootstrap {
		t.Fatalf("handoff state = %#v", decoded)
	}
	if decoded.RequestDigest != strings.Repeat("a", 64) || decoded.InstalledGeneration != "0" {
		t.Fatalf("status did not preserve identity fields: %#v", decoded)
	}
}

func TestReadFileRejectsUnsupportedEnvelopeVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	data, err := persistedrecord.MarshalEnvelope(persistedrecord.Envelope{
		RecordType:    RecordType,
		RecordVersion: 2,
		Payload:       []byte("{}\n"),
	})
	if err != nil {
		t.Fatalf("MarshalEnvelope() error = %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write status: %v", err)
	}
	_, err = ReadFile(path)
	if err == nil || !strings.Contains(err.Error(), "unsupported persisted record") {
		t.Fatalf("ReadFile() error = %v, want unsupported persisted record", err)
	}
}

func TestValidateCleanGenerationZeroRejectsOperationMutationEvidence(t *testing.T) {
	root := t.TempDir()
	writeCleanGenerationZero(t, root)
	store, err := operation.NewStore(filepath.Join(root, "var/lib/katl/operations"))
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	_, err = store.Create(operation.OperationRecord{
		OperationID:             "bootstrap-001",
		OperationKind:           "bootstrap-init",
		Scope:                   "kubeadm-state",
		RequestDigest:           "sha256:" + strings.Repeat("1", 64),
		PreviousGenerationID:    "0",
		CandidateGenerationID:   "1",
		MutatingToolRan:         true,
		MutatingToolInvocations: []string{"kubeadm init"},
	}, "accepted", time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	err = ValidateCleanGenerationZero(root, "0")
	if err == nil || !strings.Contains(err.Error(), "operation bootstrap-001 has mutation evidence") {
		t.Fatalf("ValidateCleanGenerationZero() error = %v, want operation mutation refusal", err)
	}
}

func TestValidateCleanGenerationZeroAllowsCurrentAcceptedOperation(t *testing.T) {
	root := t.TempDir()
	writeCleanGenerationZero(t, root)
	store, err := operation.NewStore(filepath.Join(root, "var/lib/katl/operations"))
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	_, err = store.Create(operation.OperationRecord{
		OperationID:           "bootstrap-accepted",
		OperationKind:         "bootstrap-init",
		Scope:                 "kubeadm-state",
		RequestDigest:         strings.Repeat("1", 64),
		PreviousGenerationID:  "0",
		CandidateGenerationID: "1",
		Phase:                 "accepted",
	}, "accepted", time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := ValidateCleanGenerationZero(root, "0"); err == nil || !strings.Contains(err.Error(), "stale mutation evidence") {
		t.Fatalf("ValidateCleanGenerationZero() error = %v, want generic refusal", err)
	}
	if err := ValidateCleanGenerationZeroForOperation(root, "0", "bootstrap-accepted"); err != nil {
		t.Fatalf("ValidateCleanGenerationZeroForOperation() error = %v", err)
	}
	if _, err := store.Update("bootstrap-accepted", "marker-start", "pre-exec-mutation", func(record operation.OperationRecord) (operation.OperationRecord, error) {
		record.ExternalMutationStarted = true
		return record, nil
	}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if err := ValidateCleanGenerationZeroForOperation(root, "0", "bootstrap-accepted"); err == nil || !strings.Contains(err.Error(), "mutation evidence") {
		t.Fatalf("ValidateCleanGenerationZeroForOperation() error = %v, want mutation refusal", err)
	}
}

func TestValidateCleanGenerationZeroAllowsRenderedKubeadmInput(t *testing.T) {
	root := t.TempDir()
	writeCleanGenerationZero(t, root)
	writeFile(t, root, "etc/katl/kubeadm/control-plane/config.yaml", "apiVersion: kubeadm.k8s.io/v1beta4\n")
	writeFile(t, root, "var/lib/katl/generations/0/confext/etc/katl/kubeadm/control-plane/config.yaml", "apiVersion: kubeadm.k8s.io/v1beta4\n")

	if err := ValidateCleanGenerationZero(root, "0"); err != nil {
		t.Fatalf("ValidateCleanGenerationZero() error = %v, want rendered kubeadm input allowed", err)
	}
}

func TestValidateCleanGenerationZeroAllowsNonKubernetesSysext(t *testing.T) {
	root := t.TempDir()
	writeCleanGenerationZeroWithSysexts(t, root, []generation.ExtensionRef{{
		Name:           "endpoint-advertiser",
		ActivationPath: "/run/extensions/katl-endpoint-advertiser.raw",
	}})

	if err := ValidateCleanGenerationZero(root, "0"); err != nil {
		t.Fatalf("ValidateCleanGenerationZero() error = %v, want endpoint advertiser allowed", err)
	}
}

func TestValidateCleanGenerationZeroRejectsKubernetesSysext(t *testing.T) {
	root := t.TempDir()
	writeCleanGenerationZeroWithSysexts(t, root, []generation.ExtensionRef{{
		Name:           "kubernetes",
		ActivationPath: "/run/extensions/katl-kubernetes.raw",
	}})

	err := ValidateCleanGenerationZero(root, "0")
	if err == nil || !strings.Contains(err.Error(), "selected Kubernetes sysexts are forbidden") {
		t.Fatalf("ValidateCleanGenerationZero() error = %v, want Kubernetes sysext refusal", err)
	}
}

func writeCleanGenerationZero(t *testing.T, root string) {
	t.Helper()
	writeCleanGenerationZeroWithSysexts(t, root, nil)
}

func writeCleanGenerationZeroWithSysexts(t *testing.T, root string, sysexts []generation.ExtensionRef) {
	t.Helper()
	for i := range sysexts {
		sysexts[i].Path = "/var/lib/katl/generations/0/sysext/" + sysexts[i].Name + ".raw"
		sysexts[i].SHA256 = strings.Repeat("c", 64)
		sysexts[i].ArtifactVersion = "0.1.0"
		sysexts[i].PayloadVersion = "0.1.0"
		sysexts[i].Architecture = "x86_64"
		sysexts[i].Compatibility.RuntimeInterfaces = []string{"katl-runtime-1"}
	}
	record := generation.Record{
		APIVersion:     generation.APIVersion,
		Kind:           generation.Kind,
		GenerationID:   "0",
		RuntimeVersion: "0.1.0",
		Root: generation.RootSelection{
			Slot:                  "root-a",
			PartitionUUID:         "11111111-2222-3333-4444-555555555555",
			RuntimeVersion:        "0.1.0",
			RuntimeInterface:      "katl-runtime-1",
			Architecture:          "x86_64",
			RuntimeArtifactSHA256: strings.Repeat("b", 64),
		},
		Boot: generation.BootSelection{
			UKIPath: "/efi/EFI/Linux/katl-0.efi",
		},
		Sysexts:   sysexts,
		CreatedAt: time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC),
	}
	spec := generation.SpecFromRecord(record)
	status, err := generation.NewGenerationStatus(spec, generation.CommitStateCommitted, generation.BootStatePending, generation.HealthStateUnknown, record.CreatedAt)
	if err != nil {
		t.Fatalf("NewGenerationStatus() error = %v", err)
	}
	if err := generation.WriteGeneration(root, spec, status); err != nil {
		t.Fatalf("WriteGeneration() error = %v", err)
	}
}

func writeFile(t *testing.T, root string, path string, data string) {
	t.Helper()
	full := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}
