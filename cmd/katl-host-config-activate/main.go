package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/installer/configapply"
	"github.com/katl-dev/katl/internal/installer/generation"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "katl-host-config-activate: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("katl-host-config-activate", flag.ContinueOnError)
	root := flags.String("root", "/", "runtime root containing /var/lib/katl")
	generationID := flags.String("generation", "", "selected generation id; defaults to katl.generation from cmdline")
	cmdline := flags.String("cmdline", "/proc/cmdline", "kernel command line path")
	phase := flags.String("phase", configapply.HostConfigurationPhasePrepare, "activation phase: prepare or verify")
	if err := flags.Parse(args); err != nil {
		return err
	}
	selected := strings.TrimSpace(*generationID)
	if selected == "" {
		data, err := os.ReadFile(*cmdline)
		if err != nil {
			return fmt.Errorf("read kernel command line: %w", err)
		}
		selected, err = generation.SelectedGenerationFromCommandLine(string(data))
		if err != nil {
			return err
		}
	}
	value, _, err := configapply.ReadEffectiveGenerationManifest(*root, selected)
	if err != nil {
		return err
	}
	if *phase != configapply.HostConfigurationPhasePrepare && *phase != configapply.HostConfigurationPhaseVerify {
		return fmt.Errorf("phase = %q, want %q or %q", *phase, configapply.HostConfigurationPhasePrepare, configapply.HostConfigurationPhaseVerify)
	}
	plan := configapply.PlanHostConfigurationActivation(value.Node.HostConfiguration, *phase)
	statusPath, statusErr := generation.ConfigApplyStatusPath(*root, selected)
	if statusErr != nil {
		return statusErr
	}
	status, statusErr := generation.ReadConfigApplyStatus(statusPath)
	if statusErr != nil && !errors.Is(statusErr, os.ErrNotExist) {
		return statusErr
	}
	var observeErr error
	observe := func(effect generation.ConfigApplyEffect) {
		if statusErr != nil || observeErr != nil {
			return
		}
		updateHostConfigurationEffect(&status, effect)
		status.UpdatedAt = time.Now().UTC()
		observeErr = generation.WriteConfigApplyStatus(statusPath, status)
	}
	if err := configapply.ExecuteHostConfigurationActivation(ctx, plan, hostConfigRunner, observe); err != nil {
		return err
	}
	if observeErr != nil {
		return observeErr
	}
	if statusErr == nil {
		markHostConfigurationActionPassed(&status)
		status.UpdatedAt = time.Now().UTC()
		if err := generation.WriteConfigApplyStatus(statusPath, status); err != nil {
			return err
		}
	}
	if stdout != nil {
		fmt.Fprintf(stdout, "katl-host-config-activate generation=%s phase=%s effects=%d\n", selected, *phase, len(plan.Effects))
	}
	return nil
}

type execCommandRunner struct{}

var hostConfigRunner configapply.CommandRunner = execCommandRunner{}

func (execCommandRunner) Run(ctx context.Context, command configapply.Command) (configapply.CommandResult, error) {
	if len(command.Argv) == 0 {
		return configapply.CommandResult{}, fmt.Errorf("command argv is required")
	}
	cmd := exec.CommandContext(ctx, command.Argv[0], command.Argv[1:]...)
	stdout, err := cmd.Output()
	result := configapply.CommandResult{Stdout: string(stdout)}
	if err == nil {
		return result, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.ExitStatus = exitErr.ExitCode()
		result.Stderr = string(exitErr.Stderr)
		return result, nil
	}
	return result, err
}

func updateHostConfigurationEffect(status *generation.ConfigApplyStatus, observed generation.ConfigApplyEffect) {
	for actionIndex := range status.DomainActions {
		action := &status.DomainActions[actionIndex]
		if action.Domain != configapply.DomainHostConfiguration {
			continue
		}
		for effectIndex := range action.Effects {
			effect := &action.Effects[effectIndex]
			if effect.Action == observed.Action && effect.Target == observed.Target {
				*effect = observed
			}
		}
	}
}

func markHostConfigurationActionPassed(status *generation.ConfigApplyStatus) {
	for index := range status.DomainActions {
		action := &status.DomainActions[index]
		if action.Domain != configapply.DomainHostConfiguration {
			continue
		}
		for effectIndex := range action.Effects {
			effect := &action.Effects[effectIndex]
			if effect.Status == generation.ConfigApplyActionPlanned {
				effect.Status = generation.ConfigApplyActionPassed
				effect.Diagnostic = "selected configuration was visible before its boot consumer"
			}
			if effect.Status != generation.ConfigApplyActionPassed {
				return
			}
		}
		action.Status = generation.ConfigApplyActionPassed
		action.Diagnostic = ""
	}
}
