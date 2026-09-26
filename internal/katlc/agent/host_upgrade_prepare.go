package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/configapply"
	"github.com/katl-dev/katl/internal/installer/katlosimage"
)

func commitPreparedHostUpgrade(root string, handoff generation.UpgradeHandoff, payload katlosimage.Payload, plan katlosimage.HostUpgradePlan, extensions hostExtensionPlan) error {
	if plan.Spec.Root.Slot != handoff.RootSlot || !strings.EqualFold(plan.Spec.Root.PartitionUUID, handoff.RootPartitionUUID) || plan.Spec.Boot.UKIPath != handoff.UKIPath || plan.Spec.Boot.LoaderEntryPath != handoff.LoaderEntryPath {
		return fmt.Errorf("target generation changes the staged preboot contract")
	}
	machineID, err := os.ReadFile(filepath.Join(runtimeRoot(root), "etc/machine-id"))
	if err != nil {
		return err
	}
	expectedEntry, err := generation.RenderEntry(generation.LoaderRequest{Record: generation.RecordFromSplit(plan.Spec, plan.Status), MachineID: strings.TrimSpace(string(machineID))})
	if err != nil {
		return err
	}
	actualEntry, err := os.ReadFile(filepath.Join(runtimeRoot(root), "efi", handoff.LoaderEntryPath))
	if err != nil {
		return fmt.Errorf("read staged loader entry: %w", err)
	}
	if string(actualEntry) != expectedEntry.Content {
		return fmt.Errorf("target generation requires a different preboot loader entry")
	}
	if err := katlosimage.StagePreservedAssets(runtimeRoot(root), plan); err != nil {
		return err
	}
	if err := katlosimage.StageBundledAssets(runtimeRoot(root), plan); err != nil {
		return err
	}
	if _, _, err := configapply.MaterializeSystemExtensions(root, handoff.CandidateGenerationID, plan.Spec.Root, plan.Spec.ExtensionRelease, extensions.desired, extensions.materials); err != nil {
		return err
	}
	if err := generation.WriteGeneration(root, plan.Spec, plan.Status); err != nil {
		return err
	}
	if err := configapply.WriteGenerationManifest(root, handoff.CandidateGenerationID, extensions.manifest); err != nil {
		return err
	}
	plan.Status.CommitState = generation.CommitStateCommitted
	plan.Status.BootState = generation.BootStateTrying
	plan.Status.CommittedAt = &handoff.CreatedAt
	plan.Status.CommittedByOperation = handoff.OperationID
	plan.Status.UpdatedAt = handoff.CreatedAt
	if err := generation.WriteGenerationStatus(root, plan.Spec, plan.Status); err != nil {
		return err
	}
	return nil
}
