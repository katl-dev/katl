package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/katl-dev/katl/internal/installer/operation"
)

var destructiveResetMutationScopes = []string{
	"katlos-boot-artifacts",
	"disk-boot-path",
}

var destructiveResetBootArtifactGlobs = []string{
	"loader/entries/katl-*.conf",
	"EFI/Linux/katl*.efi",
	"EFI/Linux/katl*.EFI",
}

var destructiveResetBootArtifactPaths = []string{
	"EFI/BOOT/BOOTX64.EFI",
	"EFI/BOOT/BOOTX64.efi",
	"EFI/systemd/systemd-bootx64.efi",
	"EFI/systemd/systemd-bootx64.EFI",
}

var destructiveResetPoweroffArgv = []string{
	"systemd-run",
	"--unit=katl-destructive-reset-poweroff",
	"--collect",
	"--on-active=10s",
	"systemctl",
	"poweroff",
}

func (e *Executor) executeDestructiveReset(ctx context.Context, record operation.OperationRecord) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if record.OperationKind != OperationKindDestructiveReset {
		err := fmt.Errorf("operation kind %q does not match destructive reset request", record.OperationKind)
		_, markErr := e.failRecordPhase(record.OperationID, "destructive-reset-kind-invalid", "destructive-reset-invalid", "dispatch-failed", "resubmit a valid destructive reset request", err)
		return errors.Join(err, markErr)
	}
	request := record.DestructiveResetRequest
	if request == nil {
		err := fmt.Errorf("destructive reset request is required")
		_, markErr := e.failRecordPhase(record.OperationID, "destructive-reset-missing-request", "destructive-reset-invalid", "dispatch-failed", "resubmit a valid destructive reset request", err)
		return errors.Join(err, markErr)
	}
	if err := operation.ValidateDestructiveReset(*request); err != nil {
		_, markErr := e.failRecordPhase(record.OperationID, "destructive-reset-request-invalid", "destructive-reset-invalid", "dispatch-failed", "resubmit a valid destructive reset request", err)
		return errors.Join(err, markErr)
	}
	targetGeneration := strings.TrimSpace(request.TargetGenerationID)
	if targetGeneration != "" {
		err := fmt.Errorf("destructive reset target generation %q is unsupported; disk boot invalidation does not select a local generation", targetGeneration)
		_, markErr := e.failRecordPhase(record.OperationID, "destructive-reset-target-invalid", "destructive-reset-invalid", "dispatch-failed", "resubmit destructive reset without a target generation", err)
		return errors.Join(err, markErr)
	}

	startedAt := e.clock()
	record, err := e.Store.Update(record.OperationID, "destructive-reset-start", "destructive-reset", func(record operation.OperationRecord) (operation.OperationRecord, error) {
		record.Phase = "destructive-reset"
		record.ExternalMutationStarted = true
		record.MutationScopes = appendMissing(record.MutationScopes, destructiveResetMutationScopes...)
		record.NextAction = "remove Katl disk boot artifacts so reinstall media or PXE is the next boot source"
		record.UpdatedAt = startedAt
		return record, nil
	})
	if err != nil {
		return err
	}

	if err := e.invalidateDestructiveResetDiskBoot(ctx); err != nil {
		_, markErr := e.failRecordPhase(record.OperationID, "destructive-reset-boot-invalidation-failed", "destructive-reset", "destructive-reset", "explicit repair required after disk boot invalidation failed", err)
		return errors.Join(err, markErr)
	}

	routingPaused, err := pauseManagedRoutingForPowerTransition(ctx, e.Root, e.endpointLifecycleRunner())
	if err != nil {
		_, markErr := e.failRecordPhase(record.OperationID, "destructive-reset-routing-pause-failed", "schedule-poweroff", "destructive-reset", "power off the node manually after repairing managed routing withdrawal", err)
		return errors.Join(err, markErr)
	}
	result := e.poweroffRunner()(ctx, destructiveResetPoweroffArgv, nil)
	if result.Err != nil || result.ExitStatus != 0 {
		var resumeErr error
		if routingPaused {
			resumeErr = resumeManagedRoutingAfterFailedPowerTransition(context.Background(), e.Root, e.endpointLifecycleRunner())
		}
		err := errors.Join(fmt.Errorf("schedule poweroff: %s", toolFailure(result)), resumeErr)
		_, markErr := e.failRecordPhase(record.OperationID, "destructive-reset-poweroff-schedule-failed", "schedule-poweroff", "destructive-reset", "power off the node manually; Katl disk boot artifacts have already been removed", err)
		return errors.Join(err, markErr)
	}

	completedAt := e.clock()
	_, err = e.Store.Update(record.OperationID, "destructive-reset-complete", "operation-complete", func(record operation.OperationRecord) (operation.OperationRecord, error) {
		record.Phase = operation.HostBookkeepingCompletionPhase
		record.CompletedPhases = appendMissing(record.CompletedPhases, "accepted", "preflight-destructive-reset", "destructive-reset", "schedule-poweroff", operation.HostBookkeepingCompletionPhase)
		record.PhaseIndex = len(record.CompletedPhases)
		record.MutatingToolRan = true
		record.MutatingToolInvocations = appendMissing(record.MutatingToolInvocations, "katlc destructive-reset", "systemd-run katl-destructive-reset-poweroff")
		record.CompletedAt = &completedAt
		record.Terminal = true
		record.Result = operation.ResultSucceeded
		record.NextAction = "node is powering off; select installer media or PXE, then start it to reinstall"
		record.UpdatedAt = completedAt
		return record, nil
	})
	return err
}

func (e *Executor) invalidateDestructiveResetDiskBoot(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	bootRoot := filepath.Join(runtimeRoot(e.Root), "efi")
	if runtimeRoot(e.Root) != "/" {
		return removeKatlBootArtifacts(bootRoot)
	}
	if e.MountBootRoot == nil {
		return fmt.Errorf("boot root mounter is required for live destructive reset")
	}
	if err := e.MountBootRoot(ctx, bootRoot); err != nil {
		return fmt.Errorf("mount boot root for disk boot invalidation: %w", err)
	}
	return removeKatlBootArtifacts(bootRoot)
}

func removeKatlBootArtifacts(bootRoot string) error {
	var joined error
	for _, pattern := range destructiveResetBootArtifactGlobs {
		matches, err := filepath.Glob(filepath.Join(bootRoot, filepath.FromSlash(pattern)))
		if err != nil {
			joined = errors.Join(joined, fmt.Errorf("scan ESP artifact pattern %s: %w", pattern, err))
			continue
		}
		for _, path := range matches {
			joined = errors.Join(joined, removeBootArtifact(path))
		}
	}
	for _, rel := range destructiveResetBootArtifactPaths {
		joined = errors.Join(joined, removeBootArtifact(filepath.Join(bootRoot, filepath.FromSlash(rel))))
	}
	return joined
}

func removeBootArtifact(path string) error {
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("remove ESP boot artifact %s: %w", path, err)
	}
	return nil
}
