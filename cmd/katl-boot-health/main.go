package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/installer/generation"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "katl-boot-health: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("katl-boot-health", flag.ContinueOnError)
	root := flags.String("root", "/", "runtime root containing /var/lib/katl")
	generationID := flags.String("generation", "", "selected generation id; defaults to katl.generation from cmdline")
	cmdline := flags.String("cmdline", "/proc/cmdline", "kernel command line path")
	result := flags.String("result", generation.BootHealthSuccess, "boot health result: success, failure, or timeout")
	reason := flags.String("reason", "", "boot health transition reason")
	rebootRequestPath := flags.String("reboot-request-path", "/run/katl/boot-health/reboot-requested", "path for timeout/failure reboot request marker")
	requestReboot := flags.Bool("request-reboot", false, "record a reboot request marker for failure or timeout")
	if err := flags.Parse(args); err != nil {
		return err
	}

	data, err := os.ReadFile(*cmdline)
	if err != nil {
		return fmt.Errorf("read kernel command line: %w", err)
	}
	commandLine := string(data)
	selected := *generationID
	if selected == "" {
		selected, err = generation.SelectedGenerationFromCommandLine(commandLine)
		if err != nil {
			return err
		}
	}
	bootHealthClockValue := bootHealthClock()
	requestedResult := strings.TrimSpace(*result)
	requestedReason := strings.TrimSpace(*reason)
	forceFailure := false
	var failedUnits []string
	if requestedResult == generation.BootHealthSuccess {
		failedUnits, err = systemdFailedUnits(ctx)
		if err != nil {
			return fmt.Errorf("inspect systemd failed units: %w", err)
		}
		if len(failedUnits) > 0 {
			requestedResult = generation.BootHealthFailure
			requestedReason = "systemd failed units: " + strings.Join(failedUnits, ", ")
			forceFailure = true
		}
	}
	record, err := generation.RecordBootHealth(generation.BootHealthRequest{
		Root:               *root,
		GenerationID:       selected,
		Result:             requestedResult,
		Reason:             requestedReason,
		Now:                bootHealthClockValue,
		CommandLine:        commandLine,
		RebootRequestPath:  *rebootRequestPath,
		WriteRebootRequest: *requestReboot,
		ForceFailure:       forceFailure,
		SetBootDefault:     bootDefaultCommand,
	})
	if err != nil {
		return err
	}
	if requestedResult == generation.BootHealthSuccess {
		if err := markConfigApplyBootActive(*root, selected, bootHealthClockValue); err != nil {
			return err
		}
	}
	if stdout != nil {
		fmt.Fprintf(stdout, "katl-boot-health generation=%s result=%s default=%s promoted=%t failed=%t recoveryRequired=%t rebootRequested=%t\n",
			record.GenerationID,
			record.Result,
			record.DefaultGeneration,
			record.Promoted,
			record.Failed,
			record.RecoveryRequired,
			record.RebootRequested,
		)
	}
	if len(failedUnits) > 0 {
		return fmt.Errorf("systemd has failed units: %s", strings.Join(failedUnits, ", "))
	}
	return nil
}

func markConfigApplyBootActive(root, generationID string, now time.Time) error {
	path, err := generation.ConfigApplyStatusPath(root, generationID)
	if err != nil {
		return err
	}
	status, err := generation.ReadConfigApplyStatus(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if status.Phase != generation.ConfigApplyPhaseNextBoot {
		return nil
	}
	status, err = generation.MarkConfigApplyPhase(status, generation.ConfigApplyPhaseActive, now)
	if err != nil {
		return err
	}
	status.HealthState = generation.HealthStateHealthy
	return generation.WriteConfigApplyStatus(path, status)
}

var systemdFailedUnits = func(ctx context.Context) ([]string, error) {
	cmd := exec.CommandContext(ctx, "systemctl", "list-units", "--failed", "--no-legend", "--plain", "--no-pager")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("systemctl list-units --failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	var units []string
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		units = append(units, fields[0])
	}
	return units, nil
}

var bootHealthClock = func() time.Time {
	return time.Now().UTC()
}

var bootDefaultCommand generation.BootDefaultSetter = func(root string, bootEntry string) error {
	bootEntry = filepath.Base(strings.TrimSpace(bootEntry))
	if bootEntry == "." || bootEntry == "" {
		return fmt.Errorf("boot entry is required")
	}
	args := []string{"set-default", bootEntry}
	root = strings.TrimSpace(root)
	if root != "" && root != "/" {
		args = append([]string{"--esp-path=" + filepath.Join(root, "efi")}, args...)
	}
	cmd := exec.Command("bootctl", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("bootctl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}
