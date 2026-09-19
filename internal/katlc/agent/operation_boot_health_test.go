package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/operation"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
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
