package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/configapply"
	"github.com/katl-dev/katl/internal/installer/operation"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestSelectionCompensatesAfterRPCCancellation(t *testing.T) {
	server, _ := knownGoodNode(t, healthSucceeded)
	selection, err := generation.ReadBootSelection(server.Root)
	if err != nil {
		t.Fatal(err)
	}
	selection.DefaultGenerationID, selection.TargetBootGenerationID = "old", "once"
	selection.DefaultBootEntry, selection.TargetBootEntry = "loader/entries/katl-old.conf", "loader/entries/katl-once.conf"
	selection.OneShot = true
	if err := generation.WriteBootSelection(server.Root, selection); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	firmwareDefault, firmwareNext := selection.DefaultBootEntry, selection.TargetBootEntry
	const newEntry = "loader/entries/katl-generation-0.conf"
	server.SetBootDefault = func(efiCtx context.Context, _ string, entry string) error {
		if err := efiCtx.Err(); err != nil {
			return err
		}
		firmwareDefault = entry
		if entry == newEntry {
			cancel()
		}
		return nil
	}
	server.SetBootOneshot = func(efiCtx context.Context, _ string, entry string) error {
		if err := efiCtx.Err(); err != nil {
			return err
		}
		if _, bounded := efiCtx.Deadline(); !bounded {
			t.Error("EFI write has no deadline")
		}
		firmwareNext = entry
		if entry == newEntry {
			return errors.New("one-shot update failed after write")
		}
		return nil
	}

	_, err = server.SelectGeneration(ctx, &agentapi.GenerationMutationRequest{
		GenerationId: "generation-0", ExpectedEnrollmentId: testEnrollmentID, ExpectedInventoryNodeName: testInventoryNodeName,
		ExpectedMachineId: testMachineID, ExpectedCurrentGenerationId: "generation-0",
	})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "one-shot update failed after write") {
		t.Fatalf("select = %v, want write failure after cancellation", err)
	}
	after, err := generation.ReadBootSelection(server.Root)
	if err != nil || after != selection || firmwareDefault != selection.DefaultBootEntry || firmwareNext != selection.TargetBootEntry {
		t.Fatalf("after cancellation selection = %+v, %v; EFI default = %q, one-shot = %q", after, err, firmwareDefault, firmwareNext)
	}
}

func TestGenerationMutationBinding(t *testing.T) {
	server := newTestServer(t)
	writeCleanGenerationZeroState(t, server.Root)
	req := &agentapi.GenerationMutationRequest{GenerationId: "0", ExpectedEnrollmentId: "wrong", ExpectedInventoryNodeName: testInventoryNodeName, ExpectedMachineId: testMachineID, ExpectedCurrentGenerationId: "generation-0"}
	if _, err := server.SelectGeneration(context.Background(), req); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("binding: %v", err)
	}
	if _, err := server.RemoveGeneration(context.Background(), req); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("binding: %v", err)
	}
}

func TestGenerationFallbackAfterEtcdMutation(t *testing.T) {
	server := newTestServer(t)
	writeCleanGenerationZeroState(t, server.Root)
	spec, _, err := generation.ReadGeneration(server.Root, "generation-0")
	if err != nil {
		t.Fatal(err)
	}
	spec.GenerationID = "old-kubernetes"
	spec.Confexts = nil
	spec.Boot.LoaderEntryPath = "loader/entries/katl-old-kubernetes.conf"
	spec.Sysexts = []generation.ExtensionRef{{Name: "kubernetes", PayloadVersion: "v1.36.1", Path: "/var/lib/katl/generations/old-kubernetes/sysext/kubernetes.raw", ActivationPath: "/run/extensions/katl-kubernetes.raw", SHA256: strings.Repeat("a", 64), Architecture: "x86_64", Compatibility: generation.ExtensionCompatibility{RuntimeInterfaces: []string{"katl-runtime-1"}}}}
	state, err := generation.NewGenerationStatus(spec, generation.CommitStateCommitted, generation.BootStateGood, generation.HealthStateHealthy, spec.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := generation.WriteGeneration(server.Root, spec, state); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	_, err = server.Store.Create(operation.OperationRecord{
		OperationID: "failed-etcd-upgrade", OperationKind: OperationKindKubeadmUpgrade, Scope: "kubeadm-state", RequestDigest: strings.Repeat("b", 64), Phase: "etcd-rejoin", Terminal: true, Result: operation.ResultFailedNeedsRepair, CompletedAt: &now, RecoveryRequired: true, ExternalMutationStarted: true,
		KubernetesSysextUpdate: &operation.KubernetesSysextUpdate{SourcePayloadVersion: "v1.36.1", TargetPayloadVersion: "v1.36.2", TargetSysextPath: "/var/lib/katl/artifacts/target.raw", TargetSysextSHA256: strings.Repeat("c", 64), CandidateGenerationID: "new-kubernetes", UpgradeRole: "apply"},
		KubeadmUpgradeEvidence: &operation.KubeadmUpgradeEvidence{TargetKubeadmAccessMode: kubeadmAccessOperationPrivate, KubeletActivationGate: kubeletGateOperationReleased, KubeletGateState: "locked", SourceKubeletPolicy: "keep-running"},
	}, "failed", now)
	if err != nil {
		t.Fatal(err)
	}
	server.MountBootRoot = func(context.Context, string) error { t.Fatal("fallback reached boot mutation"); return nil }
	request := &agentapi.GenerationMutationRequest{GenerationId: spec.GenerationID, ExpectedEnrollmentId: testEnrollmentID, ExpectedInventoryNodeName: testInventoryNodeName, ExpectedMachineId: testMachineID, ExpectedCurrentGenerationId: "generation-0"}
	_, err = server.SelectGeneration(context.Background(), request)
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "potentially migrated data") {
		t.Fatalf("fallback after etcd mutation = %v", err)
	}
}

func TestGenerationManagementAPI(t *testing.T) {
	server := newTestServer(t)
	writeCleanGenerationZeroState(t, server.Root)
	spec, _, err := generation.ReadGeneration(server.Root, "generation-0")
	if err != nil {
		t.Fatal(err)
	}
	// Publish a generation whose runtime has since been replaced on this slot.
	other := spec
	other.GenerationID = "old"
	other.Boot.LoaderEntryPath = "loader/entries/katl-old.conf"
	other.Boot.UKIPath = "/efi/EFI/katl/old.efi"
	other.Root.RuntimeArtifactSHA256 = strings.Repeat("a", 64)
	other.Confexts = nil
	other.CreatedAt = spec.CreatedAt.Add(-1)
	oldState, err := generation.NewGenerationStatus(other, generation.CommitStateCommitted, generation.BootStateGood, generation.HealthStateHealthy, spec.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := generation.WriteGeneration(server.Root, other, oldState); err != nil {
		t.Fatal(err)
	}
	// Initial generation's fixture has not passed health yet. Removal still
	// protects it because it is the selected default, independently of health.
	writeTestBootArtifacts(t, server.Root, other)
	path := filepath.Join(server.Root, "efi", other.Boot.LoaderEntryPath)
	server.MountBootRoot = func(context.Context, string) error { return nil }
	req := &agentapi.GenerationMutationRequest{GenerationId: "generation-0", ExpectedEnrollmentId: testEnrollmentID, ExpectedInventoryNodeName: testInventoryNodeName, ExpectedMachineId: testMachineID, ExpectedCurrentGenerationId: "generation-0"}
	if _, err := server.RemoveGeneration(context.Background(), req); err == nil || !strings.Contains(err.Error(), "protected") {
		t.Fatalf("protected removal: %v", err)
	}
	req.GenerationId = "old"
	for range 2 {
		if _, err := server.RemoveGeneration(context.Background(), req); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("boot entry remained: %v", err)
	}
}

func TestStageRefusesUnverifiedFallback(t *testing.T) {
	server := newTestServer(t)
	writeConfigApplyBaseState(t, server.Root)
	before, err := generation.ReadBootSelection(server.Root)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(server.Root, "dev/vda1")
	if err := os.Truncate(path, 96); err != nil {
		t.Fatal(err)
	}
	executor := NewExecutor(server.Root, server.Store, server.AgentStartID)
	executor.Async = false
	executor.SetBootOneshot = func(context.Context, string, string) error {
		t.Fatal("staged a one-shot without a verified fallback")
		return nil
	}
	server.Dispatcher = executor

	accepted, err := server.StageGeneration(t.Context(), &agentapi.GenerationApplyRequest{
		ApiVersion: APIVersion, Kind: "GenerationApplyRequest", ClientRequestId: "stage-invalid-fallback",
		Actor: "test", ExpectedMachineId: testMachineID, CandidateGenerationId: "next", ConfigYaml: configApplyYAML("next-boot"),
	})
	if err != nil {
		t.Fatal(err)
	}
	completed, err := server.GetOperation(t.Context(), &agentapi.GetOperationRequest{OperationId: accepted.OperationId})
	if err != nil || !completed.GetTerminal() || completed.GetResult() == operation.ResultSucceeded ||
		!strings.Contains(completed.GetFailureReason(), "bootability indeterminate") {
		t.Fatalf("stage = %+v, %v, want unverified fallback refusal", completed, err)
	}
	if after, err := generation.ReadBootSelection(server.Root); err != nil || before != after {
		t.Fatalf("selection changed after refusal: %+v, %v", after, err)
	}
}

func TestStageAfterOneShot(t *testing.T) {
	server := newTestServer(t)
	writeConfigApplyBaseState(t, server.Root)
	selection, err := generation.ReadBootSelection(server.Root)
	if err != nil {
		t.Fatal(err)
	}
	selection.OneShot = true
	if err := generation.WriteBootSelection(server.Root, selection); err != nil {
		t.Fatal(err)
	}
	executor := NewExecutor(server.Root, server.Store, server.AgentStartID)
	executor.Async = false
	server.Dispatcher = executor
	accepted, err := server.StageGeneration(context.Background(), &agentapi.GenerationApplyRequest{ApiVersion: APIVersion, Kind: "GenerationApplyRequest", ClientRequestId: "stage-after-one-shot", Actor: "test", ExpectedMachineId: testMachineID, CandidateGenerationId: "next", ConfigYaml: configApplyYAML("next-boot")})
	if err != nil {
		t.Fatal(err)
	}
	completed, err := server.GetOperation(context.Background(), &agentapi.GetOperationRequest{OperationId: accepted.OperationId})
	if err != nil {
		t.Fatal(err)
	}
	if completed.GetResult() != "succeeded" {
		t.Fatalf("stage failed: %+v", completed)
	}
	selection, err = generation.ReadBootSelection(server.Root)
	if err != nil {
		t.Fatal(err)
	}
	if selection.OneShot || !selection.PendingHealthValidation || selection.TargetBootGenerationID != "next" {
		t.Fatalf("new stage inherited temporary intent: %+v", selection)
	}
}

func TestGenerationCleanupFollowsBootHealth(t *testing.T) {
	server := newTestServer(t).Server
	root := server.Root
	record, err := generation.NewFirstInstallRecord(generation.FirstInstallRequest{
		Root: generation.RootSelection{
			RuntimeVersion: "0.1.0", RuntimeInterface: "katl-runtime-1", Architecture: "x86_64",
			Slot: "root-a", PartitionUUID: "11111111-1111-1111-1111-111111111111",
			RuntimeArtifactSHA256: testSHA(testBootRootImage()),
		},
		GenerationID: "generation-0",
		UKIPath:      "/efi/EFI/Linux/katl-generation-0.efi",
		GeneratedConfext: generation.GeneratedConfext{
			Name: "katl-node", Path: "/var/lib/katl/generations/generation-0/confext",
			ActivationPath: "/run/confexts/katl-node", SHA256: strings.Repeat("b", 64),
			Compatibility: generation.ConfextCompatibility{ID: "katlos", VersionID: "0.1.0", ConfextLevel: 1},
		},
		CreatedAt: server.clock().Add(-time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	current := generation.SpecFromRecord(record)
	current.Boot.LoaderEntryPath = "loader/entries/katl-generation-0.conf"
	if err := generation.Initialize(root, current); err != nil {
		t.Fatal(err)
	}
	writeTestBootDisk(t, root, current.Root.PartitionUUID)
	writeTestBootArtifacts(t, root, current)
	commandLine := "root=PARTUUID=11111111-1111-1111-1111-111111111111 rootfstype=squashfs ro katl.generation=generation-0"
	writeProcCmdline(t, root, commandLine)
	manifest := validVolumeStatusManifest()
	keepLast := 1
	manifest.Node.GenerationRetention = &generation.Retention{KeepLast: &keepLast, MaxAge: "30d"}
	if err := configapply.WriteGenerationManifest(root, "generation-0", manifest); err != nil {
		t.Fatal(err)
	}
	server.MountBootRoot = func(context.Context, string) error { return nil }

	fallback := current
	fallback.GenerationID = "aged-fallback"
	fallback.Boot.LoaderEntryPath = "loader/entries/katl-aged-fallback.conf"
	fallback.Confexts = nil
	fallback.CreatedAt = current.CreatedAt.Add(-60 * 24 * time.Hour)
	status, err := generation.NewGenerationStatus(fallback, generation.CommitStateCommitted, generation.BootStateGood, generation.HealthStateHealthy, fallback.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := generation.WriteGeneration(root, fallback, status); err != nil {
		t.Fatal(err)
	}
	writeTestBootArtifacts(t, root, fallback)

	fallbackPath := filepath.Join(root, "var/lib/katl/generations/aged-fallback")
	if complete, err := server.pruneGenerations(t.Context()); err != nil || complete {
		t.Fatalf("cleanup during pending boot = complete %t, error %v", complete, err)
	}
	if _, err := os.Stat(fallbackPath); err != nil {
		t.Fatalf("pending trial removed fallback generation: %v", err)
	}
	if _, err := generation.RecordBootHealth(generation.BootHealthRequest{
		Root: root, GenerationID: "generation-0", CommandLine: commandLine,
		Result: generation.BootHealthSuccess, Now: server.clock(),
		SetBootDefault: func(string, string) error { return nil },
	}); err != nil {
		t.Fatal(err)
	}

	unit := &healthUnit{}
	server.RunBootHealthStatus = unit.runWithFailedSSH
	for _, tc := range []struct{ state, properties string }{
		{"failed", healthDependencyFailed}, {"pending", healthStalled}, {"unknown", ""},
	} {
		unit.set(tc.properties)
		complete, err := server.pruneGenerations(t.Context())
		if _, statErr := os.Stat(fallbackPath); statErr != nil {
			t.Fatalf("cleanup removed an eligible generation during %s boot health: %v", tc.state, statErr)
		}
		if err != nil || complete {
			t.Fatalf("cleanup during %s boot health = complete %t, error %v", tc.state, complete, err)
		}
	}

	unit.set(healthSucceeded)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); server.maintainGenerations(ctx, 10*time.Millisecond) }()
	t.Cleanup(func() { cancel(); <-done })
	deadline := time.After(2 * time.Second)
	for {
		if _, err := os.Stat(fallbackPath); errors.Is(err, os.ErrNotExist) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		select {
		case <-deadline:
			t.Fatal("aged fallback generation survived healthy boot")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if _, _, err := generation.ReadGeneration(root, "generation-0"); err != nil {
		t.Fatalf("active generation was removed: %v", err)
	}
}
