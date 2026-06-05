package configapply

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/zariel/katl/internal/installer/generation"
)

type Command struct {
	Name    string
	Argv    []string
	Timeout time.Duration
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

	var err error
	status, err = generation.MarkConfigApplyPhase(status, generation.ConfigApplyPhaseActivating, e.now())
	if err != nil {
		return status, err
	}
	if err := e.writeStatus(status); err != nil {
		return status, err
	}

	if err := e.Activator.Activate(ctx, plan.GenerationRecord); err != nil {
		return e.failAndRollback(ctx, status, plan, fmt.Errorf("activate selected confext: %w", err))
	}

	if err := e.runActions(ctx, &status); err != nil {
		return e.failAndRollback(ctx, status, plan, err)
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

func (e Executor) runActions(ctx context.Context, status *generation.ConfigApplyStatus) error {
	for i := range status.DomainActions {
		action := &status.DomainActions[i]
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
			if result.ExitStatus != 0 {
				err := commandFailure(command, result)
				action.Status = generation.ConfigApplyActionFailed
				action.Diagnostic = generation.RedactConfigApplyMessage(err.Error())
				_ = e.writeStatus(*status)
				return err
			}
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
	for _, action := range actions {
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

func (e Executor) commandsForDomain(domain string) ([]Command, error) {
	if commands, ok := e.ActionCommands[domain]; ok {
		return withDefaults(commands, e.timeout()), nil
	}
	commands := []Command{{
		Name: "systemd-daemon-reload",
		Argv: []string{"systemctl", "daemon-reload"},
	}}
	switch domain {
	case DomainResolved:
		commands = append(commands, Command{Name: "systemd-resolved-reload", Argv: []string{"systemctl", "reload-or-restart", "systemd-resolved.service"}})
	case DomainSysctl:
		commands = append(commands, Command{Name: "systemd-sysctl", Argv: []string{"systemd-sysctl"}})
	case DomainTmpfiles:
		commands = append(commands, Command{Name: "systemd-tmpfiles", Argv: []string{"systemd-tmpfiles", "--create", "--remove"}})
	case DomainNetworkd:
		commands = append(commands, Command{Name: "networkctl-reload", Argv: []string{"networkctl", "reload"}})
	case DomainBootstrapNodeMetadata:
		commands = append(commands, Command{Name: "node-metadata-refresh", Argv: []string{"systemctl", "try-reload-or-restart", "katl-runtime-handoff-status.service"}})
	default:
		return nil, fmt.Errorf("domain %q has no bounded live executor action", domain)
	}
	return withDefaults(commands, e.timeout()), nil
}

func (e Executor) failAndRollback(ctx context.Context, status generation.ConfigApplyStatus, plan Result, cause error) (generation.ConfigApplyStatus, error) {
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
	status, err = generation.MarkConfigApplyRollback(status, target, generation.ConfigApplyActionPassed, cause.Error(), e.now())
	if err != nil {
		return status, err
	}
	if writeErr := e.writeStatus(status); writeErr != nil {
		return status, writeErr
	}
	return status, cause
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
