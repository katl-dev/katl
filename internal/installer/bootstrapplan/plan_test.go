package bootstrapplan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zariel/katl/internal/installer"
	"github.com/zariel/katl/internal/installer/generation"
	"github.com/zariel/katl/internal/installer/manifest"
	"github.com/zariel/katl/internal/installer/operation"
)

func TestCreateAcceptsBootstrapInitFromStoredIntent(t *testing.T) {
	root := cleanRoot(t, "control-plane")
	now := time.Date(2026, 6, 16, 18, 0, 0, 0, time.UTC)

	plan, err := Create(Request{
		Root:        root,
		OperationID: "bootstrap-init-cp-1",
		Kind:        OperationKindInit,
		Actor:       "katlctl",
		ClientID:    "client-1",
		Now:         now,
		Bootstrap: operation.BootstrapRequest{
			InventoryNodeName:        "cp-1",
			SystemRole:               "control-plane",
			KubernetesPayloadVersion: "v1.36.1",
			BootstrapProfileRef:      "control-plane",
			ControlPlaneEndpoint:     "api.katl.test:6443",
			CandidateGenerationID:    " 1 ",
			KubeadmInputDigest:       strings.Repeat("d", 64),
		},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if plan.Operation.OperationID != "bootstrap-init-cp-1" || plan.Operation.OperationKind != OperationKindInit {
		t.Fatalf("operation = %#v", plan.Operation)
	}
	if plan.Operation.GenerationCommitState != operation.GenerationCommitCandidate || plan.Operation.CandidateGenerationID != "1" {
		t.Fatalf("commit state = %#v", plan.Operation)
	}
	if plan.Operation.BootstrapRequest.CandidateGenerationID != "1" || plan.Operation.RequestDigest != requestDigest(OperationKindInit, *plan.Operation.BootstrapRequest) {
		t.Fatalf("normalized bootstrap request/digest = %#v", plan.Operation)
	}
	if plan.Operation.ExpectedCurrentGenerationID != "0" || plan.Operation.ExpectedClusterIntentDigest == "" {
		t.Fatalf("operation expected state = %#v", plan.Operation)
	}
	if plan.Operation.CreatedAt != now || plan.Operation.Terminal || plan.Operation.MutatingToolRan {
		t.Fatalf("operation mutation/time state = %#v", plan.Operation)
	}
	if plan.RuntimeInputs.SelectedKubernetesSysext.PayloadVersion != "v1.36.1" || plan.RuntimeInputs.SelectedKubernetesSysext.RuntimeInterface != "katl-runtime-1" {
		t.Fatalf("selected sysext = %#v", plan.RuntimeInputs.SelectedKubernetesSysext)
	}
	if plan.RuntimeInputs.KubeadmInput.Path != "/etc/katl/kubeadm/control-plane/config.yaml" || plan.RuntimeInputs.KubeadmInput.Digest != strings.Repeat("d", 64) {
		t.Fatalf("kubeadm input = %#v", plan.RuntimeInputs.KubeadmInput)
	}
	if plan.RuntimeInputs.HostConfig.NodeMetadataPath != "/etc/katl/node.json" || plan.RuntimeInputs.HostConfig.ControlPlaneEndpoint != "api.katl.test:6443" {
		t.Fatalf("host config = %#v", plan.RuntimeInputs.HostConfig)
	}
	if plan.RuntimeInputs.KubernetesProjection.What != generation.KubernetesSource || plan.RuntimeInputs.KubernetesProjection.Where != generation.KubernetesTarget {
		t.Fatalf("projection = %#v", plan.RuntimeInputs.KubernetesProjection)
	}
	gotGolden, err := json.MarshalIndent(plan.RuntimeInputs, "", "  ")
	if err != nil {
		t.Fatalf("marshal runtime inputs: %v", err)
	}
	gotGolden = append(gotGolden, '\n')
	wantGolden, err := os.ReadFile(filepath.Join("testdata", "bootstrap-init-runtime-inputs.golden.json"))
	if err != nil {
		t.Fatalf("read golden runtime inputs: %v", err)
	}
	if string(gotGolden) != string(wantGolden) {
		t.Fatalf("runtime inputs golden mismatch\ngot:\n%s\nwant:\n%s", gotGolden, wantGolden)
	}

	store, err := operation.NewStore(filepath.Join(root, "var/lib/katl/operations"))
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	read, err := store.Read("bootstrap-init-cp-1")
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if read.OperationID != plan.Operation.OperationID || read.Phase != "accepted" {
		t.Fatalf("stored operation = %#v", read)
	}
	assertBootSelectionStillGeneration0(t, root)
	assertNoKubeadmMutation(t, root)
}

func TestCreateAcceptsWorkerJoinFromStoredIntent(t *testing.T) {
	root := cleanRoot(t, "worker")
	plan, err := Create(Request{
		Root:        root,
		OperationID: "bootstrap-join-worker-1",
		Kind:        OperationKindJoinWorker,
		Bootstrap: operation.BootstrapRequest{
			InventoryNodeName:        "worker-1",
			SystemRole:               "worker",
			KubernetesPayloadVersion: "v1.36.1",
			BootstrapProfileRef:      "worker",
			CandidateGenerationID:    "1",
			KubeadmInputDigest:       strings.Repeat("d", 64),
			JoinMaterialRef:          "operation:bootstrap-init-cp-1/join-worker",
		},
	})
	if err != nil {
		t.Fatalf("Create(worker join) error = %v", err)
	}
	if plan.Operation.OperationKind != OperationKindJoinWorker || plan.Operation.BootstrapRequest.JoinMaterialRef == "" {
		t.Fatalf("operation = %#v", plan.Operation)
	}
	if plan.RuntimeInputs.KubeadmInput.Intent != "worker" || plan.RuntimeInputs.KubeadmInput.Path != "/etc/katl/kubeadm/worker/config.yaml" {
		t.Fatalf("kubeadm input = %#v", plan.RuntimeInputs.KubeadmInput)
	}
	assertBootSelectionStillGeneration0(t, root)
	assertNoKubeadmMutation(t, root)
}

func TestCreateRefusesUnsupportedControlPlaneJoinWithoutMutation(t *testing.T) {
	root := cleanRoot(t, "control-plane")
	before := bootSelectionBytes(t, root)
	_, err := Create(Request{
		Root:        root,
		OperationID: "bootstrap-join-cp-2",
		Kind:        OperationKindJoinControlPlane,
		Bootstrap: operation.BootstrapRequest{
			InventoryNodeName:        "cp-1",
			SystemRole:               "control-plane",
			KubernetesPayloadVersion: "v1.36.1",
			BootstrapProfileRef:      "control-plane",
			CandidateGenerationID:    "1",
			KubeadmInputDigest:       strings.Repeat("d", 64),
			JoinMaterialRef:          "operation:bootstrap-init-cp-1/join-control-plane",
		},
	})
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("Create(control-plane join) error = %v, want unsupported refusal", err)
	}
	assertUnchanged(t, before, bootSelectionBytes(t, root), "boot selection")
	assertNoOperation(t, root, "bootstrap-join-cp-2")
	assertNoKubeadmMutation(t, root)
}

func TestCreateRefusalsLeaveNoPartialPersistentState(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(string)
		req    operation.BootstrapRequest
		want   string
	}{
		{
			name: "dirty generation zero",
			mutate: func(root string) {
				writeFile(t, filepath.Join(root, "var/lib/katl/kubernetes/etc-kubernetes/admin.conf"), "kubeconfig")
			},
			req:  controlPlaneRequest(),
			want: "generation 0 is not clean",
		},
		{
			name: "payload mismatch",
			req: func() operation.BootstrapRequest {
				req := controlPlaneRequest()
				req.KubernetesPayloadVersion = "v1.37.0"
				return req
			}(),
			want: "kubernetesPayloadVersion",
		},
		{
			name: "missing sysext",
			mutate: func(root string) {
				if err := os.Remove(filepath.Join(root, "var/lib/katl/artifacts/katlos-image/katl-kubernetes.raw")); err != nil {
					t.Fatalf("remove sysext: %v", err)
				}
			},
			req:  controlPlaneRequest(),
			want: "bundled Kubernetes sysext",
		},
		{
			name: "sysext digest mismatch",
			mutate: func(root string) {
				writeFile(t, filepath.Join(root, "var/lib/katl/artifacts/katlos-image/katl-kubernetes.raw"), strings.Repeat("x", len("kubernetes-sysext-payload")))
			},
			req:  controlPlaneRequest(),
			want: "SHA-256",
		},
		{
			name: "sysext path escape",
			mutate: func(root string) {
				editIntent(t, root, func(intent *installer.ClusterIntent) {
					intent.Kubernetes.SysextPath = "../../outside.raw"
				})
			},
			req:  controlPlaneRequest(),
			want: "absolute",
		},
		{
			name: "kubeadm path outside etc katl",
			mutate: func(root string) {
				editIntent(t, root, func(intent *installer.ClusterIntent) {
					intent.Kubeadm.ConfigPath = "/etc/kubernetes/admin.conf"
				})
			},
			req:  controlPlaneRequest(),
			want: "under /etc/katl/kubeadm",
		},
		{
			name: "bootstrap profile refs incomplete",
			mutate: func(root string) {
				editIntent(t, root, func(intent *installer.ClusterIntent) {
					intent.BootstrapProfile.ResolvedID = ""
				})
			},
			req:  controlPlaneRequest(),
			want: "resolvedID",
		},
		{
			name: "worker join lacks join material",
			mutate: func(root string) {
				writeIntent(t, root, "worker")
			},
			req: operation.BootstrapRequest{
				InventoryNodeName:        "worker-1",
				SystemRole:               "worker",
				KubernetesPayloadVersion: "v1.36.1",
				BootstrapProfileRef:      "worker",
				CandidateGenerationID:    "1",
				KubeadmInputDigest:       strings.Repeat("d", 64),
			},
			want: "joinMaterialRef",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := cleanRoot(t, "control-plane")
			if tt.mutate != nil {
				tt.mutate(root)
			}
			before := bootSelectionBytes(t, root)
			beforeKubeadm := kubeadmMutationSnapshot(t, root)
			_, err := Create(Request{
				Root:        root,
				OperationID: "refused-bootstrap",
				Kind:        kindForRequest(tt.req),
				Bootstrap:   tt.req,
			})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Create() error = %v, want %q", err, tt.want)
			}
			assertUnchanged(t, before, bootSelectionBytes(t, root), "boot selection")
			assertNoOperation(t, root, "refused-bootstrap")
			assertKubeadmSnapshotUnchanged(t, beforeKubeadm, kubeadmMutationSnapshot(t, root))
		})
	}
}

func cleanRoot(t *testing.T, role string) string {
	t.Helper()
	root := t.TempDir()
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
				ID:           "katl",
				VersionID:    "0.1.0",
				ConfextLevel: 1,
			},
		},
		CreatedAt: time.Date(2026, 6, 16, 17, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("NewFirstInstallRecord() error = %v", err)
	}
	spec := generation.SpecFromRecord(record)
	status := generation.StatusFromRecord(record, mustSpecDigest(t, spec))
	if err := generation.WriteGeneration(root, spec, status); err != nil {
		t.Fatalf("WriteGeneration() error = %v", err)
	}
	if err := generation.WriteBootSelection(root, generation.BootSelectionRecord{
		APIVersion:            generation.APIVersion,
		Kind:                  generation.BootSelectionKind,
		DefaultGenerationID:   "0",
		DefaultBootEntry:      "loader/entries/katl-0.conf",
		Generation0FallbackID: "0",
		UpdatedAt:             time.Date(2026, 6, 16, 17, 5, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("WriteBootSelection() error = %v", err)
	}
	writeSysext(t, root, "kubernetes-sysext-payload")
	writeIntent(t, root, role)
	return root
}

func writeIntent(t *testing.T, root string, role string) {
	t.Helper()
	node := "cp-1"
	profile := "control-plane"
	intentValue := "control-plane"
	if role == "worker" {
		node = "worker-1"
		profile = "worker"
		intentValue = "worker"
	}
	payload := []byte("kubernetes-sysext-payload")
	intent := installer.ClusterIntent{
		APIVersion:   installer.ClusterIntentAPIVersion,
		Kind:         installer.ClusterIntentKind,
		GenerationID: "0",
		SystemRole:   role,
		Identity:     installer.ClusterIntentIdentity{Hostname: node + "-host"},
		Inventory: installer.ClusterIntentInventory{
			ClusterName:          "lab",
			NodeName:             node,
			ControlPlaneEndpoint: "api.katl.test:6443",
		},
		BootstrapProfile: &installer.ClusterIntentProfile{
			Ref:                profile,
			ResolvedID:         "kubeadm:" + profile,
			KubeadmConfigRef:   profile,
			KubeadmInputDigest: strings.Repeat("d", 64),
		},
		Kubernetes: installer.ClusterIntentKubernetes{
			PayloadVersion: "v1.36.1",
			CatalogRef:     "default",
			SysextPath:     "/var/lib/katl/artifacts/katlos-image/katl-kubernetes.raw",
			SysextSHA256:   digest(payload),
			SysextSize:     uint64(len(payload)),
		},
		Kubeadm: &installer.ClusterIntentKubeadm{
			ConfigRef:   profile,
			ConfigPath:  "/etc/katl/kubeadm/" + profile + "/config.yaml",
			Intent:      intentValue,
			InputDigest: strings.Repeat("d", 64),
		},
		KatlosImage: manifest.KatlosImage{
			SHA256:           strings.Repeat("e", 64),
			Version:          "0.1.0",
			Architecture:     "x86_64",
			RuntimeInterface: "katl-runtime-1",
			Role:             "install",
		},
		Source:        installer.ClusterIntentSource{RequestDigest: strings.Repeat("f", 64)},
		RequestDigest: strings.Repeat("f", 64),
		InstalledAt:   time.Date(2026, 6, 16, 17, 10, 0, 0, time.UTC),
	}
	data, err := json.MarshalIndent(intent, "", "  ")
	if err != nil {
		t.Fatalf("marshal intent: %v", err)
	}
	path := filepath.Join(root, "var/lib/katl/cluster/intent.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("create intent dir: %v", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatalf("write intent: %v", err)
	}
}

func editIntent(t *testing.T, root string, edit func(*installer.ClusterIntent)) {
	t.Helper()
	path := filepath.Join(root, "var/lib/katl/cluster/intent.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read intent: %v", err)
	}
	var intent installer.ClusterIntent
	if err := json.Unmarshal(data, &intent); err != nil {
		t.Fatalf("decode intent: %v", err)
	}
	edit(&intent)
	data, err = json.MarshalIndent(intent, "", "  ")
	if err != nil {
		t.Fatalf("marshal intent: %v", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatalf("write intent: %v", err)
	}
}

func writeSysext(t *testing.T, root string, content string) {
	t.Helper()
	writeFile(t, filepath.Join(root, "var/lib/katl/artifacts/katlos-image/katl-kubernetes.raw"), content)
}

func controlPlaneRequest() operation.BootstrapRequest {
	return operation.BootstrapRequest{
		InventoryNodeName:        "cp-1",
		SystemRole:               "control-plane",
		KubernetesPayloadVersion: "v1.36.1",
		BootstrapProfileRef:      "control-plane",
		ControlPlaneEndpoint:     "api.katl.test:6443",
		CandidateGenerationID:    "1",
		KubeadmInputDigest:       strings.Repeat("d", 64),
	}
}

func kindForRequest(request operation.BootstrapRequest) string {
	if request.SystemRole == "worker" {
		return OperationKindJoinWorker
	}
	return OperationKindInit
}

func mustSpecDigest(t *testing.T, spec generation.GenerationSpec) string {
	t.Helper()
	digest, err := generation.CanonicalSpecDigest(spec)
	if err != nil {
		t.Fatalf("CanonicalSpecDigest() error = %v", err)
	}
	return digest
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func bootSelectionBytes(t *testing.T, root string) []byte {
	t.Helper()
	path, err := generation.BootSelectionPath(root)
	if err != nil {
		t.Fatalf("BootSelectionPath() error = %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read boot selection: %v", err)
	}
	return data
}

func assertBootSelectionStillGeneration0(t *testing.T, root string) {
	t.Helper()
	selection, err := generation.ReadBootSelection(root)
	if err != nil {
		t.Fatalf("ReadBootSelection() error = %v", err)
	}
	if selection.DefaultGenerationID != "0" || selection.TargetBootGenerationID != "" || selection.TrialGenerationID != "" || selection.PendingHealthValidation {
		t.Fatalf("boot selection changed = %#v", selection)
	}
}

func assertNoKubeadmMutation(t *testing.T, root string) {
	t.Helper()
	snapshot := kubeadmMutationSnapshot(t, root)
	if len(snapshot) > 0 {
		t.Fatalf("unexpected kubeadm mutation paths = %#v", snapshot)
	}
}

func kubeadmMutationSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	snapshot := map[string]string{}
	for _, top := range []string{
		"etc/katl/kubeadm",
		"etc/kubernetes",
		"var/lib/katl/kubernetes/etc-kubernetes",
		"var/lib/kubelet",
		"var/lib/etcd",
	} {
		base := filepath.Join(root, top)
		if _, err := os.Stat(base); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			t.Fatalf("inspect %s: %v", top, err)
		}
		if err := filepath.WalkDir(base, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			if entry.IsDir() {
				snapshot[filepath.ToSlash(rel)] = "dir"
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			snapshot[filepath.ToSlash(rel)] = digest(data)
			return nil
		}); err != nil {
			t.Fatalf("snapshot kubeadm state under %s: %v", top, err)
		}
	}
	return snapshot
}

func assertKubeadmSnapshotUnchanged(t *testing.T, before map[string]string, after map[string]string) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatalf("kubeadm state changed\nbefore: %#v\nafter: %#v", before, after)
	}
	for path, digest := range before {
		if after[path] != digest {
			t.Fatalf("kubeadm state changed at %s\nbefore: %#v\nafter: %#v", path, before, after)
		}
	}
}

func assertNoOperation(t *testing.T, root string, id string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(root, "var/lib/katl/operations", id)); err == nil {
		t.Fatalf("unexpected operation %s", id)
	} else if !os.IsNotExist(err) {
		t.Fatalf("inspect operation %s: %v", id, err)
	}
}

func assertUnchanged(t *testing.T, before []byte, after []byte, name string) {
	t.Helper()
	if string(before) != string(after) {
		t.Fatalf("%s changed\nbefore:\n%s\nafter:\n%s", name, before, after)
	}
}

func writeFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create parent for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
