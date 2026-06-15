package agent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zariel/katl/internal/bootstrap/inventory"
	"github.com/zariel/katl/internal/installer/generation"
	"github.com/zariel/katl/internal/installer/operation"
	agentapi "github.com/zariel/katl/internal/katlc/agentapi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
)

const (
	APIVersion = operation.APIVersion

	RequestKind = "SubmitOperationRequest"

	DefaultListen = "tcp://0.0.0.0:9443"
)

var bootstrapOperationKinds = []string{
	"bootstrap-init",
	"bootstrap-join-control-plane",
	"bootstrap-join-worker",
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
		Now:                     func() time.Time { return time.Now().UTC() },
		OperationID:             defaultOperationID,
	}
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
	return &agentapi.NodeStatus{
		ApiVersion:              APIVersion,
		MachineId:               machineID,
		AgentStartId:            s.AgentStartID,
		AgentStartedAt:          formatTime(s.StartedAt),
		SupportedApiVersions:    []string{APIVersion},
		SupportedOperationKinds: append([]string(nil), s.supportedOperationKinds()...),
		OperationLockHeld:       len(ids) > 0,
		ActiveOperationIds:      ids,
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
	if err := s.Dispatcher.Dispatch(context.Background(), created); err != nil {
		updated, updateErr := s.markDispatchFailed(created.OperationID, err)
		if updateErr != nil {
			return nil, status.Errorf(codes.Internal, "dispatch failed and status update failed: %v; %v", err, updateErr)
		}
		created = updated
	}
	return s.acceptedFromRecord(created), nil
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
		return operation.OperationRecord{}, &agentapi.OperationAccepted{
			OperationKind: req.OperationKind,
			RequestDigest: digest,
			AcceptedAt:    formatTime(now),
			InitialStatus: &agentapi.OperationStatus{
				OperationKind:          req.OperationKind,
				RequestDigest:          digest,
				CandidateGenerationId:  req.GetBootstrap().GetCandidateGenerationId(),
				ActivationMode:         operation.ActivationModeLive,
				ActivationState:        operation.ActivationStatePending,
				GenerationCommitState:  operation.GenerationCommitCandidate,
				PostKubeadmHealthState: operation.PostKubeadmHealthNotRun,
				BootHealthPending:      true,
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
	bootstrapRequest := bootstrapRequestFromProto(req.GetBootstrap())
	candidateID := strings.TrimSpace(bootstrapRequest.CandidateGenerationID)
	if candidateID == "" {
		candidateID = id + "-candidate"
	}
	plan, err := kubeadmPlanFromSubmit(req, id)
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
		PhasePlan:                   []string{"accepted", plan.Phase},
		CandidateGenerationID:       candidateID,
		BootstrapRequest:            &bootstrapRequest,
		ActivationMode:              operation.ActivationModeLive,
		ActivationState:             operation.ActivationStatePending,
		GenerationCommitState:       operation.GenerationCommitCandidate,
		PostKubeadmHealthState:      operation.PostKubeadmHealthNotRun,
		BootHealthPending:           true,
		ResourceLocks:               locks,
		ExecutorPlan:                &plan,
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
	diagnostics, err := includeDiagnostics(req.IncludeDiagnostics)
	if err != nil {
		return nil, err
	}
	return operationStatus(record, diagnostics), nil
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
	include, err := includeDiagnostics(req.IncludeDiagnostics)
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
			if err := stream.Send(operationEvent(event, include)); err != nil {
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
	if err := validateBootstrapRequest(req.OperationKind, req.GetBootstrap()); err != nil {
		return status.Errorf(codes.InvalidArgument, "bootstrap request: %v", err)
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
	if len(s.SupportedOperationKinds) > 0 {
		return append([]string(nil), s.SupportedOperationKinds...)
	}
	return append([]string(nil), bootstrapOperationKinds...)
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
		RequestDigest:           record.RequestDigest,
		Phase:                   record.Phase,
		PhaseIndex:              int32(record.PhaseIndex),
		CompletedPhases:         append([]string(nil), record.CompletedPhases...),
		Terminal:                record.Terminal,
		Result:                  record.Result,
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
	}
}

func RequestDigest(req *agentapi.SubmitOperationRequest) (string, error) {
	if req == nil {
		return "", fmt.Errorf("request is required")
	}
	clone := *req
	clone.RequestDigest = ""
	data, err := protojson.MarshalOptions{EmitUnpopulated: false}.Marshal(&clone)
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
	default:
		return nil
	}
}

func operationScope(kind string) string {
	switch kind {
	case "bootstrap-init", "bootstrap-join-control-plane", "bootstrap-join-worker":
		return "kubeadm-state"
	default:
		return "host-generation"
	}
}

func kubeadmPlanFromSubmit(req *agentapi.SubmitOperationRequest, operationID string) (toolPlan, error) {
	if req == nil || req.Bootstrap == nil {
		return toolPlan{}, fmt.Errorf("bootstrap request is required")
	}
	if err := cleanPublicID("operationID", operationID); err != nil {
		return toolPlan{}, err
	}
	configPath := "/etc/katl/kubeadm/" + operationID + ".yaml"
	plan := toolPlan{
		MutationScopes: []string{"kubeadm-state", "etc-kubernetes"},
	}
	switch req.OperationKind {
	case "bootstrap-init":
		plan.Phase = "kubeadm-init"
		plan.MarkerID = "kubeadm-init"
		plan.Argv = []string{"/usr/bin/kubeadm", "init", "--config", configPath}
	case "bootstrap-join-control-plane":
		plan.Phase = "kubeadm-join-control-plane"
		plan.MarkerID = "kubeadm-join-control-plane"
		plan.Argv = []string{"/usr/bin/kubeadm", "join", "--config", configPath}
	case "bootstrap-join-worker":
		plan.Phase = "kubeadm-join-worker"
		plan.MarkerID = "kubeadm-join-worker"
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
	if operationKind != "bootstrap-init" && strings.TrimSpace(request.JoinMaterialRef) == "" {
		return fmt.Errorf("joinMaterialRef is required for join operations")
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
		BootstrapProfileRef:      strings.TrimSpace(request.BootstrapProfileRef),
		ControlPlaneEndpoint:     strings.TrimSpace(request.ControlPlaneEndpoint),
		StableEndpoint:           strings.TrimSpace(request.StableEndpoint),
		CandidateGenerationID:    strings.TrimSpace(request.CandidateGenerationId),
		KubeadmInputDigest:       strings.TrimSpace(request.KubeadmInputDigest),
		JoinMaterialRef:          strings.TrimSpace(request.JoinMaterialRef),
	}
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

func includeDiagnostics(value string) (bool, error) {
	switch strings.TrimSpace(value) {
	case "", "normal":
		return false, nil
	case "verbose":
		return true, nil
	default:
		return false, status.Error(codes.InvalidArgument, "includeDiagnostics must be normal or verbose")
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
