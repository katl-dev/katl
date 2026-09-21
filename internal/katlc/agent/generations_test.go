package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/generation"
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
