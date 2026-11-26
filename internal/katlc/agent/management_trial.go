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

	managementTrialRevertUnitPrefix = "katl-management-revert-"

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

// managementListener tracks the management trial the current listener serves
// and wakes the listener loop when management state changes, so one loop owns
// every switch between the installed mode and a trial.
type managementListener struct {
	mu        sync.Mutex
	id        string
	committed chan struct{}
	changed   chan struct{}
}

// serve records the trial a new listener serves, if any, and returns a channel
// closed when it is committed while served.
func (l *managementListener) serve(trial *nodeidentity.ManagementTrial) <-chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.id = ""
	if trial != nil {
		l.id = trial.ID
	}
	l.committed = make(chan struct{})
	return l.committed
}

func (l *managementListener) servedID() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.id
}

// changes is signalled when the listener must select its credentials again.
func (l *managementListener) changes() chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.changed == nil {
		// One pending signal is enough: the loop reads management state after
		// taking it.
		l.changed = make(chan struct{}, 1)
	}
	return l.changed
}

// reselect asks the listener loop to serve whatever management state now
// selects.
func (l *managementListener) reselect() {
	select {
	case l.changes() <- struct{}{}:
	default:
	}
}

// drain discards a pending reselect before the loop reads management state;
// a change signalled after that read is kept.
func (l *managementListener) drain() {
	select {
	case <-l.changes():
	default:
	}
}

// end reports that trial id was committed or reverted. It is ignored unless
// the current listener serves that trial.
func (l *managementListener) end(id string, committed bool) {
	l.mu.Lock()
	served := id != "" && id == l.id
	if served && committed && l.committed != nil {
		close(l.committed)
		l.committed = nil
	}
	l.mu.Unlock()
	if served && !committed {
		l.reselect()
	}
}

// trialRemaining is read before the listener serves a trial, so the first
// deadline check happens before any client can reach it.
func trialRemaining(trial nodeidentity.ManagementTrial, clock nodeidentity.TrialClock) time.Duration {
	sinceBoot, err := clock.SinceBoot()
	if err != nil {
		log.Printf("read boot time for management trial %s: %v; reverting", trial.ID, err)
		return 0
	}
	return trial.DeadlineSinceBoot - sinceBoot
}

// awaitTrialDeadline reports whether the served trial reached its
// CLOCK_BOOTTIME deadline before it was committed. It enforces the deadline
// independently of the systemd revert timer, so either alone stops the trial.
func (s *Server) awaitTrialDeadline(ctx context.Context, trial nodeidentity.ManagementTrial, clock nodeidentity.TrialClock, remaining time.Duration, committed <-chan struct{}) bool {
	poll := s.managementTrialPoll
	if poll <= 0 {
		poll = defaultManagementTrialPoll
	}
	for remaining > 0 {
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
		case <-wait.C:
		}
		remaining = trialRemaining(trial, clock)
	}
	return true
}

// refuseDuringManagementTrial holds off mutating requests while a management
// trial is live: its commit or revert changes which clients reach the node,
// and a reboot reverts it.
func (s *Server) refuseDuringManagementTrial() error {
	trial, found, err := nodeidentity.ReadManagementTrial(s.Root)
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "%v; clear it with katlc agent management-trial revert on the node", err)
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
	// operations submitted during it.
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
	if trial.State == nodeidentity.ManagementTrialLive && trial.ID != s.management.servedID() {
		// The listener loop waits for this response before it re-serves.
		s.management.reselect()
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
		s.management.end(result.ID, false)
		s.stopTrialRevertTimer(ctx, result.ID)
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	if result.ID != "" {
		s.management.end(result.ID, true)
		s.stopTrialRevertTimer(ctx, result.ID)
	}
	return managementTransitionStatus(result), nil
}

// stopTrialRevertTimer cancels an ended trial's revert. The revert ignores a
// trial that is no longer live, so a timer left behind only costs a no-op run.
func (s *Server) stopTrialRevertTimer(ctx context.Context, id string) {
	if id == "" {
		return
	}
	if result := s.runManagementTrial(ctx, []string{"systemctl", "stop", managementTrialRevertUnitPrefix + id + ".timer"}); result.Err != nil || result.ExitStatus != 0 {
		log.Printf("stop management trial %s revert timer: %s", id, toolFailure(result))
	}
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
