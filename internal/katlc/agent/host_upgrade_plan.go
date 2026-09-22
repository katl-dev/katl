package agent

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/confext"
	"github.com/katl-dev/katl/internal/installer/configapply"
	"github.com/katl-dev/katl/internal/installer/katlosimage"
	"github.com/katl-dev/katl/internal/installer/operation"
)

type preparedHostUpgrade struct {
	plan       katlosimage.HostUpgradePlan
	extensions hostExtensionPlan
	slots      rootSlots
	previous   generation.GenerationSpec
	workDir    string
	domains    []string
	indexes    configapply.PreparedModuleIndexes
}

func (p preparedHostUpgrade) close() {
	_ = p.indexes.Close()
	if p.workDir != "" {
		_ = os.RemoveAll(p.workDir)
	}
}

// Preparation acquires and validates the complete future selection without
// writing an inactive slot, changing live configuration, or selecting a boot.
func (e *Executor) planHostUpgrade(ctx context.Context, record operation.OperationRecord, payload katlosimage.Payload) (preparedHostUpgrade, error) {
	if err := generation.ValidateMutationBase(e.Root, record.ExpectedCurrentGenerationID); err != nil {
		return preparedHostUpgrade{}, err
	}
	currentID, err := currentGenerationID(e.Root)
	if err != nil {
		return preparedHostUpgrade{}, err
	}
	previous, previousStatus, err := generation.ReadGeneration(e.Root, currentID)
	if err != nil {
		return preparedHostUpgrade{}, fmt.Errorf("read current generation: %w", err)
	}
	current, _, err := configapply.ReadEffectiveGenerationManifest(e.Root, currentID)
	if err != nil {
		return preparedHostUpgrade{}, fmt.Errorf("read current generation configuration: %w", err)
	}
	if err := validateHostUpgradeBootEvidence(e.Root, currentID, previous); err != nil {
		return preparedHostUpgrade{}, err
	}
	inactiveSlot, err := inactiveRoot(previous.Root.Slot)
	if err != nil {
		return preparedHostUpgrade{}, err
	}
	slots, err := e.inspectRootSlots(ctx, previous.Root.PartitionUUID)
	if err != nil {
		return preparedHostUpgrade{}, err
	}
	kubernetesState, err := inspectKubernetesNodeState(e.Root, e.Store)
	if err != nil {
		return preparedHostUpgrade{}, fmt.Errorf("inspect Kubernetes node state: %w", err)
	}
	candidate := record.HostUpgradeRequest.CandidateGenerationID
	var extensions hostExtensionPlan
	var files []confext.NativeEtcFile
	var domains []string
	if record.HostUpgradeRequest.ConfigYAML != "" {
		extensions, files, domains, err = e.planUpgradeConfig(ctx, candidate, record.HostUpgradeRequest.ConfigYAML, payload)
	}
	if record.HostUpgradeRequest.ConfigYAML == "" || errors.Is(err, configapply.ErrNoChanges) {
		document := extensions.document
		extensions, err = planHostExtensions(ctx, current, payload, candidate, e.ResolveSystemExtension)
		extensions.document = document
	}
	if err != nil {
		return preparedHostUpgrade{}, err
	}
	ukiPath := generation.UKIDirectory + "/katl-" + inactiveSlot + "-1.efi"
	if ukiPath == previous.Boot.UKIPath {
		return preparedHostUpgrade{}, fmt.Errorf("inactive root slot UKI path is still used by the active generation")
	}
	plan, err := payload.HostUpgradePlan(katlosimage.HostUpgradeRequest{
		ReplaceExtensions: extensions.replace,
		Sysexts:           extensions.sysexts,
		BundledConfexts:   extensions.confexts,
		GenerationID:      candidate,
		PreviousSpec:      previous,
		PreviousStatus:    previousStatus,
		RootSlot:          inactiveSlot,
		RootPartitionUUID: slots.InactivePartUUID,
		UKIPath:           ukiPath,
		LoaderEntryPath:   "loader/entries/katl-" + candidate + ".conf",
		OperationID:       record.OperationID,
		Bootstrapped:      kubernetesState.bootstrapped,
		CreatedAt:         e.clock(),
	})
	if err != nil {
		return preparedHostUpgrade{}, err
	}
	work := ""
	if files != nil {
		work, err = e.prepareUpgradeConfig(&plan, extensions.manifest, files)
		if err != nil {
			return preparedHostUpgrade{}, err
		}
	}
	if err := katlosimage.ValidateUpgradeAssets(e.Root, plan); err != nil {
		if work != "" {
			_ = os.RemoveAll(work)
		}
		return preparedHostUpgrade{}, err
	}
	indexes, err := e.prepareHostModuleIndexes(ctx, payload, &plan, extensions)
	if err != nil {
		if work != "" {
			_ = os.RemoveAll(work)
		}
		return preparedHostUpgrade{}, err
	}
	return preparedHostUpgrade{
		indexes:    indexes,
		workDir:    work,
		domains:    domains,
		plan:       plan,
		extensions: extensions,
		slots:      slots,
		previous:   previous,
	}, nil
}
