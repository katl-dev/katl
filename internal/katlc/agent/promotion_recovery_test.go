package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/operation"
	"github.com/katl-dev/katl/internal/katlc/agentapi"
)

func TestRecoverLivePromotion(t *testing.T) {
	for _, failure := range []bool{false, true} {
		name := "completed"
		if failure {
			name = "boot default failure"
		}
		t.Run(name, func(t *testing.T) { testRecoverLivePromotion(t, failure) })
	}
}

func testRecoverLivePromotion(t *testing.T, failure bool) {
	server := newTestServer(t)
	writeConfigApplyBaseState(t, server.Root)
	var pending operation.OperationRecord
	server.Dispatcher = dispatchFunc(func(_ context.Context, record operation.OperationRecord) error { pending = record; return nil })
	accepted, err := server.ApplyGeneration(context.Background(), &agentapi.GenerationApplyRequest{
		ApiVersion: APIVersion, Kind: "GenerationApplyRequest", ClientRequestId: "interrupted-promotion", Actor: "test", CandidateGenerationId: "live-recovery", ConfigYaml: configApplyLiveYAML(),
	})
	if err != nil {
		t.Fatal(err)
	}
	executor := NewExecutor(server.Root, server.Store, server.AgentStartID)
	executor.ConfigApplyRunner = &fakeConfigApplyRunner{}
	executor.ConfigApplyActivator = &fakeConfigApplyActivator{}
	// Simulate process loss after durable promotion but before the external
	// default change. Panicking bypasses ordinary error compensation.
	crashed := false
	executor.SetBootDefault = func(context.Context, string, string) error { crashed = true; panic("process lost") }
	func() {
		defer func() {
			if recovered := recover(); recovered != "process lost" {
				t.Fatalf("unexpected interruption: %v", recovered)
			}
		}()
		if err := executor.Execute(context.Background(), pending); err != nil {
			t.Fatal(err)
		}
	}()
	if !crashed {
		t.Fatal("promotion interruption was not reached")
	}
	before, err := server.Store.Read(accepted.OperationId)
	if err != nil {
		t.Fatal(err)
	}
	if before.Terminal {
		t.Fatal("interrupted operation is terminal")
	}

	// Use a fresh executor with no activation doubles: recovery must finish
	// bookkeeping without attempting runtime activation again.
	restarted := NewExecutor(server.Root, server.Store, "restarted-agent")
	defaultPath := filepath.Join(server.Root, "observed-boot-default")
	restarted.SetBootDefault = func(_ context.Context, _ string, entry string) error {
		if failure {
			return errors.New("boot default unavailable")
		}
		return os.WriteFile(defaultPath, []byte(entry), 0o600)
	}
	// A reboot into the old generation invalidates the old live-health receipt.
	if err := restarted.recoverLivePromotions(context.Background(), "different-boot"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(defaultPath); !os.IsNotExist(err) {
		t.Fatalf("old live receipt changed boot default after reboot: %v", err)
	}
	if err := restarted.recoverLivePromotions(context.Background(), currentBootID()); err != nil {
		t.Fatal(err)
	}
	if _, err := AuditStartup(server.Store, server.Now()); err != nil {
		t.Fatal(err)
	}
	after, err := server.GetOperation(context.Background(), &agentapi.GetOperationRequest{OperationId: accepted.OperationId})
	if err != nil {
		t.Fatal(err)
	}
	if failure {
		if !after.Terminal || after.Result != operation.ResultFailedNeedsRepair {
			t.Fatalf("failed recovery left operation unresolved: %+v", after)
		}
		return
	}
	if !after.Terminal || after.Result != operation.ResultSucceeded || after.BootHealthPending {
		t.Fatalf("recovered operation: %+v", after)
	}
	entry, err := os.ReadFile(defaultPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(entry) != "loader/entries/katl-live-recovery.conf" {
		t.Fatalf("boot default = %q", entry)
	}
	_, state, err := generation.ReadGeneration(server.Root, "live-recovery")
	if err != nil {
		t.Fatal(err)
	}
	if !generation.IsKnownGood(state) {
		t.Fatalf("recovered generation: %+v", state)
	}
	if err := restarted.recoverLivePromotions(context.Background(), currentBootID()); err != nil {
		t.Fatalf("repeat recovery: %v", err)
	}
}
