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
	server, operationID := interruptLivePromotion(t)

	// Use a fresh executor with no activation doubles: recovery must finish
	// bookkeeping without attempting runtime activation again.
	restarted := NewExecutor(server.Root, server.Store, "restarted-agent")
	defaultPath := filepath.Join(server.Root, "observed-boot-default")
	restarted.SetBootOneshot = func(context.Context, string, string) error { return nil }
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
	after, err := server.GetOperation(context.Background(), &agentapi.GetOperationRequest{OperationId: operationID})
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

// A reboot back into the candidate resumes promotion only when this boot's
// activation recorded the candidate. The durable booted generation can be left
// over from an earlier boot when activation failed before recording this one.
func TestRecoverLivePromotionAfterReboot(t *testing.T) {
	for _, tc := range []struct {
		name      string
		activated string
		promoted  bool
	}{
		{name: "activation did not record this boot"},
		{name: "activation recorded another generation", activated: "generation-0"},
		{name: "activation recorded the candidate", activated: "live-recovery", promoted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, operationID := interruptLivePromotion(t)

			// The kernel selected the candidate, and the durable selection
			// already names it as booted with no pending validation.
			writeProcCmdline(t, server.Root, "root=PARTUUID=11111111-1111-1111-1111-111111111111 katl.generation=live-recovery")
			selection, err := generation.ReadBootSelection(server.Root)
			if err != nil {
				t.Fatal(err)
			}
			selection.BootedGenerationID = "live-recovery"
			selection.BootedBootEntry = "loader/entries/katl-live-recovery.conf"
			selection.PendingHealthValidation = false
			if err := generation.WriteBootSelection(server.Root, selection); err != nil {
				t.Fatal(err)
			}
			if tc.activated != "" {
				writeTestFile(t, filepath.Join(server.Root, "run/katl/boot-health/current"), `{"generationID":"`+tc.activated+`"}`+"\n")
			}

			restarted := NewExecutor(server.Root, server.Store, "restarted-agent")
			defaultPath := filepath.Join(server.Root, "observed-boot-default")
			restarted.SetBootOneshot = func(context.Context, string, string) error { return nil }
			restarted.SetBootDefault = func(_ context.Context, _ string, entry string) error {
				return os.WriteFile(defaultPath, []byte(entry), 0o600)
			}
			if err := restarted.recoverLivePromotions(context.Background(), "different-boot"); err != nil {
				t.Fatal(err)
			}

			after, err := server.Store.Read(operationID)
			if err != nil {
				t.Fatal(err)
			}
			_, state, err := generation.ReadGeneration(server.Root, "live-recovery")
			if err != nil {
				t.Fatal(err)
			}
			_, defaultErr := os.Stat(defaultPath)
			if !tc.promoted {
				if after.Terminal || !os.IsNotExist(defaultErr) {
					t.Fatalf("recovery promoted without this boot's activation: terminal=%t result=%s default=%v", after.Terminal, after.Result, defaultErr)
				}
				return
			}
			if !after.Terminal || after.Result != operation.ResultSucceeded || defaultErr != nil || !generation.IsKnownGood(state) {
				t.Fatalf("recovery did not resume promotion: terminal=%t result=%s default=%v state=%+v", after.Terminal, after.Result, defaultErr, state)
			}
		})
	}
}

// interruptLivePromotion leaves a committed live promotion whose process was
// lost before it changed the boot default.
func interruptLivePromotion(t *testing.T) (*testServer, string) {
	t.Helper()
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
	return server, accepted.OperationId
}
