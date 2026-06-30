package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zariel/katl/internal/bootstrap/inventory"
	"github.com/zariel/katl/internal/installer/generation"
	"github.com/zariel/katl/internal/installer/operation"
	installstatus "github.com/zariel/katl/internal/installer/status"
)

var destructiveResetMutationScopes = []string{
	"katlos-target-partitions",
	"kubernetes",
	"kubelet-state",
	"etcd-state",
	"cni-state",
	"container-runtime-state",
	"operation-history",
	"generation-state",
	"node-identity",
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
	if targetGeneration == "" {
		targetGeneration = "0"
	}
	if targetGeneration != "0" {
		err := fmt.Errorf("destructive reset target generation %q is unsupported", targetGeneration)
		_, markErr := e.failRecordPhase(record.OperationID, "destructive-reset-target-invalid", "destructive-reset-invalid", "dispatch-failed", "resubmit destructive reset with target generation 0", err)
		return errors.Join(err, markErr)
	}

	startedAt := e.clock()
	record, err := e.Store.Update(record.OperationID, "destructive-reset-start", "destructive-reset", func(record operation.OperationRecord) (operation.OperationRecord, error) {
		record.Phase = "destructive-reset"
		record.ExternalMutationStarted = true
		record.MutationScopes = appendMissing(record.MutationScopes, destructiveResetMutationScopes...)
		record.NextAction = "clean local Kubernetes, generation, operation, and identity state"
		record.UpdatedAt = startedAt
		return record, nil
	})
	if err != nil {
		return err
	}

	if err := e.quiesceDestructiveReset(ctx); err != nil {
		_, markErr := e.failRecordPhase(record.OperationID, "destructive-reset-quiesce-failed", "destructive-reset", "destructive-reset", "explicit repair required after reset service quiesce failed", err)
		return errors.Join(err, markErr)
	}
	if err := e.cleanDestructiveResetState(record.OperationID, targetGeneration); err != nil {
		_, markErr := e.failRecordPhase(record.OperationID, "destructive-reset-clean-failed", "destructive-reset", "destructive-reset", "explicit repair required after destructive reset cleanup failed", err)
		return errors.Join(err, markErr)
	}
	if err := e.selectDestructiveResetGeneration(ctx, targetGeneration, record.OperationID); err != nil {
		_, markErr := e.failRecordPhase(record.OperationID, "destructive-reset-generation-failed", "destructive-reset", "destructive-reset", "explicit repair required after generation 0 selection failed", err)
		return errors.Join(err, markErr)
	}
	if err := installstatus.ValidateCleanGenerationZeroForOperation(e.Root, targetGeneration, record.OperationID); err != nil {
		_, markErr := e.failRecordPhase(record.OperationID, "destructive-reset-clean-generation-zero-failed", "destructive-reset", "destructive-reset", "explicit repair required after clean generation 0 validation failed", err)
		return errors.Join(err, markErr)
	}

	completedAt := e.clock()
	_, err = e.Store.Update(record.OperationID, "destructive-reset-complete", "operation-complete", func(record operation.OperationRecord) (operation.OperationRecord, error) {
		record.Phase = operation.HostBookkeepingCompletionPhase
		record.CompletedPhases = appendMissing(record.CompletedPhases, "accepted", "preflight-destructive-reset", "destructive-reset", operation.HostBookkeepingCompletionPhase)
		record.PhaseIndex = len(record.CompletedPhases)
		record.MutatingToolRan = true
		record.MutatingToolInvocations = appendMissing(record.MutatingToolInvocations, "katlc destructive-reset")
		record.CompletedAt = &completedAt
		record.Terminal = true
		record.Result = operation.ResultSucceeded
		record.NextAction = "node is clean generation 0 and ready for fresh cluster bootstrap"
		record.UpdatedAt = completedAt
		return record, nil
	})
	return err
}

func (e *Executor) quiesceDestructiveReset(ctx context.Context) error {
	if runtimeRoot(e.Root) != "/" {
		return nil
	}
	commands := [][]string{
		{"/usr/bin/systemctl", "stop", "kubelet.service", "containerd.service"},
		{"/usr/bin/systemctl", "stop", "etc-kubernetes.mount"},
	}
	for _, argv := range commands {
		result := e.toolRunner()(ctx, argv, nil)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		_ = result
	}
	return nil
}

func (e *Executor) cleanDestructiveResetState(operationID string, targetGeneration string) error {
	cleaners := []func() error{
		func() error { return removeChildren(e.Root, generation.KubernetesSource) },
		func() error { return removeChildren(e.Root, generation.KubernetesTarget) },
		func() error { return removeRooted(e.Root, "/var/lib/kubelet") },
		func() error { return removeRooted(e.Root, "/var/lib/etcd") },
		func() error { return removeRooted(e.Root, "/var/lib/cni") },
		func() error { return removeRooted(e.Root, "/var/lib/containerd") },
		func() error { return removeRooted(e.Root, "/etc/cni/net.d") },
		func() error { return removeRooted(e.Root, "/run/flannel") },
		func() error { return removeRooted(e.Root, "/run/cilium") },
		func() error { return removeRooted(e.Root, "/run/calico") },
		func() error { return cleanGenerationHistory(e.Root, targetGeneration) },
		func() error { return cleanOperationHistory(e.Store, operationID) },
		func() error { return removeRooted(e.Root, "/var/lib/katl/identity") },
	}
	var joined error
	for _, clean := range cleaners {
		joined = errors.Join(joined, clean())
	}
	if err := mkdirRooted(e.Root, generation.KubernetesSource, 0o755); err != nil {
		joined = errors.Join(joined, err)
	}
	if err := mkdirRooted(e.Root, generation.KubernetesTarget, 0o755); err != nil {
		joined = errors.Join(joined, err)
	}
	return joined
}

func (e *Executor) selectDestructiveResetGeneration(ctx context.Context, generationID string, operationID string) error {
	spec, status, err := generation.ReadGeneration(e.Root, generationID)
	if err != nil {
		return fmt.Errorf("read reset target generation: %w", err)
	}
	record := generation.RecordFromSplit(spec, status)
	if _, err := generation.ApplyActivation(e.Root, record); err != nil {
		return fmt.Errorf("activate reset target generation: %w", err)
	}
	entry := strings.TrimSpace(spec.Boot.LoaderEntryPath)
	if entry == "" {
		entry = "loader/entries/katl-" + generationID + ".conf"
	}
	now := e.clock()
	selection := generation.BootSelectionRecord{
		APIVersion:                    generation.APIVersion,
		Kind:                          generation.BootSelectionKind,
		DefaultGenerationID:           generationID,
		BootedGenerationID:            generationID,
		Generation0FallbackID:         generationID,
		PreviousKnownGoodGenerationID: generationID,
		DefaultBootEntry:              entry,
		BootedBootEntry:               entry,
		PreviousKnownGoodBootEntry:    entry,
		PendingTransactionID:          operationID,
		UpdatedAt:                     now,
	}
	if err := generation.WriteBootSelection(e.Root, selection); err != nil {
		return fmt.Errorf("write reset boot selection: %w", err)
	}
	if e.SetBootOneshot != nil && entry != "" {
		if err := e.SetBootOneshot(ctx, e.Root, entry); err != nil {
			return fmt.Errorf("set reset target boot entry: %w", err)
		}
	}
	if err := e.refreshDestructiveResetActivation(ctx); err != nil {
		return err
	}
	return nil
}

func (e *Executor) refreshDestructiveResetActivation(ctx context.Context) error {
	if runtimeRoot(e.Root) != "/" {
		return nil
	}
	for _, argv := range [][]string{
		{"/usr/bin/systemctl", "daemon-reload"},
		{"/usr/bin/systemd-sysext", "refresh"},
		{"/usr/bin/systemd-confext", "refresh"},
	} {
		result := e.toolRunner()(ctx, argv, nil)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if result.Err != nil || result.ExitStatus != 0 {
			return fmt.Errorf("%s: %s", strings.Join(argv, " "), inventory.Redact(toolFailure(result)))
		}
	}
	return nil
}

func cleanGenerationHistory(root string, keepGeneration string) error {
	dir, err := rootedPathForReset(root, generation.GenerationRecordsDir)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read generation history: %w", err)
	}
	var joined error
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == keepGeneration {
			continue
		}
		joined = errors.Join(joined, os.RemoveAll(filepath.Join(dir, entry.Name())))
	}
	return joined
}

func cleanOperationHistory(store operation.Store, keepOperation string) error {
	ids, err := store.OperationIDs()
	if err != nil {
		return err
	}
	var joined error
	for _, id := range ids {
		if id == keepOperation {
			continue
		}
		joined = errors.Join(joined, os.RemoveAll(filepath.Join(filepath.Clean(store.Root), id)))
	}
	return joined
}

func removeRooted(root string, absolutePath string) error {
	path, err := rootedPathForReset(root, absolutePath)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove %s: %w", absolutePath, err)
	}
	return nil
}

func removeChildren(root string, absolutePath string) error {
	path, err := rootedPathForReset(root, absolutePath)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read %s: %w", absolutePath, err)
	}
	var joined error
	for _, entry := range entries {
		joined = errors.Join(joined, os.RemoveAll(filepath.Join(path, entry.Name())))
	}
	return joined
}

func mkdirRooted(root string, absolutePath string, mode os.FileMode) error {
	path, err := rootedPathForReset(root, absolutePath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(path, mode); err != nil {
		return fmt.Errorf("create %s: %w", absolutePath, err)
	}
	return nil
}

func rootedPathForReset(root string, absolutePath string) (string, error) {
	if strings.TrimSpace(absolutePath) == "" || !filepath.IsAbs(absolutePath) {
		return "", fmt.Errorf("reset path must be absolute: %q", absolutePath)
	}
	cleaned := filepath.Clean(absolutePath)
	if cleaned == string(filepath.Separator) {
		return "", fmt.Errorf("reset path must not be root")
	}
	root = strings.TrimSpace(root)
	if root == "" {
		root = "/"
	}
	return filepath.Join(filepath.Clean(root), strings.TrimPrefix(cleaned, string(filepath.Separator))), nil
}
