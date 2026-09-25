package agent

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/katlosimage"
	"github.com/katl-dev/katl/internal/installer/operation"
)

func TestActiveHostUpgradeImage(t *testing.T) {
	root := t.TempDir()
	writeCleanGenerationZeroState(t, root)
	spec, state, err := generation.ReadGeneration(root, "generation-0")
	if err != nil {
		t.Fatal(err)
	}
	state.CommittedByOperation = "upgrade-1"
	state.BootState = generation.BootStateGood
	state.HealthState = generation.HealthStateHealthy
	if err := generation.WriteGenerationStatus(root, spec, state); err != nil {
		t.Fatal(err)
	}
	store, err := operation.NewStore(filepath.Join(root, "var/lib/katl/operations"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	digest := strings.Repeat("e", 64)
	_, err = store.Create(operation.OperationRecord{
		OperationID: "upgrade-1", OperationKind: OperationKindHostUpgrade, Scope: "host-generation",
		RequestDigest: strings.Repeat("d", 64), Phase: "complete", PhasePlan: []string{"complete"}, CompletedPhases: []string{"complete"}, PhaseIndex: 1,
		CandidateGenerationID: "generation-0", HostUpgradeRequest: &operation.HostUpgrade{ImageLocalRef: "upgrade.squashfs", ImageSHA256: digest, ImageSizeBytes: 4096, CandidateGenerationID: "generation-0"},
		Terminal: true, Result: operation.ResultSucceeded, CreatedAt: now, UpdatedAt: now, CompletedAt: &now,
	}, "complete", now)
	if err != nil {
		t.Fatal(err)
	}
	executor := &Executor{Root: root, Store: store}
	payload := katlosimage.Payload{ImageSHA256: digest, ImageSizeBytes: 4096, Index: katlosimage.Index{Version: spec.RuntimeVersion}}
	if !executor.activeHostUpgradeImage(payload, "") {
		t.Fatal("identical committed image should require no upgrade")
	}
	payload.ImageSHA256 = strings.Repeat("f", 64)
	if executor.activeHostUpgradeImage(payload, "") {
		t.Fatal("different image with the same version must still upgrade")
	}
	payload.ImageSHA256 = digest
	if executor.activeHostUpgradeImage(payload, "config: changed") {
		t.Fatal("configuration change must still upgrade")
	}
	state.HealthState = generation.HealthStateUnhealthy
	if err := generation.WriteGenerationStatus(root, spec, state); err != nil {
		t.Fatal(err)
	}
	if executor.activeHostUpgradeImage(payload, "") {
		t.Fatal("unhealthy generation must not be reported as unchanged")
	}
}
