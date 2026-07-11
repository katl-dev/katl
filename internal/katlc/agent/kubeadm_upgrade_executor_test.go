package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zariel/katl/internal/installer/generation"
	"github.com/zariel/katl/internal/installer/operation"
	agentapi "github.com/zariel/katl/internal/katlc/agentapi"
)

func TestExecutorRunsApplyUpgradeWithPrivateKubeadmAndGate(t *testing.T) {
	root, store, record, now := kubeadmUpgradeFixture(t, "apply")
	var commands [][]string
	executor := NewExecutor(root, store, "agent-test")
	executor.Async = false
	executor.Now = func() time.Time { return now.Add(time.Minute) }
	executor.RunTool = func(_ context.Context, argv []string, _ func(int)) ToolResult {
		commands = append(commands, append([]string(nil), argv...))
		joined := strings.Join(argv, " ")
		switch {
		case strings.Contains(joined, "/usr/bin/kubeadm version"):
			return ToolResult{Stdout: []byte("v1.36.2\n")}
		case strings.Contains(joined, "kubelet --version"):
			return ToolResult{Stdout: []byte("Kubernetes v1.36.2\n")}
		default:
			return ToolResult{}
		}
	}
	if err := executor.Execute(context.Background(), record); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	completed, err := store.Read(record.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if !completed.Terminal || completed.Result != operation.ResultSucceeded || completed.RecoveryRequired {
		t.Fatalf("completed operation = %+v", completed)
	}
	if !completed.ExternalMutationStarted || !completed.MutatingToolRan || len(completed.PreExecMutationMarkers) != 1 {
		t.Fatalf("mutation evidence = external %v ran %v markers %+v", completed.ExternalMutationStarted, completed.MutatingToolRan, completed.PreExecMutationMarkers)
	}
	if completed.KubeadmUpgradeEvidence.TargetKubeadmAccessMode != kubeadmAccessOperationPrivate || completed.KubeadmUpgradeEvidence.KubeletGateState != "target-observed" || completed.KubeadmUpgradeEvidence.GlobalTargetActiveBeforeKubeadm {
		t.Fatalf("upgrade evidence = %+v", completed.KubeadmUpgradeEvidence)
	}
	assertCommandOrder(t, commands, "kubeadm upgrade plan v1.36.2", "kubeadm upgrade apply --yes v1.36.2", "systemd-sysext refresh", "systemctl restart kubelet.service")
	spec, status, err := generation.ReadGeneration(root, "gen1")
	if err != nil {
		t.Fatal(err)
	}
	if status.CommitState != generation.CommitStateCommitted || status.BootState != generation.BootStateTrying || spec.Sysexts[0].PayloadVersion != "v1.36.2" {
		t.Fatalf("candidate = spec %+v status %+v", spec, status)
	}
	if len(spec.Confexts) != 1 || spec.Confexts[0].Path != "/var/lib/katl/generations/gen1/confext" {
		t.Fatalf("candidate confext refs = %+v", spec.Confexts)
	}
	if data, err := os.ReadFile(filepath.Join(root, "var/lib/katl/generations/gen1/confext/etc/systemd/network/20-node.network")); err != nil || !strings.Contains(string(data), "DHCP=yes") {
		t.Fatalf("candidate inherited confext = %q, %v", data, err)
	}
	gate := filepath.Join(root, "run/katl/operation-gates/kubeadm-upgrade-1/target-kubelet-released")
	if data, err := os.ReadFile(gate); err != nil || strings.TrimSpace(string(data)) != record.OperationID {
		t.Fatalf("gate = %q, %v", data, err)
	}
	dropIn := filepath.Join(root, "run/systemd/system/kubelet.service.d/20-katl-upgrade-gate.conf")
	if _, err := os.Stat(dropIn); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("completed kubelet gate drop-in still exists: %v", err)
	}
}

func TestInstallKubeletGate(t *testing.T) {
	root := t.TempDir()
	executor := NewExecutor(root, operation.Store{}, "agent-test")
	gate := "/run/katl/operation-gates/upgrade-1/target-kubelet-released"
	unit := "kubelet.service.d/20-katl-upgrade-gate.conf"
	if err := executor.installKubeletGate(gate, unit); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "run/systemd/system", unit))
	if err != nil || string(data) != "[Unit]\nConditionPathExists="+gate+"\n" {
		t.Fatalf("kubelet gate drop-in = %q, %v", data, err)
	}
}

func TestExecutorKeepsRecoveryRequiredAfterKubeadmMutationFailure(t *testing.T) {
	root, store, record, now := kubeadmUpgradeFixture(t, "worker")
	executor := NewExecutor(root, store, "agent-test")
	executor.Async = false
	executor.Now = func() time.Time { return now.Add(time.Minute) }
	sawUnmount := false
	executor.RunTool = func(_ context.Context, argv []string, _ func(int)) ToolResult {
		joined := strings.Join(argv, " ")
		if strings.Contains(joined, "systemd-dissect --umount") {
			sawUnmount = true
		}
		if strings.Contains(joined, "/usr/bin/kubeadm version") {
			return ToolResult{Stdout: []byte("v1.36.2\n")}
		}
		if strings.Contains(joined, "kubeadm upgrade node") {
			return ToolResult{Err: errors.New("interrupted"), ExitStatus: 1, Stderr: []byte("token=abcdef.0123456789abcdef\n")}
		}
		return ToolResult{}
	}
	if err := executor.Execute(context.Background(), record); err == nil {
		t.Fatal("Execute() error = nil")
	}
	failed, err := store.Read(record.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if !failed.Terminal || !failed.RecoveryRequired || failed.Result != operation.ResultFailedNeedsRepair || failed.HostRollback != "gen0" {
		t.Fatalf("failed operation = %+v", failed)
	}
	if !strings.Contains(failed.NextAction, "host rollback does not repair") {
		t.Fatalf("next action = %q", failed.NextAction)
	}
	if len(failed.DiagnosticArtifacts) == 0 {
		t.Fatal("missing redacted diagnostic artifact")
	}
	data, err := os.ReadFile(filepath.Join(root, "var/lib/katl/operations", record.OperationID, strings.TrimPrefix(failed.DiagnosticArtifacts[0].Path, "/")))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "0123456789abcdef") {
		t.Fatalf("diagnostic leaked secret: %q", data)
	}
	if sawUnmount {
		t.Fatal("operation-private target kubeadm repair view was removed after mutation failure")
	}
}

func TestValidateKubeadmUpgradeExecutionSafetyInputs(t *testing.T) {
	base := &agentapi.KubernetesSysextUpdateOperationRequest{
		TargetPayloadVersion: "v1.36.2", TargetSysextPath: "/var/lib/katl/artifacts/kubernetes.raw", TargetSysextSha256: strings.Repeat("a", 64),
		CandidateGenerationId: "gen1", UpgradeRole: "apply", SourcePayloadVersion: "v1.36.1", SnapshotRef: "snapshot-1", SnapshotDigest: strings.Repeat("b", 64),
		SnapshotCreatedAt: "2026-07-11T12:00:00Z", CapturedMemberListDigest: strings.Repeat("c", 64), SnapshotStorageLocation: "/var/lib/katl/etcd-snapshots/snapshot-1.db", SnapshotOperatorIdentity: "operator-a",
	}
	if err := validateKubernetesSysextUpdateRequest(OperationKindKubeadmUpgrade, base); err != nil {
		t.Fatalf("valid request: %v", err)
	}
	missing := *base
	missing.SnapshotDigest = ""
	if err := validateKubernetesSysextUpdateRequest(OperationKindKubeadmUpgrade, &missing); err == nil || !strings.Contains(err.Error(), "snapshotDigest is required") {
		t.Fatalf("missing snapshot error = %v", err)
	}
	skip := *base
	skip.TargetPayloadVersion = "v1.38.0"
	if err := validateKubernetesSysextUpdateRequest(OperationKindKubeadmUpgrade, &skip); err == nil || !strings.Contains(err.Error(), "only a newer patch or the next minor") {
		t.Fatalf("skip error = %v", err)
	}
}

func TestExecutorRefusesUpgradeBeforeMutationWhenSafetyGateFails(t *testing.T) {
	for _, tc := range []struct {
		name        string
		prepare     func(t *testing.T, root string, record operation.OperationRecord)
		failCommand string
		want        string
	}{
		{name: "snapshot digest", prepare: func(t *testing.T, root string, record operation.OperationRecord) {
			if err := os.WriteFile(filepath.Join(root, strings.TrimPrefix(record.KubernetesSysextUpdate.SnapshotStorageLocation, "/")), []byte("corrupt"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, want: "verify referenced etcd snapshot"},
		{name: "private sysext mount", failCommand: "systemd-dissect --mount", want: "mount operation-private target sysext"},
		{name: "kubelet gate reload", failCommand: "systemctl daemon-reload", want: "load target kubelet activation gate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, store, record, now := kubeadmUpgradeFixture(t, "control-plane")
			if tc.prepare != nil {
				tc.prepare(t, root, record)
			}
			executor := NewExecutor(root, store, "agent-test")
			executor.Async = false
			executor.Now = func() time.Time { return now.Add(time.Minute) }
			executor.RunTool = func(_ context.Context, argv []string, _ func(int)) ToolResult {
				joined := strings.Join(argv, " ")
				if tc.failCommand != "" && strings.Contains(joined, tc.failCommand) {
					return ToolResult{Err: errors.New("refused"), ExitStatus: 1}
				}
				if strings.Contains(joined, "/usr/bin/kubeadm version") {
					return ToolResult{Stdout: []byte("v1.36.2\n")}
				}
				return ToolResult{}
			}
			if err := executor.Execute(context.Background(), record); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Execute() error = %v, want %q", err, tc.want)
			}
			failed, err := store.Read(record.OperationID)
			if err != nil {
				t.Fatal(err)
			}
			if !failed.Terminal || failed.RecoveryRequired || failed.ExternalMutationStarted || failed.MutatingToolRan || failed.GenerationCommitState != operation.GenerationCommitAbandoned {
				t.Fatalf("pre-mutation failure = %+v", failed)
			}
			if _, status, err := generation.ReadGeneration(root, "gen1"); err == nil && status.CommitState != generation.CommitStateAbandoned {
				t.Fatalf("candidate status = %+v", status)
			}
		})
	}
}

func kubeadmUpgradeFixture(t *testing.T, role string) (string, operation.Store, operation.OperationRecord, time.Time) {
	t.Helper()
	root := t.TempDir()
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc/machine-id"), []byte("0123456789abcdef0123456789abcdef\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := []byte("target Kubernetes sysext")
	targetDigest := sha256.Sum256(target)
	targetSHA := hex.EncodeToString(targetDigest[:])
	snapshot := []byte("verified stacked etcd snapshot")
	snapshotDigest := sha256.Sum256(snapshot)
	snapshotSHA := hex.EncodeToString(snapshotDigest[:])
	confextPath := filepath.Join(root, "var/lib/katl/generations/gen0/confext")
	if err := os.MkdirAll(filepath.Join(confextPath, "etc/systemd/network"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(confextPath, "etc/systemd/network/20-node.network"), []byte("[Network]\nDHCP=yes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	confextSHA, err := generation.DigestDirectory(confextPath)
	if err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(root, "var/lib/katl/artifacts/kubernetes.raw")
	if err := os.MkdirAll(filepath.Dir(artifact), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(artifact, target, 0o600); err != nil {
		t.Fatal(err)
	}
	snapshotPath := filepath.Join(root, "var/lib/katl/etcd-snapshots/snapshot-1.db")
	if err := os.MkdirAll(filepath.Dir(snapshotPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(snapshotPath, snapshot, 0o600); err != nil {
		t.Fatal(err)
	}
	previous := generation.GenerationSpec{
		APIVersion: generation.APIVersion, Kind: generation.SpecKind, GenerationID: "gen0", RuntimeVersion: "2026.7.0-dev.0", CreatedAt: now.Add(-time.Hour),
		Root:     generation.RootSelection{Slot: "root-a", PartitionUUID: "aaaaaaaa-1111-2222-3333-444444444444", RuntimeVersion: "2026.7.0-dev.0", RuntimeInterface: "katl-runtime-1", Architecture: "x86_64", RuntimeArtifactSHA256: strings.Repeat("d", 64)},
		Boot:     generation.BootSelection{UKIPath: "/efi/EFI/Linux/katl.efi", LoaderEntryPath: "loader/entries/katl-gen0.conf"},
		Sysexts:  []generation.ExtensionRef{{Name: "kubernetes", Path: "/var/lib/katl/generations/gen0/sysext/kubernetes.raw", ActivationPath: "/run/extensions/katl-kubernetes.raw", SHA256: strings.Repeat("e", 64), ArtifactVersion: "v1.36.1", PayloadVersion: "v1.36.1", Architecture: "x86_64", Compatibility: generation.ExtensionCompatibility{RuntimeInterfaces: []string{"katl-runtime-1"}}}},
		Confexts: []generation.GeneratedConfext{{Name: "katl-node", Path: "/var/lib/katl/generations/gen0/confext", ActivationPath: "/run/confexts/katl-node", SHA256: confextSHA, Compatibility: generation.ConfextCompatibility{ID: "katl", VersionID: "1", ConfextLevel: 1}}},
	}
	status, err := generation.NewGenerationStatus(previous, generation.CommitStateCommitted, generation.BootStateGood, generation.HealthStateHealthy, previous.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := generation.WriteGeneration(root, previous, status); err != nil {
		t.Fatal(err)
	}
	if err := generation.WriteBootSelection(root, generation.BootSelectionRecord{APIVersion: generation.APIVersion, Kind: generation.BootSelectionKind, DefaultGenerationID: "gen0", BootedGenerationID: "gen0", Generation0FallbackID: "gen0", UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	store, err := operation.NewStore(filepath.Join(root, "var/lib/katl/operations"))
	if err != nil {
		t.Fatal(err)
	}
	request := &operation.KubernetesSysextUpdate{TargetPayloadVersion: "v1.36.2", TargetSysextPath: "/var/lib/katl/artifacts/kubernetes.raw", TargetSysextSHA256: targetSHA, TargetSysextSize: uint64(len(target)), CandidateGenerationID: "gen1", UpgradeRole: role, SourcePayloadVersion: "v1.36.1", SnapshotRef: "snapshot-1", SnapshotDigest: snapshotSHA, SnapshotCreatedAt: now.Format(time.RFC3339), CapturedMemberListDigest: strings.Repeat("c", 64), SnapshotStorageLocation: "/var/lib/katl/etcd-snapshots/snapshot-1.db", SnapshotOperatorIdentity: "operator-a"}
	record, err := store.Create(operation.OperationRecord{OperationID: "kubeadm-upgrade-1", OperationKind: OperationKindKubeadmUpgrade, Scope: "kubeadm-state", RequestDigest: strings.Repeat("f", 64), Phase: "accepted", PhasePlan: kubeadmUpgradePhasePlan(role), CompletedPhases: []string{"accepted"}, PhaseIndex: 1, CandidateGenerationID: "gen1", KubernetesSysextUpdate: request, KubeadmUpgradeEvidence: &operation.KubeadmUpgradeEvidence{TargetKubeadmAccessMode: kubeadmAccessOperationPrivate, KubeletActivationGate: kubeletGateOperationReleased, KubeletGateState: "locked", SourceKubeletPolicy: "keep-running"}, ActivationMode: operation.ActivationModeNextBoot, ActivationState: operation.ActivationStatePending, GenerationCommitState: operation.GenerationCommitCandidate, PostKubeadmHealthState: operation.PostKubeadmHealthNotRun, ResourceLocks: []string{"generation-state.lock", "kubeadm-state.lock"}}, "accepted", now)
	if err != nil {
		t.Fatal(err)
	}
	return root, store, record, now
}

func assertCommandOrder(t *testing.T, commands [][]string, needles ...string) {
	t.Helper()
	index := -1
	for _, needle := range needles {
		found := -1
		for i := index + 1; i < len(commands); i++ {
			if strings.Contains(strings.Join(commands[i], " "), needle) {
				found = i
				break
			}
		}
		if found < 0 {
			t.Fatalf("command %q not found after %d in %v", needle, index, commands)
		}
		index = found
	}
}
