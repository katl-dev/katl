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
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

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

func TestGenerationManagementAPI(t *testing.T) {
	server := newTestServer(t)
	writeCleanGenerationZeroState(t, server.Root)
	spec, _, err := generation.ReadGeneration(server.Root, "generation-0")
	if err != nil {
		t.Fatal(err)
	}
	// Use an independently published older generation as the removable target.
	other := spec
	other.GenerationID = "old"
	other.Boot.LoaderEntryPath = "loader/entries/katl-old.conf"
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
	path := filepath.Join(server.Root, "efi/loader/entries/katl-old.conf")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("old entry"), 0o644); err != nil {
		t.Fatal(err)
	}
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
			RuntimeArtifactSHA256: strings.Repeat("a", 64),
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
	commandLine := "root=PARTUUID=11111111-1111-1111-1111-111111111111 rootfstype=squashfs ro katl.generation=generation-0"
	writeProcCmdline(t, root, commandLine)
	if err := configapply.WriteGenerationManifest(root, "generation-0", validVolumeStatusManifest()); err != nil {
		t.Fatal(err)
	}
	server.MountBootRoot = func(context.Context, string) error { return nil }

	obsolete := current
	obsolete.GenerationID = "obsolete"
	obsolete.RuntimeVersion = "0.0.9"
	obsolete.Root.Slot = "root-b"
	obsolete.Root.PartitionUUID = "22222222-2222-2222-2222-222222222222"
	obsolete.Root.RuntimeArtifactSHA256 = strings.Repeat("c", 64)
	obsolete.Boot.LoaderEntryPath = "loader/entries/katl-obsolete.conf"
	obsolete.Boot.UKIPath = "/efi/EFI/Linux/katl-obsolete.efi"
	obsolete.Confexts = nil
	obsolete.CreatedAt = current.CreatedAt.Add(-time.Hour)
	status, err := generation.NewGenerationStatus(obsolete, generation.CommitStateCommitted, generation.BootStateGood, generation.HealthStateHealthy, obsolete.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	status.UnavailableReason = "OS slot has been replaced"
	if err := generation.WriteGeneration(root, obsolete, status); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); server.maintainGenerations(ctx, 10*time.Millisecond) }()
	t.Cleanup(func() { cancel(); <-done })

	obsoletePath := filepath.Join(root, "var/lib/katl/generations/obsolete")
	if complete, err := server.pruneGenerations(ctx); err != nil || complete {
		t.Fatalf("cleanup during pending boot = complete %t, error %v", complete, err)
	}
	if _, err := os.Stat(obsoletePath); err != nil {
		t.Fatalf("pending boot removed obsolete generation: %v", err)
	}
	if _, err := generation.RecordBootHealth(generation.BootHealthRequest{
		Root: root, GenerationID: "generation-0", CommandLine: commandLine,
		Result: generation.BootHealthSuccess, Now: server.clock(),
		SetBootDefault: func(string, string) error { return nil },
	}); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(2 * time.Second)
	for {
		if _, err := os.Stat(obsoletePath); errors.Is(err, os.ErrNotExist) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		select {
		case <-deadline:
			t.Fatal("obsolete generation survived healthy boot")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if _, _, err := generation.ReadGeneration(root, "generation-0"); err != nil {
		t.Fatalf("active generation was removed: %v", err)
	}
}
