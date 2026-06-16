package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zariel/katl/internal/installer/generation"
	"github.com/zariel/katl/internal/installer/operation"
	installstatus "github.com/zariel/katl/internal/installer/status"
)

func TestRuntimeStatusUpdatesExistingInstallStatus(t *testing.T) {
	root := t.TempDir()
	writeCleanGenerationZero(t, root)
	record := installStatusRecord("0")
	if err := installstatus.WriteFile(filepath.Join(root, "var/lib/katl/install/status.json"), record); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	var stdout bytes.Buffer
	if err := run(t.Context(), []string{"--root", root}, &stdout); err != nil {
		t.Fatalf("run() error = %v", err)
	}

	data, err := os.ReadFile(filepath.Join(root, "var/lib/katl/install/status.json"))
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	var decoded installstatus.Record
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if decoded.State != installstatus.StateWaitingForClusterBootstrap || decoded.FinalHandoff != installstatus.StateWaitingForClusterBootstrap {
		t.Fatalf("runtime state = %#v", decoded)
	}
	if decoded.RequestDigest != strings.Repeat("a", 64) || decoded.InstalledGeneration != "0" {
		t.Fatalf("runtime status did not preserve install identity: %#v", decoded)
	}
	if !strings.Contains(stdout.String(), installstatus.StateWaitingForClusterBootstrap) {
		t.Fatalf("stdout = %q, want handoff state", stdout.String())
	}
}

func TestRuntimeStatusRefusesDirtyGenerationZero(t *testing.T) {
	root := t.TempDir()
	record := installStatusRecord("0")
	if err := installstatus.WriteFile(filepath.Join(root, "var/lib/katl/install/status.json"), record); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	writeCleanGenerationZero(t, root)
	dirty := filepath.Join(root, "var/lib/katl/kubernetes/etc-kubernetes/admin.conf")
	if err := os.MkdirAll(filepath.Dir(dirty), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dirty, []byte("kubeconfig"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := run(t.Context(), []string{"--root", root}, nil)
	if err == nil || !strings.Contains(err.Error(), "generation 0 is not clean") {
		t.Fatalf("run() error = %v, want dirty generation 0 refusal", err)
	}
	decoded, readErr := installstatus.ReadFile(filepath.Join(root, "var/lib/katl/install/status.json"))
	if readErr != nil {
		t.Fatalf("read status: %v", readErr)
	}
	if decoded.State != installstatus.StateRuntimeFailedNeedsRepair || !strings.Contains(decoded.RefusalReason, "kubeadm kubeconfigs") {
		t.Fatalf("repair status = %#v", decoded)
	}
}

func TestRuntimeStatusRefusesDirtyGenerationZeroCases(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, root string)
		want   string
	}{
		{
			name: "selected Kubernetes sysext",
			mutate: func(t *testing.T, root string) {
				t.Helper()
				if err := os.RemoveAll(filepath.Join(root, "var/lib/katl/generations/0")); err != nil {
					t.Fatal(err)
				}
				writeGenerationZero(t, root, []generation.ExtensionRef{{
					Name:            "katl-kubernetes",
					ArtifactVersion: "2026.06.04",
					PayloadVersion:  "v1.36.2",
					Architecture:    "x86_64",
					SHA256:          strings.Repeat("d", 64),
					ActivationPath:  "/run/extensions/katl-kubernetes.raw",
					Compatibility: generation.ExtensionCompatibility{
						RuntimeInterfaces: []string{"katl-runtime-1"},
					},
				}})
			},
			want: "selected Kubernetes sysexts are forbidden",
		},
		{
			name: "kubeadm PKI",
			mutate: func(t *testing.T, root string) {
				t.Helper()
				writeFile(t, root, "var/lib/katl/kubernetes/etc-kubernetes/pki/ca.crt", "certificate")
			},
			want: "kubeadm PKI exists",
		},
		{
			name: "static pod manifest",
			mutate: func(t *testing.T, root string) {
				t.Helper()
				writeFile(t, root, "var/lib/katl/kubernetes/etc-kubernetes/manifests/kube-apiserver.yaml", "pod")
			},
			want: "static pod manifests exist",
		},
		{
			name: "kubelet bootstrap state",
			mutate: func(t *testing.T, root string) {
				t.Helper()
				writeFile(t, root, "var/lib/kubelet/bootstrap-kubeconfig", "kubeconfig")
			},
			want: "kubelet bootstrap kubeconfig exists",
		},
		{
			name: "kubelet join state",
			mutate: func(t *testing.T, root string) {
				t.Helper()
				writeFile(t, root, "var/lib/kubelet/config.yaml", "kind: KubeletConfiguration\n")
			},
			want: "kubelet config exists",
		},
		{
			name: "stacked etcd data",
			mutate: func(t *testing.T, root string) {
				t.Helper()
				writeFile(t, root, "var/lib/etcd/member/snap/db", "etcd")
			},
			want: "stacked etcd data exists",
		},
		{
			name: "kubelet enabled",
			mutate: func(t *testing.T, root string) {
				t.Helper()
				writeFile(t, root, "etc/systemd/system/multi-user.target.wants/kubelet.service", "[Service]\n")
			},
			want: "kubelet is enabled",
		},
		{
			name: "active Kubernetes sysext",
			mutate: func(t *testing.T, root string) {
				t.Helper()
				writeFile(t, root, "var/lib/katl/artifacts/katl-kubernetes.raw", "sysext")
				writeSymlink(t, root, "run/extensions/katl-kubernetes.raw", "/var/lib/katl/artifacts/katl-kubernetes.raw")
			},
			want: "active Kubernetes sysext links exist",
		},
		{
			name: "wrong kubernetes projection",
			mutate: func(t *testing.T, root string) {
				t.Helper()
				writeFile(t, root, "etc/systemd/system/etc-kubernetes.mount", "[Mount]\nWhat=/var/lib/katl/wrong/etc-kubernetes\nWhere=/etc/kubernetes\n")
			},
			want: "/etc/kubernetes projection points",
		},
		{
			name: "operation mutation boundary",
			mutate: func(t *testing.T, root string) {
				t.Helper()
				store, err := operation.NewStore(filepath.Join(root, "var/lib/katl/operations"))
				if err != nil {
					t.Fatalf("NewStore() error = %v", err)
				}
				_, err = store.Create(operation.OperationRecord{
					OperationID:           "bootstrap-001",
					OperationKind:         "bootstrap-init",
					Scope:                 "kubeadm-state",
					RequestDigest:         "sha256:" + strings.Repeat("1", 64),
					PreviousGenerationID:  "0",
					CandidateGenerationID: "1",
					PreExecMutationMarkers: []operation.PreExecMutationMarker{{
						MarkerID:               "marker-001",
						Phase:                  "kubeadm-init",
						Tool:                   "kubeadm",
						ArgvDigest:             "sha256:" + strings.Repeat("2", 64),
						ExpectedMutationScopes: []string{"kubeadm-state"},
						MarkedAt:               time.Date(2026, 6, 15, 12, 1, 0, 0, time.UTC),
					}},
				}, "accepted", time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC))
				if err != nil {
					t.Fatalf("Create() error = %v", err)
				}
			},
			want: "operation bootstrap-001 has mutation evidence",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			record := installStatusRecord("0")
			if err := installstatus.WriteFile(filepath.Join(root, "var/lib/katl/install/status.json"), record); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
			writeCleanGenerationZero(t, root)
			tt.mutate(t, root)

			err := run(t.Context(), []string{"--root", root}, nil)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("run() error = %v, want %q", err, tt.want)
			}
			decoded, readErr := installstatus.ReadFile(filepath.Join(root, "var/lib/katl/install/status.json"))
			if readErr != nil {
				t.Fatalf("read status: %v", readErr)
			}
			if decoded.State != installstatus.StateRuntimeFailedNeedsRepair || !strings.Contains(decoded.RefusalReason, tt.want) {
				t.Fatalf("repair status = %#v, want refusal %q", decoded, tt.want)
			}
		})
	}
}

func writeCleanGenerationZero(t *testing.T, root string) {
	t.Helper()
	writeGenerationZero(t, root, nil)
}

func writeGenerationZero(t *testing.T, root string, sysexts []generation.ExtensionRef) {
	t.Helper()
	spec := generation.GenerationSpec{
		APIVersion:     generation.APIVersion,
		Kind:           generation.SpecKind,
		GenerationID:   "0",
		RuntimeVersion: "2026.06.04",
		Root: generation.RootSelection{
			Slot:                  "root-a",
			PartitionUUID:         "11111111-2222-3333-4444-555555555555",
			RuntimeVersion:        "2026.06.04",
			RuntimeInterface:      "katl-runtime-1",
			Architecture:          "x86_64",
			RuntimeArtifactSHA256: strings.Repeat("c", 64),
		},
		Boot: generation.BootSelection{
			UKIPath: "/efi/EFI/Linux/katl-0.efi",
		},
		Sysexts:   sysexts,
		CreatedAt: time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC),
	}
	status, err := generation.NewGenerationStatus(spec, generation.CommitStateCommitted, generation.BootStatePending, generation.HealthStateUnknown, time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC))
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

func writeSymlink(t *testing.T, root string, path string, target string) {
	t.Helper()
	full := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, full); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeStatusMissingInstallStatusWritesRepairState(t *testing.T) {
	root := t.TempDir()

	err := run(t.Context(), []string{"--root", root}, nil)
	if err == nil {
		t.Fatal("run() error = nil, want missing status failure")
	}

	data, readErr := os.ReadFile(filepath.Join(root, "var/lib/katl/install/status.json"))
	if readErr != nil {
		t.Fatalf("read status: %v", readErr)
	}
	var decoded installstatus.Record
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if decoded.State != installstatus.StateRuntimeFailedNeedsRepair || decoded.FinalHandoff != "" {
		t.Fatalf("repair status = %#v", decoded)
	}
}

func installStatusRecord(generationID string) installstatus.Record {
	record := installstatus.New(installstatus.StateRebootRequested, time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC))
	record.InputMode = installstatus.InputModePXEPreseed
	record.InputSource = "https://example.invalid/install.json"
	record.RequestDigest = strings.Repeat("a", 64)
	record.KatlosImage = installstatus.Image{
		URL:              "https://example.invalid/katlos.squashfs",
		SHA256:           strings.Repeat("b", 64),
		Version:          "2026.06.04",
		Architecture:     "x86_64",
		RuntimeInterface: "katl-runtime-1",
		Role:             "install",
	}
	record.TargetDiskStableID = "/dev/disk/by-id/ata-root"
	record.SelectedRootSlot = "root-a"
	record.InstalledGeneration = generationID
	return record
}

func TestRuntimeStatusIncompleteInstallStatusWritesRepairState(t *testing.T) {
	root := t.TempDir()
	record := installstatus.New(installstatus.StateRebootRequested, time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC))
	record.RequestDigest = strings.Repeat("a", 64)
	if err := installstatus.WriteFile(filepath.Join(root, "var/lib/katl/install/status.json"), record); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	err := run(t.Context(), []string{"--root", root}, nil)
	if err == nil {
		t.Fatal("run() error = nil, want incomplete status failure")
	}

	data, readErr := os.ReadFile(filepath.Join(root, "var/lib/katl/install/status.json"))
	if readErr != nil {
		t.Fatalf("read status: %v", readErr)
	}
	var decoded installstatus.Record
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if decoded.State != installstatus.StateRuntimeFailedNeedsRepair || decoded.FinalHandoff != "" {
		t.Fatalf("repair status = %#v", decoded)
	}
	if decoded.RequestDigest != strings.Repeat("a", 64) {
		t.Fatalf("repair status did not preserve fields: %#v", decoded)
	}
}
