package configapply

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/installer/bgpapivip"
	"github.com/katl-dev/katl/internal/installer/generation"
	"github.com/katl-dev/katl/internal/installer/manifest"
)

type Command struct {
	Name                string
	EffectAction        string
	EffectTarget        string
	Argv                []string
	ExpectedStdout      string
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
	Root              string
	Runner            CommandRunner
	Activator         ConfextActivator
	StatusPath        string
	ActionCommands    map[string][]Command
	HostConfiguration *HostConfigurationChangePlan
	ApplyVolumes      func(context.Context, manifest.Manifest, manifest.Manifest) error
	Timeout           time.Duration
	Now               func() time.Time
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
	sysctlSnapshot, err := e.snapshotHostSysctls(ctx, status.DomainActions)
	if err != nil {
		return e.failBeforeActivation(status, err)
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
	if containsDomainAction(status.DomainActions, DomainVolumes) {
		if e.ApplyVolumes == nil {
			return e.failBeforeActivation(status, errors.New("live volume applicator is not configured"))
		}
		current, err := ReadGenerationManifest(e.Root, plan.GenerationRecord.ConfigApply.PreviousGeneration)
		if err != nil {
			return e.failBeforeActivation(status, fmt.Errorf("read current volume manifest: %w", err))
		}
		desired, err := ReadGenerationManifest(e.Root, plan.GenerationRecord.GenerationID)
		if err != nil {
			return e.failBeforeActivation(status, fmt.Errorf("read desired volume manifest: %w", err))
		}
		if err := e.ApplyVolumes(ctx, current, desired); err != nil {
			return e.failBeforeActivation(status, fmt.Errorf("prepare volumes: %w", err))
		}
	}

	status, err = generation.MarkConfigApplyPhase(status, generation.ConfigApplyPhaseActivating, e.now())
	if err != nil {
		return status, err
	}
	if err := e.writeStatus(status); err != nil {
		return status, err
	}

	if err := e.Activator.Activate(ctx, plan.GenerationRecord); err != nil {
		return e.failAndRollback(ctx, status, plan, fmt.Errorf("activate selected confext: %w", err), false, sysctlSnapshot)
	}
	if err := e.refreshConfext(ctx); err != nil {
		return e.failAndRollback(ctx, status, plan, err, false, sysctlSnapshot)
	}

	if err := e.runActions(ctx, &status); err != nil {
		return e.failAndRollback(ctx, status, plan, err, true, sysctlSnapshot)
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
		if !generation.IsGeneratedConfextName(candidate.Name) {
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
		if action.Domain == DomainHostConfiguration {
			if err := e.applyHostSysctls(ctx, action, status); err != nil {
				action.Status = generation.ConfigApplyActionFailed
				action.Diagnostic = generation.RedactConfigApplyMessage(err.Error())
				_ = e.writeStatus(*status)
				return err
			}
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
				markHostEffect(action, command, generation.ConfigApplyActionFailed, err)
				action.Status = generation.ConfigApplyActionFailed
				action.Diagnostic = generation.RedactConfigApplyMessage(err.Error())
				_ = e.writeStatus(*status)
				return fmt.Errorf("%s: %w", command.Name, err)
			}
			if !commandSucceeded(command, result) {
				err := commandFailure(command, result)
				markHostEffect(action, command, generation.ConfigApplyActionFailed, err)
				action.Status = generation.ConfigApplyActionFailed
				action.Diagnostic = generation.RedactConfigApplyMessage(err.Error())
				_ = e.writeStatus(*status)
				return err
			}
			markHostEffect(action, command, generation.ConfigApplyActionPassed, nil)
			if action.Domain == DomainHostConfiguration {
				if err := e.writeStatus(*status); err != nil {
					return err
				}
			}
		}
		if kubeadmInputDomain(action.Domain) {
			kubeletRebound = true
		}
		action.Status = generation.ConfigApplyActionPassed
		if action.Domain != DomainHostConfiguration {
			action.Diagnostic = ""
		}
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
	if domain == DomainHostConfiguration && e.HostConfiguration != nil {
		return withDefaults(e.HostConfiguration.Commands, e.timeout()), nil
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
	case DomainTmpfiles:
		commands = append(commands, Command{Name: "systemd-tmpfiles", Argv: []string{"systemd-tmpfiles", "--create", "--remove"}})
	case DomainBootstrapNodeMetadata:
		commands = append(commands, Command{Name: "node-metadata-refresh", Argv: []string{"systemctl", "try-reload-or-restart", "katl-runtime-handoff-status.service"}})
	case DomainVolumes:
		commands = append(commands, Command{Name: "volume-mount-activate", Argv: []string{"systemctl", "restart", "katl-volumes.target"}})
	case DomainControlPlaneEndpointRouting:
		commands = append(commands,
			Command{Name: "endpoint-routing-validate", Argv: []string{bgpapivip.BirdExecutablePath, "-p", "-c", bgpapivip.BirdConfigPath}},
			Command{Name: "endpoint-withdraw", Argv: []string{"systemctl", "stop", "katl-app-bgp-api-vip.service"}},
			Command{Name: "endpoint-link-reload", Argv: []string{"networkctl", "reload"}},
			Command{Name: "endpoint-routing-reload", Argv: []string{bgpapivip.BirdClientPath, "-s", bgpapivip.BirdControlSocketPath, "configure"}},
			Command{Name: "endpoint-resume", Argv: []string{"systemctl", "start", "katl-app-bgp-api-vip.service"}},
		)
	default:
		return nil, fmt.Errorf("domain %q has no bounded live executor action", domain)
	}
	return withDefaults(commands, e.timeout()), nil
}

func (e Executor) snapshotHostSysctls(ctx context.Context, actions []generation.ConfigApplyDomainAction) (map[string]string, error) {
	if !containsDomainAction(actions, DomainHostConfiguration) || e.HostConfiguration == nil || len(e.HostConfiguration.SysctlAssignments) == 0 {
		return nil, nil
	}
	snapshot := make(map[string]string, len(e.HostConfiguration.SysctlAssignments))
	for _, assignment := range e.HostConfiguration.SysctlAssignments {
		command := Command{
			Name:    "sysctl-snapshot-" + assignment.Key,
			Argv:    []string{"/usr/sbin/sysctl", "-n", assignment.Key},
			Timeout: e.timeout(),
		}
		if err := validateBoundedCommand(command); err != nil {
			return nil, err
		}
		result, err := e.Runner.Run(ctx, command)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", command.Name, err)
		}
		if !commandSucceeded(command, result) {
			return nil, commandFailure(command, result)
		}
		value := strings.TrimSpace(result.Stdout)
		if value == "" || strings.ContainsAny(value, "\x00\r\n") {
			return nil, fmt.Errorf("sysctl %q returned an unsafe runtime value", assignment.Key)
		}
		snapshot[assignment.Key] = value
	}
	return snapshot, nil
}

func (e Executor) applyHostSysctls(ctx context.Context, action *generation.ConfigApplyDomainAction, status *generation.ConfigApplyStatus) error {
	if e.HostConfiguration == nil {
		return nil
	}
	for _, assignment := range e.HostConfiguration.SysctlAssignments {
		apply := Command{
			Name:    "sysctl-apply-" + assignment.Key,
			Argv:    []string{"/usr/sbin/sysctl", "-w", assignment.Key + "=" + assignment.Value},
			Timeout: e.timeout(),
		}
		result, err := e.Runner.Run(ctx, apply)
		if err != nil {
			markEffect(action, "apply-and-verify", "sysctl "+assignment.Key, generation.ConfigApplyActionFailed, err)
			return fmt.Errorf("%s: %w", apply.Name, err)
		}
		if !commandSucceeded(apply, result) {
			err := commandFailure(apply, result)
			markEffect(action, "apply-and-verify", "sysctl "+assignment.Key, generation.ConfigApplyActionFailed, err)
			return err
		}
		verify := Command{
			Name:    "sysctl-verify-" + assignment.Key,
			Argv:    []string{"/usr/sbin/sysctl", "-n", assignment.Key},
			Timeout: e.timeout(),
		}
		result, err = e.Runner.Run(ctx, verify)
		if err != nil {
			markEffect(action, "apply-and-verify", "sysctl "+assignment.Key, generation.ConfigApplyActionFailed, err)
			return fmt.Errorf("%s: %w", verify.Name, err)
		}
		if !commandSucceeded(verify, result) {
			err := commandFailure(verify, result)
			markEffect(action, "apply-and-verify", "sysctl "+assignment.Key, generation.ConfigApplyActionFailed, err)
			return err
		}
		if strings.TrimSpace(result.Stdout) != assignment.Value {
			err := fmt.Errorf("sysctl %q verification returned %q, want %q", assignment.Key, strings.TrimSpace(result.Stdout), assignment.Value)
			markEffect(action, "apply-and-verify", "sysctl "+assignment.Key, generation.ConfigApplyActionFailed, err)
			return err
		}
		markEffect(action, "apply-and-verify", "sysctl "+assignment.Key, generation.ConfigApplyActionPassed, nil)
		if err := e.writeStatus(*status); err != nil {
			return err
		}
	}
	return nil
}

func markHostEffect(action *generation.ConfigApplyDomainAction, command Command, result string, cause error) {
	if action == nil || strings.TrimSpace(command.EffectAction) == "" || strings.TrimSpace(command.EffectTarget) == "" {
		return
	}
	markEffect(action, command.EffectAction, command.EffectTarget, result, cause)
}

func markEffect(action *generation.ConfigApplyDomainAction, effectAction, target, result string, cause error) {
	if action == nil {
		return
	}
	for i := range action.Effects {
		effect := &action.Effects[i]
		if effect.Action != effectAction || effect.Target != target {
			continue
		}
		effect.Status = result
		effect.Diagnostic = ""
		if cause != nil {
			effect.Diagnostic = generation.RedactConfigApplyMessage(cause.Error())
		}
		return
	}
}

func (e Executor) restoreHostSysctls(ctx context.Context, snapshot map[string]string) error {
	if len(snapshot) == 0 {
		return nil
	}
	keys := make([]string, 0, len(snapshot))
	for key := range snapshot {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		command := Command{
			Name:    "sysctl-restore-" + key,
			Argv:    []string{"/usr/sbin/sysctl", "-w", key + "=" + snapshot[key]},
			Timeout: e.timeout(),
		}
		result, err := e.Runner.Run(ctx, command)
		if err != nil {
			return fmt.Errorf("%s: %w", command.Name, err)
		}
		if !commandSucceeded(command, result) {
			return commandFailure(command, result)
		}
	}
	return nil
}

func (e Executor) failAndRollback(ctx context.Context, status generation.ConfigApplyStatus, plan Result, cause error, replayActions bool, sysctlSnapshot map[string]string) (generation.ConfigApplyStatus, error) {
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
		if replayErr := e.replayRollbackActions(ctx, status.DomainActions, sysctlSnapshot); replayErr != nil {
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

func (e Executor) replayRollbackActions(ctx context.Context, actions []generation.ConfigApplyDomainAction, sysctlSnapshot map[string]string) error {
	kubeletRebound := false
	for _, action := range actions {
		if action.Status == generation.ConfigApplyActionSkipped {
			continue
		}
		if kubeadmInputDomain(action.Domain) && kubeletRebound {
			continue
		}
		if action.Domain == DomainHostConfiguration {
			if err := e.restoreHostSysctls(ctx, sysctlSnapshot); err != nil {
				return err
			}
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
