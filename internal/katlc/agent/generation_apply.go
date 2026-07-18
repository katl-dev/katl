package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/installer"
	"github.com/katl-dev/katl/internal/installer/configapply"
	"github.com/katl-dev/katl/internal/installer/generation"
	"github.com/katl-dev/katl/internal/installer/kubeadmconfig"
	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/installer/operation"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	katlconfig "github.com/katl-dev/katl/internal/katlc/config"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	ConfigApplyRequestKind = "ConfigApplyRequest"

	OperationKindGenerationApply = "generation-apply"
	OperationKindGenerationStage = "generation-stage"
)

func (s *Server) ValidateConfig(ctx context.Context, req *agentapi.ValidateConfigRequest) (*agentapi.ConfigValidationResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	if req.ApiVersion != APIVersion {
		return nil, status.Errorf(codes.InvalidArgument, "apiVersion must be %q", APIVersion)
	}
	if req.Kind != "ValidateConfigRequest" {
		return nil, status.Errorf(codes.InvalidArgument, "kind must be %q", "ValidateConfigRequest")
	}
	if strings.TrimSpace(req.Actor) == "" {
		return nil, status.Error(codes.InvalidArgument, "actor is required")
	}
	applyMode := strings.TrimSpace(req.ApplyMode)
	if applyMode == "" {
		applyMode = generation.ApplyModeAuto
	}
	if err := validateApplyMode(applyMode); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if strings.TrimSpace(req.ClientRequestId) == "" {
		return nil, status.Error(codes.InvalidArgument, "clientRequestID is required")
	}
	candidateID := strings.TrimSpace(req.CandidateGenerationId)
	if candidateID == "" {
		return nil, status.Error(codes.InvalidArgument, "candidateGenerationID is required for validation")
	}
	if err := cleanPublicID("candidateGenerationID", candidateID); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if strings.TrimSpace(req.ConfigYaml) == "" {
		return nil, status.Error(codes.InvalidArgument, "configYAML is required")
	}
	provisionalOperationKind := OperationKindGenerationStage
	if applyMode == generation.ApplyModeLive {
		provisionalOperationKind = OperationKindGenerationApply
	}
	submitBase := &agentapi.GenerationApplyRequest{
		ApiVersion:                  APIVersion,
		Kind:                        "GenerationApplyRequest",
		ClientRequestId:             req.ClientRequestId,
		Actor:                       req.Actor,
		ExpectedMachineId:           req.ExpectedMachineId,
		ExpectedCurrentGenerationId: req.ExpectedCurrentGenerationId,
		RequestDigest:               "",
		OperationTimeout:            req.OperationTimeout,
		CandidateGenerationId:       candidateID,
		NodeName:                    req.NodeName,
		ConfigYaml:                  req.ConfigYaml,
	}
	submit := generationSubmitRequest(submitBase, provisionalOperationKind, applyMode)
	requestDigest, err := RequestDigest(submit)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "request digest: %v", err)
	}
	if applyMode != generation.ApplyModeAuto {
		if err := s.validateSubmit(submit); err != nil {
			return nil, err
		}
	}
	rejected := func(err error, diagnostics []string) *agentapi.ConfigValidationResult {
		return &agentapi.ConfigValidationResult{
			ApiVersion:            APIVersion,
			Kind:                  "ConfigValidationResult",
			Accepted:              false,
			RequestDigest:         requestDigest,
			RequestedApplyMode:    applyMode,
			CandidateGenerationId: candidateID,
			Diagnostics:           diagnostics,
			FailureReason:         inventory.Redact(err.Error()),
		}
	}
	if err := s.validateCandidateGenerationAvailable(candidateID); err != nil {
		return rejected(err, nil), nil
	}
	base, err := s.configApplyBase(req.NodeName, candidateID)
	if err != nil {
		return rejected(err, nil), nil
	}
	configDiagnostics := validateConfigApplyDocument(req.ConfigYaml, base.KubeadmConfigs)
	if !configDiagnostics.Accepted() {
		diagnostics := configDiagnostics.Strings()
		return rejected(configValidationError(diagnostics), diagnostics), nil
	}
	decoded, err := configapply.DecodeNodeConfigurationChange(strings.NewReader(req.ConfigYaml), base)
	if err != nil {
		return rejected(err, nil), nil
	}
	decoded.ApplyMode = applyMode
	decoded.GenerationID = candidateID
	plan, err := configapply.PlanTrustedBundle(decoded)
	if err != nil {
		if errors.Is(err, configapply.ErrNoChanges) {
			return &agentapi.ConfigValidationResult{
				ApiVersion:            APIVersion,
				Kind:                  "ConfigValidationResult",
				Accepted:              true,
				RequestDigest:         requestDigest,
				RequestedApplyMode:    applyMode,
				CandidateGenerationId: candidateID,
				NoChanges:             true,
			}, nil
		}
		return rejected(err, configApplyDiagnostics(plan.Plan.Decision)), nil
	}
	operationKind, err := configApplyOperationKind(plan.Plan.Decision.AcceptedMode)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	submit = generationSubmitRequest(submitBase, operationKind, applyMode)
	if err := s.validateSubmit(submit); err != nil {
		return nil, err
	}
	requestDigest, err = RequestDigest(submit)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "request digest: %v", err)
	}
	return &agentapi.ConfigValidationResult{
		ApiVersion:            APIVersion,
		Kind:                  "ConfigValidationResult",
		Accepted:              true,
		RequestDigest:         requestDigest,
		RequestedApplyMode:    applyMode,
		AcceptedApplyMode:     plan.Plan.Decision.AcceptedMode,
		CandidateGenerationId: candidateID,
		ChangedDomains:        append([]string(nil), plan.Plan.Decision.ChangedDomains...),
	}, nil
}

func kubeadmConfigNames(configs map[string]kubeadmconfig.Plan) map[string]struct{} {
	if len(configs) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(configs))
	for name := range configs {
		out[name] = struct{}{}
	}
	return out
}

func configValidationError(diagnostics []string) error {
	if len(diagnostics) == 0 {
		return fmt.Errorf("config validation rejected")
	}
	return fmt.Errorf("config validation rejected: %s", diagnostics[0])
}

func (s *Server) ApplyGeneration(ctx context.Context, req *agentapi.GenerationApplyRequest) (*agentapi.OperationAccepted, error) {
	return s.submitGenerationOperation(ctx, req, OperationKindGenerationApply, generation.ApplyModeLive)
}

func (s *Server) StageGeneration(ctx context.Context, req *agentapi.GenerationApplyRequest) (*agentapi.OperationAccepted, error) {
	return s.submitGenerationOperation(ctx, req, OperationKindGenerationStage, generation.ApplyModeNextBoot)
}

func (s *Server) submitGenerationOperation(ctx context.Context, req *agentapi.GenerationApplyRequest, operationKind string, applyMode string) (*agentapi.OperationAccepted, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	if req.ApiVersion != APIVersion {
		return nil, status.Errorf(codes.InvalidArgument, "apiVersion must be %q", APIVersion)
	}
	if req.Kind != "GenerationApplyRequest" {
		return nil, status.Errorf(codes.InvalidArgument, "kind must be %q", "GenerationApplyRequest")
	}
	submit := generationSubmitRequest(req, operationKind, applyMode)
	return s.SubmitOperation(ctx, submit)
}

func generationSubmitRequest(req *agentapi.GenerationApplyRequest, operationKind string, applyMode string) *agentapi.SubmitOperationRequest {
	return &agentapi.SubmitOperationRequest{
		ApiVersion:                  APIVersion,
		Kind:                        RequestKind,
		ClientRequestId:             req.ClientRequestId,
		OperationKind:               operationKind,
		Actor:                       req.Actor,
		ExpectedMachineId:           req.ExpectedMachineId,
		ExpectedCurrentGenerationId: req.ExpectedCurrentGenerationId,
		RequestDigest:               req.RequestDigest,
		OperationTimeout:            req.OperationTimeout,
		ConfigApply: &agentapi.ConfigApplyOperationRequest{
			CandidateGenerationId: req.CandidateGenerationId,
			ApplyMode:             applyMode,
			NodeName:              req.NodeName,
			ConfigYaml:            req.ConfigYaml,
		},
	}
}

func (s *Server) ListGenerations(ctx context.Context, req *agentapi.ListGenerationsRequest) (*agentapi.ListGenerationsResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	includeConfigApply := req != nil && req.IncludeConfigApply
	root := strings.TrimSpace(s.Root)
	if root == "" {
		root = "/"
	}
	dir := filepath.Join(filepath.Clean(root), strings.TrimPrefix(generation.GenerationRecordsDir, "/"))
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return &agentapi.ListGenerationsResponse{}, nil
		}
		return nil, status.Errorf(codes.Internal, "list generations: %v", err)
	}
	var ids []string
	for _, entry := range entries {
		if entry.IsDir() {
			ids = append(ids, entry.Name())
		}
	}
	sort.Strings(ids)
	out := &agentapi.ListGenerationsResponse{Generations: make([]*agentapi.Generation, 0, len(ids))}
	for _, id := range ids {
		gen, err := s.generationReadModel(id, includeConfigApply)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "read generation %s: %v", id, err)
		}
		out.Generations = append(out.Generations, gen)
	}
	return out, nil
}

func (s *Server) GetGeneration(ctx context.Context, req *agentapi.GetGenerationRequest) (*agentapi.Generation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req == nil || strings.TrimSpace(req.GenerationId) == "" {
		return nil, status.Error(codes.InvalidArgument, "generationID is required")
	}
	if err := cleanPublicID("generationID", req.GenerationId); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	gen, err := s.generationReadModel(req.GenerationId, req.IncludeConfigApply)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "read generation: %v", err)
	}
	return gen, nil
}

func (s *Server) generationReadModel(id string, includeConfigApply bool) (*agentapi.Generation, error) {
	spec, genStatus, err := generation.ReadGeneration(s.Root, id)
	if err != nil {
		return nil, err
	}
	out := &agentapi.Generation{
		GenerationId:         spec.GenerationID,
		RuntimeVersion:       spec.RuntimeVersion,
		RuntimeArchitecture:  spec.Root.Architecture,
		PreviousGenerationId: spec.PreviousGenerationID,
		CommitState:          genStatus.CommitState,
		BootState:            genStatus.BootState,
		HealthState:          genStatus.HealthState,
		SpecDigest:           genStatus.SpecDigest,
		CreatedAt:            formatTime(spec.CreatedAt),
		UpdatedAt:            formatTime(genStatus.UpdatedAt),
	}
	for _, ref := range spec.Sysexts {
		out.Sysexts = append(out.Sysexts, &agentapi.ExtensionRef{
			Name:            ref.Name,
			Path:            ref.Path,
			ActivationPath:  ref.ActivationPath,
			Sha256:          ref.SHA256,
			ArtifactVersion: ref.ArtifactVersion,
			PayloadVersion:  ref.PayloadVersion,
			Architecture:    ref.Architecture,
		})
	}
	for _, ref := range spec.Confexts {
		out.Confexts = append(out.Confexts, &agentapi.GeneratedConfext{
			Name:           ref.Name,
			Path:           ref.Path,
			ActivationPath: ref.ActivationPath,
			Sha256:         ref.SHA256,
		})
	}
	if includeConfigApply {
		statusPath, err := generation.ConfigApplyStatusPath(s.Root, id)
		if err != nil {
			return nil, err
		}
		statusRecord, err := generation.ReadConfigApplyStatus(statusPath)
		if err == nil {
			out.ConfigApply = &agentapi.ConfigApplyStatus{
				Phase:                     statusRecord.Phase,
				RequestedApplyMode:        statusRecord.RequestedApplyMode,
				AcceptedApplyMode:         statusRecord.AcceptedApplyMode,
				ChangedDomains:            append([]string(nil), statusRecord.ChangedDomains...),
				FailureReason:             generation.RedactConfigApplyMessage(statusRecord.FailureReason),
				KubeadmActionRequired:     statusRecord.Kubeadm.Required,
				PreviousKubeadmConfigName: statusRecord.Kubeadm.PreviousConfigName,
				SelectedKubeadmConfigName: statusRecord.Kubeadm.SelectedConfigName,
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	return out, nil
}

func (s *Server) acceptConfigApplyOperation(req *agentapi.SubmitOperationRequest, digest string, id string, locks []string, now time.Time) (operation.OperationRecord, *agentapi.OperationAccepted, error) {
	configReq := configApplyRequestFromProto(req.GetConfigApply())
	if strings.TrimSpace(configReq.CandidateGenerationID) == "" {
		configReq.CandidateGenerationID = id + "-candidate"
	}
	if err := s.validateCandidateGenerationAvailable(configReq.CandidateGenerationID); err != nil {
		return operation.OperationRecord{}, nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	base, err := s.configApplyBase(configReq.NodeName, configReq.CandidateGenerationID)
	if err != nil {
		return operation.OperationRecord{}, nil, status.Errorf(codes.FailedPrecondition, "config apply base state: %v", err)
	}
	if diagnostics := validateConfigApplyDocument(configReq.ConfigYAML, base.KubeadmConfigs); !diagnostics.Accepted() {
		return operation.OperationRecord{}, nil, status.Error(codes.InvalidArgument, configValidationError(diagnostics.Strings()).Error())
	}
	decoded, err := configapply.DecodeNodeConfigurationChange(strings.NewReader(configReq.ConfigYAML), base)
	if err != nil {
		return operation.OperationRecord{}, nil, status.Errorf(codes.InvalidArgument, "config validation rejected: %v", err)
	}
	decoded.ApplyMode = configReq.ApplyMode
	decoded.GenerationID = configReq.CandidateGenerationID
	plan, err := configapply.PlanTrustedBundle(decoded)
	if err == nil {
		operationKind, err := configApplyOperationKind(plan.Plan.Decision.AcceptedMode)
		if err != nil {
			return operation.OperationRecord{}, nil, status.Error(codes.Internal, err.Error())
		}
		if req.OperationKind != operationKind {
			return operation.OperationRecord{}, nil, status.Errorf(codes.InvalidArgument, "operationKind %q does not match accepted applyMode %q", req.OperationKind, plan.Plan.Decision.AcceptedMode)
		}
	}
	record := operation.OperationRecord{
		OperationID:                 id,
		OperationKind:               req.OperationKind,
		Scope:                       operationScope(req.OperationKind),
		ClientRequestID:             req.ClientRequestId,
		Actor:                       req.Actor,
		ExpectedMachineID:           req.ExpectedMachineId,
		ExpectedCurrentGenerationID: req.ExpectedCurrentGenerationId,
		ExpectedClusterIntentDigest: req.ExpectedClusterIntentDigest,
		RequestDigest:               digest,
		Phase:                       "accepted",
		PhasePlan:                   []string{"accepted", "render-generation", "record-operation-complete"},
		CandidateGenerationID:       configReq.CandidateGenerationID,
		ConfigApplyRequest:          &configReq,
		ActivationMode:              configApplyActivationMode(req.OperationKind),
		ActivationState:             operation.ActivationStatePending,
		GenerationCommitState:       operation.GenerationCommitCandidate,
		ResourceLocks:               locks,
		NextAction:                  "queued for katlc agent config apply executor",
	}
	created, err := s.Store.Create(record, "accepted", now)
	if err != nil {
		return operation.OperationRecord{}, nil, status.Errorf(codes.Internal, "create operation record: %v", err)
	}
	return created, nil, nil
}

func validateConfigApplyDocument(configYAML string, configs map[string]kubeadmconfig.Plan) katlconfig.Result {
	return katlconfig.ValidateNodeConfigurationChange(configYAML, katlconfig.Options{
		CheckKubeadmRefs:   true,
		KubeadmConfigNames: kubeadmConfigNames(configs),
	})
}

func (e *Executor) executeConfigApply(ctx context.Context, record operation.OperationRecord) error {
	if record.ConfigApplyRequest == nil {
		return fmt.Errorf("configApplyRequest is required")
	}
	startedAt := e.clock()
	if _, err := e.Store.Update(record.OperationID, "render-generation-start", "render-generation", func(record operation.OperationRecord) (operation.OperationRecord, error) {
		record.Phase = "render-generation"
		record.NextAction = "render generation from accepted config apply request"
		record.UpdatedAt = startedAt
		return record, nil
	}); err != nil {
		return err
	}
	base, err := configApplyBase(e.Root, record.ConfigApplyRequest.NodeName, record.ConfigApplyRequest.CandidateGenerationID, e.clock)
	if err != nil {
		_, markErr := e.failRecordPhase(record.OperationID, "render-generation-refused", "render-generation", "render-generation", "config apply base state could not be loaded", err)
		return errorsJoin(err, markErr)
	}
	decoded, err := configapply.DecodeNodeConfigurationChange(strings.NewReader(record.ConfigApplyRequest.ConfigYAML), base)
	if err != nil {
		_, markErr := e.failRecordPhase(record.OperationID, "render-generation-refused", "render-generation", "render-generation", "config apply request failed validation", err)
		return errorsJoin(err, markErr)
	}
	decoded.ApplyMode = record.ConfigApplyRequest.ApplyMode
	decoded.GenerationID = record.ConfigApplyRequest.CandidateGenerationID
	plan, err := configapply.PlanTrustedBundle(decoded)
	if err != nil {
		cause := err
		if diagnostics := strings.TrimSpace(strings.Join(configApplyDiagnostics(plan.Plan.Decision), "\n")); diagnostics != "" {
			_, artifactErr := e.Store.AddDiagnosticArtifact(record.OperationID, "config-apply-plan-diagnostics", []byte(inventory.Redact(diagnostics)+"\n"), e.clock())
			cause = errorsJoin(err, fmt.Errorf("%s", diagnostics), artifactErr)
		}
		_, markErr := e.failRecordPhase(record.OperationID, "render-generation-refused", "render-generation", "render-generation", "config apply request failed planning", cause)
		return errorsJoin(err, markErr)
	}
	if plan.Plan.Decision.AcceptedMode == generation.ApplyModeLive {
		if err := e.markLiveConfigApplyStarted(record.OperationID, plan, startedAt); err != nil {
			return err
		}
		runner := e.ConfigApplyRunner
		if runner == nil {
			runner = commandRunnerFunc(runConfigApplyCommand)
		}
		activator := e.ConfigApplyActivator
		if activator == nil {
			activator = generationActivator{root: e.Root}
		}
		decoded.Executor = &configapply.Executor{
			Runner:    runner,
			Activator: activator,
			Now:       e.clock,
		}
	}
	result, err := configapply.ApplyTrustedBundle(ctx, decoded)
	completedAt := e.clock()
	if result.Plan.GenerationRecord.GenerationID != "" {
		result.Plan.GenerationRecord = inheritCurrentKernelCommandLine(e.Root, result.Plan.GenerationRecord)
		if splitErr := writeSplitGeneration(e.Root, result.Plan.GenerationRecord); splitErr != nil {
			err = errorsJoin(err, splitErr)
		}
	}
	if err == nil && result.Plan.Decision.AcceptedMode == generation.ApplyModeNextBoot {
		if commitErr := e.commitCandidateGeneration(ctx, record, completedAt, "next-boot runtime configuration apply staged by katlc agent executor"); commitErr != nil {
			err = commitErr
		}
	}
	if err != nil {
		_, artifactErr := e.Store.AddDiagnosticArtifact(record.OperationID, "config-apply-error", []byte(inventory.Redact(err.Error())), completedAt)
		_, markErr := e.failConfigApplyRecord(record.OperationID, result, "render-generation-failed", "render-generation", "render-generation", "config apply failed before completion", errorsJoin(err, artifactErr), completedAt)
		return errorsJoin(err, artifactErr, markErr)
	}
	_, updateErr := e.Store.Update(record.OperationID, "operation-complete", "operation-complete", func(record operation.OperationRecord) (operation.OperationRecord, error) {
		record.Phase = operation.HostBookkeepingCompletionPhase
		record.CompletedPhases = appendMissing(record.CompletedPhases, "render-generation", operation.HostBookkeepingCompletionPhase)
		record.PhaseIndex = len(record.CompletedPhases)
		record.PreviousGenerationID = result.Plan.GenerationRecord.ConfigApply.PreviousGeneration
		record.CandidateGenerationID = result.Plan.GenerationRecord.GenerationID
		record.ConfigApplyPhase = result.Status.Phase
		record.ChangedDomains = append([]string(nil), result.Status.ChangedDomains...)
		record.GenerationCommitState = operation.GenerationCommitCandidate
		if result.Plan.Decision.AcceptedMode == generation.ApplyModeNextBoot {
			record.GenerationCommitState = operation.GenerationCommitCommitted
			record.BootHealthPending = true
		}
		record.ActivationState = configApplyActivationState(result.Status, false)
		completeConfigApplyInvocation(record.Invocations, liveConfigApplyInvocationID(record.OperationID), completedAt, operation.ResultSucceeded)
		record.CompletedAt = &completedAt
		record.Terminal = true
		record.Result = operation.ResultSucceeded
		record.NextAction = "generation config apply completed by katlc agent executor"
		if result.Plan.Decision.AcceptedMode == generation.ApplyModeNextBoot {
			record.NextAction = "reboot into committed config apply generation for boot health validation"
		}
		record.UpdatedAt = completedAt
		return record, nil
	})
	return updateErr
}

func (e *Executor) markLiveConfigApplyStarted(operationID string, result configapply.TrustedBundleResult, now time.Time) error {
	_, err := e.Store.Update(operationID, "config-apply-live-start", "config-apply-live-start", func(record operation.OperationRecord) (operation.OperationRecord, error) {
		record.ExternalMutationStarted = true
		record.MutationScopes = appendMissing(record.MutationScopes, configApplyMutationScopes(result.Plan.Decision)...)
		record.Invocations = append(record.Invocations, operation.InvocationRecord{
			InvocationID:      liveConfigApplyInvocationID(operationID),
			AgentStartID:      e.AgentStartID,
			ExecutorAttemptID: e.AgentStartID,
			ChildProcess:      []string{"katlc-agent", "config-apply-live"},
			BootID:            currentBootID(),
			StartedAt:         now,
			Result:            "started",
		})
		record.PreviousGenerationID = result.Status.PreviousGeneration
		record.ConfigApplyPhase = generation.ConfigApplyPhaseActivating
		record.ChangedDomains = append([]string(nil), result.Status.ChangedDomains...)
		record.ActivationState = operation.ActivationStateActivating
		record.UpdatedAt = now
		return record, nil
	})
	return err
}

func (e *Executor) failConfigApplyRecord(operationID string, result configapply.TrustedBundleResult, eventID string, eventType string, phase string, nextAction string, cause error, now time.Time) (operation.OperationRecord, error) {
	return e.Store.Update(operationID, eventID, eventType, func(record operation.OperationRecord) (operation.OperationRecord, error) {
		record.Phase = phase
		record.Result = operation.ResultFailedNeedsRepair
		record.RecoveryRequired = true
		record.NextAction = nextAction
		record.FailureReason = inventory.Redact(cause.Error())
		if result.Plan.GenerationRecord.GenerationID != "" {
			record.PreviousGenerationID = result.Plan.GenerationRecord.ConfigApply.PreviousGeneration
			record.CandidateGenerationID = result.Plan.GenerationRecord.GenerationID
		}
		if result.Status.Phase != "" {
			record.ConfigApplyPhase = result.Status.Phase
			record.ChangedDomains = append([]string(nil), result.Status.ChangedDomains...)
		}
		record.ActivationState = configApplyActivationState(result.Status, true)
		completeConfigApplyInvocation(record.Invocations, liveConfigApplyInvocationID(operationID), now, operation.ResultFailedNeedsRepair)
		record.Terminal = true
		record.UpdatedAt = now
		record.CompletedAt = &now
		return record, nil
	})
}

func configApplyRequestFromProto(req *agentapi.ConfigApplyOperationRequest) operation.ConfigApplyRequest {
	if req == nil {
		return operation.ConfigApplyRequest{}
	}
	return operation.ConfigApplyRequest{
		ApplyMode:             strings.TrimSpace(req.ApplyMode),
		NodeName:              strings.TrimSpace(req.NodeName),
		CandidateGenerationID: strings.TrimSpace(req.CandidateGenerationId),
		ConfigYAML:            strings.TrimSpace(req.ConfigYaml),
	}
}

func configApplyActivationMode(operationKind string) string {
	if operationKind == OperationKindGenerationStage {
		return operation.ActivationModeNextBoot
	}
	return operation.ActivationModeLive
}

func configApplyOperationKind(acceptedMode string) (string, error) {
	switch acceptedMode {
	case generation.ApplyModeLive:
		return OperationKindGenerationApply, nil
	case generation.ApplyModeNextBoot:
		return OperationKindGenerationStage, nil
	default:
		return "", fmt.Errorf("unsupported accepted applyMode %q", acceptedMode)
	}
}

func validateConfigApplyRequest(operationKind string, req *agentapi.ConfigApplyOperationRequest) error {
	if req == nil {
		return fmt.Errorf("configApply is required")
	}
	switch operationKind {
	case OperationKindGenerationApply:
		if req.ApplyMode != generation.ApplyModeLive && req.ApplyMode != generation.ApplyModeAuto {
			return fmt.Errorf("generation-apply requires applyMode %q or %q", generation.ApplyModeLive, generation.ApplyModeAuto)
		}
	case OperationKindGenerationStage:
		if req.ApplyMode != generation.ApplyModeNextBoot && req.ApplyMode != generation.ApplyModeAuto {
			return fmt.Errorf("generation-stage requires applyMode %q or %q", generation.ApplyModeNextBoot, generation.ApplyModeAuto)
		}
	default:
		return fmt.Errorf("operationKind %q does not accept configApply", operationKind)
	}
	if strings.TrimSpace(req.CandidateGenerationId) != "" {
		if err := cleanPublicID("candidateGenerationID", req.CandidateGenerationId); err != nil {
			return err
		}
	}
	if strings.TrimSpace(req.ConfigYaml) == "" {
		return fmt.Errorf("configYAML is required")
	}
	return nil
}

func requestCandidateGenerationID(req *agentapi.SubmitOperationRequest) string {
	if req.GetHostUpgrade() != nil {
		return strings.TrimSpace(req.GetHostUpgrade().GetCandidateGenerationId())
	}
	if req.GetConfigApply() != nil {
		return strings.TrimSpace(req.GetConfigApply().GetCandidateGenerationId())
	}
	return strings.TrimSpace(req.GetBootstrap().GetCandidateGenerationId())
}

func requestActivationMode(req *agentapi.SubmitOperationRequest) string {
	if req.GetHostUpgrade() != nil {
		return operation.ActivationModeNextBoot
	}
	if req.GetConfigApply() != nil {
		return strings.TrimSpace(req.GetConfigApply().GetApplyMode())
	}
	return operation.ActivationModeLive
}

func (s *Server) configApplyBase(nodeName string, generationID string) (configapply.TrustedBundleRequest, error) {
	return configApplyBase(s.Root, nodeName, generationID, s.clock)
}

func configApplyBase(root string, nodeName string, generationID string, now func() time.Time) (configapply.TrustedBundleRequest, error) {
	currentID, err := currentGenerationID(root)
	if err != nil {
		return configapply.TrustedBundleRequest{}, err
	}
	spec, status, err := generation.ReadGeneration(root, currentID)
	if err != nil {
		return configapply.TrustedBundleRequest{}, fmt.Errorf("read current generation: %w", err)
	}
	current := generation.RecordFromSplit(spec, status)
	manifestPath := filepath.Join(filepath.Clean(root), "var/lib/katl/install/manifest.json")
	manifestFile, err := os.Open(manifestPath)
	if err != nil {
		return configapply.TrustedBundleRequest{}, fmt.Errorf("open current install manifest: %w", err)
	}
	defer manifestFile.Close()
	currentManifest, err := manifest.Decode(manifestFile)
	if err != nil {
		return configapply.TrustedBundleRequest{}, err
	}
	if strings.TrimSpace(nodeName) == "" {
		nodeName = currentManifest.Node.Identity.Hostname
	}
	kubeadmConfigs, err := installedKubeadmConfigs(root, currentManifest)
	if err != nil {
		return configapply.TrustedBundleRequest{}, err
	}
	kubernetesVersion := currentKubernetesPayloadVersion(current)
	kubernetesActivationPath := currentKubernetesActivationPath(current)
	if currentManifest.Node.Kubernetes.Kubeadm.ConfigRef != "" && (kubernetesVersion == "" || kubernetesActivationPath == "") {
		intent, _, err := installer.ReadClusterIntent(root)
		if err != nil {
			return configapply.TrustedBundleRequest{}, fmt.Errorf("read current cluster intent: %w", err)
		}
		if kubernetesVersion == "" {
			kubernetesVersion = strings.TrimSpace(intent.Kubernetes.PayloadVersion)
		}
		if kubernetesActivationPath == "" {
			kubernetesActivationPath = clusterIntentKubernetesActivationPath(intent)
		}
	}
	return configapply.TrustedBundleRequest{
		Root:                            root,
		NodeName:                        strings.TrimSpace(nodeName),
		GenerationID:                    strings.TrimSpace(generationID),
		CurrentManifest:                 currentManifest,
		CurrentRecord:                   current,
		KubeadmConfigs:                  kubeadmConfigs,
		RuntimeKubernetesVersion:        kubernetesVersion,
		RuntimeKubernetesActivationPath: kubernetesActivationPath,
		Chown:                           func(string, int, int) error { return nil },
		Now:                             now,
	}, nil
}

func clusterIntentKubernetesActivationPath(intent installer.ClusterIntent) string {
	path := strings.TrimSpace(intent.Kubernetes.SysextPath)
	if path == "" {
		if strings.TrimSpace(intent.Kubernetes.PayloadVersion) == "" {
			return ""
		}
		return "/run/extensions/katl-kubernetes.raw"
	}
	return "/run/extensions/" + filepath.Base(path)
}

func currentKubernetesPayloadVersion(record generation.Record) string {
	for _, sysext := range record.Sysexts {
		if sysext.Name == "kubernetes" {
			return strings.TrimSpace(sysext.PayloadVersion)
		}
	}
	return ""
}

func currentKubernetesActivationPath(record generation.Record) string {
	for _, sysext := range record.Sysexts {
		if sysext.Name == "kubernetes" {
			return strings.TrimSpace(sysext.ActivationPath)
		}
	}
	return ""
}

func installedKubeadmConfigs(root string, currentManifest manifest.Manifest) (map[string]kubeadmconfig.Plan, error) {
	ref := strings.TrimSpace(currentManifest.Node.Kubernetes.Kubeadm.ConfigRef)
	if ref == "" {
		return nil, nil
	}
	dir, err := installer.StoredKubeadmInputDir(root, ref)
	if err != nil {
		return nil, err
	}
	var files []kubeadmconfig.File
	if err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("stored kubeadm input %s is not a regular file", path)
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
			return fmt.Errorf("stored kubeadm input %s escapes input directory", path)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files = append(files, kubeadmconfig.File{
			RenderPath: filepath.ToSlash(filepath.Join("/etc/katl/kubeadm", ref, rel)),
			Content:    content,
			Mode:       info.Mode().Perm(),
		})
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read installed kubeadm config %q: %w", ref, err)
	}
	plan, err := kubeadmconfig.PlanFromRenderedFiles(ref, files)
	if err != nil {
		return nil, err
	}
	return map[string]kubeadmconfig.Plan{ref: plan}, nil
}

func (s *Server) validateCandidateGenerationAvailable(generationID string) error {
	if err := cleanPublicID("candidateGenerationID", generationID); err != nil {
		return err
	}
	dir, err := generation.GenerationDir(s.Root, generationID)
	if err != nil {
		return err
	}
	if _, err := os.Stat(dir); err == nil {
		return fmt.Errorf("candidateGenerationID %q already exists", generationID)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read candidate generation directory: %w", err)
	}
	return nil
}

func currentGenerationID(root string) (string, error) {
	selection, err := generation.ReadBootSelection(root)
	if err != nil {
		return "", err
	}
	for _, candidate := range []string{selection.BootedGenerationID, selection.TargetBootGenerationID, selection.DefaultGenerationID} {
		if strings.TrimSpace(candidate) != "" {
			return strings.TrimSpace(candidate), nil
		}
	}
	return "", fmt.Errorf("current generation is not recorded")
}

func writeSplitGeneration(root string, record generation.Record) error {
	dir, err := generation.GenerationDir(root, record.GenerationID)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, "spec.json")); err == nil {
		return fmt.Errorf("generation split spec already exists")
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read generation split spec: %w", err)
	}
	spec := generation.SpecFromRecord(record)
	digest, err := generation.CanonicalSpecDigest(spec)
	if err != nil {
		return err
	}
	status := generation.StatusFromRecord(record, digest)
	return generation.WriteGeneration(root, spec, status)
}

func inheritCurrentKernelCommandLine(root string, record generation.Record) generation.Record {
	options, err := readCurrentKernelCommandLine(root)
	if err != nil {
		return record
	}
	record.KernelCommandLine = mergeKernelCommandLine(record.KernelCommandLine, options)
	return record
}

func readCurrentKernelCommandLine(root string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(runtimeRoot(root), "proc/cmdline"))
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(data)), nil
}

func mergeKernelCommandLine(base []string, current []string) []string {
	return generation.MergeKernelCommandLine(base, current)
}

type commandRunnerFunc func(context.Context, configapply.Command) (configapply.CommandResult, error)

func (f commandRunnerFunc) Run(ctx context.Context, command configapply.Command) (configapply.CommandResult, error) {
	return f(ctx, command)
}

func runConfigApplyCommand(ctx context.Context, command configapply.Command) (configapply.CommandResult, error) {
	if len(command.Argv) == 0 {
		return configapply.CommandResult{}, fmt.Errorf("command argv is required")
	}
	runCtx := ctx
	cancel := func() {}
	if command.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, command.Timeout)
	}
	defer cancel()
	cmd := exec.CommandContext(runCtx, command.Argv[0], command.Argv[1:]...)
	stdout, err := cmd.Output()
	result := configapply.CommandResult{Stdout: string(stdout)}
	if exitErr, ok := err.(*exec.ExitError); ok {
		result.ExitStatus = exitErr.ExitCode()
		result.Stderr = string(exitErr.Stderr)
		return result, nil
	}
	if err != nil {
		result.ExitStatus = -1
		return result, err
	}
	result.ExitStatus = 0
	return result, nil
}

type generationActivator struct {
	root string
}

func (a generationActivator) Activate(ctx context.Context, record generation.Record) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := generation.ApplyActivation(a.root, record)
	return err
}

func (a generationActivator) Rollback(ctx context.Context, targetGenerationID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	metadataPath, err := generation.MetadataPath(a.root, targetGenerationID)
	if err != nil {
		return err
	}
	record, err := generation.ReadRecord(metadataPath)
	if err != nil {
		return err
	}
	_, err = generation.ApplyActivation(a.root, record)
	return err
}

func validateApplyMode(mode string) error {
	switch strings.TrimSpace(mode) {
	case generation.ApplyModeAuto, generation.ApplyModeLive, generation.ApplyModeNextBoot:
		return nil
	default:
		return fmt.Errorf("applyMode must be %q, %q, or %q", generation.ApplyModeAuto, generation.ApplyModeLive, generation.ApplyModeNextBoot)
	}
}

func configApplyDiagnostics(decision configapply.Decision) []string {
	out := make([]string, 0, len(decision.Diagnostics))
	for _, diagnostic := range decision.Diagnostics {
		parts := []string{diagnostic.Domain, diagnostic.Decision}
		if diagnostic.Message != "" {
			parts = append(parts, diagnostic.Message)
		}
		out = append(out, strings.Join(parts, ": "))
	}
	return out
}

func configApplyMutationScopes(decision configapply.Decision) []string {
	out := []string{"generation-state", "confext-activation"}
	for _, domain := range decision.ChangedDomains {
		out = append(out, "config-domain:"+domain)
	}
	return out
}

func liveConfigApplyInvocationID(operationID string) string {
	return "config-apply-live-" + operationID
}

func completeConfigApplyInvocation(invocations []operation.InvocationRecord, id string, completedAt time.Time, result string) {
	for i := range invocations {
		if invocations[i].InvocationID == id && invocations[i].CompletedAt == nil {
			invocations[i].CompletedAt = &completedAt
			invocations[i].Result = result
			if result == operation.ResultSucceeded {
				invocations[i].ExitStatus = 0
			} else {
				invocations[i].ExitStatus = 1
			}
			return
		}
	}
}

func configApplyActivationState(status generation.ConfigApplyStatus, failed bool) string {
	switch status.Phase {
	case generation.ConfigApplyPhaseActive:
		return operation.ActivationStateActiveLive
	case generation.ConfigApplyPhaseRolledBack:
		return operation.ActivationStateRolledBack
	case generation.ConfigApplyPhaseActivating, generation.ConfigApplyPhaseRollingBack:
		return operation.ActivationStateActivating
	}
	if failed {
		return operation.ActivationStateFailed
	}
	if status.AcceptedApplyMode == generation.ApplyModeNextBoot {
		return operation.ActivationStatePending
	}
	return operation.ActivationStateActiveLive
}

func errorsJoin(errs ...error) error {
	var out error
	for _, err := range errs {
		if err != nil {
			if out == nil {
				out = err
			} else {
				out = fmt.Errorf("%w; %v", out, err)
			}
		}
	}
	return out
}
