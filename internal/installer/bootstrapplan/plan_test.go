package bootstrapplan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/installer"
	"github.com/katl-dev/katl/internal/installer/artifact"
	"github.com/katl-dev/katl/internal/installer/generation"
	"github.com/katl-dev/katl/internal/installer/operation"
	"github.com/katl-dev/katl/internal/installer/sysextcatalog"
)

func TestCreateAcceptsBootstrapInitFromStoredIntent(t *testing.T) {
	root := cleanRoot(t, "control-plane")
	now := time.Date(2026, 6, 16, 18, 0, 0, 0, time.UTC)
	source, ref, client := serveKubernetesBundleFixture(t, "v1.36.1", "bootstrap init kubernetes sysext payload")
	req := controlPlaneRequest()
	req.KubernetesBundleSource = source
	req.KubernetesBundleRef = ref

	plan, err := Create(Request{
		Root:         root,
		OperationID:  "bootstrap-init-cp-1",
		Kind:         OperationKindInit,
		Actor:        "katlctl",
		ClientID:     "client-1",
		Now:          now,
		Bootstrap:    req,
		BundleClient: client,
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
	if plan.RuntimeInputs.SelectedKubernetesSysext.BundleSource != source ||
		plan.RuntimeInputs.SelectedKubernetesSysext.BundleRef != ref ||
		plan.RuntimeInputs.SelectedKubernetesSysext.BundleManifestDigest == "" {
		t.Fatalf("selected bundle = %#v", plan.RuntimeInputs.SelectedKubernetesSysext)
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
	editIntent(t, root, func(intent *installer.ClusterIntent) {
		intent.Kubernetes.PayloadVersion = ""
	})
	source, ref, client := serveKubernetesBundleFixture(t, "v1.36.1", "worker join kubernetes sysext payload")
	req := operation.BootstrapRequest{
		InventoryNodeName:        "worker-1",
		SystemRole:               "worker",
		KubernetesPayloadVersion: "v1.36.1",
		KubernetesBundleSource:   source,
		KubernetesBundleRef:      ref,
		BootstrapProfileRef:      "worker",
		CandidateGenerationID:    "1",
		KubeadmInputDigest:       strings.Repeat("d", 64),
		JoinMaterialRef:          "operation:bootstrap-init-cp-1/join-worker",
		JoinMaterialDigest:       strings.Repeat("e", 64),
		JoinMaterialExpiresAt:    "2026-06-15T13:00:00Z",
		TemporaryJoinConfigPath:  "/run/katl/bootstrap-join/bootstrap-join-worker-1/config.yaml",
	}
	plan, err := Create(Request{
		Root:         root,
		OperationID:  "bootstrap-join-worker-1",
		Kind:         OperationKindJoinWorker,
		Bootstrap:    req,
		BundleClient: client,
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

func TestCreateAcceptsControlPlaneJoinFromStoredIntent(t *testing.T) {
	root := cleanRoot(t, "control-plane")
	source, ref, client := serveKubernetesBundleFixture(t, "v1.36.1", "control-plane join kubernetes sysext payload")
	req := operation.BootstrapRequest{
		InventoryNodeName:        "cp-1",
		SystemRole:               "control-plane",
		KubernetesPayloadVersion: "v1.36.1",
		KubernetesBundleSource:   source,
		KubernetesBundleRef:      ref,
		BootstrapProfileRef:      "control-plane",
		CandidateGenerationID:    "1",
		KubeadmInputDigest:       strings.Repeat("d", 64),
		JoinMaterialRef:          "operation:bootstrap-init-cp-1/join-control-plane",
		JoinMaterialDigest:       strings.Repeat("e", 64),
		JoinMaterialExpiresAt:    "2026-06-15T13:00:00Z",
		TemporaryJoinConfigPath:  "/run/katl/bootstrap-join/bootstrap-join-cp-2/config.yaml",
	}
	plan, err := Create(Request{
		Root:         root,
		OperationID:  "bootstrap-join-cp-2",
		Kind:         OperationKindJoinControlPlane,
		Bootstrap:    req,
		BundleClient: client,
	})
	if err != nil {
		t.Fatalf("Create(control-plane join) error = %v", err)
	}
	if plan.Operation.OperationKind != OperationKindJoinControlPlane || plan.Operation.BootstrapRequest.JoinMaterialRef == "" {
		t.Fatalf("operation = %#v", plan.Operation)
	}
	gotPhasePlan := phasePlan(OperationKindJoinControlPlane)
	foundJoinPhase := false
	for _, phase := range gotPhasePlan {
		if phase == "kubeadm-join-control-plane" {
			foundJoinPhase = true
		}
	}
	if !foundJoinPhase {
		t.Fatalf("phase plan = %#v", gotPhasePlan)
	}
	if plan.RuntimeInputs.KubeadmInput.Intent != "control-plane" || plan.RuntimeInputs.KubeadmInput.Path != "/etc/katl/kubeadm/control-plane/config.yaml" {
		t.Fatalf("kubeadm input = %#v", plan.RuntimeInputs.KubeadmInput)
	}
	assertBootSelectionStillGeneration0(t, root)
	assertNoKubeadmMutation(t, root)
}

func TestCreateFetchesKubernetesBundleFromOperationRequest(t *testing.T) {
	root := cleanRoot(t, "control-plane")
	fixture := writeKubernetesBundleFixture(t, "v1.36.1", "fetched kubernetes sysext payload")
	server := httptest.NewTLSServer(http.FileServer(http.Dir(fixture.root)))
	t.Cleanup(server.Close)
	editIntent(t, root, func(intent *installer.ClusterIntent) {
		intent.Kubernetes.SysextPath = ""
		intent.Kubernetes.SysextSHA256 = ""
		intent.Kubernetes.SysextSize = 0
	})
	req := controlPlaneRequest()
	req.KubernetesBundleSource = server.URL
	req.KubernetesBundleRef = fixture.ref

	plan, err := Create(Request{
		Root:         root,
		OperationID:  "bootstrap-init-cp-1",
		Kind:         OperationKindInit,
		Bootstrap:    req,
		BundleClient: server.Client(),
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	selected := plan.RuntimeInputs.SelectedKubernetesSysext
	if selected.PayloadVersion != "v1.36.1" || selected.BundleManifestDigest != fixture.bundleDigest || selected.SysextPayloadDigest == "" {
		t.Fatalf("selected fetched sysext = %#v", selected)
	}
	if !strings.HasPrefix(selected.Path, "/var/lib/katl/artifacts/kubernetes-bundles/sysext/sha256-") {
		t.Fatalf("selected path = %q", selected.Path)
	}
	if selected.ArtifactVersion == "" || selected.BundleSource != server.URL || selected.BundleRef != fixture.ref {
		t.Fatalf("selected bundle identity = %#v", selected)
	}
	read, err := operation.NewStore(filepath.Join(root, "var/lib/katl/operations"))
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	record, err := read.Read("bootstrap-init-cp-1")
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if record.BootstrapRequest == nil || record.BootstrapRequest.KubernetesBundleManifestDigest != fixture.bundleDigest || record.BootstrapRequest.KubernetesSysextPayloadDigest == "" {
		t.Fatalf("stored bootstrap request = %#v", record.BootstrapRequest)
	}
	assertBootSelectionStillGeneration0(t, root)
	assertNoKubeadmMutation(t, root)
}

func TestCreateAcceptsMirroredKubernetesBundle(t *testing.T) {
	root := cleanRoot(t, "control-plane")
	fixture := writeKubernetesBundleFixture(t, "v1.36.1", "mirrored kubernetes sysext payload")
	server := httptest.NewTLSServer(http.FileServer(http.Dir(fixture.root)))
	t.Cleanup(server.Close)
	req := controlPlaneRequest()
	req.KubernetesBundleSource = server.URL
	req.KubernetesBundleRef = fixture.ref

	plan, err := Create(Request{
		Root:         root,
		OperationID:  "bootstrap-init-mirrored-bundle",
		Kind:         OperationKindInit,
		Bootstrap:    req,
		BundleClient: server.Client(),
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	selected := plan.RuntimeInputs.SelectedKubernetesSysext
	if selected.BundleSource != server.URL || selected.BundleRef != fixture.ref || selected.BundleManifestDigest != fixture.bundleDigest {
		t.Fatalf("selected mirrored bundle = %#v", selected)
	}
}

func TestCreateFetchesKubernetesBundleFromOperationWhenStoredIntentUnpinned(t *testing.T) {
	root := cleanRoot(t, "control-plane")
	fixture := writeKubernetesBundleFixture(t, "v1.36.1", "operation selected kubernetes sysext payload")
	server := httptest.NewTLSServer(http.FileServer(http.Dir(fixture.root)))
	t.Cleanup(server.Close)
	req := controlPlaneRequest()
	req.KubernetesBundleSource = server.URL
	req.KubernetesBundleRef = fixture.ref

	plan, err := Create(Request{
		Root:         root,
		OperationID:  "bootstrap-init-cp-1",
		Kind:         OperationKindInit,
		Bootstrap:    req,
		BundleClient: server.Client(),
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	selected := plan.RuntimeInputs.SelectedKubernetesSysext
	if selected.PayloadVersion != "v1.36.1" || selected.BundleSource != server.URL || selected.BundleRef != fixture.ref {
		t.Fatalf("selected fetched sysext = %#v", selected)
	}
	if selected.BundleManifestDigest != fixture.bundleDigest || selected.SysextPayloadDigest == "" {
		t.Fatalf("selected bundle digests = %#v", selected)
	}
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
			name: "missing bundle source and ref",
			req:  controlPlaneRequest(),
			want: "kubernetesBundleSource and kubernetesBundleRef are required",
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
				ID:           "katlos",
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
		},
		Kubeadm: &installer.ClusterIntentKubeadm{
			ConfigRef:   profile,
			ConfigPath:  "/etc/katl/kubeadm/" + profile + "/config.yaml",
			Intent:      intentValue,
			InputDigest: strings.Repeat("d", 64),
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

type kubernetesBundleFixture struct {
	root         string
	ref          string
	bundleDigest string
}

func writeKubernetesBundleFixture(t *testing.T, payloadVersion string, payload string) kubernetesBundleFixture {
	t.Helper()
	sourceDir := t.TempDir()
	outputDir := t.TempDir()
	rawPath := filepath.Join(sourceDir, "katl-kubernetes-"+payloadVersion+".raw")
	if err := os.WriteFile(rawPath, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	meta := artifact.LocalMeta{
		Name:           sysextcatalog.KubernetesName,
		Kind:           artifact.ArtifactSysext,
		Format:         "sysext",
		Path:           filepath.Base(rawPath),
		SizeBytes:      int64(len(payload)),
		SHA256:         digest([]byte(payload)),
		Version:        payloadVersion + "-build.1",
		PayloadVersion: payloadVersion,
		Architecture:   "x86_64",
		SourceRepo: &artifact.SourceRepo{
			ID:      "kubernetes",
			BaseURL: "https://pkgs.k8s.io/core:/stable:/v1.36/rpm/",
			Minor:   "v1.36",
		},
		PackageVersions: map[string]string{
			"cri-tools": "0:" + strings.TrimPrefix(payloadVersion, "v") + "-150500.1.1",
			"kubeadm":   "0:" + strings.TrimPrefix(payloadVersion, "v") + "-150500.1.1",
			"kubectl":   "0:" + strings.TrimPrefix(payloadVersion, "v") + "-150500.1.1",
			"kubelet":   "0:" + strings.TrimPrefix(payloadVersion, "v") + "-150500.1.1",
		},
		RuntimeInterface: "katl-runtime-1",
		CompatibleRuntime: &artifact.Compat{
			Interface:    "katl-runtime-1",
			ArtifactPath: filepath.Join(sourceDir, "katl-runtime-root.squashfs"),
		},
		Created: "2026-06-04T20:00:00Z",
	}
	metadataPath := rawPath + ".json"
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metadataPath, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	staged, err := sysextcatalog.StageKubernetesSysext(sysextcatalog.StageRequest{
		MetadataPath: metadataPath,
		OutputDir:    outputDir,
	})
	if err != nil {
		t.Fatalf("StageKubernetesSysext() error = %v", err)
	}
	return kubernetesBundleFixture{
		root:         outputDir,
		ref:          payloadVersion + "@" + staged.BundleManifestDigest,
		bundleDigest: staged.BundleManifestDigest,
	}
}

func serveKubernetesBundleFixture(t *testing.T, payloadVersion string, payload string) (string, string, *http.Client) {
	t.Helper()
	fixture := writeKubernetesBundleFixture(t, payloadVersion, payload)
	server := httptest.NewTLSServer(http.FileServer(http.Dir(fixture.root)))
	t.Cleanup(server.Close)
	return server.URL, fixture.ref, server.Client()
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
