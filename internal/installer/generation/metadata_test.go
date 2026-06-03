package generation

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNewFirstInstallRecordSerializesConfextSelection(t *testing.T) {
	record, err := NewFirstInstallRecord(FirstInstallRequest{
		GenerationID:          "2026.05.31-001",
		RuntimeVersion:        "0.1.0",
		RuntimeInterface:      "katl-runtime-1",
		RuntimeArchitecture:   "x86_64",
		RootSlot:              "root-a",
		RootPartitionUUID:     "11111111-2222-3333-4444-555555555555",
		RuntimeArtifactSHA256: strings.Repeat("a", 64),
		UKIPath:               "/efi/EFI/Linux/katl-2026.05.31-001.efi",
		Sysexts: []ExtensionRef{
			{
				Name:            "kubernetes",
				Path:            "/var/lib/katl/generations/2026.05.31-001/sysext/kubernetes.raw",
				ActivationPath:  "/run/extensions/kubernetes.raw",
				SHA256:          strings.Repeat("b", 64),
				ArtifactVersion: "k8s-sysext-001",
				PayloadVersion:  "v1.34.8",
				Architecture:    "x86_64",
				Compatibility: ExtensionCompatibility{
					RuntimeInterfaces: []string{"katl-runtime-1"},
				},
			},
		},
		GeneratedConfext: GeneratedConfext{
			Name:           "katl-node",
			Path:           "/var/lib/katl/generations/2026.05.31-001/confext",
			ActivationPath: "/run/confexts/katl-node",
			SHA256:         strings.Repeat("c", 64),
			Compatibility: ConfextCompatibility{
				ID:           "katl",
				VersionID:    "0.1.0",
				ConfextLevel: 1,
			},
		},
		KernelCommandLine: []string{"console=ttyS0", "root=PARTUUID=${KATL_ROOT_A_PARTUUID}"},
		CreatedAt:         time.Date(2026, 5, 31, 22, 30, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("NewFirstInstallRecord() error = %v", err)
	}

	data, err := MarshalRecord(record)
	if err != nil {
		t.Fatalf("MarshalRecord() error = %v", err)
	}
	want := `{
  "apiVersion": "katl.dev/v1alpha1",
  "kind": "GenerationRecord",
  "generationID": "2026.05.31-001",
  "runtimeVersion": "0.1.0",
  "root": {
    "slot": "root-a",
    "partitionUUID": "11111111-2222-3333-4444-555555555555",
    "runtimeVersion": "0.1.0",
    "runtimeInterface": "katl-runtime-1",
    "architecture": "x86_64",
    "runtimeArtifactSHA256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  },
  "boot": {
    "ukiPath": "/efi/EFI/Linux/katl-2026.05.31-001.efi"
  },
  "sysexts": [
    {
      "name": "kubernetes",
      "path": "/var/lib/katl/generations/2026.05.31-001/sysext/kubernetes.raw",
      "activationPath": "/run/extensions/kubernetes.raw",
      "sha256": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
      "artifactVersion": "k8s-sysext-001",
      "payloadVersion": "v1.34.8",
      "architecture": "x86_64",
      "compatibility": {
        "runtimeInterfaces": [
          "katl-runtime-1"
        ]
      }
    }
  ],
  "confexts": [
    {
      "name": "katl-node",
      "path": "/var/lib/katl/generations/2026.05.31-001/confext",
      "activationPath": "/run/confexts/katl-node",
      "sha256": "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
      "compatibility": {
        "id": "katl",
        "versionID": "0.1.0",
        "confextLevel": 1
      }
    }
  ],
  "kernelCommandLine": [
    "console=ttyS0",
    "root=PARTUUID=${KATL_ROOT_A_PARTUUID}"
  ],
  "createdAt": "2026-05-31T22:30:00Z",
  "bootState": "pending",
  "healthState": "unknown"
}
`
	if string(data) != want {
		t.Fatalf("record json:\n%s\nwant:\n%s", data, want)
	}

	if len(record.Confexts) != 1 || record.Confexts[0].ActivationPath != "/run/confexts/katl-node" {
		t.Fatalf("confext selection = %#v", record.Confexts)
	}
	if record.Root.Slot != "root-a" || len(record.Sysexts) != 1 {
		t.Fatalf("rollback selection is not recorded as one generation: %#v", record)
	}
}

func TestWriteRecordPersistsMetadataJSON(t *testing.T) {
	record, err := NewFirstInstallRecord(validFirstInstallRequest(t.TempDir()))
	if err != nil {
		t.Fatalf("NewFirstInstallRecord() error = %v", err)
	}

	path := filepath.Join(t.TempDir(), "metadata.json")
	if err := WriteRecord(path, record); err != nil {
		t.Fatalf("WriteRecord() error = %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	var decoded Record
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("decode metadata: %v", err)
	}
	if decoded.GenerationID != record.GenerationID || decoded.Confexts[0].SHA256 != record.Confexts[0].SHA256 {
		t.Fatalf("decoded record = %#v, want %#v", decoded, record)
	}
}

func TestDigestDirectoryIsDeterministic(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "etc", "b.conf"), "b\n", 0o600)
	mustWrite(t, filepath.Join(root, "etc", "a.conf"), "a\n", 0o644)

	digestA, err := DigestDirectory(root)
	if err != nil {
		t.Fatalf("DigestDirectory() error = %v", err)
	}
	digestB, err := DigestDirectory(root)
	if err != nil {
		t.Fatalf("DigestDirectory() second error = %v", err)
	}
	if digestA != digestB {
		t.Fatalf("digest changed between runs: %s != %s", digestA, digestB)
	}

	mustWrite(t, filepath.Join(root, "etc", "a.conf"), "changed\n", 0o644)
	digestC, err := DigestDirectory(root)
	if err != nil {
		t.Fatalf("DigestDirectory() changed error = %v", err)
	}
	if digestC == digestA {
		t.Fatalf("digest did not change after content update: %s", digestC)
	}
}

func TestFirstInstallRecordRequiresConfextMetadata(t *testing.T) {
	request := validFirstInstallRequest(t.TempDir())
	request.GeneratedConfext.Compatibility = ConfextCompatibility{}
	_, err := NewFirstInstallRecord(request)
	if err == nil {
		t.Fatal("NewFirstInstallRecord() error = nil, want compatibility failure")
	}
	if !strings.Contains(err.Error(), "compatibility metadata is required") {
		t.Fatalf("error = %q, want compatibility failure", err)
	}
}

func TestCompatOK(t *testing.T) {
	current := validRecord(t, "0.1.0", "katl-runtime-1", "v1.34.8")

	tests := []struct {
		name string
		next Record
	}{
		{name: "katlos only", next: validRecord(t, "0.2.0", "katl-runtime-1", "v1.34.8")},
		{name: "kubernetes only", next: validRecord(t, "0.1.0", "katl-runtime-1", "v1.35.1")},
		{name: "combined", next: validRecord(t, "0.2.0", "katl-runtime-1", "v1.35.1")},
	}
	if err := ValidateRecord(current); err != nil {
		t.Fatalf("ValidateRecord(current) error = %v", err)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateRecord(tt.next); err != nil {
				t.Fatalf("ValidateRecord() error = %v", err)
			}
		})
	}
}

func TestCompatReject(t *testing.T) {
	request := validFirstInstallRequest(t.TempDir())
	request.RuntimeVersion = "0.2.0"
	request.RuntimeInterface = "katl-runtime-2"
	request.Sysexts = []ExtensionRef{{
		Name:            "kubernetes",
		Path:            filepath.Join("/var/lib/katl/generations", request.GenerationID, "sysext", "kubernetes.raw"),
		ActivationPath:  "/run/extensions/kubernetes.raw",
		SHA256:          strings.Repeat("b", 64),
		ArtifactVersion: "k8s-v1.34.8",
		PayloadVersion:  "v1.34.8",
		Architecture:    "x86_64",
		Compatibility: ExtensionCompatibility{
			RuntimeInterfaces: []string{"katl-runtime-1"},
		},
	}}

	_, err := NewFirstInstallRecord(request)
	if err == nil {
		t.Fatal("NewFirstInstallRecord() error = nil, want incompatible pair")
	}
	if !strings.Contains(err.Error(), "does not support runtime interface") {
		t.Fatalf("error = %q, want runtime interface failure", err)
	}
}

func TestABFixtures(t *testing.T) {
	first := abRecord(t, "2026.06.01-001", "root-a", "11111111-2222-3333-4444-555555555555", "0.1.0", "v1.34.8", time.Date(2026, 6, 1, 8, 0, 0, 0, time.UTC))
	if first.BootState != "pending" || first.HealthState != "unknown" {
		t.Fatalf("first state = %s/%s, want pending/unknown", first.BootState, first.HealthState)
	}
	if first.Root.Slot != "root-a" || !strings.Contains(strings.Join(first.KernelCommandLine, " "), first.Root.PartitionUUID) {
		t.Fatalf("first boot selection = %#v", first)
	}

	active := markGood(first)
	candidate := abRecord(t, "2026.06.01-002", "root-b", "66666666-7777-8888-9999-000000000000", "0.2.0", "v1.35.1", time.Date(2026, 6, 2, 8, 0, 0, 0, time.UTC))
	candidate.BootState = "trying"
	if candidate.Root.Slot != "root-b" || !strings.Contains(candidate.Boot.UKIPath, candidate.GenerationID) {
		t.Fatalf("candidate boot selection = %#v", candidate)
	}

	booted := markGood(candidate)
	if booted.BootState != "good" || booted.HealthState != "healthy" {
		t.Fatalf("booted state = %s/%s, want good/healthy", booted.BootState, booted.HealthState)
	}

	failed := candidate
	failed.BootState = "failed"
	failed.HealthState = "unhealthy"
	selected, ok := selectRollback([]Record{active, failed}, failed.GenerationID)
	if !ok {
		t.Fatal("selectRollback() ok = false, want previous known-good generation")
	}
	if selected.GenerationID != active.GenerationID || selected.Root.Slot != "root-a" || selected.Confexts[0].Path == failed.Confexts[0].Path {
		t.Fatalf("rollback selection = %#v, want active root-a generation", selected)
	}
}

func abRecord(t *testing.T, id string, slot string, uuid string, version string, kube string, created time.Time) Record {
	t.Helper()
	request := validFirstInstallRequest(t.TempDir())
	request.GenerationID = id
	request.RuntimeVersion = version
	request.RootSlot = slot
	request.RootPartitionUUID = uuid
	request.UKIPath = "/efi/EFI/Linux/katl-" + id + ".efi"
	request.KernelCommandLine = []string{"root=PARTUUID=" + uuid, "rootfstype=squashfs", "ro"}
	request.GeneratedConfext.Path = filepath.Join("/var/lib/katl/generations", id, "confext")
	request.Sysexts = []ExtensionRef{{
		Name:            "kubernetes",
		Path:            filepath.Join("/var/lib/katl/generations", id, "sysext", "kubernetes.raw"),
		ActivationPath:  "/run/extensions/kubernetes.raw",
		SHA256:          strings.Repeat("b", 64),
		ArtifactVersion: "k8s-" + kube,
		PayloadVersion:  kube,
		Architecture:    "x86_64",
		Compatibility: ExtensionCompatibility{
			RuntimeInterfaces: []string{"katl-runtime-1"},
		},
	}}
	request.CreatedAt = created
	record, err := NewFirstInstallRecord(request)
	if err != nil {
		t.Fatalf("NewFirstInstallRecord() error = %v", err)
	}
	return record
}

func markGood(record Record) Record {
	record.BootState = "good"
	record.HealthState = "healthy"
	return record
}

func selectRollback(records []Record, failedID string) (Record, bool) {
	var selected Record
	ok := false
	for _, record := range records {
		if record.GenerationID == failedID || record.HealthState != "healthy" {
			continue
		}
		if !ok || record.CreatedAt.After(selected.CreatedAt) {
			selected = record
			ok = true
		}
	}
	return selected, ok
}

func validFirstInstallRequest(root string) FirstInstallRequest {
	return FirstInstallRequest{
		GenerationID:          "2026.05.31-001",
		RuntimeVersion:        "0.1.0",
		RuntimeInterface:      "katl-runtime-1",
		RuntimeArchitecture:   "x86_64",
		RootSlot:              "root-a",
		RootPartitionUUID:     "11111111-2222-3333-4444-555555555555",
		RuntimeArtifactSHA256: strings.Repeat("a", 64),
		UKIPath:               "/efi/EFI/Linux/katl-2026.05.31-001.efi",
		GeneratedConfext: GeneratedConfext{
			Name:           "katl-node",
			Path:           filepath.Join(root, "generations", "2026.05.31-001", "confext"),
			ActivationPath: "/run/confexts/katl-node",
			SHA256:         strings.Repeat("c", 64),
			Compatibility: ConfextCompatibility{
				ID:           "katl",
				VersionID:    "0.1.0",
				ConfextLevel: 1,
			},
		},
		CreatedAt: time.Date(2026, 5, 31, 22, 30, 0, 0, time.UTC),
	}
}

func validRecord(t *testing.T, runtimeVersion string, runtimeInterface string, kubeVersion string) Record {
	t.Helper()
	request := validFirstInstallRequest(t.TempDir())
	request.RuntimeVersion = runtimeVersion
	request.RuntimeInterface = runtimeInterface
	request.Sysexts = []ExtensionRef{{
		Name:            "kubernetes",
		Path:            filepath.Join("/var/lib/katl/generations", request.GenerationID, "sysext", "kubernetes.raw"),
		ActivationPath:  "/run/extensions/kubernetes.raw",
		SHA256:          strings.Repeat("b", 64),
		ArtifactVersion: "k8s-" + kubeVersion,
		PayloadVersion:  kubeVersion,
		Architecture:    "x86_64",
		Compatibility: ExtensionCompatibility{
			RuntimeInterfaces: []string{"katl-runtime-1"},
		},
	}}
	record, err := NewFirstInstallRecord(request)
	if err != nil {
		t.Fatalf("NewFirstInstallRecord() error = %v", err)
	}
	return record
}

func mustWrite(t *testing.T, path string, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
}
