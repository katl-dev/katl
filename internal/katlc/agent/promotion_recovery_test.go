package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/operation"
	"github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/operatorconsole"
)

// An intent this owner cannot read stops it from moving the boot target, so a
// promotion waits until the intent is readable.
func TestUnreadableBootMoveDefersPromotionRecovery(t *testing.T) {
	server, id := interruptLivePromotion(t)
	path := filepath.Join(server.Root, "var/lib/katl/boot/move-intent.json")
	intent, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, path, "not a boot move intent")
	before, err := server.Store.Read(id)
	if err != nil {
		t.Fatal(err)
	}
	restarted := newExecutorFor(server.BootMover, server.Root, server.Store, "restarted-agent")
	restarted.BootMover = server.restartAgent(t)
	setDefault := false
	server.efi.defaultHook = func(string) error { setDefault = true; return nil }

	deferred, err := restarted.recoverLivePromotions(t.Context(), currentBootID())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AuditStartup(server.Store, server.Now(), deferred...); err != nil {
		t.Fatal(err)
	}
	after, err := server.Store.Read(id)
	if err != nil || setDefault || !reflect.DeepEqual(before, after) {
		t.Fatalf("pending move changed promotion receipt or EFI: result=%s terminal=%t EFI write=%t error=%v", after.Result, after.Terminal, setDefault, err)
	}

	writeTestFile(t, path, string(intent))
	if _, err := restarted.recoverLivePromotions(t.Context(), currentBootID()); err != nil {
		t.Fatal(err)
	}
	completed, err := server.Store.Read(id)
	if err != nil || !setDefault || !completed.Terminal || completed.Result != operation.ResultSucceeded {
		t.Fatalf("later startup did not complete promotion: result=%s terminal=%t EFI write=%t error=%v", completed.Result, completed.Terminal, setDefault, err)
	}
}

func TestPendingBootMoveStatus(t *testing.T) {
	server, _ := interruptLivePromotion(t)
	selection, err := generation.ReadBootSelection(server.Root)
	if err != nil {
		t.Fatal(err)
	}
	selection.ActiveGenerationID = "live-recovery"
	if err := generation.WriteBootSelection(server.Root, selection); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(server.Root, "run/katl/boot-health/current"), `{"generationID":"generation-0"}`)
	writeLivePromotionIntent(t, server.Root)

	status, err := server.GetNodeStatus(t.Context(), &agentapi.GetNodeStatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if status.GetCurrentGenerationId() != "generation-0" || status.GetBootTargetGenerationId() != "live-recovery" {
		t.Fatalf("node status current=%q next=%q", status.GetCurrentGenerationId(), status.GetBootTargetGenerationId())
	}
	if err := server.validateExpectedCurrentGeneration(status.GetCurrentGenerationId()); err != nil {
		t.Fatalf("node rejected the generation it reports as current: %v", err)
	}
	var snapshot operatorconsole.Snapshot
	operatorconsole.Collector{Mode: operatorconsole.ModeRuntime, Root: server.Root}.Collect(&snapshot)
	if snapshot.CurrentSoftware.Generation != "generation-0" || snapshot.NextBootSoftware.Generation != "live-recovery" {
		t.Fatalf("console current=%q next=%q error=%q", snapshot.CurrentSoftware.Generation, snapshot.NextBootSoftware.Generation, snapshot.GenerationError)
	}
}

func TestRecoveryBootMoveStatusWithoutTarget(t *testing.T) {
	server := newTestServer(t)
	writeConfigApplyBaseState(t, server.Root)
	writeTestFile(t, filepath.Join(server.Root, "run/katl/boot-health/current"), `{"generationID":"generation-0"}`)
	writeTestFile(t, filepath.Join(server.Root, "var/lib/katl/boot/move-intent.json"), `{
		"recordType": "katl.boot.move-intent",
		"recordVersion": 1,
		"payload": {
			"operationID": "recover-without-destination",
			"bootID": "6baea692-085e-49ac-9bd6-c948a8da8406",
			"expectedSelectionRevision": 0,
			"sourceGenerationID": "generation-0",
			"destinationGenerationID": "",
			"priorDefault": {"generationID":"generation-0","entry":"katl-generation-0.conf"},
			"priorOneShot": {"generationID":"","entry":""},
			"desiredDefault": {"generationID":"","entry":""},
			"desiredOneShot": {"generationID":"","entry":""},
			"desiredSelection": {
				"apiVersion":"katl.dev/v1alpha1","kind":"BootSelection","revision":1,
				"recoveryRequired":true,"updatedAt":"2026-11-26T15:00:00Z"
			},
			"statusChanges": [],
			"reboot": {"trialID":"","eligible":false,"spent":false}
		}
	}`)

	status, err := server.GetNodeStatus(t.Context(), &agentapi.GetNodeStatusRequest{})
	if err != nil || status.GetCurrentGenerationId() != "generation-0" || status.GetBootTargetGenerationId() != "" {
		t.Errorf("node status current=%q next=%q error=%v", status.GetCurrentGenerationId(), status.GetBootTargetGenerationId(), err)
	}

	var snapshot operatorconsole.Snapshot
	operatorconsole.Collector{Mode: operatorconsole.ModeRuntime, Root: server.Root}.Collect(&snapshot)
	if snapshot.CurrentSoftware.Generation != "generation-0" || snapshot.NextBootSoftware.Generation != "" || snapshot.GenerationError != "" {
		t.Errorf("console current=%q next=%q error=%q", snapshot.CurrentSoftware.Generation, snapshot.NextBootSoftware.Generation, snapshot.GenerationError)
	}
	renderer := operatorconsole.NewRenderer(operatorconsole.NewRenderTarget(nil, 80, 24), false)
	if rendered := string(renderer.Render(&snapshot, nil)); !strings.Contains(rendered, "Boot recovery:") || !strings.Contains(rendered, "No next target for current generation generation-0") {
		t.Errorf("console did not display the missing boot target:\n%s", rendered)
	}
}

func writeLivePromotionIntent(t *testing.T, root string) string {
	t.Helper()
	path := filepath.Join(root, "var/lib/katl/boot/move-intent.json")
	writeTestFile(t, path, `{
		"recordType": "katl.boot.move-intent",
		"recordVersion": 1,
		"payload": {
			"operationID": "promote-live",
			"bootID": "6baea692-085e-49ac-9bd6-c948a8da8406",
			"expectedSelectionRevision": 0,
			"sourceGenerationID": "generation-0",
			"destinationGenerationID": "live-recovery",
			"priorDefault": {"generationID":"generation-0","entry":"katl-generation-0.conf"},
			"priorOneShot": {"generationID":"live-recovery","entry":""},
			"desiredDefault": {"generationID":"live-recovery","entry":"katl-live-recovery.conf"},
			"desiredOneShot": {"generationID":"","entry":""},
			"desiredSelection": {
				"apiVersion":"katl.dev/v1alpha1","kind":"BootSelection","revision":1,
				"defaultGenerationID":"live-recovery","defaultBootEntry":"loader/entries/katl-live-recovery.conf",
				"updatedAt":"2026-11-26T15:00:00Z"
			},
			"statusChanges": [],
			"reboot": {"trialID":"","eligible":false,"spent":false}
		}
	}`)
	return path
}

func TestRecoverLivePromotion(t *testing.T) {
	// Recovery uses a fresh executor with no activation doubles: it must
	// finish bookkeeping without attempting runtime activation again.
	t.Run("completed", func(t *testing.T) {
		server, operationID := interruptLivePromotion(t)
		restarted := newExecutorFor(server.restartAgent(t), server.Root, server.Store, "restarted-agent")

		if _, err := restarted.recoverLivePromotions(context.Background(), currentBootID()); err != nil {
			t.Fatal(err)
		}
		if _, err := AuditStartup(server.Store, server.Now()); err != nil {
			t.Fatal(err)
		}

		after, err := server.GetOperation(context.Background(), &agentapi.GetOperationRequest{OperationId: operationID})
		if err != nil {
			t.Fatal(err)
		}
		if !after.Terminal || after.Result != operation.ResultSucceeded || after.BootHealthPending {
			t.Fatalf("recovered operation: %+v", after)
		}
		if got := server.efi.observed().Default; got != "katl-live-recovery.conf" {
			t.Fatalf("boot default = %q", got)
		}
		_, state, err := generation.ReadGeneration(server.Root, "live-recovery")
		if err != nil {
			t.Fatal(err)
		}
		if !generation.IsKnownGood(state) {
			t.Fatalf("recovered generation: %+v", state)
		}
		if _, err := restarted.recoverLivePromotions(context.Background(), currentBootID()); err != nil {
			t.Fatalf("repeat recovery: %v", err)
		}
	})

	t.Run("boot default failure", func(t *testing.T) {
		server, operationID := interruptLivePromotion(t)
		restarted := newExecutorFor(server.restartAgent(t), server.Root, server.Store, "restarted-agent")
		server.efi.defaultErr = errors.New("boot default unavailable")

		if _, err := restarted.recoverLivePromotions(context.Background(), currentBootID()); err == nil || !strings.Contains(err.Error(), "boot default unavailable") {
			t.Fatalf("recovery = %v, want the boot default failure", err)
		}
		if after, err := server.Store.Read(operationID); err != nil || after.Terminal {
			t.Fatalf("a failed boot default write resolved the operation: %+v, %v", after, err)
		}

		if _, err := restarted.recoverLivePromotions(context.Background(), currentBootID()); err != nil {
			t.Fatalf("retry after the boot default failure: %v", err)
		}
		if after, err := server.Store.Read(operationID); err != nil || !after.Terminal || after.Result != operation.ResultSucceeded {
			t.Fatalf("retried recovery = %+v, %v", after, err)
		}
	})

	// A reboot into the old generation invalidates the old live-health receipt.
	t.Run("reboot into the old generation", func(t *testing.T) {
		server, operationID := interruptLivePromotion(t)
		restarted := newExecutorFor(server.bootAs(t, "different-boot", defaultRootPartUUID, "generation-0"), server.Root, server.Store, "restarted-agent")

		if _, err := restarted.recoverLivePromotions(context.Background(), "different-boot"); err != nil {
			t.Fatal(err)
		}
		if after, err := server.Store.Read(operationID); err != nil || after.Terminal {
			t.Fatalf("old live receipt resolved the operation after a reboot: %+v, %v", after, err)
		}
	})
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

			// The kernel selected the candidate, which passed boot health, so the
			// durable selection names it as booted with no pending validation.
			writeProcCmdline(t, server.Root, bootCommandLine(defaultRootPartUUID, "live-recovery"))
			mover := server.bootAs(t, "after-reboot", defaultRootPartUUID, "live-recovery")
			if _, _, err := mover.Activate("live-recovery"); err != nil {
				t.Fatal(err)
			}
			if _, err := mover.ConfirmBoot("test boot health"); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(server.Root, "run/katl/boot-health/current")); err != nil {
				t.Fatal(err)
			}
			if tc.activated != "" {
				recordActivation(t, server.Root, tc.activated)
			}

			restarted := newExecutorFor(mover, server.Root, server.Store, "restarted-agent")
			if _, err := restarted.recoverLivePromotions(context.Background(), "different-boot"); err != nil {
				t.Fatal(err)
			}

			after, err := server.Store.Read(operationID)
			if err != nil {
				t.Fatal(err)
			}
			if after.Terminal != tc.promoted || tc.promoted && after.Result != operation.ResultSucceeded {
				t.Fatalf("recovery resolved the operation = %t (result %q), want %t", after.Terminal, after.Result, tc.promoted)
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
	executor := newExecutorFor(server.BootMover, server.Root, server.Store, server.AgentStartID)
	executor.ConfigApplyRunner = &fakeConfigApplyRunner{}
	executor.ConfigApplyActivator = &fakeConfigApplyActivator{}
	// Simulate process loss after durable promotion but before the external
	// default change. Panicking bypasses ordinary error compensation.
	crashed := false
	executor.BootMover = server.BootMover
	server.efi.defaultHook = func(string) error { crashed = true; panic("process lost") }
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
	server.efi.defaultHook = nil
	before, err := server.Store.Read(accepted.OperationId)
	if err != nil {
		t.Fatal(err)
	}
	if before.Terminal {
		t.Fatal("interrupted operation is terminal")
	}
	return server, accepted.OperationId
}
