package agent

import (
	"fmt"
	"strings"
	"time"

	"github.com/zariel/katl/internal/installer/operation"
	agentapi "github.com/zariel/katl/internal/katlc/agentapi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const OperationKindHostUpgrade = "host-upgrade"

func hostUpgradeFromProto(req *agentapi.HostUpgradeOperationRequest) operation.HostUpgrade {
	if req == nil {
		return operation.HostUpgrade{}
	}
	return operation.HostUpgrade{
		ImageURL:              strings.TrimSpace(req.ImageUrl),
		ImageLocalRef:         strings.TrimSpace(req.ImageLocalRef),
		ImageSHA256:           strings.TrimSpace(req.ImageSha256),
		ImageSizeBytes:        req.ImageSizeBytes,
		CandidateGenerationID: strings.TrimSpace(req.CandidateGenerationId),
	}
}

func validateHostUpgradeRequest(kind string, req *agentapi.HostUpgradeOperationRequest) error {
	if kind != OperationKindHostUpgrade {
		return fmt.Errorf("operationKind %q does not accept hostUpgrade", kind)
	}
	return operation.ValidateHostUpgrade(hostUpgradeFromProto(req))
}

func (s *Server) acceptHostUpgradeOperation(req *agentapi.SubmitOperationRequest, digest, id string, locks []string, now time.Time) (operation.OperationRecord, *agentapi.OperationAccepted, error) {
	request := hostUpgradeFromProto(req.GetHostUpgrade())
	if err := s.validateCandidateGenerationAvailable(request.CandidateGenerationID); err != nil {
		return operation.OperationRecord{}, nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	record := operation.OperationRecord{
		OperationID:                 id,
		OperationKind:               OperationKindHostUpgrade,
		Scope:                       "host-generation",
		ClientRequestID:             req.ClientRequestId,
		Actor:                       req.Actor,
		ExpectedMachineID:           req.ExpectedMachineId,
		ExpectedCurrentGenerationID: req.ExpectedCurrentGenerationId,
		RequestDigest:               digest,
		Phase:                       "accepted",
		PhasePlan:                   []string{"accepted", "verify-katlos-image", "stage-sysupdate-components", "write-candidate-generation", "arm-trial-boot"},
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
