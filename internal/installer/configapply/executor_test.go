package configapply

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zariel/katl/internal/installer/generation"
)

func TestExecutorActivatesSelectedConfextAndRecordsSuccess(t *testing.T) {
	plan := liveExecutorPlan(t, []Change{{Domain: DomainSysctl}})
	statusPath := filepath.Join(t.TempDir(), "config-apply-status.json")
	activator := &fakeActivator{}
	runner := &fakeCommandRunner{}

	status, err := Executor{
		Runner:     runner,
		Activator:  activator,
		StatusPath: statusPath,
		Now:        fixedNow,
	}.ExecuteLive(context.Background(), plan)
	if err != nil {
		t.Fatalf("ExecuteLive() error = %v", err)
	}
	if status.Phase != generation.ConfigApplyPhaseActive {
		t.Fatalf("phase = %q, want active", status.Phase)
	}
	if activator.activated != plan.GenerationRecord.GenerationID || activator.rollbackTarget != "" {
		t.Fatalf("activator = %#v", activator)
	}
	for _, action := range status.DomainActions {
		if action.Status != generation.ConfigApplyActionPassed {
			t.Fatalf("action = %#v, want passed", action)
		}
	}
	if got, want := strings.Join(runner.commandNames(), ","), "systemd-daemon-reload,systemd-sysctl"; got != want {
		t.Fatalf("commands = %q, want %q", got, want)
	}
	persisted, err := generation.ReadConfigApplyStatus(statusPath)
	if err != nil {
		t.Fatalf("ReadConfigApplyStatus() error = %v", err)
	}
	if persisted.Phase != generation.ConfigApplyPhaseActive || persisted.DomainActions[0].Status != generation.ConfigApplyActionPassed {
		t.Fatalf("persisted status = %#v", persisted)
	}
}

func TestExecutorFailureRecordsRollbackAndRedactsStatus(t *testing.T) {
	plan := liveExecutorPlan(t, []Change{{Domain: DomainSysctl}})
	statusPath := filepath.Join(t.TempDir(), "config-apply-status.json")
	activator := &fakeActivator{}
	secret := "abcdef.0123456789abcdef"
	runner := &fakeCommandRunner{
		results: map[string]CommandResult{
			"systemd-sysctl": {ExitStatus: 1, Stderr: "failed with token " + secret},
		},
	}

	status, err := Executor{
		Runner:     runner,
		Activator:  activator,
		StatusPath: statusPath,
		Now:        fixedNow,
	}.ExecuteLive(context.Background(), plan)
	if err == nil {
		t.Fatalf("ExecuteLive() error = nil, status = %#v", status)
	}
	if status.Phase != generation.ConfigApplyPhaseRolledBack || status.Rollback == nil {
		t.Fatalf("status = %#v, want rolled-back with rollback record", status)
	}
	if activator.rollbackTarget != "2026.06.05-001" {
		t.Fatalf("rollback target = %q", activator.rollbackTarget)
	}
	if strings.Contains(status.FailureReason, secret) || strings.Contains(status.DomainActions[0].Diagnostic, secret) {
		t.Fatalf("status leaked secret: %#v", status)
	}
	if !strings.Contains(status.FailureReason, "[REDACTED BOOTSTRAP TOKEN]") {
		t.Fatalf("failure reason = %q, want redacted token marker", status.FailureReason)
	}
	if got, want := strings.Join(runner.commandNames(), ","), "systemd-daemon-reload,systemd-sysctl,systemd-daemon-reload,systemd-sysctl"; got != want {
		t.Fatalf("commands = %q, want failed apply followed by rollback replay %q", got, want)
	}
	data, err := os.ReadFile(statusPath)
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	if strings.Contains(string(data), secret) {
		t.Fatalf("persisted status leaked secret:\n%s", data)
	}
}

func TestExecutorRecordsRollbackFailure(t *testing.T) {
	plan := liveExecutorPlan(t, []Change{{Domain: DomainSysctl}})
	activator := &fakeActivator{rollbackErr: errors.New("rollback failed with Bearer secret-token")}
	runner := &fakeCommandRunner{
		results: map[string]CommandResult{
			"systemd-sysctl": {ExitStatus: 1, Stderr: "sysctl failed"},
		},
	}

	status, err := Executor{
		Runner:    runner,
		Activator: activator,
		Now:       fixedNow,
	}.ExecuteLive(context.Background(), plan)
	if err == nil {
		t.Fatalf("ExecuteLive() error = nil, status = %#v", status)
	}
	if status.Phase != generation.ConfigApplyPhaseFailed || status.Rollback == nil || status.Rollback.Result != generation.ConfigApplyActionFailed {
		t.Fatalf("rollback failure status = %#v", status)
	}
	if strings.Contains(status.Rollback.Reason, "secret-token") || !strings.Contains(status.Rollback.Reason, "Bearer [REDACTED]") {
		t.Fatalf("rollback reason was not redacted: %q", status.Rollback.Reason)
	}
}

func TestExecutorRecordsRollbackReplayFailure(t *testing.T) {
	plan := liveExecutorPlan(t, []Change{{Domain: DomainSysctl}})
	runner := &fakeCommandRunner{
		results: map[string]CommandResult{
			"systemd-sysctl": {ExitStatus: 1, Stderr: "sysctl failed"},
		},
		errs: map[string]error{
			"systemd-daemon-reload": errors.New("daemon reload failed with Bearer secret-token"),
		},
	}

	status, err := Executor{
		Runner:    runner,
		Activator: &fakeActivator{},
		Now:       fixedNow,
	}.ExecuteLive(context.Background(), plan)
	if err == nil {
		t.Fatalf("ExecuteLive() error = nil, status = %#v", status)
	}
	if status.Phase != generation.ConfigApplyPhaseFailed || status.Rollback == nil || status.Rollback.Result != generation.ConfigApplyActionFailed {
		t.Fatalf("rollback replay failure status = %#v", status)
	}
	if strings.Contains(status.Rollback.Reason, "secret-token") || !strings.Contains(status.Rollback.Reason, "Bearer [REDACTED]") {
		t.Fatalf("rollback reason was not redacted: %q", status.Rollback.Reason)
	}
	if got, want := strings.Join(runner.commandNames(), ","), "systemd-daemon-reload,systemd-daemon-reload"; got != want {
		t.Fatalf("commands = %q, want failed apply followed by rollback replay %q", got, want)
	}
}

func TestExecutorRefusesForbiddenLiveActionsBeforeActivation(t *testing.T) {
	tests := []struct {
		name string
		argv []string
		want string
	}{
		{name: "kubeadm", argv: []string{"kubeadm", "init"}, want: "kubeadm"},
		{name: "kubectl", argv: []string{"kubectl", "apply", "-f", "x.yaml"}, want: "kubectl"},
		{name: "package manager", argv: []string{"apt-get", "install", "curl"}, want: "apt-get"},
		{name: "cni installer", argv: []string{"cilium", "install"}, want: "cilium"},
		{name: "gitops controller", argv: []string{"flux", "reconcile", "source", "git", "cluster"}, want: "flux"},
		{name: "etc kubernetes", argv: []string{"systemctl", "cat", "/etc/kubernetes/admin.conf"}, want: "/etc/kubernetes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := liveExecutorPlan(t, []Change{{Domain: DomainSysctl}})
			activator := &fakeActivator{}
			runner := &fakeCommandRunner{}
			status, err := Executor{
				Runner:    runner,
				Activator: activator,
				ActionCommands: map[string][]Command{
					DomainSysctl: {{Name: tt.name, Argv: tt.argv}},
				},
				Now: fixedNow,
			}.ExecuteLive(context.Background(), plan)
			if err == nil {
				t.Fatalf("ExecuteLive() error = nil, status = %#v", status)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %q, want %q", err, tt.want)
			}
			if status.Phase != generation.ConfigApplyPhaseFailed {
				t.Fatalf("status phase = %q, want failed", status.Phase)
			}
			if activator.activated != "" || len(runner.commands) != 0 {
				t.Fatalf("forbidden action reached activation or runner: activator=%#v commands=%#v", activator, runner.commands)
			}
		})
	}
}

func TestExecutorRejectsNonLivePlans(t *testing.T) {
	plan, err := PlanChange(currentRecord(), NodeConfigurationChange{
		APIVersion:       generation.APIVersion,
		Kind:             NodeConfigurationChangeKind,
		GenerationID:     "2026.06.05-002",
		SourceDigest:     strings.Repeat("d", 64),
		Apply:            Apply{Mode: generation.ApplyModeNextBoot},
		Changes:          []Change{{Domain: DomainNodeIdentity}},
		GeneratedConfext: candidateConfext("2026.06.05-002"),
	})
	if err != nil {
		t.Fatalf("PlanChange() error = %v", err)
	}
	status, err := Executor{
		Runner:    &fakeCommandRunner{},
		Activator: &fakeActivator{},
		Now:       fixedNow,
	}.ExecuteLive(context.Background(), plan)
	if err == nil {
		t.Fatalf("ExecuteLive() error = nil, status = %#v", status)
	}
	if !strings.Contains(err.Error(), "accepted live plan") {
		t.Fatalf("error = %q", err)
	}
}

func liveExecutorPlan(t *testing.T, changes []Change) Result {
	t.Helper()
	plan, err := PlanChange(currentRecord(), NodeConfigurationChange{
		APIVersion:       generation.APIVersion,
		Kind:             NodeConfigurationChangeKind,
		GenerationID:     "2026.06.05-002",
		SourceDigest:     strings.Repeat("d", 64),
		Apply:            Apply{Mode: generation.ApplyModeLive},
		Changes:          changes,
		GeneratedConfext: candidateConfext("2026.06.05-002"),
		RequestedAt:      fixedNow(),
	})
	if err != nil {
		t.Fatalf("PlanChange() error = %v, diagnostics = %#v", err, plan.Decision.Diagnostics)
	}
	return plan
}

type fakeActivator struct {
	activated      string
	rollbackTarget string
	activateErr    error
	rollbackErr    error
}

func (a *fakeActivator) Activate(_ context.Context, record generation.Record) error {
	a.activated = record.GenerationID
	return a.activateErr
}

func (a *fakeActivator) Rollback(_ context.Context, targetGenerationID string) error {
	a.rollbackTarget = targetGenerationID
	return a.rollbackErr
}

type fakeCommandRunner struct {
	commands []Command
	results  map[string]CommandResult
	errs     map[string]error
}

func (r *fakeCommandRunner) Run(_ context.Context, command Command) (CommandResult, error) {
	r.commands = append(r.commands, command)
	if err := r.errs[command.Name]; err != nil {
		return CommandResult{}, err
	}
	if result, ok := r.results[command.Name]; ok {
		delete(r.results, command.Name)
		return result, nil
	}
	return CommandResult{ExitStatus: 0}, nil
}

func (r *fakeCommandRunner) commandNames() []string {
	names := make([]string, 0, len(r.commands))
	for _, command := range r.commands {
		names = append(names, command.Name)
	}
	return names
}

func fixedNow() time.Time {
	return time.Date(2026, 6, 5, 17, 0, 0, 0, time.UTC)
}
