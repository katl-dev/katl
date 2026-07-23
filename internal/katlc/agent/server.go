package agent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/katl-dev/katl/internal/bootstrap/cluster"
	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/installer"
	"github.com/katl-dev/katl/internal/installer/generation"
	"github.com/katl-dev/katl/internal/installer/kubernetesbundle"
	"github.com/katl-dev/katl/internal/installer/operation"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const (
	APIVersion = operation.APIVersion

	RequestKind                   = "SubmitOperationRequest"
	RebootRequestKind             = "RebootRequest"
	ShutdownRequestKind           = "ShutdownRequest"
	WorkerJoinMaterialRequestKind = "CreateWorkerJoinMaterialRequest"

	DefaultListen = "tcp://0.0.0.0:9443"

	defaultWorkerJoinMaterialTTL     = 30 * time.Minute
	maxWorkerJoinMaterialTTL         = 24 * time.Hour
	workerJoinMaterialCommandTimeout = 30 * time.Second
)

var bootstrapOperationKinds = []string{
	"bootstrap-init",
	"bootstrap-join-control-plane",
	"bootstrap-join-worker",
	"generation-apply",
	"generation-stage",
	"kubeadm-upgrade",
	OperationKindKubeadmControlPlaneConfig,
	OperationKindDestructiveReset,
	OperationKindHostUpgrade,
}

type Dispatcher interface {
	Dispatch(ctx context.Context, record operation.OperationRecord) error
}

type Server struct {
	agentapi.UnimplementedKatlcAgentServer

	Root                    string
	Store                   operation.Store
	MachineID               string
	AgentStartID            string
	StartedAt               time.Time
	SupportedOperationKinds []string
	Dispatcher              Dispatcher
	RunJoinMaterial         ToolRunner
	RunEndpointLifecycle    ToolRunner
	RunKubernetesStatus     ToolRunner
	RunReboot               ToolRunner
	RunShutdown             ToolRunner
	Now                     func() time.Time
	OperationID             func(string, time.Time) (string, error)
	submitMu                sync.Mutex
}

func NewServer(root string, store operation.Store) *Server {
	now := time.Now().UTC()
	startID, _ := randomID("agent")
	return &Server{
		Root:                    strings.TrimSpace(root),
		Store:                   store,
		AgentStartID:            startID,
		StartedAt:               now,
		SupportedOperationKinds: append([]string(nil), bootstrapOperationKinds...),
		RunJoinMaterial:         runChildProcess,
		RunEndpointLifecycle:    runChildProcess,
		RunKubernetesStatus:     runChildProcess,
		RunReboot:               runChildProcess,
		RunShutdown:             runChildProcess,
		Now:                     func() time.Time { return time.Now().UTC() },
		OperationID:             defaultOperationID,
	}
}

func (s *Server) Reboot(ctx context.Context, req *agentapi.RebootRequest) (*agentapi.RebootAccepted, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	if req.ApiVersion != APIVersion {
		return nil, status.Errorf(codes.InvalidArgument, "apiVersion must be %q", APIVersion)
	}
	if req.Kind != RebootRequestKind {
		return nil, status.Errorf(codes.InvalidArgument, "kind must be %q", RebootRequestKind)
	}
	if strings.TrimSpace(req.Actor) == "" {
		return nil, status.Error(codes.InvalidArgument, "actor is required")
	}
	machineID, err := s.machineID()
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "read machine id: %v", err)
	}
	if strings.TrimSpace(req.ExpectedMachineId) == "" || req.ExpectedMachineId != machineID {
		return nil, status.Error(codes.FailedPrecondition, "expectedMachineID does not match node machine id")
	}
	target := strings.TrimSpace(req.TargetGenerationId)
	if err := cleanPublicID("targetGenerationID", target); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	_, generationStatus, err := generation.ReadGeneration(s.Root, target)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "read target generation: %v", err)
	}
	if generationStatus.CommitState != generation.CommitStateCommitted {
		return nil, status.Errorf(codes.FailedPrecondition, "target generation %q is not committed", target)
	}
	selection, err := generation.ReadBootSelection(s.Root)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "read boot selection: %v", err)
	}
	if selection.TargetBootGenerationID != target && selection.DefaultGenerationID != target {
		return nil, status.Errorf(codes.FailedPrecondition, "target generation %q is not selected for boot", target)
	}
	active, err := s.activeOperationIDs()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read operation locks: %v", err)
	}
	if len(active) > 0 {
		return nil, status.Error(codes.FailedPrecondition, "cannot reboot while another operation is active")
	}
	if s.RunReboot == nil {
		return nil, status.Error(codes.FailedPrecondition, "node reboot runner is not configured")
	}
	routingPaused, err := pauseManagedRoutingForPowerTransition(ctx, s.Root, s.RunEndpointLifecycle)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "prepare node routing for reboot: %s", inventory.Redact(err.Error()))
	}
	result := s.RunReboot(ctx, []string{"systemd-run", "--unit=katl-reboot", "--collect", "--on-active=2s", "systemctl", "reboot"}, nil)
	if result.Err != nil || result.ExitStatus != 0 {
		var resumeErr error
		if routingPaused {
			resumeErr = resumeManagedRoutingAfterFailedPowerTransition(context.Background(), s.Root, s.RunEndpointLifecycle)
		}
		return nil, status.Errorf(codes.Internal, "schedule reboot: %s", inventory.Redact(errors.Join(errors.New(toolFailure(result)), resumeErr).Error()))
	}
	return &agentapi.RebootAccepted{Scheduled: true, TargetGenerationId: target}, nil
}

func (s *Server) Shutdown(ctx context.Context, req *agentapi.ShutdownRequest) (*agentapi.ShutdownAccepted, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	if req.ApiVersion != APIVersion {
		return nil, status.Errorf(codes.InvalidArgument, "apiVersion must be %q", APIVersion)
	}
	if req.Kind != ShutdownRequestKind {
		return nil, status.Errorf(codes.InvalidArgument, "kind must be %q", ShutdownRequestKind)
	}
	if strings.TrimSpace(req.Actor) == "" {
		return nil, status.Error(codes.InvalidArgument, "actor is required")
	}
	machineID, err := s.machineID()
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "read machine id: %v", err)
	}
	if strings.TrimSpace(req.ExpectedMachineId) == "" || req.ExpectedMachineId != machineID {
		return nil, status.Error(codes.FailedPrecondition, "expectedMachineID does not match node machine id")
	}
	active, err := s.activeOperationIDs()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read operation locks: %v", err)
	}
	if len(active) > 0 {
		return nil, status.Error(codes.FailedPrecondition, "cannot shut down while another operation is active")
	}
	if s.RunShutdown == nil {
		return nil, status.Error(codes.FailedPrecondition, "node shutdown runner is not configured")
	}
	routingPaused, err := pauseManagedRoutingForPowerTransition(ctx, s.Root, s.RunEndpointLifecycle)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "prepare node routing for shutdown: %s", inventory.Redact(err.Error()))
	}
	result := s.RunShutdown(ctx, []string{"systemd-run", "--unit=katl-shutdown", "--collect", "--on-active=2s", "systemctl", "poweroff"}, nil)
	if result.Err != nil || result.ExitStatus != 0 {
		var resumeErr error
		if routingPaused {
			resumeErr = resumeManagedRoutingAfterFailedPowerTransition(context.Background(), s.Root, s.RunEndpointLifecycle)
		}
		return nil, status.Errorf(codes.Internal, "schedule shutdown: %s", inventory.Redact(errors.Join(errors.New(toolFailure(result)), resumeErr).Error()))
	}
	return &agentapi.ShutdownAccepted{Scheduled: true}, nil
}

func (s *Server) GetNodeStatus(ctx context.Context, _ *agentapi.GetNodeStatusRequest) (*agentapi.NodeStatus, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ids, err := s.activeOperationIDs()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read operation locks: %v", err)
	}
	machineID, err := s.machineID()
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "read machine id: %v", err)
	}
	currentGenerationID, _ := currentGenerationID(s.Root)
	endpointStatus, err := controlPlaneEndpointStatus(s.Root)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read control-plane endpoint status: %v", err)
	}
	kubernetesStatus, err := nodeKubernetesStatus(ctx, s.Root, s.RunKubernetesStatus)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read Kubernetes status: %v", err)
	}
	bootTargetGenerationID := currentGenerationID
	if selection, selectionErr := generation.ReadBootSelection(s.Root); selectionErr == nil {
		if target := strings.TrimSpace(selection.TargetBootGenerationID); target != "" {
			bootTargetGenerationID = target
		} else if selected := strings.TrimSpace(selection.DefaultGenerationID); selected != "" {
			bootTargetGenerationID = selected
		}
	}
	return &agentapi.NodeStatus{
		ApiVersion:              APIVersion,
		MachineId:               machineID,
		AgentStartId:            s.AgentStartID,
		AgentStartedAt:          formatTime(s.StartedAt),
		SupportedApiVersions:    []string{APIVersion},
		SupportedOperationKinds: append([]string(nil), s.supportedOperationKinds()...),
		OperationLockHeld:       len(ids) > 0,
		ActiveOperationIds:      ids,
		CurrentGenerationId:     currentGenerationID,
		BootTargetGenerationId:  bootTargetGenerationID,
		ControlPlaneEndpoint:    endpointStatus,
		Kubernetes:              kubernetesStatus,
	}, nil
}

func (s *Server) SubmitOperation(ctx context.Context, req *agentapi.SubmitOperationRequest) (*agentapi.OperationAccepted, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	if err := s.validateSubmit(req); err != nil {
		return nil, err
	}
	digest, err := RequestDigest(req)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "request digest: %v", err)
	}
	if strings.TrimSpace(req.RequestDigest) != "" && req.RequestDigest != digest {
		return nil, status.Error(codes.InvalidArgument, "requestDigest does not match normalized request")
	}
	req.RequestDigest = digest
	if !req.DryRun && s.Dispatcher == nil {
		return nil, status.Error(codes.FailedPrecondition, "agent executor is not configured")
	}
	created, dryRun, err := s.acceptOperation(req, digest)
	if err != nil {
		return nil, err
	}
	if dryRun != nil {
		return dryRun, nil
	}
	if created.Terminal {
		return s.acceptedFromRecord(created), nil
	}
	if err := s.Dispatcher.Dispatch(context.Background(), created); err != nil {
		updated, updateErr := s.markDispatchFailed(created.OperationID, err)
		if updateErr != nil {
			return nil, status.Errorf(codes.Internal, "dispatch failed and status update failed: %v; %v", err, updateErr)
		}
		created = updated
	}
	return s.acceptedFromRecord(created), nil
}

func (s *Server) CreateWorkerJoinMaterial(ctx context.Context, req *agentapi.CreateWorkerJoinMaterialRequest) (*agentapi.CreateWorkerJoinMaterialResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	if req.ApiVersion != APIVersion {
		return nil, status.Errorf(codes.InvalidArgument, "apiVersion must be %q", APIVersion)
	}
	if req.Kind != WorkerJoinMaterialRequestKind {
		return nil, status.Errorf(codes.InvalidArgument, "kind must be %q", WorkerJoinMaterialRequestKind)
	}
	if strings.TrimSpace(req.Actor) == "" {
		return nil, status.Error(codes.InvalidArgument, "actor is required")
	}
	if strings.TrimSpace(req.RequestRef) != "" && inventory.Redact(req.RequestRef) != req.RequestRef {
		return nil, status.Error(codes.InvalidArgument, "requestRef must be an opaque reference, not raw join material")
	}
	if strings.TrimSpace(req.ExpectedMachineId) != "" {
		machineID, err := s.machineID()
		if err != nil {
			return nil, status.Errorf(codes.FailedPrecondition, "read machine id: %v", err)
		}
		if req.ExpectedMachineId != machineID {
			return nil, status.Error(codes.FailedPrecondition, "expectedMachineID does not match node machine id")
		}
	}
	ttl, err := workerJoinTTL(req.Ttl)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "ttl: %v", err)
	}
	if s.RunJoinMaterial == nil {
		return nil, status.Error(codes.FailedPrecondition, "worker join material runner is not configured")
	}
	s.submitMu.Lock()
	defer s.submitMu.Unlock()
	active, err := s.activeOperationIDs()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read operation locks: %v", err)
	}
	if len(active) > 0 {
		return nil, status.Errorf(codes.FailedPrecondition, "operation locks conflict with active operation %s", strings.Join(active, ","))
	}
	startedAt := s.clock()
	controlPlane := joinMaterialRequestRole(req.RequestRef) == "control-plane"
	runCtx, cancel := context.WithTimeout(ctx, workerJoinMaterialCommandTimeout)
	defer cancel()
	var material cluster.JoinMaterial
	if controlPlane {
		keyResult := s.RunJoinMaterial(runCtx, []string{"/usr/bin/kubeadm", "init", "phase", "upload-certs", "--upload-certs", "--kubeconfig", "/etc/kubernetes/admin.conf"}, func(int) {})
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) && keyResult.Err == nil {
			keyResult.Err = runCtx.Err()
			keyResult.ExitStatus = -1
		}
		if keyResult.Err != nil || keyResult.ExitStatus != 0 {
			return nil, status.Errorf(codes.FailedPrecondition, "create control-plane certificate key: %s", inventory.Redact(toolFailure(keyResult)))
		}
		certificateKey := cluster.CertificateKey(string(keyResult.Stdout) + "\n" + string(keyResult.Stderr))
		if certificateKey == "" {
			return nil, status.Error(codes.FailedPrecondition, "create control-plane certificate key: kubeadm did not print a certificate key")
		}
		result := s.RunJoinMaterial(runCtx, []string{"/usr/bin/kubeadm", "token", "create", "--print-join-command", "--ttl", ttl.String(), "--certificate-key", certificateKey, "--kubeconfig", "/etc/kubernetes/admin.conf"}, func(int) {})
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) && result.Err == nil {
			result.Err = runCtx.Err()
			result.ExitStatus = -1
		}
		if result.Err != nil || result.ExitStatus != 0 {
			return nil, status.Errorf(codes.FailedPrecondition, "create control-plane join material: %s", inventory.Redact(toolFailure(result)))
		}
		parsed, err := cluster.ControlPlaneJoinMaterial(string(result.Stdout), certificateKey)
		if err != nil {
			return nil, status.Errorf(codes.FailedPrecondition, "parse control-plane join material: %v", err)
		}
		material = parsed
	} else {
		result := s.RunJoinMaterial(runCtx, []string{"/usr/bin/kubeadm", "token", "create", "--print-join-command", "--ttl", ttl.String(), "--kubeconfig", "/etc/kubernetes/admin.conf"}, func(int) {})
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) && result.Err == nil {
			result.Err = runCtx.Err()
			result.ExitStatus = -1
		}
		if result.Err != nil || result.ExitStatus != 0 {
			return nil, status.Errorf(codes.FailedPrecondition, "create worker join material: %s", inventory.Redact(toolFailure(result)))
		}
		parsed, err := cluster.ParseJoinMaterial(string(result.Stdout))
		if err != nil {
			return nil, status.Errorf(codes.FailedPrecondition, "parse worker join material: %v", err)
		}
		if err := validateWorkerJoinMaterial(parsed); err != nil {
			return nil, status.Errorf(codes.FailedPrecondition, "parse worker join material: %v", err)
		}
		material = parsed
	}
	expiresAt := startedAt.Add(ttl).UTC()
	response := &agentapi.CreateWorkerJoinMaterialResponse{
		MaterialRef: workerJoinMaterialRef(req.RequestRef, material, expiresAt),
		WorkerJoinMaterial: &agentapi.WorkerJoinMaterial{
			JoinArgv:  append([]string(nil), material.Argv...),
			ExpiresAt: expiresAt.Format(time.RFC3339),
		},
		CreatedAt: formatTime(startedAt),
	}
	return response, nil
}

func (s *Server) acceptOperation(req *agentapi.SubmitOperationRequest, digest string) (operation.OperationRecord, *agentapi.OperationAccepted, error) {
	s.submitMu.Lock()
	defer s.submitMu.Unlock()
	if existing, ok, err := s.findClientRequest(req.ClientRequestId); err != nil {
		return operation.OperationRecord{}, nil, status.Errorf(codes.Internal, "read idempotency state: %v", err)
	} else if ok {
		if existing.RequestDigest != digest {
			return operation.OperationRecord{}, nil, status.Error(codes.AlreadyExists, "clientRequestID already used with a different requestDigest")
		}
		return operation.OperationRecord{}, s.acceptedFromRecord(existing), nil
	}
	locks := resourceLocks(req.OperationKind)
	if conflict, err := s.conflictingOperation(locks); err != nil {
		return operation.OperationRecord{}, nil, status.Errorf(codes.Internal, "check operation locks: %v", err)
	} else if conflict != "" {
		return operation.OperationRecord{}, nil, status.Errorf(codes.FailedPrecondition, "operation locks conflict with active operation %s", conflict)
	}
	now := s.clock()
	if req.DryRun {
		if req.GetKubeadmControlPlaneConfig() != nil {
			if _, err := s.validateKubeadmControlPlaneConfigState(req); err != nil {
				return operation.OperationRecord{}, nil, status.Errorf(codes.FailedPrecondition, "kubeadm control-plane config preflight: %v", err)
			}
			return operation.OperationRecord{}, &agentapi.OperationAccepted{OperationKind: req.OperationKind, RequestDigest: digest, AcceptedAt: formatTime(now), InitialStatus: &agentapi.OperationStatus{OperationKind: req.OperationKind, RequestDigest: digest, Phase: "dry-run", UpdatedAt: formatTime(now), ResourceLocks: locks, NextAction: "all node dry-runs must pass before serial rollout execution"}}, nil
		}
		if req.GetKubernetesSysextUpdate() != nil {
			accepted, err := s.dryRunKubernetesSysextUpdateOperation(req, digest, locks, now)
			if err != nil {
				return operation.OperationRecord{}, nil, err
			}
			return operation.OperationRecord{}, accepted, nil
		}
		candidateID := requestCandidateGenerationID(req)
		return operation.OperationRecord{}, &agentapi.OperationAccepted{
			OperationKind: req.OperationKind,
			RequestDigest: digest,
			AcceptedAt:    formatTime(now),
			InitialStatus: &agentapi.OperationStatus{
				OperationKind:          req.OperationKind,
				RequestDigest:          digest,
				CandidateGenerationId:  candidateID,
				ActivationMode:         requestActivationMode(req),
				ActivationState:        operation.ActivationStatePending,
				GenerationCommitState:  operation.GenerationCommitCandidate,
				PostKubeadmHealthState: operation.PostKubeadmHealthNotRun,
				Phase:                  "dry-run",
				UpdatedAt:              formatTime(now),
				ResourceLocks:          locks,
				NextAction:             "submit with dryRun=false to create an operation record",
			},
		}, nil
	}
	id, err := s.operationID(req.OperationKind, now)
	if err != nil {
		return operation.OperationRecord{}, nil, status.Errorf(codes.Internal, "generate operation id: %v", err)
	}
	if req.GetConfigApply() != nil {
		return s.acceptConfigApplyOperation(req, digest, id, locks, now)
	}
	if req.GetKubeadmControlPlaneConfig() != nil {
		return s.acceptKubeadmControlPlaneConfigOperation(req, digest, id, locks, now)
	}
	if req.GetKubernetesSysextUpdate() != nil {
		return s.acceptKubernetesSysextUpdateOperation(req, digest, id, locks, now)
	}
	if req.GetDestructiveReset() != nil {
		return s.acceptDestructiveResetOperation(req, digest, id, locks, now)
	}
	if req.GetHostUpgrade() != nil {
		return s.acceptHostUpgradeOperation(req, digest, id, locks, now)
	}
	bootstrapRequest := bootstrapRequestFromProto(req.GetBootstrap())
	candidateID := strings.TrimSpace(bootstrapRequest.CandidateGenerationID)
	if candidateID == "" {
		candidateID = id + "-candidate"
	}
	bootstrapRequest.CandidateGenerationID = candidateID
	temporaryJoinConfig := ""
	if isJoinOperation(req.OperationKind) {
		temporaryJoinConfig = temporaryJoinConfigPath(id)
		bootstrapRequest.JoinMaterialDigest = workerJoinMaterialDigest(req.GetBootstrap().GetWorkerJoinMaterial())
		bootstrapRequest.JoinMaterialExpiresAt = strings.TrimSpace(req.GetBootstrap().GetWorkerJoinMaterial().GetExpiresAt())
		bootstrapRequest.TemporaryJoinConfigPath = temporaryJoinConfig
		if bootstrapRequest.JoinMaterialRef == "" {
			bootstrapRequest.JoinMaterialRef = "request:" + bootstrapRequest.JoinMaterialDigest[:12]
		}
	}
	plan, err := kubeadmPlanFromSubmit(req, id, temporaryJoinConfig)
	if err != nil {
		return operation.OperationRecord{}, nil, status.Errorf(codes.InvalidArgument, "operation request: %v", err)
	}
	plan.Timeout = strings.TrimSpace(req.OperationTimeout)
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
		PhasePlan:                   []string{"accepted", "prepare-bootstrap-runtime", "bootstrap-runtime-ready", plan.Phase, "post-kubeadm-health", operation.HostBookkeepingCompletionPhase},
		CandidateGenerationID:       candidateID,
		BootstrapRequest:            &bootstrapRequest,
		ActivationMode:              operation.ActivationModeLive,
		ActivationState:             operation.ActivationStatePending,
		GenerationCommitState:       operation.GenerationCommitCandidate,
		PostKubeadmHealthState:      operation.PostKubeadmHealthNotRun,
		ResourceLocks:               locks,
		ExecutorPlan:                &plan,
		NextAction:                  "queued for katlc agent executor",
	}
	created, err := s.Store.Create(record, "accepted", now)
	if err != nil {
		return operation.OperationRecord{}, nil, status.Errorf(codes.Internal, "create operation record: %v", err)
	}
	if isJoinOperation(req.OperationKind) {
		metadata, err := s.materializeJoinConfig(req, id)
		if err != nil {
			updated, updateErr := s.markMaterializationFailed(id, err)
			if updateErr != nil {
				return operation.OperationRecord{}, nil, status.Errorf(codes.Internal, "materialize join config failed and status update failed: %v; %v", err, updateErr)
			}
			return updated, nil, nil
		}
		created, err = s.Store.Update(id, "join-material-ready", "join-material-ready", func(record operation.OperationRecord) (operation.OperationRecord, error) {
			if record.BootstrapRequest == nil {
				return record, fmt.Errorf("operation bootstrapRequest is required")
			}
			record.BootstrapRequest.JoinMaterialDigest = metadata.digest
			record.BootstrapRequest.JoinMaterialExpiresAt = metadata.expiresAt
			record.BootstrapRequest.TemporaryJoinConfigPath = metadata.configPath
			record.UpdatedAt = s.clock()
			return record, nil
		})
		if err != nil {
			cleanupTemporaryJoinConfig(s.Root, operation.OperationRecord{BootstrapRequest: &operation.BootstrapRequest{TemporaryJoinConfigPath: metadata.configPath}})
			return operation.OperationRecord{}, nil, status.Errorf(codes.Internal, "record join material: %v", err)
		}
	}
	return created, nil, nil
}

func (s *Server) acceptDestructiveResetOperation(req *agentapi.SubmitOperationRequest, digest, id string, locks []string, now time.Time) (operation.OperationRecord, *agentapi.OperationAccepted, error) {
	resetRequest := destructiveResetFromProto(req.GetDestructiveReset())
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
		PhasePlan:                   []string{"accepted", "preflight-destructive-reset", "destructive-reset", "schedule-poweroff", operation.HostBookkeepingCompletionPhase},
		DestructiveResetRequest:     &resetRequest,
		ResourceLocks:               locks,
		NextAction:                  "queued for katlc agent executor",
	}
	created, err := s.Store.Create(record, "accepted", now)
	if err != nil {
		return operation.OperationRecord{}, nil, status.Errorf(codes.Internal, "create operation record: %v", err)
	}
	return created, nil, nil
}

func (s *Server) GetOperation(ctx context.Context, req *agentapi.GetOperationRequest) (*agentapi.OperationStatus, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req == nil || strings.TrimSpace(req.OperationId) == "" {
		return nil, status.Error(codes.InvalidArgument, "operationID is required")
	}
	record, err := s.Store.Read(req.OperationId)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "read operation: %v", err)
	}
	if strings.TrimSpace(req.ExpectedRequestDigest) != "" && record.RequestDigest != req.ExpectedRequestDigest {
		return nil, status.Error(codes.FailedPrecondition, "operation requestDigest does not match expectedRequestDigest")
	}
	options, err := responseOptions(req.IncludeDiagnostics)
	if err != nil {
		return nil, err
	}
	status := operationStatus(record, options.Diagnostics)
	if options.BootstrapOutput {
		kubeconfig, err := s.adminKubeconfigOutput(record)
		if err != nil {
			return nil, err
		}
		status.AdminKubeconfig = kubeconfig
	}
	return status, nil
}

func (s *Server) ListOperations(ctx context.Context, req *agentapi.ListOperationsRequest) (*agentapi.ListOperationsResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req == nil {
		req = &agentapi.ListOperationsRequest{}
	}
	if req.Limit < 0 || req.Limit > 100 {
		return nil, status.Error(codes.InvalidArgument, "limit must be between 0 and 100")
	}
	options, err := responseOptions(req.IncludeDiagnostics)
	if err != nil {
		return nil, err
	}
	records, err := s.Store.List()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list operations: %v", err)
	}
	limit := int(req.Limit)
	if limit == 0 {
		limit = 20
	}
	response := &agentapi.ListOperationsResponse{}
	for _, record := range records {
		if req.ActiveOnly && record.Terminal {
			continue
		}
		response.Operations = append(response.Operations, operationStatus(record, options.Diagnostics))
		if len(response.Operations) == limit {
			break
		}
	}
	return response, nil
}

func (s *Server) WatchOperation(req *agentapi.WatchOperationRequest, stream agentapi.KatlcAgent_WatchOperationServer) error {
	if req == nil || strings.TrimSpace(req.OperationId) == "" {
		return status.Error(codes.InvalidArgument, "operationID is required")
	}
	record, err := s.Store.Read(req.OperationId)
	if err != nil {
		return status.Errorf(codes.NotFound, "read operation: %v", err)
	}
	if strings.TrimSpace(req.ExpectedRequestDigest) != "" && record.RequestDigest != req.ExpectedRequestDigest {
		return status.Error(codes.FailedPrecondition, "operation requestDigest does not match expectedRequestDigest")
	}
	options, err := responseOptions(req.IncludeDiagnostics)
	if err != nil {
		return err
	}
	if record.Terminal && int(record.LatestJournalSeq) <= int(req.AfterJournalSeq) {
		return nil
	}
	timeout, err := watchTimeout(req.WatchTimeout)
	if err != nil {
		return err
	}
	ctx := stream.Context()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	afterSeq := int(req.AfterJournalSeq)
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		events, err := s.Store.EventsAfter(req.OperationId, afterSeq)
		if err != nil {
			return status.Errorf(codes.NotFound, "read operation: %v", err)
		}
		for _, event := range events {
			if err := stream.Send(operationEvent(event, options.Diagnostics)); err != nil {
				return err
			}
			afterSeq = event.Sequence
			if event.Record.Terminal {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil
			}
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (s *Server) validateSubmit(req *agentapi.SubmitOperationRequest) error {
	if req.ApiVersion != APIVersion {
		return status.Errorf(codes.InvalidArgument, "apiVersion must be %q", APIVersion)
	}
	if req.Kind != RequestKind {
		return status.Errorf(codes.InvalidArgument, "kind must be %q", RequestKind)
	}
	if strings.TrimSpace(req.ClientRequestId) == "" {
		return status.Error(codes.InvalidArgument, "clientRequestID is required")
	}
	if !contains(s.supportedOperationKinds(), req.OperationKind) {
		return status.Errorf(codes.InvalidArgument, "operationKind %q is unsupported", req.OperationKind)
	}
	if strings.TrimSpace(req.Actor) == "" {
		return status.Error(codes.InvalidArgument, "actor is required")
	}
	bodyCount := 0
	if req.GetBootstrap() != nil {
		bodyCount++
	}
	if req.GetConfigApply() != nil {
		bodyCount++
	}
	if req.GetKubernetesSysextUpdate() != nil {
		bodyCount++
	}
	if req.GetDestructiveReset() != nil {
		bodyCount++
	}
	if req.GetHostUpgrade() != nil {
		bodyCount++
	}
	if req.GetKubeadmControlPlaneConfig() != nil {
		bodyCount++
	}
	if bodyCount != 1 {
		return status.Error(codes.InvalidArgument, "exactly one operation request body is required")
	}
	if req.GetConfigApply() != nil {
		if err := validateConfigApplyRequest(req.OperationKind, req.GetConfigApply()); err != nil {
			return status.Errorf(codes.InvalidArgument, "config apply request: %v", err)
		}
	} else if req.GetKubeadmControlPlaneConfig() != nil {
		if err := validateKubeadmControlPlaneConfigRequest(req.OperationKind, req.GetKubeadmControlPlaneConfig()); err != nil {
			return status.Errorf(codes.InvalidArgument, "kubeadm control-plane config request: %v", err)
		}
	} else if req.GetKubernetesSysextUpdate() != nil {
		if err := validateKubernetesSysextUpdateRequest(req.OperationKind, req.GetKubernetesSysextUpdate()); err != nil {
			return status.Errorf(codes.InvalidArgument, "kubernetes sysext update request: %v", err)
		}
	} else if req.GetDestructiveReset() != nil {
		if err := validateDestructiveResetRequest(req.OperationKind, req.GetDestructiveReset()); err != nil {
			return status.Errorf(codes.InvalidArgument, "destructive reset request: %v", err)
		}
	} else if req.GetHostUpgrade() != nil {
		if err := validateHostUpgradeRequest(req.OperationKind, req.GetHostUpgrade()); err != nil {
			return status.Errorf(codes.InvalidArgument, "host upgrade request: %v", err)
		}
	} else {
		if err := validateBootstrapRequest(req.OperationKind, req.GetBootstrap()); err != nil {
			return status.Errorf(codes.InvalidArgument, "bootstrap request: %v", err)
		}
		if err := s.validateJoinMaterial(req.OperationKind, req.GetBootstrap()); err != nil {
			return err
		}
	}
	if strings.TrimSpace(req.ExpectedCurrentGenerationId) != "" {
		if err := cleanPublicID("expectedCurrentGenerationID", req.ExpectedCurrentGenerationId); err != nil {
			return status.Error(codes.InvalidArgument, err.Error())
		}
	}
	if strings.TrimSpace(req.ExpectedClusterIntentDigest) != "" {
		if err := validateDigestValue("expectedClusterIntentDigest", req.ExpectedClusterIntentDigest); err != nil {
			return status.Error(codes.InvalidArgument, err.Error())
		}
	}
	if strings.TrimSpace(req.OperationTimeout) != "" {
		timeout, err := time.ParseDuration(req.OperationTimeout)
		if err != nil || timeout <= 0 {
			return status.Error(codes.InvalidArgument, "operationTimeout must be a positive Go duration")
		}
		if timeout > maxToolTimeout {
			return status.Errorf(codes.InvalidArgument, "operationTimeout must not exceed %s", maxToolTimeout)
		}
	}
	if strings.TrimSpace(req.ExpectedMachineId) != "" {
		machineID, err := s.machineID()
		if err != nil {
			return status.Errorf(codes.FailedPrecondition, "read machine id: %v", err)
		}
		if req.ExpectedMachineId != machineID {
			return status.Error(codes.FailedPrecondition, "expectedMachineID does not match node machine id")
		}
	}
	if strings.TrimSpace(req.ExpectedCurrentGenerationId) != "" {
		if err := s.validateExpectedCurrentGeneration(req.ExpectedCurrentGenerationId); err != nil {
			return err
		}
	}
	if strings.TrimSpace(req.ExpectedClusterIntentDigest) != "" {
		if err := s.validateExpectedClusterIntentDigest(req.ExpectedClusterIntentDigest); err != nil {
			return err
		}
	}
	return nil
}

type workerJoinMetadata struct {
	digest     string
	expiresAt  string
	configPath string
}

func (s *Server) validateJoinMaterial(operationKind string, request *agentapi.BootstrapOperationRequest) error {
	if request == nil {
		return nil
	}
	material := request.GetWorkerJoinMaterial()
	if !isJoinOperation(operationKind) {
		if material != nil {
			return status.Error(codes.InvalidArgument, "bootstrap request: workerJoinMaterial is only valid for bootstrap join operations")
		}
		return nil
	}
	if material == nil || len(material.GetJoinArgv()) == 0 {
		return status.Errorf(codes.InvalidArgument, "bootstrap request: workerJoinMaterial is required for %s", operationKind)
	}
	parsed, err := joinMaterial(material)
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "bootstrap request: workerJoinMaterial: %v", err)
	}
	if operationKind == "bootstrap-join-control-plane" {
		if err := validateControlPlaneJoinMaterial(parsed); err != nil {
			return status.Errorf(codes.InvalidArgument, "bootstrap request: workerJoinMaterial: %v", err)
		}
	} else if err := validateWorkerJoinMaterial(parsed); err != nil {
		return status.Errorf(codes.InvalidArgument, "bootstrap request: workerJoinMaterial: %v", err)
	}
	expiresAt, err := parseJoinMaterialExpiry(material.GetExpiresAt())
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "bootstrap request: workerJoinMaterial expiresAt: %v", err)
	}
	if !expiresAt.After(s.clock()) {
		return status.Error(codes.InvalidArgument, "bootstrap request: workerJoinMaterial is expired")
	}
	return nil
}

func (s *Server) acceptedFromRecord(record operation.OperationRecord) *agentapi.OperationAccepted {
	return &agentapi.OperationAccepted{
		OperationId:   record.OperationID,
		OperationKind: record.OperationKind,
		RequestDigest: record.RequestDigest,
		RecordPath:    filepath.ToSlash(filepath.Join(s.operationStoreRoot(), record.OperationID, "record.json")),
		AcceptedAt:    formatTime(record.CreatedAt),
		InitialStatus: operationStatus(record, false),
	}
}

func (s *Server) markDispatchFailed(operationID string, err error) (operation.OperationRecord, error) {
	now := s.clock()
	return s.Store.Update(operationID, "dispatch-failed", "dispatch-failed", func(record operation.OperationRecord) (operation.OperationRecord, error) {
		record.Phase = "dispatch-failed"
		record.Result = operation.ResultFailedNeedsRepair
		record.RecoveryRequired = true
		record.NextAction = "agent executor dispatch failed"
		record.FailureReason = inventory.Redact(err.Error())
		record.Terminal = true
		record.UpdatedAt = now
		record.CompletedAt = &now
		return record, nil
	})
}

func (s *Server) markMaterializationFailed(operationID string, err error) (operation.OperationRecord, error) {
	now := s.clock()
	return s.Store.Update(operationID, "join-material-failed", "join-material-failed", func(record operation.OperationRecord) (operation.OperationRecord, error) {
		record.Phase = "prepare-bootstrap-runtime"
		record.Result = operation.ResultFailedNeedsRepair
		record.RecoveryRequired = true
		switch record.OperationKind {
		case "bootstrap-join-control-plane":
			record.NextAction = "submit a new control-plane join operation with valid join material"
		default:
			record.NextAction = "submit a new worker join operation with valid join material"
		}
		record.FailureReason = inventory.Redact(err.Error())
		record.Terminal = true
		record.UpdatedAt = now
		record.CompletedAt = &now
		return record, nil
	})
}

func (s *Server) findClientRequest(clientRequestID string) (operation.OperationRecord, bool, error) {
	clientRequestID = strings.TrimSpace(clientRequestID)
	if clientRequestID == "" {
		return operation.OperationRecord{}, false, nil
	}
	ids, err := s.Store.OperationIDs()
	if err != nil {
		return operation.OperationRecord{}, false, err
	}
	for _, id := range ids {
		record, err := s.Store.Read(id)
		if err != nil {
			return operation.OperationRecord{}, false, err
		}
		if record.ClientRequestID == clientRequestID {
			return record, true, nil
		}
	}
	return operation.OperationRecord{}, false, nil
}

func (s *Server) conflictingOperation(locks []string) (string, error) {
	if len(locks) == 0 {
		return "", nil
	}
	ids, err := s.Store.OperationIDs()
	if err != nil {
		return "", err
	}
	for _, id := range ids {
		record, err := s.Store.Read(id)
		if err != nil {
			return "", err
		}
		if record.Terminal {
			continue
		}
		for _, held := range record.ResourceLocks {
			if contains(locks, held) {
				return record.OperationID, nil
			}
		}
	}
	return "", nil
}

func (s *Server) activeOperationIDs() ([]string, error) {
	ids, err := s.Store.OperationIDs()
	if err != nil {
		return nil, err
	}
	var active []string
	for _, id := range ids {
		record, err := s.Store.Read(id)
		if err != nil {
			return nil, err
		}
		if !record.Terminal && len(record.ResourceLocks) > 0 {
			active = append(active, record.OperationID)
		}
	}
	sort.Strings(active)
	return active, nil
}

func (s *Server) machineID() (string, error) {
	if strings.TrimSpace(s.MachineID) != "" {
		return strings.TrimSpace(s.MachineID), nil
	}
	root := s.Root
	if strings.TrimSpace(root) == "" {
		root = "/"
	}
	for _, path := range []string{
		filepath.Join(root, "var/lib/katl/identity/machine-id"),
		filepath.Join(root, "etc/machine-id"),
	} {
		data, err := os.ReadFile(path)
		if err == nil {
			value := strings.TrimSpace(string(data))
			if value != "" {
				return value, nil
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	return "", fmt.Errorf("machine identity is not initialized")
}

func (s *Server) operationStoreRoot() string {
	if strings.TrimSpace(s.Store.Root) != "" {
		return s.Store.Root
	}
	root := s.Root
	if strings.TrimSpace(root) == "" {
		root = "/"
	}
	return filepath.Join(root, "var/lib/katl/operations")
}

func (s *Server) supportedOperationKinds() []string {
	kinds := bootstrapOperationKinds
	if len(s.SupportedOperationKinds) > 0 {
		kinds = s.SupportedOperationKinds
	}
	return append([]string(nil), kinds...)
}

func (s *Server) clock() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Server) operationID(kind string, now time.Time) (string, error) {
	if s.OperationID != nil {
		return s.OperationID(kind, now)
	}
	return defaultOperationID(kind, now)
}

func (s *Server) validateExpectedCurrentGeneration(expected string) error {
	selection, err := generation.ReadBootSelection(s.Root)
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "read boot selection for expectedCurrentGenerationID: %v", err)
	}
	current := strings.TrimSpace(selection.BootedGenerationID)
	if current == "" {
		current = strings.TrimSpace(selection.DefaultGenerationID)
	}
	if current == "" {
		return status.Error(codes.FailedPrecondition, "current generation is not recorded")
	}
	if expected != current {
		return status.Errorf(codes.FailedPrecondition, "expectedCurrentGenerationID %q does not match current generation %q", expected, current)
	}
	return nil
}

func (s *Server) validateExpectedClusterIntentDigest(expected string) error {
	root := strings.TrimSpace(s.Root)
	if root == "" {
		root = "/"
	}
	path := filepath.Join(root, "var/lib/katl/cluster/intent.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "read cluster intent for expectedClusterIntentDigest: %v", err)
	}
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	if normalizeDigest(expected) != got {
		return status.Errorf(codes.FailedPrecondition, "expectedClusterIntentDigest does not match stored cluster intent")
	}
	return nil
}

func (s *Server) adminKubeconfigOutput(record operation.OperationRecord) (string, error) {
	if record.OperationKind != "bootstrap-init" {
		return "", status.Error(codes.FailedPrecondition, "admin kubeconfig output is only available for bootstrap-init operations")
	}
	if !record.Terminal || record.Result != operation.ResultSucceeded {
		return "", status.Error(codes.FailedPrecondition, "admin kubeconfig output requires a successful terminal bootstrap-init operation")
	}
	root := strings.TrimSpace(s.Root)
	if root == "" {
		root = "/"
	}
	data, err := os.ReadFile(filepath.Join(root, "etc/kubernetes/admin.conf"))
	if err != nil {
		return "", status.Errorf(codes.FailedPrecondition, "read admin kubeconfig output: %v", err)
	}
	if strings.TrimSpace(string(data)) == "" {
		return "", status.Error(codes.FailedPrecondition, "admin kubeconfig output is empty")
	}
	return string(data), nil
}

func operationStatus(record operation.OperationRecord, includeDiagnostics bool) *agentapi.OperationStatus {
	diagnostics := make([]*agentapi.DiagnosticArtifact, 0, len(record.DiagnosticArtifacts))
	if includeDiagnostics {
		diagnostics = diagnosticArtifacts(record)
	}
	invocations := make([]*agentapi.OperationInvocation, 0, len(record.Invocations))
	for _, invocation := range record.Invocations {
		invocations = append(invocations, &agentapi.OperationInvocation{
			InvocationId:      invocation.InvocationID,
			AgentStartId:      invocation.AgentStartID,
			ExecutorAttemptId: invocation.ExecutorAttemptID,
			ChildProcess:      redactArgv(invocation.ChildProcess),
			Pid:               int32(invocation.PID),
			ExitStatus:        int32(invocation.ExitStatus),
			StartedAt:         formatTime(invocation.StartedAt),
			CompletedAt:       formatTimePtr(invocation.CompletedAt),
			Result:            invocation.Result,
		})
	}
	return &agentapi.OperationStatus{
		OperationId:             record.OperationID,
		OperationKind:           record.OperationKind,
		ClientRequestId:         record.ClientRequestID,
		RequestDigest:           record.RequestDigest,
		Phase:                   record.Phase,
		PhaseIndex:              int32(record.PhaseIndex),
		CompletedPhases:         append([]string(nil), record.CompletedPhases...),
		Terminal:                record.Terminal,
		Result:                  record.Result,
		PreviousGenerationId:    record.PreviousGenerationID,
		CandidateGenerationId:   record.CandidateGenerationID,
		ExternalMutationStarted: record.ExternalMutationStarted,
		MutationScopes:          append([]string(nil), record.MutationScopes...),
		ResourceLocks:           append([]string(nil), record.ResourceLocks...),
		LatestJournalSeq:        int32(record.LatestJournalSeq),
		UpdatedAt:               formatTime(record.UpdatedAt),
		NextAction:              record.NextAction,
		Diagnostics:             diagnostics,
		RecoveryRequired:        record.RecoveryRequired,
		FailureReason:           inventory.Redact(record.FailureReason),
		Invocations:             invocations,
		ActivationMode:          record.ActivationMode,
		ActivationState:         record.ActivationState,
		GenerationCommitState:   record.GenerationCommitState,
		PostKubeadmHealthState:  record.PostKubeadmHealthState,
		BootHealthPending:       record.BootHealthPending,
		ConfigApplyPhase:        record.ConfigApplyPhase,
		ChangedDomains:          append([]string(nil), record.ChangedDomains...),
	}
}

func RequestDigest(req *agentapi.SubmitOperationRequest) (string, error) {
	if req == nil {
		return "", fmt.Errorf("request is required")
	}
	clone := proto.Clone(req).(*agentapi.SubmitOperationRequest)
	clone.RequestDigest = ""
	data, err := protojson.MarshalOptions{EmitUnpopulated: false}.Marshal(clone)
	if err != nil {
		return "", err
	}
	var normalized any
	if err := json.Unmarshal(data, &normalized); err != nil {
		return "", err
	}
	canonical, err := json.Marshal(normalized)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

func resourceLocks(kind string) []string {
	switch kind {
	case "bootstrap-init", "bootstrap-join-control-plane", "bootstrap-join-worker":
		return []string{"generation-state.lock", "kubeadm-state.lock"}
	case "kubeadm-upgrade":
		return []string{"generation-state.lock", "kubeadm-state.lock"}
	case OperationKindKubeadmControlPlaneConfig:
		return []string{"kubeadm-state.lock"}
	case "generation-apply", "generation-stage":
		return []string{"generation-state.lock", "config-apply.lock"}
	case OperationKindDestructiveReset:
		return []string{"generation-state.lock", "kubeadm-state.lock", "destructive-reset.lock"}
	case OperationKindHostUpgrade:
		return []string{"generation-state.lock", "sysupdate.lock"}
	default:
		return nil
	}
}

func operationScope(kind string) string {
	switch kind {
	case "bootstrap-init", "bootstrap-join-control-plane", "bootstrap-join-worker":
		return "kubeadm-state"
	case "kubeadm-upgrade":
		return "kubeadm-state"
	case OperationKindKubeadmControlPlaneConfig:
		return "kubeadm-state"
	case "generation-apply", "generation-stage":
		return "host-generation"
	case OperationKindDestructiveReset:
		return "destructive-reset"
	case OperationKindHostUpgrade:
		return "host-generation"
	default:
		return "host-generation"
	}
}

func kubeadmPlanFromSubmit(req *agentapi.SubmitOperationRequest, operationID string, temporaryJoinConfigPath string) (toolPlan, error) {
	if req == nil || req.Bootstrap == nil {
		return toolPlan{}, fmt.Errorf("bootstrap request is required")
	}
	if err := cleanPublicID("operationID", operationID); err != nil {
		return toolPlan{}, err
	}
	ref := strings.TrimSpace(req.Bootstrap.BootstrapProfileRef)
	if err := cleanPublicID("bootstrapProfileRef", ref); err != nil {
		return toolPlan{}, err
	}
	configPath := "/etc/katl/kubeadm/" + ref + "/config.yaml"
	plan := toolPlan{
		MutationScopes: []string{"etc-kubernetes", "kubelet-state", "etcd-state", "cluster-objects"},
	}
	switch req.OperationKind {
	case "bootstrap-init":
		plan.Phase = "kubeadm-init"
		plan.MarkerID = "kubeadm-init"
		plan.Argv = []string{"/usr/bin/kubeadm", "init", "--config", configPath}
	case "bootstrap-join-worker":
		configPath = strings.TrimSpace(temporaryJoinConfigPath)
		if configPath == "" {
			return toolPlan{}, fmt.Errorf("temporary join config path is required")
		}
		plan.Phase = "kubeadm-join-worker"
		plan.MarkerID = "kubeadm-join-worker"
		plan.Argv = []string{"/usr/bin/kubeadm", "join", "--config", configPath}
	case "bootstrap-join-control-plane":
		configPath = strings.TrimSpace(temporaryJoinConfigPath)
		if configPath == "" {
			return toolPlan{}, fmt.Errorf("temporary join config path is required")
		}
		plan.Phase = "kubeadm-join-control-plane"
		plan.MarkerID = "kubeadm-join-control-plane"
		plan.Argv = []string{"/usr/bin/kubeadm", "join", "--config", configPath}
	default:
		return toolPlan{}, fmt.Errorf("operationKind %q has no executor plan", req.OperationKind)
	}
	return plan, nil
}

func validateBootstrapRequest(operationKind string, request *agentapi.BootstrapOperationRequest) error {
	if request == nil {
		return fmt.Errorf("typed bootstrap request is required")
	}
	if err := cleanPublicID("inventoryNodeName", request.InventoryNodeName); err != nil {
		return err
	}
	switch strings.TrimSpace(request.SystemRole) {
	case "control-plane":
		if operationKind == "bootstrap-join-worker" {
			return fmt.Errorf("systemRole control-plane does not match operationKind %s", operationKind)
		}
	case "worker":
		if operationKind != "bootstrap-join-worker" {
			return fmt.Errorf("systemRole worker does not match operationKind %s", operationKind)
		}
	default:
		return fmt.Errorf("systemRole must be control-plane or worker")
	}
	if strings.TrimSpace(request.KubernetesPayloadVersion) == "" {
		return fmt.Errorf("kubernetesPayloadVersion is required")
	}
	if (strings.TrimSpace(request.KubernetesBundleSource) == "") != (strings.TrimSpace(request.KubernetesBundleRef) == "") {
		return fmt.Errorf("kubernetesBundleSource and kubernetesBundleRef must be set together")
	}
	if strings.TrimSpace(request.KubernetesBundleSource) != "" {
		parsed, err := url.Parse(strings.TrimSpace(request.KubernetesBundleSource))
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
			return fmt.Errorf("kubernetesBundleSource must be an absolute HTTPS URL")
		}
	}
	if strings.TrimSpace(request.KubernetesBundleRef) != "" {
		payloadVersion, err := kubernetesbundle.PayloadVersionFromRef(request.KubernetesBundleRef)
		if err != nil || payloadVersion != strings.TrimSpace(request.KubernetesPayloadVersion) {
			return fmt.Errorf("kubernetesBundleRef must select kubernetesPayloadVersion")
		}
	}
	if strings.TrimSpace(request.BootstrapProfileRef) == "" {
		return fmt.Errorf("bootstrapProfileRef is required")
	}
	if strings.TrimSpace(request.CandidateGenerationId) != "" {
		if err := cleanPublicID("candidateGenerationID", request.CandidateGenerationId); err != nil {
			return err
		}
	}
	if strings.HasPrefix(strings.TrimSpace(request.ControlPlaneEndpoint), "/") || strings.Contains(request.ControlPlaneEndpoint, "\x00") {
		return fmt.Errorf("controlPlaneEndpoint must be a network endpoint, not a path")
	}
	if strings.HasPrefix(strings.TrimSpace(request.StableEndpoint), "/") || strings.Contains(request.StableEndpoint, "\x00") {
		return fmt.Errorf("stableEndpoint must be a network endpoint, not a path")
	}
	if strings.TrimSpace(request.KubeadmInputDigest) != "" {
		if err := validateDigestValue("kubeadmInputDigest", request.KubeadmInputDigest); err != nil {
			return err
		}
	}
	if strings.TrimSpace(request.JoinMaterialRef) != "" && inventory.Redact(request.JoinMaterialRef) != request.JoinMaterialRef {
		return fmt.Errorf("joinMaterialRef must be an opaque reference, not raw join material")
	}
	return nil
}

func bootstrapRequestFromProto(request *agentapi.BootstrapOperationRequest) operation.BootstrapRequest {
	if request == nil {
		return operation.BootstrapRequest{}
	}
	return operation.BootstrapRequest{
		InventoryNodeName:        strings.TrimSpace(request.InventoryNodeName),
		SystemRole:               strings.TrimSpace(request.SystemRole),
		KubernetesPayloadVersion: strings.TrimSpace(request.KubernetesPayloadVersion),
		KubernetesBundleSource:   strings.TrimSpace(request.KubernetesBundleSource),
		KubernetesBundleRef:      strings.TrimSpace(request.KubernetesBundleRef),
		BootstrapProfileRef:      strings.TrimSpace(request.BootstrapProfileRef),
		ControlPlaneEndpoint:     strings.TrimSpace(request.ControlPlaneEndpoint),
		StableEndpoint:           strings.TrimSpace(request.StableEndpoint),
		CandidateGenerationID:    strings.TrimSpace(request.CandidateGenerationId),
		KubeadmInputDigest:       strings.TrimSpace(request.KubeadmInputDigest),
		JoinMaterialRef:          strings.TrimSpace(request.JoinMaterialRef),
	}
}

func (s *Server) materializeJoinConfig(req *agentapi.SubmitOperationRequest, operationID string) (workerJoinMetadata, error) {
	bootstrap := req.GetBootstrap()
	material, err := joinMaterial(bootstrap.GetWorkerJoinMaterial())
	if err != nil {
		return workerJoinMetadata{}, err
	}
	ref := strings.TrimSpace(bootstrap.GetBootstrapProfileRef())
	inputDir, err := installer.StoredKubeadmInputDir(s.Root, ref)
	if err != nil {
		return workerJoinMetadata{}, err
	}
	base, err := os.ReadFile(filepath.Join(inputDir, "config.yaml"))
	if err != nil {
		return workerJoinMetadata{}, fmt.Errorf("read stored kubeadm config: %w", err)
	}
	runtimePath := temporaryJoinConfigPath(operationID)
	discoveryPath := ""
	if len(material.DiscoveryKubeconfig) > 0 {
		discoveryPath = temporaryJoinDiscoveryPath(operationID)
	}
	var rendered []byte
	switch req.OperationKind {
	case "bootstrap-join-control-plane":
		rendered, err = cluster.RenderControlPlaneJoinConfig(base, material, discoveryPath)
	case "bootstrap-join-worker":
		rendered, err = cluster.RenderWorkerJoinConfig(base, material, discoveryPath)
	default:
		return workerJoinMetadata{}, fmt.Errorf("operationKind %q is not a join operation", req.OperationKind)
	}
	if err != nil {
		return workerJoinMetadata{}, err
	}
	target := filepath.Join(filepath.Clean(s.Root), strings.TrimPrefix(runtimePath, "/"))
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return workerJoinMetadata{}, fmt.Errorf("create temporary join config directory: %w", err)
	}
	if discoveryPath != "" {
		discoveryTarget := filepath.Join(filepath.Clean(s.Root), strings.TrimPrefix(discoveryPath, "/"))
		if err := os.WriteFile(discoveryTarget, material.DiscoveryKubeconfig, 0o600); err != nil {
			cleanupTemporaryJoinConfig(s.Root, operation.OperationRecord{BootstrapRequest: &operation.BootstrapRequest{TemporaryJoinConfigPath: runtimePath}})
			return workerJoinMetadata{}, fmt.Errorf("write temporary join discovery kubeconfig: %w", err)
		}
	}
	if err := os.WriteFile(target, rendered, 0o600); err != nil {
		cleanupTemporaryJoinConfig(s.Root, operation.OperationRecord{BootstrapRequest: &operation.BootstrapRequest{TemporaryJoinConfigPath: runtimePath}})
		return workerJoinMetadata{}, fmt.Errorf("write temporary join config: %w", err)
	}
	return workerJoinMetadata{
		digest:     workerJoinMaterialDigest(bootstrap.GetWorkerJoinMaterial()),
		expiresAt:  strings.TrimSpace(bootstrap.GetWorkerJoinMaterial().GetExpiresAt()),
		configPath: runtimePath,
	}, nil
}

func temporaryJoinConfigPath(operationID string) string {
	return "/run/katl/bootstrap-join/" + operationID + "/config.yaml"
}

func temporaryJoinDiscoveryPath(operationID string) string {
	return "/run/katl/bootstrap-join/" + operationID + "/discovery.conf"
}

func joinMaterial(material *agentapi.WorkerJoinMaterial) (cluster.JoinMaterial, error) {
	if material == nil {
		return cluster.JoinMaterial{}, fmt.Errorf("is required")
	}
	argv := make([]string, 0, len(material.GetJoinArgv()))
	for _, arg := range material.GetJoinArgv() {
		arg = strings.TrimSpace(arg)
		if arg == "" {
			return cluster.JoinMaterial{}, fmt.Errorf("join argv contains an empty argument")
		}
		argv = append(argv, arg)
	}
	parsed, err := cluster.ParseJoinMaterial(strings.Join(argv, " "))
	if err != nil {
		return cluster.JoinMaterial{}, err
	}
	parsed.DiscoveryKubeconfig = append([]byte(nil), material.GetDiscoveryKubeconfig()...)
	return parsed, nil
}

func validateWorkerJoinMaterial(material cluster.JoinMaterial) error {
	if len(material.Argv) == 0 {
		return fmt.Errorf("is required")
	}
	for _, arg := range material.Argv {
		if arg == "--control-plane" {
			return fmt.Errorf("must not include --control-plane")
		}
	}
	for i := 0; i < len(material.Argv); i++ {
		arg := material.Argv[i]
		if arg == "--certificate-key" || strings.HasPrefix(arg, "--certificate-key=") {
			return fmt.Errorf("must not include --certificate-key")
		}
	}
	return nil
}

func validateControlPlaneJoinMaterial(material cluster.JoinMaterial) error {
	if len(material.Argv) == 0 {
		return fmt.Errorf("is required")
	}
	hasControlPlane := false
	certificateKey := ""
	for i := 0; i < len(material.Argv); i++ {
		arg := material.Argv[i]
		if arg == "--control-plane" {
			hasControlPlane = true
		}
		if arg == "--certificate-key" {
			if i+1 < len(material.Argv) {
				certificateKey = strings.TrimSpace(material.Argv[i+1])
			}
		}
		if value, ok := strings.CutPrefix(arg, "--certificate-key="); ok {
			certificateKey = strings.TrimSpace(value)
		}
	}
	if !hasControlPlane {
		return fmt.Errorf("must include --control-plane")
	}
	if certificateKey == "" {
		return fmt.Errorf("must include --certificate-key value")
	}
	if !validCertificateKey(certificateKey) {
		return fmt.Errorf("must include a 64-character hex --certificate-key value")
	}
	return nil
}

func validCertificateKey(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}

func isJoinOperation(kind string) bool {
	return kind == "bootstrap-join-worker" || kind == "bootstrap-join-control-plane"
}

func joinMaterialRequestRole(ref string) string {
	for _, part := range strings.Split(strings.TrimSpace(ref), "/") {
		if strings.HasPrefix(part, "control-plane:") {
			return "control-plane"
		}
	}
	return "worker"
}

func parseJoinMaterialExpiry(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, fmt.Errorf("is required")
	}
	expiresAt, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("must be RFC3339")
	}
	return expiresAt.UTC(), nil
}

func workerJoinTTL(value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return defaultWorkerJoinMaterialTTL, nil
	}
	ttl, err := time.ParseDuration(value)
	if err != nil || ttl <= 0 {
		return 0, fmt.Errorf("must be a positive Go duration")
	}
	if ttl > maxWorkerJoinMaterialTTL {
		return 0, fmt.Errorf("must not exceed %s", maxWorkerJoinMaterialTTL)
	}
	return ttl, nil
}

func workerJoinMaterialDigest(material *agentapi.WorkerJoinMaterial) string {
	discoveryDigest := ""
	if len(material.GetDiscoveryKubeconfig()) > 0 {
		sum := sha256.Sum256(material.GetDiscoveryKubeconfig())
		discoveryDigest = hex.EncodeToString(sum[:])
	}
	payload := struct {
		JoinArgv              []string `json:"joinArgv"`
		ExpiresAt             string   `json:"expiresAt"`
		DiscoveryConfigSHA256 string   `json:"discoveryConfigSHA256,omitempty"`
	}{
		JoinArgv:              append([]string(nil), material.GetJoinArgv()...),
		ExpiresAt:             strings.TrimSpace(material.GetExpiresAt()),
		DiscoveryConfigSHA256: discoveryDigest,
	}
	data, _ := json.Marshal(payload)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func workerJoinMaterialRef(requestRef string, material cluster.JoinMaterial, expiresAt time.Time) string {
	requestRef = strings.TrimSpace(requestRef)
	if requestRef != "" {
		return requestRef
	}
	payload := agentapi.WorkerJoinMaterial{
		JoinArgv:  append([]string(nil), material.Argv...),
		ExpiresAt: expiresAt.UTC().Format(time.RFC3339),
	}
	return "worker-join:" + workerJoinMaterialDigest(&payload)[:12]
}

func operationEvent(event operation.JournalEvent, includeDiagnostics bool) *agentapi.OperationEvent {
	return &agentapi.OperationEvent{
		OperationId: event.Record.OperationID,
		JournalSeq:  int32(event.Sequence),
		EventType:   event.EventType,
		Phase:       event.Record.Phase,
		Terminal:    event.Record.Terminal,
		Status:      operationStatus(event.Record, false),
		Diagnostics: diagnosticArtifactsIf(event.Record, includeDiagnostics),
	}
}

func diagnosticArtifactsIf(record operation.OperationRecord, include bool) []*agentapi.DiagnosticArtifact {
	if !include {
		return nil
	}
	return diagnosticArtifacts(record)
}

func diagnosticArtifacts(record operation.OperationRecord) []*agentapi.DiagnosticArtifact {
	diagnostics := make([]*agentapi.DiagnosticArtifact, 0, len(record.DiagnosticArtifacts))
	for _, artifact := range record.DiagnosticArtifacts {
		diagnostics = append(diagnostics, &agentapi.DiagnosticArtifact{
			ArtifactId: artifact.ArtifactID,
			Path:       inventory.Redact(artifact.Path),
			Sha256:     artifact.SHA256,
			Redacted:   artifact.Redacted,
			CreatedAt:  formatTime(artifact.CreatedAt),
		})
	}
	return diagnostics
}

type operationResponseOptions struct {
	Diagnostics     bool
	BootstrapOutput bool
}

func responseOptions(value string) (operationResponseOptions, error) {
	switch strings.TrimSpace(value) {
	case "", "normal":
		return operationResponseOptions{}, nil
	case "verbose":
		return operationResponseOptions{Diagnostics: true}, nil
	case "bootstrap-output":
		return operationResponseOptions{BootstrapOutput: true}, nil
	default:
		return operationResponseOptions{}, status.Error(codes.InvalidArgument, "includeDiagnostics must be normal, verbose, or bootstrap-output")
	}
}

func watchTimeout(value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 30 * time.Second, nil
	}
	timeout, err := time.ParseDuration(value)
	if err != nil || timeout <= 0 {
		return 0, status.Error(codes.InvalidArgument, "watchTimeout must be a positive Go duration")
	}
	return timeout, nil
}

func cleanPublicID(name string, value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("%s is required", name)
	}
	if strings.Contains(value, "/") || strings.Contains(value, "\\") || strings.Contains(value, "..") || strings.ContainsAny(value, " \t\r\n") {
		return fmt.Errorf("%s must be a clean identifier", name)
	}
	return nil
}

func validateDigestValue(name string, value string) error {
	value = normalizeDigest(value)
	if value == "" {
		return fmt.Errorf("%s is required", name)
	}
	if len(value) != sha256.Size*2 {
		return fmt.Errorf("%s must be a SHA-256 digest", name)
	}
	if _, err := hex.DecodeString(value); err != nil {
		return fmt.Errorf("%s must be a SHA-256 digest", name)
	}
	return nil
}

func normalizeDigest(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "sha256:")
	return strings.ToLower(value)
}

func defaultOperationID(kind string, now time.Time) (string, error) {
	suffix, err := randomID("")
	if err != nil {
		return "", err
	}
	cleanKind := strings.NewReplacer("_", "-", ".", "-", "/", "-").Replace(strings.TrimSpace(kind))
	if cleanKind == "" {
		cleanKind = "operation"
	}
	return fmt.Sprintf("%s-%s-%s", cleanKind, now.UTC().Format("20060102T150405Z"), suffix), nil
}

func randomID(prefix string) (string, error) {
	var data [6]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(data[:])
	if strings.TrimSpace(prefix) == "" {
		return id, nil
	}
	return prefix + "-" + id, nil
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func formatTimePtr(value *time.Time) string {
	if value == nil {
		return ""
	}
	return formatTime(*value)
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
