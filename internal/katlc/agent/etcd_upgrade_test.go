package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/installer/operation"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"gopkg.in/yaml.v3"
)

func TestUpgradeRoleMatchesNode(t *testing.T) {
	for _, tc := range []struct {
		name, role, controlPlanePath string
		wantError                    bool
	}{
		{"worker", "worker", "", false},
		{"control-plane", "apply", "/etc/kubernetes/admin.conf", false},
		{"additional control-plane", "control-plane", "/etc/kubernetes/manifests/etcd.yaml", false},
		{"worker role with admin config", "worker", "/etc/kubernetes/admin.conf", true},
		{"worker role with API manifest", "worker", "/etc/kubernetes/manifests/kube-apiserver.yaml", true},
		{"worker role with etcd manifest", "worker", "/etc/kubernetes/manifests/etcd.yaml", true},
		{"worker role with etcd data", "worker", "/var/lib/etcd/member", true},
		{"control-plane role on worker", "apply", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, store, record, now := kubeadmUpgradeFixture(t, "worker")
			for _, path := range []string{"/etc/kubernetes/admin.conf", "/etc/kubernetes/manifests/kube-apiserver.yaml", "/etc/kubernetes/manifests/etcd.yaml"} {
				if err := os.Remove(rootedRuntimePath(root, path)); err != nil {
					t.Fatal(err)
				}
			}
			if tc.controlPlanePath == "/var/lib/etcd/member" {
				if err := os.MkdirAll(rootedRuntimePath(root, tc.controlPlanePath), 0o700); err != nil {
					t.Fatal(err)
				}
			} else if tc.controlPlanePath != "" {
				writeTestFile(t, rootedRuntimePath(root, tc.controlPlanePath), "control-plane state")
			}
			server := NewServer(root, store)
			update := record.KubernetesSysextUpdate
			request := &agentapi.SubmitOperationRequest{OperationKind: OperationKindKubeadmUpgrade, KubernetesSysextUpdate: &agentapi.KubernetesSysextUpdateOperationRequest{
				SourcePayloadVersion: update.SourcePayloadVersion, TargetPayloadVersion: update.TargetPayloadVersion,
				TargetSysextPath: update.TargetSysextPath, TargetSysextSha256: update.TargetSysextSHA256,
				CandidateGenerationId: "role-target", UpgradeRole: tc.role,
			}}

			_, err := server.planKubernetesSysextUpdateOperation(request, strings.Repeat("a", 64), "role-check", nil, now)

			if (err != nil) != tc.wantError {
				t.Fatalf("upgrade role result = %v, want error %t", err, tc.wantError)
			}
			if tc.wantError && !strings.Contains(err.Error(), "does not match local control-plane state") {
				t.Fatalf("unexpected refusal: %v", err)
			}
		})
	}
}

func TestEtcdPreviewUsesPatchedManifest(t *testing.T) {
	for _, fails := range []bool{false, true} {
		t.Run(fmt.Sprint("render-fails=", fails), func(t *testing.T) {
			root, store, record, _ := kubeadmUpgradeFixture(t, "apply")
			patch := "spec:\n  containers:\n  - name: etcd\n    image: registry.example/database:operator-choice\n"
			writeKubeadmConfigFixture(t, root, "gen0", "custom", "apiVersion: kubeadm.k8s.io/v1beta4\nkind: InitConfiguration\n", map[string]string{"etcd+strategic.yaml": patch})
			manifest := filepath.Join(root, "etc/kubernetes/manifests/etcd.yaml")
			before, err := os.ReadFile(manifest)
			if err != nil {
				t.Fatal(err)
			}
			previewDir := filepath.Join(root, "var/lib/katl/operations", record.OperationID, "tools/etcd-preview")
			writeTestFile(t, filepath.Join(previewDir, "etcd.yaml"), string(before))
			executor := Executor{Root: root, Store: store}
			executor.RunTool = func(_ context.Context, argv []string, _ func(int)) ToolResult {
				if strings.Contains(strings.Join(argv, " "), "get configmap kubeadm-config") {
					return ToolResult{Stdout: []byte("apiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\n")}
				}
				if len(argv) < 10 || !slices.Equal(argv[:9], []string{"env", "KUBEADM_INIT_DRYRUN_DIR=" + previewDir, "target-kubeadm", "init", "phase", "etcd", "local", "--dry-run", "--config"}) {
					t.Fatalf("native manifest inspection missing: %v", argv)
				}
				data, err := os.ReadFile(argv[9])
				if err != nil {
					t.Fatal(err)
				}
				var initInput struct {
					Patches struct {
						Directory string `yaml:"directory"`
					} `yaml:"patches"`
				}
				if err := yaml.NewDecoder(bytes.NewReader(data)).Decode(&initInput); err != nil {
					t.Fatal(err)
				}
				got, err := os.ReadFile(filepath.Join(initInput.Patches.Directory, "etcd+strategic.yaml"))
				if err != nil || string(got) != patch {
					t.Fatalf("selected patch = %q, %v", got, err)
				}
				if _, err := os.Stat(filepath.Join(previewDir, "etcd.yaml")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("previous preview could be accepted as new evidence")
				}
				if fails {
					return ToolResult{ExitStatus: 1, Stderr: []byte("native patch rejection")}
				}
				writeTestFile(t, filepath.Join(previewDir, "etcd.yaml"), "apiVersion: v1\nkind: Pod\n"+patch)
				return ToolResult{}
			}

			evidence, err := executor.inspectEtcdUpgrade(context.Background(), record, "target-kubeadm")

			if (err != nil) != fails {
				t.Fatalf("native preview error = %v", err)
			}
			if !fails && (evidence.TargetImage != "registry.example/database:operator-choice" || !evidence.ImageChanges) {
				t.Fatalf("preview did not use kubeadm's patched image: %+v", evidence)
			}
			after, err := os.ReadFile(manifest)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("serving manifest changed during preview: %v", err)
			}
		})
	}
}

func TestEtcdRestartQuorum(t *testing.T) {
	for _, tc := range []struct {
		name      string
		change    func(*operation.EtcdObservation)
		wantError bool
	}{
		{"healthy", func(*operation.EtcdObservation) {}, false},
		{"advancing writes", func(o *operation.EtcdObservation) {
			for i := range o.Members {
				o.Members[i].AppliedIndex = 110 + uint64(i)
			}
		}, false},
		{"unhealthy survivor", func(o *operation.EtcdObservation) { o.Members[1].Healthy = false }, true},
		{"lagging survivor", func(o *operation.EtcdObservation) { o.Members[2].AppliedIndex = 99 }, true},
		{"learner", func(o *operation.EtcdObservation) { o.Members[2].Learner = true }, true},
		{"downgrade in progress", func(o *operation.EtcdObservation) {
			o.Members[1].DowngradeEnabled = true
			o.Members[1].DowngradeTarget = "3.5.0"
		}, true},
		{"member replaced", func(o *operation.EtcdObservation) { o.Members[0].ID = "replacement"; o.LocalMemberID = "replacement" }, true},
		{"different cluster", func(o *operation.EtcdObservation) { o.ClusterID = "different" }, true},
		{"unknown storage version", func(o *operation.EtcdObservation) { o.Members[1].StorageVersion = "" }, true},
		{"two voters", func(o *operation.EtcdObservation) { o.Members = o.Members[:2] }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := testUpgradeEtcdEvidence().Before
			observed := testUpgradeEtcdEvidence().Before
			tc.change(observed)
			if err := validateUpgradeEtcd(observed, before, true); (err != nil) != tc.wantError {
				t.Fatalf("restart validation = %v, want error %t", err, tc.wantError)
			}
		})
	}
}

func TestEtcdVersionsAreEvidence(t *testing.T) {
	for _, tc := range []struct{ name, server, cluster, storage string }{
		{"mixed upgrade", "3.7.0", "3.6.0", "3.7.0"},
		{"completed upgrade", "3.7.0", "3.7.0", "3.7.0"},
		{"future release", "8.5.9", "8.5.0", "8.5.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := testUpgradeEtcdEvidence().Before
			observed := testUpgradeEtcdEvidence().Before
			observed.Members[0].Version = tc.server
			observed.Members[0].ClusterVersion = tc.cluster
			observed.Members[0].StorageVersion = tc.storage
			if err := validateUpgradeEtcd(observed, before, true); err != nil {
				t.Fatalf("healthy member versions became rollout policy: %v", err)
			}
		})
	}
}

func TestUpgradePreparationUsesKubeadmPolicy(t *testing.T) {
	for _, reject := range []bool{false, true} {
		t.Run(fmt.Sprint("rejected=", reject), func(t *testing.T) {
			root, store, _, _ := kubeadmUpgradeFixture(t, "apply")
			request := &operation.KubernetesSysextUpdate{SourcePayloadVersion: "v1.36.2", TargetPayloadVersion: "v1.37.0", TargetSysextPath: "/var/lib/katl/artifacts/kubernetes.raw", TargetSysextSHA256: strings.Repeat("a", 64), CandidateGenerationID: "preview", UpgradeRole: "apply", PreviewOnly: true}
			record := createUnresolvedUpgradeRecord(t, store, "native-policy", request)
			executor := newUpgradeExecutor(root, store)
			executor.inspectUpgradeEtcd = func(context.Context, operation.OperationRecord, string) (*operation.EtcdUpgradeEvidence, error) {
				evidence := testUpgradeEtcdEvidence()
				evidence.TargetImage = "registry.example/etcd:future-image"
				evidence.ImageChanges = true
				return evidence, nil
			}
			planned := false
			executor.RunTool = func(_ context.Context, argv []string, _ func(int)) ToolResult {
				joined := strings.Join(argv, " ")
				if strings.Contains(joined, "get configmap kubeadm-config") {
					return ToolResult{Stdout: []byte("apiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\n")}
				}
				if strings.Contains(joined, "upgrade plan v1.37.0") {
					planned = true
					if reject {
						return ToolResult{ExitStatus: 1, Stderr: []byte("upstream upgrade policy rejected transition")}
					}
					return ToolResult{Stdout: []byte("upgrade permitted by target kubeadm")}
				}
				t.Fatalf("preparation reached an unexpected command: %v", argv)
				return ToolResult{}
			}
			prepared, err := executor.prepareUpgradeEtcd(context.Background(), record, "target-kubeadm")
			if !planned || (err != nil) != reject {
				t.Fatalf("target kubeadm policy result = planned %t, error %v", planned, err)
			}
			if prepared.ExternalMutationStarted || prepared.KubernetesSysextUpdate.SnapshotStorageLocation != "" {
				t.Fatal("preview marked a control-plane mutation or took a snapshot")
			}
		})
	}
}

func TestSnapshotChecksum(t *testing.T) {
	valid := testEtcdSnapshot()
	corrupt := bytes.Clone(valid)
	corrupt[10] ^= 0xff
	for _, tc := range []struct {
		name      string
		data      []byte
		wantError bool
	}{
		{"valid", valid, false}, {"corrupt database", corrupt, true}, {"missing checksum", valid[:512], true}, {"truncated", valid[:510], true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "snapshot.db")
			if err := os.WriteFile(path, tc.data, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := verifyEtcdSnapshot(path); (err != nil) != tc.wantError {
				t.Fatalf("verify snapshot = %v", err)
			}
		})
	}
}

func TestPreparedUpgradePreservesArtifact(t *testing.T) {
	root, store, original, now := kubeadmUpgradeFixture(t, "apply")
	prepared, err := store.Read("prepared-1")
	if err != nil {
		t.Fatal(err)
	}
	prepared.OperationID = "prepared-bundle"
	prepared.KubernetesSysextUpdate.TargetSysextPath = "/var/lib/katl/artifacts/kubernetes-upgrades/prepared-bundle/kubernetes.raw"
	if _, err := store.Create(prepared, "etcd-prepared", now); err != nil {
		t.Fatal(err)
	}
	request := *original.KubernetesSysextUpdate
	request.PreparedOperationID = prepared.OperationID
	original.OperationID = "upgrade-bundle"
	original.KubernetesSysextUpdate = &request
	record, err := store.Create(original, "accepted", now)
	if err != nil {
		t.Fatal(err)
	}

	executor := newUpgradeExecutor(root, store)
	if _, err := executor.usePreparedUpgrade(record); err != nil {
		t.Fatal(err)
	}
	observed, err := store.Read(record.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if observed.KubernetesSysextUpdate.TargetSysextPath != request.TargetSysextPath || observed.KubernetesSysextUpdate.TargetSysextSHA256 != request.TargetSysextSHA256 || observed.KubernetesSysextUpdate.TargetSysextSize != request.TargetSysextSize {
		t.Fatalf("upgrade artifact changed: %+v", observed.KubernetesSysextUpdate)
	}
	if observed.KubernetesSysextUpdate.SnapshotDigest != prepared.KubernetesSysextUpdate.SnapshotDigest || observed.KubeadmUpgradeEvidence.Etcd.Snapshot == nil {
		t.Fatal("prepared snapshot evidence was not retained")
	}
}

func TestEtcdFailureStopsUpgrade(t *testing.T) {
	for _, failure := range []string{"cannot start", "old process still running"} {
		t.Run(failure, func(t *testing.T) {
			root, store, original, now := kubeadmUpgradeFixture(t, "apply")
			evidence := testUpgradeEtcdEvidence()
			evidence.SourceImage = "registry.k8s.io/etcd:3.6.5-0"
			evidence.TargetImage = "registry.k8s.io/etcd:3.6.5-1"
			evidence.ImageChanges = true
			prepared, err := store.Read("prepared-1")
			if err != nil {
				t.Fatal(err)
			}
			evidence.Snapshot = prepared.KubeadmUpgradeEvidence.Etcd.Snapshot
			prepared.OperationID = "prepared-failure"
			prepared.KubeadmUpgradeEvidence.Etcd = evidence
			if _, err := store.Create(prepared, "etcd-prepared", now); err != nil {
				t.Fatal(err)
			}
			request := *original.KubernetesSysextUpdate
			request.PreparedOperationID = prepared.OperationID
			original.OperationID = "upgrade-failure"
			original.KubernetesSysextUpdate = &request
			record, err := store.Create(original, "accepted", now)
			if err != nil {
				t.Fatal(err)
			}
			manifest := filepath.Join(root, "etc/kubernetes/manifests/etcd.yaml")
			writeTestFile(t, manifest, "apiVersion: v1\nkind: Pod\nspec:\n  containers:\n    - name: etcd\n      image: registry.k8s.io/etcd:3.6.5-0\n")
			executor := newUpgradeExecutor(root, store)
			executor.inspectUpgradeEtcd = func(context.Context, operation.OperationRecord, string) (*operation.EtcdUpgradeEvidence, error) {
				return evidence, nil
			}
			executor.observeUpgradeEtcd = func(context.Context, *operation.EtcdObservation) (*operation.EtcdObservation, error) {
				observed := testUpgradeEtcdEvidence().Before
				if target, err := os.Readlink(filepath.Join(root, "run/extensions/katl-kubernetes.raw")); err == nil && failure == "old process still running" && !strings.Contains(target, "/gen1/") {
					t.Fatalf("restored source Kubernetes sysext after mutation: %s", target)
				}
				data, err := os.ReadFile(manifest)
				if err != nil {
					return observed, err
				}
				if bytes.Contains(data, []byte(evidence.TargetImage)) && failure == "cannot start" {
					observed.LocalImage = ""
					observed.LocalContainerID = ""
					observed.Members[0].Healthy = false
					observed.Members[0].Error = "etcd startup failed"
					return observed, errors.New("etcd startup failed")
				}
				return observed, nil
			}
			var commands []string
			executor.RunTool = func(_ context.Context, argv []string, _ func(int)) ToolResult {
				joined := strings.Join(argv, " ")
				commands = append(commands, joined)
				switch {
				case strings.Contains(joined, "kubeadm version"):
					return ToolResult{Stdout: []byte("v1.36.2\n")}
				case strings.Contains(joined, "get configmap kubeadm-config"):
					return ToolResult{Stdout: []byte("apiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\n")}
				case strings.Contains(joined, "get endpoints kubernetes"):
					return ToolResult{Stdout: testKubeAPIServerEndpoints("10.0.0.1")}
				case strings.Contains(joined, "kubelet --version"):
					return ToolResult{Stdout: []byte("Kubernetes v1.36.2\n")}
				case strings.Contains(joined, "kubeadm upgrade apply"):
					data := []byte("apiVersion: v1\nkind: Pod\nspec:\n  containers:\n    - name: etcd\n      image: " + evidence.TargetImage + "\n")
					if err := os.WriteFile(manifest, data, 0o600); err != nil {
						return ToolResult{Err: err, ExitStatus: 1}
					}
					if failure == "cannot start" {
						return ToolResult{Err: errors.New("etcd startup failed"), ExitStatus: 1}
					}
				}
				return ToolResult{}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			if err := executor.Execute(ctx, record); err == nil {
				t.Fatal("failed or unobserved restart succeeded")
			}
			failed, err := store.Read(record.OperationID)
			if err != nil {
				t.Fatal(err)
			}
			if !failed.RecoveryRequired || !failed.Terminal || failed.PostMutationRollbackAllowed || failed.HostRollback != "" || failed.Result != operation.ResultFailedNeedsRepair {
				t.Fatalf("failure = %+v", failed)
			}
			for _, command := range commands {
				if strings.Contains(command, "snapshot restore") {
					t.Fatalf("advanced or reverted after etcd failure: %s", command)
				}
			}
			if target, err := os.Readlink(filepath.Join(root, "run/extensions/katl-kubernetes.raw")); err == nil && failure == "old process still running" && !strings.Contains(target, "/gen1/") {
				t.Fatalf("restored source Kubernetes sysext after mutation: %s", target)
			}
			data, err := os.ReadFile(manifest)
			if err != nil || !bytes.Contains(data, []byte(evidence.TargetImage)) {
				t.Fatalf("target etcd manifest was reverted: %s, %v", data, err)
			}
			observed := failed.KubeadmUpgradeEvidence.Etcd.After
			if observed == nil || len(observed.Members) != 3 || !observed.Members[1].Healthy || !observed.Members[2].Healthy {
				t.Fatalf("missing surviving-voter evidence: %+v", observed)
			}
		})
	}
}
