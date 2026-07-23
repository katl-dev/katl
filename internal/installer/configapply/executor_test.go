package configapply

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/installer/generation"
)

func TestExecutorActivatesSelectedConfextAndRecordsSuccess(t *testing.T) {
	plan := liveExecutorPlan(t, []Change{{Domain: DomainSysctl}})
	statusPath := executorStatusPath(t, plan)
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
	if got, want := strings.Join(runner.commandNames(), ","), "systemd-confext-refresh,systemd-daemon-reload,systemd-sysctl"; got != want {
		t.Fatalf("commands = %q, want %q", got, want)
	}
	if got, want := runner.commands[2].Argv, []string{"/usr/lib/systemd/systemd-sysctl", "/run/confexts/katl-node/etc/sysctl.d/90-katl.conf"}; strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("systemd-sysctl argv = %#v, want %#v", got, want)
	}
	persisted, err := generation.ReadConfigApplyStatus(statusPath)
	if err != nil {
		t.Fatalf("ReadConfigApplyStatus() error = %v", err)
	}
	if persisted.Phase != generation.ConfigApplyPhaseActive || persisted.DomainActions[0].Status != generation.ConfigApplyActionPassed {
		t.Fatalf("persisted status = %#v", persisted)
	}
}

func TestExecutorRebindsKubeletWatcherOnceForKubeadmInput(t *testing.T) {
	plan := liveExecutorPlan(t, []Change{
		{Domain: DomainKubeadmConfig},
		{Domain: DomainSelectedKubeadmConfig},
	})
	runner := &fakeCommandRunner{}

	status, err := Executor{
		Runner:    runner,
		Activator: &fakeActivator{},
		Now:       fixedNow,
	}.ExecuteLive(context.Background(), plan)
	if err != nil {
		t.Fatalf("ExecuteLive() error = %v", err)
	}
	if got, want := strings.Join(runner.commandNames(), ","), "systemd-confext-refresh,kubelet-config-watcher-rebind"; got != want {
		t.Fatalf("commands = %q, want %q", got, want)
	}
	if got, want := runner.commands[1].Argv, []string{"systemctl", "try-restart", "kubelet.service"}; strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("kubelet rebind argv = %#v, want %#v", got, want)
	}
	for _, action := range status.DomainActions {
		if action.Status != generation.ConfigApplyActionPassed {
			t.Fatalf("action = %#v, want passed", action)
		}
	}
}

func TestExecutorAllowsKubeadmInputBeforeKubeletUnitExists(t *testing.T) {
	plan := liveExecutorPlan(t, []Change{{Domain: DomainKubeadmConfig}})
	runner := &fakeCommandRunner{
		results: map[string]CommandResult{
			"kubelet-config-watcher-rebind": {
				ExitStatus: 5,
				Stderr:     "Failed to try-restart kubelet.service: Unit kubelet.service not found.",
			},
		},
	}
	activator := &fakeActivator{}

	status, err := Executor{
		Runner:    runner,
		Activator: activator,
		Now:       fixedNow,
	}.ExecuteLive(context.Background(), plan)
	if err != nil {
		t.Fatalf("ExecuteLive() error = %v", err)
	}
	if status.Phase != generation.ConfigApplyPhaseActive || status.DomainActions[0].Status != generation.ConfigApplyActionPassed {
		t.Fatalf("status = %#v, want active kubeadm input", status)
	}
	if activator.rollbackTarget != "" {
		t.Fatalf("rollback target = %q, want no rollback", activator.rollbackTarget)
	}
}

func TestExecutorFailureRecordsRollbackAndRedactsStatus(t *testing.T) {
	plan := liveExecutorPlan(t, []Change{{Domain: DomainSysctl}})
	statusPath := executorStatusPath(t, plan)
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
	if got, want := strings.Join(runner.commandNames(), ","), "systemd-confext-refresh,systemd-daemon-reload,systemd-sysctl,systemd-confext-refresh,systemd-daemon-reload,systemd-sysctl"; got != want {
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

func executorStatusPath(t *testing.T, plan Result) string {
	t.Helper()
	path, err := generation.ConfigApplyStatusPath(t.TempDir(), plan.GenerationRecord.GenerationID)
	if err != nil {
		t.Fatalf("ConfigApplyStatusPath() error = %v", err)
	}
	return path
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
	if got, want := strings.Join(runner.commandNames(), ","), "systemd-confext-refresh,systemd-daemon-reload,systemd-confext-refresh,systemd-daemon-reload"; got != want {
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

func TestExecutorDoesNotRunBirdWithoutVIPAdvertisement(t *testing.T) {
	root := t.TempDir()
	plan := liveExecutorPlan(t, []Change{{Domain: DomainControlPlaneEndpointRouting}})
	activator := &fakeActivator{}
	runner := &fakeCommandRunner{}

	status, err := Executor{
		Root:      root,
		Runner:    runner,
		Activator: activator,
		Now:       fixedNow,
	}.ExecuteLive(context.Background(), plan)
	if err == nil || !strings.Contains(err.Error(), "VIP advertisement is not enabled") {
		t.Fatalf("ExecuteLive() error = %v, want disabled advertisement diagnostic", err)
	}
	if status.Phase != generation.ConfigApplyPhaseFailed {
		t.Fatalf("status phase = %q, want failed", status.Phase)
	}
	if activator.activated != "" || len(runner.commands) != 0 {
		t.Fatalf("disabled advertisement reached activation or BIRD: activator=%#v commands=%#v", activator, runner.commands)
	}
}

func TestExecutorRunsBirdWhenVIPAdvertisementIsEnabled(t *testing.T) {
	root := t.TempDir()
	plan := liveExecutorPlan(t, []Change{{Domain: DomainControlPlaneEndpointRouting}})
	marker := filepath.Join(root, strings.TrimPrefix(plan.GenerationRecord.Confexts[0].Path, "/"), "etc/katl/apps/bgp-api-vip/advertisement-enabled")
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("enabled\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &fakeCommandRunner{}

	_, err := Executor{
		Root:      root,
		Runner:    runner,
		Activator: &fakeActivator{},
		Now:       fixedNow,
	}.ExecuteLive(context.Background(), plan)
	if err != nil {
		t.Fatalf("ExecuteLive() error = %v", err)
	}
	if got, want := strings.Join(runner.commandNames(), ","), "systemd-confext-refresh,systemd-daemon-reload,endpoint-routing-validate,endpoint-withdraw,endpoint-routing-reload,endpoint-resume"; got != want {
		t.Fatalf("commands = %q, want %q", got, want)
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
