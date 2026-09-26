package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/katlosimage"
	"github.com/katl-dev/katl/internal/installer/operation"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	OperationKindHostUpgrade        = "host-upgrade"
	operationKindHostUpgradeV2      = "host-upgrade-v2"
	operationKindHostUpgradeHandoff = "host-upgrade-handoff"
)

// The beta.16 client used this kind on SubmitOperation. Retain it while those
// clients and nodes remain supported; new clients select an advertised kind.
const hostUpgradeRequestKind = "HostUpgradeRequestV2"

func hostUpgradeFromProto(req *agentapi.HostUpgradeOperationRequest) operation.HostUpgrade {
	if req == nil {
		return operation.HostUpgrade{}
	}
	return operation.HostUpgrade{
		ConfigYAML:            strings.TrimSpace(req.ConfigYaml),
		ImageURL:              strings.TrimSpace(req.ImageUrl),
		ImageLocalRef:         strings.TrimSpace(req.ImageLocalRef),
		ImageSHA256:           strings.TrimSpace(req.ImageSha256),
		ImageSizeBytes:        req.ImageSizeBytes,
		CandidateGenerationID: strings.TrimSpace(req.CandidateGenerationId),
	}
}

func validateHostUpgradeRequest(kind string, req *agentapi.HostUpgradeOperationRequest) error {
	if kind != OperationKindHostUpgrade && kind != operationKindHostUpgradeV2 && kind != operationKindHostUpgradeHandoff {
		return fmt.Errorf("operationKind %q does not accept hostUpgrade", kind)
	}
	return operation.ValidateHostUpgrade(hostUpgradeFromProto(req))
}

func (s *Server) validateHostUpgradePlan(req *agentapi.HostUpgradeOperationRequest) error {
	request := hostUpgradeFromProto(req)
	if err := s.validateCandidateGenerationAvailable(request.CandidateGenerationID); err != nil {
		return err
	}
	currentID, err := currentGenerationID(s.Root)
	if err != nil {
		return fmt.Errorf("read current generation: %w", err)
	}
	previousSpec, previousStatus, err := generation.ReadGeneration(s.Root, currentID)
	if err != nil {
		return fmt.Errorf("read current generation %q: %w", currentID, err)
	}
	if err := validateHostUpgradeBootEvidence(s.Root, currentID, previousSpec); err != nil {
		return err
	}
	kubernetesState, err := s.kubernetesNodeState()
	if err != nil {
		return fmt.Errorf("inspect Kubernetes node state: %w", err)
	}
	if err := katlosimage.ValidateHostUpgradeSource(previousSpec, previousStatus, kubernetesState.bootstrapped); err != nil {
		return fmt.Errorf("%w; inspect the current generation, then recover it or wipe and reinstall this node before retrying the upgrade", err)
	}
	return nil
}

func validateHostUpgradeBootEvidence(root, currentID string, current generation.GenerationSpec) error {
	data, err := os.ReadFile(filepath.Join(runtimeRoot(root), "proc/cmdline"))
	if err != nil {
		return fmt.Errorf("read running kernel command line: %w", err)
	}
	commandLine := string(data)
	bootedID, err := generation.SelectedGenerationFromCommandLine(commandLine)
	if err != nil {
		return fmt.Errorf("read running generation from kernel command line: %w", err)
	}
	if bootedID != currentID {
		health := readNodeBootHealth(root)
		if health.State != nodeBootHealthHealthy {
			return fmt.Errorf("running kernel selected generation %q is not healthy: %s; reboot into the selected known-good generation before retrying the upgrade", bootedID, health.Diagnostic)
		}
		booted, _, err := generation.ReadGeneration(root, bootedID)
		if err != nil {
			return fmt.Errorf("read booted generation: %w", err)
		}
		// Live promotion changes configuration identity, not the booted OS payload.
		if booted.Root != current.Root || booted.Boot.UKIPath != current.Boot.UKIPath {
			return fmt.Errorf("active generation %q does not use the booted runtime from %q; reboot into the selected known-good generation before retrying the upgrade", currentID, bootedID)
		}
	}
	rootPartUUID, err := generation.SelectedRootPartUUIDFromCommandLine(commandLine)
	if err != nil {
		return fmt.Errorf("read running root from kernel command line: %w", err)
	}
	if !strings.EqualFold(rootPartUUID, strings.TrimSpace(current.Root.PartitionUUID)) {
		return fmt.Errorf("running root PARTUUID %q does not match current generation %q root PARTUUID %q; reboot into the selected known-good generation before retrying the upgrade", rootPartUUID, currentID, current.Root.PartitionUUID)
	}
	return nil
}

func (s *Server) acceptHostUpgradeOperation(req *agentapi.SubmitOperationRequest, digest, id string, locks []string, now time.Time) (operation.OperationRecord, *agentapi.OperationAccepted, error) {
	request := hostUpgradeFromProto(req.GetHostUpgrade())
	if err := s.validateHostUpgradePlan(req.GetHostUpgrade()); err != nil {
		return operation.OperationRecord{}, nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	kind := OperationKindHostUpgrade
	phases := []string{"accepted", "verify-katlos-image", "stage-sysupdate-components", "write-candidate-generation", "arm-trial-boot"}
	if req.OperationKind == operationKindHostUpgradeHandoff {
		kind = operationKindHostUpgradeHandoff
		phases = []string{"accepted", "verify-katlos-image", "stage-sysupdate-components", "write-candidate-generation", "arm-trial-boot"}
	}
	record := operation.OperationRecord{
		OperationID:                 id,
		OperationKind:               kind,
		Scope:                       "host-generation",
		ClientRequestID:             req.ClientRequestId,
		Actor:                       req.Actor,
		ExpectedMachineID:           req.ExpectedMachineId,
		ExpectedCurrentGenerationID: req.ExpectedCurrentGenerationId,
		RequestDigest:               digest,
		Phase:                       "accepted",
		PhasePlan:                   phases,
		CandidateGenerationID:       request.CandidateGenerationID,
		HostUpgradeRequest:          &request,
		ActivationMode:              operation.ActivationModeNextBoot,
		ActivationState:             operation.ActivationStatePending,
		GenerationCommitState:       operation.GenerationCommitCandidate,
		BootHealthPending:           true,
		ResourceLocks:               locks,
		NextAction:                  "queued for KatlOS image verification and sysupdate staging",
	}
	created, err := s.Store.Create(record, "accepted", now)
	if err != nil {
		return operation.OperationRecord{}, nil, status.Errorf(codes.Internal, "create operation record: %v", err)
	}
	return created, nil, nil
}
