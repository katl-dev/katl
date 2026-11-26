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

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/managementnetwork"
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
	requestReboot := flags.Bool("request-reboot", false, "allow systemd to reboot after a validated fallback is selected")
	forceFailureFlag := flags.Bool("force-failure", false, "exit unsuccessfully on boot failure unless recovery requests a reboot")
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
	forceFailure := *forceFailureFlag
	if requestedResult == generation.BootHealthSuccess {
		_, selectedStatus, err := generation.ReadGeneration(*root, selected)
		if err != nil {
			return err
		}
		// A network outage cannot invalidate a generation already proven healthy.
		if !generation.IsKnownGood(selectedStatus) {
			if err := waitForManagementNetwork(ctx); err != nil {
				requestedResult = generation.BootHealthFailure
				requestedReason = "management network unavailable: " + err.Error()
				forceFailure = true
			}
		}
	}
	record, err := generation.RecordBootHealth(generation.BootHealthRequest{
		Root:           *root,
		GenerationID:   selected,
		Result:         requestedResult,
		Reason:         requestedReason,
		Now:            bootHealthClockValue,
		CommandLine:    commandLine,
		RequestReboot:  *requestReboot,
		SetBootDefault: bootDefaultCommand,
	})
	if err != nil {
		return err
	}
	if requestedResult == generation.BootHealthSuccess {
		if err := markConfigApplyBootActive(*root, selected, bootHealthClockValue); err != nil {
			return err
		}
		if err := generation.ClearBootRecovery(*root); err != nil {
			return err
		}
	}
	if stdout != nil {
		fmt.Fprintf(
			stdout, "katl-boot-health generation=%s result=%s default=%s promoted=%t failed=%t recoveryRequired=%t rebootRequested=%t\n",
			record.GenerationID,
			record.Result,
			record.DefaultGeneration,
			record.Promoted,
			record.Failed,
			record.RecoveryRequired,
			record.RebootRequested,
		)
		if record.FallbackProblem != "" {
			fmt.Fprintln(stdout, record.FallbackProblem)
		}
	}
	if record.KeptKnownGood && requestedResult == generation.BootHealthFailure && (*requestReboot || forceFailure) {
		return fmt.Errorf("running generation %s stays known-good. The boot default is generation %s. "+
			"Generation %s is selected for the next boot. No automatic reboot. "+
			"See 'katlctl node status <node>', 'katlctl node generations list <node>' and 'systemctl --failed'. "+
			"Use the node console or out-of-band access if remote management is unreachable. "+
			"Fix the cause, for example with 'katlctl cluster apply', then run 'katlctl node reboot <node>'",
			record.GenerationID, record.DefaultGeneration, record.NextBootGeneration)
	}
	if *requestReboot && !record.RebootRequested {
		return fmt.Errorf("automatic reboot refused: this is not an armed trial with a validated known-good fallback; preserve boot diagnostics and recover from the console")
	}
	if forceFailure && requestedResult == generation.BootHealthFailure && !record.RebootRequested {
		return errors.New(requestedReason)
	}
	return nil
}

type managementNetworkTiming struct {
	trialTimeout time.Duration
	pollInterval time.Duration
	managementExpiryHooks
}

var managementTiming = managementNetworkTiming{
	trialTimeout: 90 * time.Second,
	pollInterval: time.Second,
}

var waitForManagementNetwork = func(ctx context.Context) error {
	return managementTiming.wait(ctx)
}

func (timing managementNetworkTiming) wait(ctx context.Context) error {
	// The trial bounds both networkctl and the wait between observations.
	deadlineCtx, cancel := context.WithTimeout(ctx, timing.trialTimeout)
	healthCtx, stop, expiry := timing.managementExpiryHooks.start(deadlineCtx, cancel)
	defer stop()
	ticker := time.NewTicker(timing.pollInterval)
	defer ticker.Stop()
	lastReason := "network state has not been observed"
	for healthCtx.Err() == nil {
		// A completed online observation wins even if the read's context expired.
		ready, reason, err := managementnetwork.Ready(healthCtx, readNetworkctlState)
		if err != nil {
			if healthCtx.Err() != nil {
				break
			}
			return err
		}
		if ready {
			return nil
		}
		lastReason = reason
		expiry.observeOffline(lastReason)
		select {
		case <-healthCtx.Done():
		case <-ticker.C:
		}
	}
	cause := context.Cause(healthCtx)
	if !errors.Is(cause, context.DeadlineExceeded) {
		return cause
	}
	expiry.report(cause)
	return fmt.Errorf("configured management network is not ready after %g seconds: %s", timing.trialTimeout.Seconds(), lastReason)
}

var readNetworkctlState = managementnetwork.ReadState

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
