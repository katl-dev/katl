package agent

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/katlc/transport"
	"github.com/katl-dev/katl/internal/managementidentity"
	"github.com/katl-dev/katl/internal/nodeidentity"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

const (
	BeginManagementTransitionRequestKind   = "BeginManagementTransitionRequest"
	ConfirmManagementTransitionRequestKind = "ConfirmManagementTransitionRequest"

	managementTrialRevertUnitPrefix  = "katl-management-revert-"
	managementTrialRestartUnitPrefix = "katl-management-restart-"

	managementTrialDeadlineReason = "the transition was not confirmed before its deadline"
	defaultManagementTrialPoll    = 5 * time.Second
)

// managementServerTLS selects what katlc-agent serves: a live management
// trial's credentials, or the installed mode. A trial whose credentials cannot
// be loaded is reverted at once, so a failed trial never leaves the node
// without a listener for its installed clients.
func managementServerTLS(root string, clock nodeidentity.TrialClock) (*tls.Config, *nodeidentity.ManagementTrial, error) {
	trial, err := nodeidentity.SettleManagementTrial(root, clock)
	if err != nil {
		log.Printf("settle management trial: %v; serving the installed management mode", err)
	}
	if trial != nil {
		config, err := transport.TrialServerTLSConfig(root, trial.TargetAuthority)
		if err == nil {
			return config, trial, nil
		}
		log.Printf("load management trial %s credentials: %v; reverting", trial.ID, err)
		if _, _, revertErr := nodeidentity.RevertManagementTrial(root, trial.ID, "katlc-agent could not load the trial credentials: "+err.Error(), clock); revertErr != nil {
			log.Printf("revert management trial %s: %v; serving the installed management mode", trial.ID, revertErr)
		}
	}
	config, err := transport.ServerTLSConfig(root)
	return config, nil, err
}

// servedTrial tracks the management trial the current listener serves, so the
// listener can stop serving it once it is committed or reverted.
type servedTrial struct {
	mu        sync.Mutex
	id        string
	committed chan struct{}
	reverted  chan struct{}
}

// serve records the trial a new listener serves, if any, and returns channels
// closed when it is committed or reverted while served.
func (t *servedTrial) serve(trial *nodeidentity.ManagementTrial) (committed, reverted <-chan struct{}) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.id = ""
	if trial != nil {
		t.id = trial.ID
	}
	t.committed, t.reverted = make(chan struct{}), make(chan struct{})
	return t.committed, t.reverted
}

func (t *servedTrial) ID() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.id
}

// end reports that trial id was committed or reverted. It is ignored unless
// the current listener serves that trial.
func (t *servedTrial) end(id string, committed bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if id == "" || id != t.id || t.committed == nil {
		return
	}
	if committed {
		close(t.committed)
	} else {
		close(t.reverted)
	}
	t.committed, t.reverted = nil, nil
}

// awaitTrialDeadline reports whether the served trial reached its
// CLOCK_BOOTTIME deadline before it was committed or reverted. It enforces the
// deadline independently of the systemd revert timer, so either alone stops
// the trial.
func (s *Server) awaitTrialDeadline(ctx context.Context, trial nodeidentity.ManagementTrial, clock nodeidentity.TrialClock, committed, reverted <-chan struct{}) bool {
	poll := s.managementTrialPoll
	if poll <= 0 {
		poll = defaultManagementTrialPoll
	}
	for {
		sinceBoot, err := clock.SinceBoot()
		if err != nil {
			log.Printf("read boot time for management trial %s: %v; reverting", trial.ID, err)
			return true
		}
		remaining := trial.DeadlineSinceBoot - sinceBoot
		if remaining <= 0 {
			return true
		}
		// Go timers do not advance while the node is suspended but
		// CLOCK_BOOTTIME does, so recheck at least every poll interval.
		wait := time.NewTimer(min(remaining, poll))
		select {
		case <-ctx.Done():
			wait.Stop()
			return false
		case <-committed:
			wait.Stop()
			return false
		case <-reverted:
			wait.Stop()
			return false
		case <-wait.C:
		}
	}
}

// refuseDuringManagementTrial holds off mutating requests while a management
// trial is live: the restarts that serve and revert the trial would cut an
// operation short.
func (s *Server) refuseDuringManagementTrial() error {
	trial, found, err := nodeidentity.ReadManagementTrial(s.Root)
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "read management trial: %v", err)
	}
	if found && trial.State == nodeidentity.ManagementTrialLive {
		return status.Errorf(codes.FailedPrecondition, "a management trust transition is in progress until %s; confirm it, or wait for it to revert, before changing the node", trial.Deadline.Format(time.RFC3339))
	}
	return nil
}

func (s *Server) BeginManagementTransition(ctx context.Context, req *agentapi.BeginManagementTransitionRequest) (*agentapi.ManagementTransition, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	if err := validateManagementTransitionRequest(req.ApiVersion, req.Kind, BeginManagementTransitionRequestKind, req.Actor); err != nil {
		return nil, err
	}
	if err := s.validateEnrolledTarget(req.ExpectedEnrollmentId, req.ExpectedInventoryNodeName, req.ExpectedMachineId); err != nil {
		return nil, err
	}
	// submitMu, the mutation lock and the active-operation check refuse a
	// trial while a node operation runs, and the live trial record refuses
	// operations submitted during it, so the restarts that serve and revert the
	// trial cannot cut one short.
	s.submitMu.Lock()
	defer s.submitMu.Unlock()
	lock, err := s.Store.AcquireMutationLock()
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	defer lock.Close()
	active, err := s.activeOperationIDs()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read operation locks: %v", err)
	}
	if len(active) > 0 {
		return nil, status.Errorf(codes.FailedPrecondition, "operation %s is active; wait for it before moving management trust", strings.Join(active, ", "))
	}

	clock := s.trialClock()
	trial, err := nodeidentity.BeginManagementTrial(s.Root, nodeidentity.ManagementTransition{
		NodeName:               req.ExpectedInventoryNodeName,
		ExpectedEnrollmentID:   req.ExpectedEnrollmentId,
		ExpectedMachineID:      req.ExpectedMachineId,
		ExpectedAuthentication: managementidentity.Authentication(req.ExpectedAuthentication),
		Replacement: managementidentity.NodeCredentials{
			CACertificate: req.CaCertificate, ServerCertificate: req.ServerCertificate, ServerPrivateKey: req.ServerPrivateKey,
		},
		Window: time.Duration(req.TrialSeconds) * time.Second,
	}, clock, func(trial nodeidentity.ManagementTrial) error { return s.armManagementRevert(ctx, trial) })
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	if trial.State == nodeidentity.ManagementTrialLive && trial.ID != s.servedTrial.ID() {
		if err := s.scheduleTrialRestart(ctx, trial.ID); err != nil {
			if _, _, revertErr := nodeidentity.RevertManagementTrial(s.Root, trial.ID, "katlc-agent restart could not be scheduled", clock); revertErr != nil {
				err = errors.Join(err, revertErr)
			}
			return nil, status.Errorf(codes.Internal, "schedule katlc-agent restart for the management trial: %v; rerun the transition", err)
		}
	}
	return managementTransitionStatus(trial), nil
}

func (s *Server) ConfirmManagementTransition(ctx context.Context, req *agentapi.ConfirmManagementTransitionRequest) (*agentapi.ManagementTransition, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	if err := validateManagementTransitionRequest(req.ApiVersion, req.Kind, ConfirmManagementTransitionRequestKind, req.Actor); err != nil {
		return nil, err
	}
	if err := s.validateEnrolledTarget(req.ExpectedEnrollmentId, req.ExpectedInventoryNodeName, req.ExpectedMachineId); err != nil {
		return nil, err
	}
	authority, err := peerAuthority(ctx)
	if err != nil {
		return nil, status.Error(codes.PermissionDenied, err.Error())
	}
	if authority != strings.TrimSpace(req.TargetAuthority) {
		return nil, status.Errorf(codes.PermissionDenied, "client certificate is issued by authority %s, not %s", authority, req.TargetAuthority)
	}

	result, err := nodeidentity.ConfirmManagementTrial(s.Root, authority, s.trialClock())
	if errors.Is(err, nodeidentity.ErrManagementTrialReverted) {
		// The listener re-serves the installed mode once this response is sent.
		s.servedTrial.end(result.ID, false)
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	if result.ID != "" {
		s.servedTrial.end(result.ID, true)
		// The revert ignores a committed trial, so a timer left behind only
		// costs a no-op run.
		if stopResult := s.runManagementTrial(ctx, []string{"systemctl", "stop", managementTrialRevertUnitPrefix + result.ID + ".timer"}); stopResult.Err != nil || stopResult.ExitStatus != 0 {
			log.Printf("stop management trial %s revert timer: %s", result.ID, toolFailure(stopResult))
		}
	}
	return managementTransitionStatus(result), nil
}

// armManagementRevert schedules the trial's revert with systemd so it runs even
// if katlc-agent is stopped or stuck when the deadline passes.
func (s *Server) armManagementRevert(ctx context.Context, trial nodeidentity.ManagementTrial) error {
	seconds := int64(math.Ceil(trial.Deadline.Sub(trial.StartedAt).Seconds()))
	result := s.runManagementTrial(ctx, []string{
		"systemd-run", "--unit=" + managementTrialRevertUnitPrefix + trial.ID, "--collect", "--timer-property=AccuracySec=1s", "--on-active=" + strconv.FormatInt(seconds, 10) + "s",
		"/usr/bin/katlc", "agent", "management-trial", "revert", "--trial=" + trial.ID, "--reason=" + managementTrialDeadlineReason,
	})
	if result.Err != nil || result.ExitStatus != 0 {
		return errors.New(toolFailure(result))
	}
	return nil
}

// scheduleTrialRestart has systemd restart katlc-agent shortly after this
// response is sent, rather than the agent stopping itself mid-request, so the
// restarted agent serves the trial. The restart unit is named for the trial,
// so a retried begin reuses a restart that is still pending instead of failing
// over it.
func (s *Server) scheduleTrialRestart(ctx context.Context, trialID string) error {
	unit := managementTrialRestartUnitPrefix + trialID
	if s.trialRestartPending(ctx, unit) {
		return nil
	}
	result := s.runManagementTrial(ctx, []string{"systemd-run", "--unit=" + unit, "--collect", "--on-active=2s", "systemctl", "restart", "katlc-agent.service"})
	if result.Err == nil && result.ExitStatus == 0 {
		return nil
	}
	if s.trialRestartPending(ctx, unit) {
		return nil
	}
	return errors.New(toolFailure(result))
}

// trialRestartPending reports whether the restart unit is waiting to fire or
// already restarting katlc-agent.
func (s *Server) trialRestartPending(ctx context.Context, unit string) bool {
	result := s.runManagementTrial(ctx, []string{"systemctl", "is-active", "--quiet", unit + ".timer", unit + ".service"})
	return result.Err == nil && result.ExitStatus == 0
}

func (s *Server) runManagementTrial(ctx context.Context, argv []string) ToolResult {
	if s.RunManagementTrial == nil {
		return ToolResult{Err: errors.New("management trial runner is not configured")}
	}
	return s.RunManagementTrial(ctx, argv, nil)
}

func (s *Server) trialClock() nodeidentity.TrialClock {
	if s.ManagementTrialClock != nil {
		return s.ManagementTrialClock
	}
	return nodeidentity.SystemTrialClock{}
}

func validateManagementTransitionRequest(apiVersion, kind, wantKind, actor string) error {
	if apiVersion != APIVersion {
		return status.Errorf(codes.InvalidArgument, "apiVersion must be %q", APIVersion)
	}
	if kind != wantKind {
		return status.Errorf(codes.InvalidArgument, "kind must be %q", wantKind)
	}
	if strings.TrimSpace(actor) == "" {
		return status.Error(codes.InvalidArgument, "actor is required")
	}
	return nil
}

// peerAuthority returns the fingerprint of the CA that issued the verified
// client certificate on this connection.
func peerAuthority(ctx context.Context) (string, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return "", fmt.Errorf("confirm the management transition over mTLS with the replacement client certificate")
	}
	info, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(info.State.VerifiedChains) == 0 || len(info.State.VerifiedChains[0]) == 0 {
		return "", fmt.Errorf("confirm the management transition over mTLS with the replacement client certificate")
	}
	chain := info.State.VerifiedChains[0]
	sum := sha256.Sum256(chain[len(chain)-1].Raw)
	return hex.EncodeToString(sum[:]), nil
}

func managementTransitionStatus(trial nodeidentity.ManagementTrial) *agentapi.ManagementTransition {
	result := &agentapi.ManagementTransition{
		State:           string(trial.State),
		TrialId:         trial.ID,
		TargetAuthority: trial.TargetAuthority,
		RevertReason:    trial.RevertReason,
	}
	if trial.State == nodeidentity.ManagementTrialLive {
		result.Deadline = formatTime(trial.Deadline)
	}
	return result
}

func nodeManagementStatus(root string) (*agentapi.ManagementStatus, error) {
	mode, err := nodeidentity.ManagementAuthentication(root)
	if err != nil {
		return nil, err
	}
	result := &agentapi.ManagementStatus{Authentication: string(mode)}
	if mode == managementidentity.MutualTLS {
		// A node without installed credentials has no authority to report.
		authority, err := nodeidentity.InstalledManagementAuthority(root)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		result.Authority = authority
	}
	// An unreadable record must not hide the rest of node status.
	trial, found, err := nodeidentity.ReadManagementTrial(root)
	if err != nil {
		log.Printf("read management trial: %v", err)
		result.Transition = &agentapi.ManagementTransition{State: "unreadable"}
		return result, nil
	}
	if found {
		result.Transition = managementTransitionStatus(trial)
	}
	return result, nil
}
