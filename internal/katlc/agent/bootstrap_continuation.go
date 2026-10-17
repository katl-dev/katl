package agent

import (
	"fmt"
	"reflect"
	"slices"
	"time"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer"
	"github.com/katl-dev/katl/internal/installer/bootstrapplan"
	"github.com/katl-dev/katl/internal/installer/configapply"
	"github.com/katl-dev/katl/internal/installer/kubeadmconfig"
	"github.com/katl-dev/katl/internal/installer/operation"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Only a recorded successful kubeadm invocation permits skipping kubeadm.
// A partial or ambiguous invocation must remain an explicit repair boundary.
func bootstrapContinuationEligible(record operation.OperationRecord) bool {
	if record.BootstrapRequest == nil || record.ExecutorPlan == nil || !record.Terminal || record.Result != operation.ResultFailedNeedsRepair {
		return false
	}
	switch record.Phase {
	case "configure-local-api-access", "post-kubeadm-health":
	default:
		return false
	}
	return slices.Contains(record.CompletedPhases, record.ExecutorPlan.Phase) &&
		(record.OperationKind == "bootstrap-init" || bootstrapJoinOperation(record.OperationKind))
}

func (s *Server) acceptBootstrapContinuation(req *agentapi.SubmitOperationRequest, digest, id string, now time.Time) (operation.OperationRecord, *agentapi.OperationAccepted, error) {
	previous, err := s.Store.Read(req.GetBootstrap().GetResumeOperationId())
	if err != nil {
		return operation.OperationRecord{}, nil, status.Errorf(codes.FailedPrecondition, "read bootstrap operation to resume: %v", err)
	}
	if err := s.validateBootstrapContinuation(previous, req); err != nil {
		return operation.OperationRecord{}, nil, status.Errorf(codes.FailedPrecondition, "resume bootstrap: %v", err)
	}
	bootstrap := *previous.BootstrapRequest
	bootstrap.ResumeOperationID = previous.OperationID
	// Each attempt has its own immutable outcome, while reusing the prepared
	// candidate. No PKI, join material, or kubeadm initialization is recreated.
	record := operation.OperationRecord{
		OperationID: id, OperationKind: previous.OperationKind, Scope: previous.Scope,
		ClientRequestID: req.ClientRequestId, RequestDigest: digest, Actor: req.Actor,
		ExpectedMachineID:           previous.ExpectedMachineID,
		ExpectedCurrentGenerationID: previous.ExpectedCurrentGenerationID,
		ExpectedClusterIntentDigest: previous.ExpectedClusterIntentDigest,
		PreviousGenerationID:        previous.PreviousGenerationID, CandidateGenerationID: previous.CandidateGenerationID,
		Phase: "configure-local-api-access", PhasePlan: slices.Clone(previous.PhasePlan),
		CompletedPhases: slices.Clone(previous.CompletedPhases), PhaseIndex: len(previous.CompletedPhases),
		BootstrapRequest: &bootstrap, ExecutorPlan: previous.ExecutorPlan,
		ResourceLocks: resourceLocks(previous.OperationKind), HostRollback: previous.HostRollback,
		ActivationMode: previous.ActivationMode, ActivationState: previous.ActivationState,
		GenerationCommitState: previous.GenerationCommitState, PostKubeadmHealthState: operation.PostKubeadmHealthNotRun,
		ExternalMutationStarted: true, NextAction: "finish local API configuration and validate the initialized Kubernetes node",
	}
	created, err := s.Store.Create(record, "bootstrap-continuation-accepted", now)
	if err != nil {
		return operation.OperationRecord{}, nil, status.Errorf(codes.Internal, "create bootstrap continuation: %v", err)
	}
	return created, nil, nil
}

func (s *Server) validateBootstrapContinuation(previous operation.OperationRecord, req *agentapi.SubmitOperationRequest) error {
	if !bootstrapContinuationEligible(previous) || previous.OperationKind != req.OperationKind {
		return fmt.Errorf("operation has no safely resumable completed kubeadm phase")
	}
	if previous.ExpectedMachineID != req.ExpectedMachineId || previous.ExpectedCurrentGenerationID != req.ExpectedCurrentGenerationId || previous.ExpectedClusterIntentDigest != req.ExpectedClusterIntentDigest {
		return fmt.Errorf("node identity or generation changed since the failed bootstrap")
	}
	got := bootstrapRequestFromProto(req.Bootstrap)
	got.KubernetesIdentityFingerprint = req.Bootstrap.GetKubernetesIdentityFingerprint()
	want := *previous.BootstrapRequest
	installed, _, err := installer.ReadClusterIntent(s.Root)
	if err != nil {
		return fmt.Errorf("read installed bootstrap identity: %w", err)
	}
	selected, err := bootstrapplan.SelectInput(s.Root, previous.ExpectedCurrentGenerationID, installed, want)
	if err != nil {
		return fmt.Errorf("validate recorded bootstrap snapshot: %w", err)
	}
	if want.KubeadmInputDigest == "" {
		candidate, err := configapply.ReadLocalGenerationConfiguration(s.Root, previous.CandidateGenerationID)
		if err != nil {
			return fmt.Errorf("failed bootstrap has no pinned input and its local candidate snapshot is unavailable; preserve node state for repair: %w", err)
		}
		ref := candidate.Manifest.Node.Kubernetes.Kubeadm.ConfigRef
		input, ok := candidate.KubeadmConfigs[ref]
		if !ok || ref != selected.Input.ConfigRef || input.Config.RenderPath != selected.Input.Path {
			return fmt.Errorf("failed bootstrap candidate does not own the recorded source input; preserve node state for repair")
		}
		input, err = kubeadmconfig.WithNodeAddress(input, candidate.Manifest.Node.Kubernetes.Address)
		if err != nil {
			return fmt.Errorf("normalize failed bootstrap candidate input: %w", err)
		}
		digest, err := input.InputDigest()
		if err != nil || digest != selected.Input.Digest {
			return fmt.Errorf("failed bootstrap candidate differs from the recorded source input; preserve node state for repair")
		}
		// This proof permits health-only continuation; it never authorizes a
		// new native invocation or changes the original unpinned receipt.
		want.KubeadmInputDigest = digest
	}
	if got.KubeadmInputDigest == "" {
		got.KubeadmInputDigest = want.KubeadmInputDigest
	}
	for _, request := range []*operation.BootstrapRequest{&got, &want} {
		request.ResumeOperationID = ""
		request.CandidateGenerationID = ""
		request.KubernetesBundleManifestDigest = ""
		request.KubernetesSysextPayloadDigest = ""
		request.KubernetesIdentityCluster = ""
		request.KubernetesIdentityDigest = ""
		request.JoinMaterialRef = ""
		request.JoinMaterialDigest = ""
		request.JoinMaterialExpiresAt = ""
		request.TemporaryJoinConfigPath = ""
	}
	if !reflect.DeepEqual(got, want) {
		return fmt.Errorf("bootstrap configuration or Kubernetes identity differs from the failed operation; use the original configuration and identity")
	}
	// Follow continuation receipts back to the actual kubeadm invocation.
	original := previous
	seen := map[string]bool{}
	for original.BootstrapRequest != nil && original.BootstrapRequest.ResumeOperationID != "" {
		if original.CandidateGenerationID != previous.CandidateGenerationID || original.ExpectedCurrentGenerationID != previous.ExpectedCurrentGenerationID {
			return fmt.Errorf("bootstrap continuation history changes candidate or source generation")
		}
		if seen[original.OperationID] {
			return fmt.Errorf("bootstrap continuation history contains a cycle")
		}
		seen[original.OperationID] = true
		var err error
		original, err = s.Store.Read(original.BootstrapRequest.ResumeOperationID)
		if err != nil {
			return fmt.Errorf("read original kubeadm receipt: %w", err)
		}
	}
	seen[original.OperationID] = true
	if original.CandidateGenerationID != previous.CandidateGenerationID || original.ExpectedCurrentGenerationID != previous.ExpectedCurrentGenerationID {
		return fmt.Errorf("original kubeadm receipt belongs to a different candidate or source generation")
	}
	if original.BootstrapRequest == nil || (original.BootstrapRequest.KubeadmInputDigest != "" && original.BootstrapRequest.KubeadmInputDigest != selected.Input.Digest) {
		return fmt.Errorf("original kubeadm receipt belongs to a different input snapshot")
	}
	proved := false
	if original.ExecutorPlan == nil || original.OperationKind != previous.OperationKind {
		return fmt.Errorf("original kubeadm receipt is invalid")
	}
	for _, invocation := range original.Invocations {
		if invocation.InvocationID == original.ExecutorPlan.MarkerID && invocation.Result == "exit-0" && invocation.CompletedAt != nil && invocation.BootID == currentBootID() {
			proved = true
		}
	}
	if !proved {
		return fmt.Errorf("successful kubeadm execution from this boot is not recorded; preserve node state for repair")
	}
	_, candidate, err := generation.ReadGeneration(s.Root, previous.CandidateGenerationID)
	if err != nil || candidate.CommitState != generation.CommitStateCandidate {
		return fmt.Errorf("prepared bootstrap generation is unavailable or already committed; inspect generation state before repair")
	}
	ids, err := s.Store.OperationIDs()
	if err != nil {
		return err
	}
	for _, otherID := range ids {
		other, err := s.Store.Read(otherID)
		if err != nil {
			return err
		}
		if !seen[other.OperationID] && !other.CreatedAt.Before(original.CreatedAt) && other.ExternalMutationStarted {
			return fmt.Errorf("operation %s changed node state after the failed bootstrap; inspect it before repair", other.OperationID)
		}
	}
	return nil
}
