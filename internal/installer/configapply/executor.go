package configapply

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/installer/bgpapivip"
	"github.com/katl-dev/katl/internal/installer/generation"
)

type Command struct {
	Name                string
	Argv                []string
	Timeout             time.Duration
	SuccessExitStatuses []int
}

type CommandResult struct {
	ExitStatus int
	Stdout     string
	Stderr     string
}

type CommandRunner interface {
	Run(ctx context.Context, command Command) (CommandResult, error)
}

type ConfextActivator interface {
	Activate(ctx context.Context, record generation.Record) error
	Rollback(ctx context.Context, targetGenerationID string) error
}

type Executor struct {
	Root           string
	Runner         CommandRunner
	Activator      ConfextActivator
	StatusPath     string
	ActionCommands map[string][]Command
	Timeout        time.Duration
	Now            func() time.Time
}

func (e Executor) ExecuteLive(ctx context.Context, plan Result) (generation.ConfigApplyStatus, error) {
	if plan.Decision.AcceptedMode != generation.ApplyModeLive {
		return plan.Status, fmt.Errorf("config apply executor requires accepted live plan, got %q", plan.Decision.AcceptedMode)
	}
	if plan.GenerationRecord.ConfigApply == nil {
		return plan.Status, errors.New("config apply executor requires runtime config generation metadata")
	}
	if e.Runner == nil {
		return plan.Status, errors.New("config apply executor requires command runner")
	}
	if e.Activator == nil {
		return plan.Status, errors.New("config apply executor requires confext activator")
	}
	status := plan.Status
	if len(status.DomainActions) == 0 {
		status.DomainActions = domainActions(generation.ApplyModeLive, plan.Decision.ChangedDomains)
	}
	if err := e.preflight(status.DomainActions); err != nil {
		status, markErr := generation.MarkConfigApplyFailed(status, err, e.now())
		if markErr != nil {
			return status, markErr
		}
		_ = e.writeStatus(status)
		return status, err
	}
	if containsDomainAction(status.DomainActions, DomainControlPlaneEndpointRouting) {
		enabled, err := e.endpointAdvertisementEnabled(plan.GenerationRecord)
		if err != nil {
			return e.failBeforeActivation(status, err)
		}
		if !enabled {
			return e.failBeforeActivation(status, errors.New("control-plane endpoint routing cannot be applied because VIP advertisement is not enabled"))
		}
	}

	var err error
	status, err = generation.MarkConfigApplyPhase(status, generation.ConfigApplyPhaseActivating, e.now())
	if err != nil {
		return status, err
	}
	if err := e.writeStatus(status); err != nil {
		return status, err
	}

	if err := e.Activator.Activate(ctx, plan.GenerationRecord); err != nil {
		return e.failAndRollback(ctx, status, plan, fmt.Errorf("activate selected confext: %w", err), false)
	}
	if err := e.refreshConfext(ctx); err != nil {
		return e.failAndRollback(ctx, status, plan, err, false)
	}

	if err := e.runActions(ctx, &status); err != nil {
		return e.failAndRollback(ctx, status, plan, err, true)
	}

	status, err = generation.MarkConfigApplyPhase(status, generation.ConfigApplyPhaseActive, e.now())
	if err != nil {
		return status, err
	}
	if err := e.writeStatus(status); err != nil {
		return status, err
	}
	return status, nil
}

func (e Executor) failBeforeActivation(status generation.ConfigApplyStatus, cause error) (generation.ConfigApplyStatus, error) {
	status, err := generation.MarkConfigApplyFailed(status, cause, e.now())
	if err != nil {
		return status, err
	}
	if err := e.writeStatus(status); err != nil {
		return status, err
	}
	return status, cause
}

func containsDomainAction(actions []generation.ConfigApplyDomainAction, domain string) bool {
	for _, action := range actions {
		if action.Domain == domain {
			return true
		}
	}
	return false
}

func (e Executor) endpointAdvertisementEnabled(record generation.Record) (bool, error) {
	for _, candidate := range record.Confexts {
		if candidate.Name != "katl-node" {
			continue
		}
		root := filepath.Clean(e.Root)
		if strings.TrimSpace(e.Root) == "" {
			root = string(filepath.Separator)
		}
		path := filepath.Join(root, strings.TrimPrefix(candidate.Path, "/"), strings.TrimPrefix(bgpapivip.AdvertisementEnabledPath, "/"))
		info, err := os.Stat(path)
		switch {
		case err == nil:
			return info.Mode().IsRegular(), nil
		case errors.Is(err, os.ErrNotExist):
			return false, nil
		default:
			return false, fmt.Errorf("inspect VIP advertisement configuration: %w", err)
		}
	}
	return false, nil
}

func (e Executor) runActions(ctx context.Context, status *generation.ConfigApplyStatus) error {
	kubeletRebound := false
	for i := range status.DomainActions {
		action := &status.DomainActions[i]
		if action.Status == generation.ConfigApplyActionSkipped {
			continue
		}
		if kubeadmInputDomain(action.Domain) && kubeletRebound {
			action.Status = generation.ConfigApplyActionPassed
			action.Diagnostic = ""
			if err := e.writeStatus(*status); err != nil {
				return err
			}
			continue
		}
		commands, err := e.commandsForDomain(action.Domain)
		if err != nil {
			action.Status = generation.ConfigApplyActionFailed
			action.Diagnostic = generation.RedactConfigApplyMessage(err.Error())
			_ = e.writeStatus(*status)
			return err
		}
		for _, command := range commands {
			result, err := e.Runner.Run(ctx, command)
			if err != nil {
				action.Status = generation.ConfigApplyActionFailed
				action.Diagnostic = generation.RedactConfigApplyMessage(err.Error())
				_ = e.writeStatus(*status)
				return fmt.Errorf("%s: %w", command.Name, err)
			}
			if !commandSucceeded(command, result) {
				err := commandFailure(command, result)
				action.Status = generation.ConfigApplyActionFailed
				action.Diagnostic = generation.RedactConfigApplyMessage(err.Error())
				_ = e.writeStatus(*status)
				return err
			}
		}
		if kubeadmInputDomain(action.Domain) {
			kubeletRebound = true
		}
		action.Status = generation.ConfigApplyActionPassed
		action.Diagnostic = ""
		if err := e.writeStatus(*status); err != nil {
			return err
		}
	}
	return nil
}

func (e Executor) preflight(actions []generation.ConfigApplyDomainAction) error {
	if err := validateBoundedCommand(e.confextRefreshCommand()); err != nil {
		return err
	}
	for _, action := range actions {
		if action.Status == generation.ConfigApplyActionSkipped {
			continue
		}
		commands, err := e.commandsForDomain(action.Domain)
		if err != nil {
			return err
		}
		for _, command := range commands {
			if err := validateBoundedCommand(command); err != nil {
				return err
			}
		}
	}
	return nil
}

func (e Executor) confextRefreshCommand() Command {
	return Command{
		Name:    "systemd-confext-refresh",
		Argv:    []string{"systemd-confext", "refresh"},
		Timeout: e.timeout(),
	}
}

func (e Executor) refreshConfext(ctx context.Context) error {
	command := e.confextRefreshCommand()
	result, err := e.Runner.Run(ctx, command)
	if err != nil {
		return fmt.Errorf("%s: %w", command.Name, err)
	}
	if result.ExitStatus != 0 {
		return commandFailure(command, result)
	}
	return nil
}

func (e Executor) commandsForDomain(domain string) ([]Command, error) {
	if commands, ok := e.ActionCommands[domain]; ok {
		return withDefaults(commands, e.timeout()), nil
	}
	commands := []Command{{
		Name: "systemd-daemon-reload",
		Argv: []string{"systemctl", "daemon-reload"},
	}}
	switch domain {
	case DomainKubeadmConfig, DomainSelectedKubeadmConfig:
		commands = []Command{{
			Name:                "kubelet-config-watcher-rebind",
			Argv:                []string{"systemctl", "try-restart", "kubelet.service"},
			SuccessExitStatuses: []int{5},
		}}
	case DomainResolved:
		commands = append(commands, Command{Name: "systemd-resolved-reload", Argv: []string{"systemctl", "reload-or-restart", "systemd-resolved.service"}})
	case DomainSysctl:
		commands = append(commands, Command{Name: "systemd-sysctl", Argv: []string{"/usr/lib/systemd/systemd-sysctl", "/run/confexts/katl-node/etc/sysctl.d/90-katl.conf"}})
	case DomainTmpfiles:
		commands = append(commands, Command{Name: "systemd-tmpfiles", Argv: []string{"systemd-tmpfiles", "--create", "--remove"}})
	case DomainNetworkd:
		commands = append(commands, Command{Name: "networkctl-reload", Argv: []string{"networkctl", "reload"}})
	case DomainBootstrapNodeMetadata:
		commands = append(commands, Command{Name: "node-metadata-refresh", Argv: []string{"systemctl", "try-reload-or-restart", "katl-runtime-handoff-status.service"}})
	case DomainControlPlaneEndpointRouting:
		commands = append(commands,
			Command{Name: "endpoint-routing-validate", Argv: []string{"/usr/bin/bird", "-p", "-c", bgpapivip.BirdConfigPath}},
			Command{Name: "endpoint-withdraw", Argv: []string{"systemctl", "stop", "katl-app-bgp-api-vip.service"}},
			Command{Name: "endpoint-routing-reload", Argv: []string{"/usr/bin/birdc", "-s", bgpapivip.BirdControlSocketPath, "configure"}},
			Command{Name: "endpoint-resume", Argv: []string{"systemctl", "start", "katl-app-bgp-api-vip.service"}},
		)
	default:
		return nil, fmt.Errorf("domain %q has no bounded live executor action", domain)
	}
	return withDefaults(commands, e.timeout()), nil
}

func (e Executor) failAndRollback(ctx context.Context, status generation.ConfigApplyStatus, plan Result, cause error, replayActions bool) (generation.ConfigApplyStatus, error) {
	status, err := generation.MarkConfigApplyFailed(status, cause, e.now())
	if err != nil {
		return status, err
	}
	if writeErr := e.writeStatus(status); writeErr != nil {
		return status, writeErr
	}
	target := plan.GenerationRecord.ConfigApply.PreviousGeneration
	status.Phase = generation.ConfigApplyPhaseRollingBack
	status.UpdatedAt = e.now().UTC()
	if writeErr := e.writeStatus(status); writeErr != nil {
		return status, writeErr
	}
	if rollbackErr := e.Activator.Rollback(ctx, target); rollbackErr != nil {
		return e.markRollbackFailed(status, target, cause, rollbackErr)
	}
	if refreshErr := e.refreshConfext(ctx); refreshErr != nil {
		return e.markRollbackFailed(status, target, cause, refreshErr)
	}
	if replayActions {
		if replayErr := e.replayRollbackActions(ctx, status.DomainActions); replayErr != nil {
			return e.markRollbackFailed(status, target, cause, replayErr)
		}
	}
	status, err = generation.MarkConfigApplyRollback(status, target, generation.ConfigApplyActionPassed, cause.Error(), e.now())
	if err != nil {
		return status, err
	}
	if writeErr := e.writeStatus(status); writeErr != nil {
		return status, writeErr
	}
	return status, cause
}

func (e Executor) replayRollbackActions(ctx context.Context, actions []generation.ConfigApplyDomainAction) error {
	kubeletRebound := false
	for _, action := range actions {
		if action.Status == generation.ConfigApplyActionSkipped {
			continue
		}
		if kubeadmInputDomain(action.Domain) && kubeletRebound {
			continue
		}
		commands, err := e.commandsForDomain(action.Domain)
		if err != nil {
			return err
		}
		for _, command := range commands {
			result, err := e.Runner.Run(ctx, command)
			if err != nil {
				return fmt.Errorf("rollback %s: %w", command.Name, err)
			}
			if !commandSucceeded(command, result) {
				return fmt.Errorf("rollback %w", commandFailure(command, result))
			}
		}
		if kubeadmInputDomain(action.Domain) {
			kubeletRebound = true
		}
	}
	return nil
}

func kubeadmInputDomain(domain string) bool {
	return domain == DomainKubeadmConfig || domain == DomainSelectedKubeadmConfig
}

func (e Executor) markRollbackFailed(status generation.ConfigApplyStatus, target string, cause error, rollbackErr error) (generation.ConfigApplyStatus, error) {
	status.Phase = generation.ConfigApplyPhaseFailed
	status.Rollback = &generation.ConfigApplyRollback{
		TargetGenerationID: target,
		Result:             generation.ConfigApplyActionFailed,
		Reason:             generation.RedactConfigApplyMessage(rollbackErr.Error()),
	}
	status.UpdatedAt = e.now().UTC()
	if writeErr := e.writeStatus(status); writeErr != nil {
		return status, writeErr
	}
	return status, fmt.Errorf("%w; rollback failed: %w", cause, rollbackErr)
}

func (e Executor) writeStatus(status generation.ConfigApplyStatus) error {
	if strings.TrimSpace(e.StatusPath) == "" {
		return nil
	}
	return generation.WriteConfigApplyStatus(e.StatusPath, status)
}

func (e Executor) timeout() time.Duration {
	if e.Timeout > 0 {
		return e.Timeout
	}
	return 30 * time.Second
}

func (e Executor) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now().UTC()
}

func withDefaults(commands []Command, timeout time.Duration) []Command {
	copied := make([]Command, 0, len(commands))
	for _, command := range commands {
		if command.Timeout == 0 {
			command.Timeout = timeout
		}
		copied = append(copied, command)
	}
	return copied
}

func validateBoundedCommand(command Command) error {
	if len(command.Argv) == 0 {
		return fmt.Errorf("bounded live action %q argv is required", command.Name)
	}
	program := filepath.Base(command.Argv[0])
	if forbiddenPrograms[program] {
		return fmt.Errorf("bounded live action %q may not run %s", command.Name, program)
	}
	for _, arg := range command.Argv {
		if strings.Contains(filepath.ToSlash(arg), "/etc/kubernetes") {
			return fmt.Errorf("bounded live action %q may not mutate /etc/kubernetes", command.Name)
		}
	}
	return nil
}

func commandFailure(command Command, result CommandResult) error {
	output := strings.TrimSpace(result.Stderr)
	if output == "" {
		output = strings.TrimSpace(result.Stdout)
	}
	if output == "" {
		output = fmt.Sprintf("exit status %d", result.ExitStatus)
	}
	return fmt.Errorf("%s exited %d: %s", command.Name, result.ExitStatus, generation.RedactConfigApplyMessage(output))
}

func commandSucceeded(command Command, result CommandResult) bool {
	if result.ExitStatus == 0 {
		return true
	}
	for _, status := range command.SuccessExitStatuses {
		if result.ExitStatus == status {
			return true
		}
	}
	return false
}

var forbiddenPrograms = map[string]bool{
	"apt":           true,
	"apt-get":       true,
	"apk":           true,
	"argocd":        true,
	"calicoctl":     true,
	"cilium":        true,
	"dnf":           true,
	"flux":          true,
	"helm":          true,
	"kubeadm":       true,
	"kubectl":       true,
	"nix":           true,
	"nix-env":       true,
	"nixos-rebuild": true,
	"pacman":        true,
	"rpm":           true,
	"yum":           true,
	"zypper":        true,
}
