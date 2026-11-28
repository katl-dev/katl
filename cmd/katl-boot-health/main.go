package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/deadline"
	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/linkrecovery"
	"github.com/katl-dev/katl/internal/managementnetwork"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "katl-boot-health: %v\n", err)
		os.Exit(1)
	}
}

// run exits successfully on a failed boot only when the owner requested the
// trial's one reboot: the recovery and deadline units reboot on success.
func run(ctx context.Context, args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("katl-boot-health", flag.ContinueOnError)
	root := flags.String("root", "/", "runtime root containing /var/lib/katl")
	cmdline := flags.String("cmdline", "/proc/cmdline", "kernel command line path")
	result := flags.String("result", generation.BootHealthSuccess, "boot health result: success, failure, or timeout")
	reason := flags.String("reason", "", "boot health transition reason")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if stdout == nil {
		stdout = io.Discard
	}

	data, err := os.ReadFile(*cmdline)
	if err != nil {
		return fmt.Errorf("read kernel command line: %w", err)
	}
	commandLine := string(data)
	selected, err := generation.SelectedGenerationFromCommandLine(commandLine)
	if err != nil {
		return err
	}
	requestedResult := strings.TrimSpace(*result)
	requestedReason := strings.TrimSpace(*reason)

	switch requestedResult {
	case generation.BootHealthSuccess:
		_, selectedStatus, err := generation.ReadGeneration(*root, selected)
		if err != nil {
			return err
		}
		// A network outage cannot invalidate a generation already proven healthy.
		if !generation.IsKnownGood(selectedStatus) {
			if err := waitForManagementNetwork(ctx); err != nil {
				failure := "management network unavailable: " + err.Error()
				if errors.Is(err, errManagementDeadlineNotArmed) {
					// Without its deadline the link check never ran, so the
					// trial is unproven rather than offline.
					failure = err.Error()
				}
				// Only a unit that reboots on success may spend the trial's
				// reboot, so katl-boot-recovery.service decides the failure.
				fmt.Fprintf(stdout, "katl-boot-health generation=%s result=%s reason=%q\n", selected, generation.BootHealthFailure, failure)
				return errors.New(failure)
			}
		}
		mover, err := newBootOwner(*root, commandLine)
		if err != nil {
			return err
		}
		record, err := mover.ConfirmBoot(requestedReason)
		if err != nil {
			return err
		}
		if record.ReportOnly {
			// The deadline already failed this boot's trial; the failure stands.
			printResult(stdout, record)
			return fmt.Errorf("generation %s already failed its boot trial on this boot, so this health success changes nothing. The boot default is generation %s and generation %s is selected for the next boot. See 'katlctl node status <node>'", record.GenerationID, record.DefaultGeneration, record.NextBootGeneration)
		}
		disarm(ctx)
		if err := markConfigApplyBootActive(*root, selected, bootHealthClock()); err != nil {
			return err
		}
		printResult(stdout, record)
		return nil
	case generation.BootHealthFailure, generation.BootHealthTimeout:
		mover, err := newBootOwner(*root, commandLine)
		if err != nil {
			return err
		}
		record, err := mover.FailTrial(requestedReason)
		if err != nil {
			return err
		}
		record.Result = requestedResult
		printResult(stdout, record)
		if record.FallbackProblem != "" {
			fmt.Fprintln(stdout, record.FallbackProblem)
		}
		if record.ReportOnly {
			return reportOnly(record)
		}
		disarm(ctx)
		if record.RebootRequested {
			return nil
		}
		return resolvedWithoutReboot(record)
	default:
		return fmt.Errorf("boot health result must be success, failure, or timeout")
	}
}

func printResult(stdout io.Writer, record generation.BootHealthResult) {
	fmt.Fprintf(
		stdout, "katl-boot-health generation=%s result=%s default=%s promoted=%t failed=%t recoveryRequired=%t rebootRequested=%t\n",
		record.GenerationID, record.Result, record.DefaultGeneration, record.Promoted, record.Failed, record.RecoveryRequired, record.RebootRequested,
	)
}

// reportOnly describes a failed boot that no trial is bound to. Boot state is
// unchanged, so the operator fixes the cause and reboots.
func reportOnly(record generation.BootHealthResult) error {
	if record.KnownGood {
		return fmt.Errorf("running generation %s stays known-good. The boot default is generation %s. "+
			"Generation %s is selected for the next boot. No automatic reboot. "+
			"See 'katlctl node status <node>', 'katlctl node generations list <node>' and 'systemctl --failed'. "+
			"Use the node console or out-of-band access if remote management is unreachable. "+
			"Fix the cause, for example with 'katlctl cluster apply', then run 'katlctl node reboot <node>'",
			record.GenerationID, record.DefaultGeneration, record.NextBootGeneration)
	}
	return fmt.Errorf("generation %s has not passed boot health and no boot trial is pending, so boot state is unchanged. "+
		"The boot default is generation %s. Generation %s is selected for the next boot. No automatic reboot. "+
		"See 'katlctl node status <node>' and 'systemctl --failed'. "+
		"Use the node console or out-of-band access if remote management is unreachable. "+
		"Fix the cause, then run 'katlctl node reboot <node>'",
		record.GenerationID, record.DefaultGeneration, record.NextBootGeneration)
}

// resolvedWithoutReboot describes a failed trial whose reboot is unavailable.
func resolvedWithoutReboot(record generation.BootHealthResult) error {
	if record.RecoveryRequired {
		return fmt.Errorf("generation %s failed boot health and no fallback could be used, so it stays the boot default and recovery is required. No automatic reboot. "+
			"Repair the cause, then run 'katlctl node reboot <node>'; a healthy boot clears the recovery state. "+
			"Otherwise boot a known-good generation from the boot menu; its healthy boot makes it the default and clears the recovery state. If none boots, reinstall the node", record.GenerationID)
	}
	return fmt.Errorf("generation %s failed boot health and generation %s is the boot default. "+
		"The trial's automatic reboot was already used, so no automatic reboot follows. "+
		"Run 'katlctl node reboot <node>' to boot generation %s", record.GenerationID, record.DefaultGeneration, record.NextBootGeneration)
}

// disarm stops the boot trial deadline once the owner decided the boot. A
// failure leaves the timer to expire into the owner's report.
func disarm(ctx context.Context) {
	if err := disarmBootTrial(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "katl-boot-health: stop the boot trial deadline: %v\n", err)
	}
}

// bootOwner is the boot-state owner's health decisions; run maps each
// outcome to an exit status.
type bootOwner interface {
	ConfirmBoot(reason string) (generation.BootHealthResult, error)
	FailTrial(reason string) (generation.BootHealthResult, error)
}

var (
	newBootOwner = func(root, commandLine string) (bootOwner, error) {
		return generation.NewBootMover(root, generation.BootMoverOptions{CommandLine: commandLine})
	}
	disarmBootTrial = deadline.DisarmBootTrial
)

var errManagementDeadlineNotArmed = errors.New("management network deadline not armed")

var waitForManagementNetwork = func(ctx context.Context) error {
	healthCtx, stop, err := deadline.Arm(ctx, deadline.ManagementLink, "")
	if err != nil {
		return fmt.Errorf("%w: %w", errManagementDeadlineNotArmed, err)
	}
	defer stop()
	return awaitManagementNetwork(healthCtx)
}

func awaitManagementNetwork(healthCtx context.Context) error {
	backoff := managementnetwork.PollBackoff()
	lastReason := "network state has not been observed"
	for healthCtx.Err() == nil {
		// A completed online observation wins even if the read's context expired.
		ready, reason, err := managementnetwork.Ready(healthCtx, readNetworkctlState)
		if err != nil {
			if healthCtx.Err() != nil {
				break
			}
			lastReason = err.Error()
		} else if ready {
			return nil
		} else if lastReason != reason {
			fmt.Fprintf(os.Stderr, "waiting for management network: %s\n", reason)
			lastReason = reason
		}
		timer := time.NewTimer(backoff.Next())
		select {
		case <-healthCtx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
	cause := context.Cause(healthCtx)
	if !errors.Is(cause, context.DeadlineExceeded) {
		return cause
	}
	// The link-recovery owner records any reconfiguration in this boot.
	if records, err := linkrecovery.Read(linkRecoveryDir); err != nil {
		lastReason += "; " + err.Error()
	} else if summary := linkrecovery.Summary(records); summary != "" {
		lastReason += "; " + summary
	}
	return fmt.Errorf("configured management network deadline expired: %s", lastReason)
}

var readNetworkctlState = managementnetwork.ReadState

var linkRecoveryDir = linkrecovery.Dir

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
