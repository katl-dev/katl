package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/operation"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestOperationBootHealthFollowsGeneration(t *testing.T) {
	for _, tc := range []struct {
		name, commit, boot, health, owner string
		pending                           bool
	}{
		{"unbooted", generation.CommitStateCommitted, generation.BootStatePending, generation.HealthStateUnknown, "bootstrap", true},
		{"healthy", generation.CommitStateCommitted, generation.BootStateGood, generation.HealthStateHealthy, "bootstrap", false},
		{"superseded", generation.CommitStateSuperseded, generation.BootStateGood, generation.HealthStateHealthy, "bootstrap", false},
		{"failed", generation.CommitStateCommitted, generation.BootStateFailed, generation.HealthStateUnhealthy, "bootstrap", true},
		{"other operation", generation.CommitStateCommitted, generation.BootStateGood, generation.HealthStateHealthy, "another", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := newTestServer(t)
			writeCleanGenerationZeroState(t, server.Root)
			spec, state, err := generation.ReadGeneration(server.Root, "generation-0")
			if err != nil {
				t.Fatal(err)
			}
			state.CommitState, state.BootState, state.HealthState = tc.commit, tc.boot, tc.health
			state.CommittedByOperation = tc.owner
			if err := generation.WriteGenerationStatus(server.Root, spec, state); err != nil {
				t.Fatal(err)
			}
			now := server.Now()
			_, err = server.Store.Create(operation.OperationRecord{
				OperationID: "bootstrap", OperationKind: "bootstrap-init", Scope: "kubeadm-state", RequestDigest: strings.Repeat("a", 64),
				Phase: "complete", PhasePlan: []string{"complete"}, CompletedPhases: []string{"complete"}, PhaseIndex: 1,
				CandidateGenerationID: "generation-0", BootHealthPending: true, Terminal: true, Result: operation.ResultSucceeded,
				CreatedAt: now, UpdatedAt: now, CompletedAt: &now,
			}, "complete", now)
			if err != nil {
				t.Fatal(err)
			}

			got, err := server.GetOperation(context.Background(), &agentapi.GetOperationRequest{OperationId: "bootstrap"})
			if err != nil {
				t.Fatal(err)
			}
			if got.GetBootHealthPending() != tc.pending {
				t.Fatalf("bootHealthPending = %v, want %v", got.GetBootHealthPending(), tc.pending)
			}
			receipt, err := server.Store.Read("bootstrap")
			if err != nil {
				t.Fatal(err)
			}
			if !receipt.BootHealthPending {
				t.Fatal("reading status rewrote the operation receipt")
			}
		})
	}
}

func TestRetiredGenerationReceiptDoesNotRequestBoot(t *testing.T) {
	for _, removed := range []bool{false, true} {
		t.Run(map[bool]string{false: "invalidated", true: "removed"}[removed], func(t *testing.T) {
			server := newTestServer(t)
			writeCleanGenerationZeroState(t, server.Root)
			spec, state, err := generation.ReadGeneration(server.Root, "generation-0")
			if err != nil {
				t.Fatal(err)
			}
			state.UnavailableReason = "OS slot has been replaced"
			if err := generation.WriteGenerationStatus(server.Root, spec, state); err != nil {
				t.Fatal(err)
			}
			if removed {
				// A new selection no longer refers to this historical receipt's generation.
				selection, _ := generation.ReadBootSelection(server.Root)
				selection.DefaultGenerationID = "new"
				selection.BootedGenerationID = "new"
				selection.Generation0FallbackID = ""
				if err := generation.WriteBootSelection(server.Root, selection); err != nil {
					t.Fatal(err)
				}
				dir, _ := generation.GenerationDir(server.Root, "generation-0")
				if err := os.RemoveAll(dir); err != nil {
					t.Fatal(err)
				}
			}
			now := server.Now()
			_, err = server.Store.Create(operation.OperationRecord{OperationID: "old-upgrade", OperationKind: "bootstrap-init", Scope: "kubeadm-state", RequestDigest: strings.Repeat("a", 64), Phase: "complete", PhasePlan: []string{"complete"}, CompletedPhases: []string{"complete"}, PhaseIndex: 1, CandidateGenerationID: "generation-0", BootHealthPending: true, Terminal: true, Result: operation.ResultSucceeded, CreatedAt: now, UpdatedAt: now, CompletedAt: &now}, "complete", now)
			if err != nil {
				t.Fatal(err)
			}
			result, err := server.GetOperation(context.Background(), &agentapi.GetOperationRequest{OperationId: "old-upgrade"})
			if err != nil {
				t.Fatal(err)
			}
			if result.BootHealthPending || !strings.Contains(result.NextAction, "no longer bootable") {
				t.Fatalf("stale boot instruction: %+v", result)
			}
		})
	}
}

func TestPendingReceiptReportsBootability(t *testing.T) {
	for _, test := range []struct {
		name, reason string
		fault        string
	}{
		{name: "missing UKI", reason: "cannot boot"},
		{name: "unreadable UKI", reason: "bootability indeterminate", fault: "UKI"},
		{name: "unreadable staged root", reason: "bootability indeterminate", fault: "root"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := newTestServer(t)
			writeCleanGenerationZeroState(t, server.Root)
			spec, _, err := generation.ReadGeneration(server.Root, "generation-0")
			if err != nil {
				t.Fatal(err)
			}
			spec.GenerationID = "candidate"
			spec.Boot.LoaderEntryPath = "loader/entries/katl-candidate.conf"
			spec.Boot.UKIPath = "/efi/EFI/Linux/katl-candidate.efi"
			spec.Confexts = nil
			now := server.Now()
			state, err := generation.NewGenerationStatus(spec, generation.CommitStateCommitted, generation.BootStatePending, generation.HealthStateUnknown, now)
			if err != nil {
				t.Fatal(err)
			}
			if err := generation.WriteGeneration(server.Root, spec, state); err != nil {
				t.Fatal(err)
			}
			writeBootSelection(t, server.Root, spec.GenerationID)
			writeTestFile(t, filepath.Join(server.Root, "efi", spec.Boot.LoaderEntryPath), "entry")
			evidence := spec.Boot.UKIPath
			switch test.fault {
			case "UKI":
				uki := filepath.Join(server.Root, spec.Boot.UKIPath)
				if err := os.MkdirAll(filepath.Dir(uki), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(uki, uki); err != nil {
					t.Fatal(err)
				}
			case "root":
				writeTestBootArtifacts(t, server.Root, spec)
				path := filepath.Join(server.Root, "dev/vda1")
				if err := os.Truncate(path, 96); err != nil {
					t.Fatal(err)
				}
				evidence = "PARTUUID " + spec.Root.PartitionUUID
			}
			const rebootAdvice = "reboot into the prepared target generation"
			_, err = server.Store.Create(operation.OperationRecord{
				OperationID: "upgrade", OperationKind: "bootstrap-init", Scope: "kubeadm-state",
				RequestDigest: strings.Repeat("a", 64), Phase: "complete", PhasePlan: []string{"complete"},
				CompletedPhases: []string{"complete"}, PhaseIndex: 1, CandidateGenerationID: spec.GenerationID,
				BootHealthPending: true, Terminal: true, Result: operation.ResultSucceeded, NextAction: rebootAdvice,
				CreatedAt: now, UpdatedAt: now, CompletedAt: &now,
			}, "complete", now)
			if err != nil {
				t.Fatal(err)
			}

			result, err := server.GetOperation(t.Context(), &agentapi.GetOperationRequest{OperationId: "upgrade"})
			if err != nil {
				t.Fatal(err)
			}
			if !result.GetBootHealthPending() || !strings.Contains(result.GetNextAction(), test.reason) ||
				!strings.Contains(result.GetNextAction(), evidence) || strings.Contains(result.GetNextAction(), rebootAdvice) {
				t.Fatalf("boot health guidance = %+v, want pending with %q", result, test.reason)
			}

			server.RunReboot = func(context.Context, []string, func(int)) ToolResult {
				t.Fatal("scheduled a reboot into a target the receipt could not verify")
				return ToolResult{}
			}
			_, err = server.Reboot(t.Context(), &agentapi.RebootRequest{
				ApiVersion: APIVersion, Kind: RebootRequestKind, Actor: "katlctl node upgrade",
				ExpectedMachineId: testMachineID, TargetGenerationId: spec.GenerationID,
			})
			if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), test.reason) {
				t.Fatalf("reboot = %v, want refusal with %q", err, test.reason)
			}
		})
	}
}
