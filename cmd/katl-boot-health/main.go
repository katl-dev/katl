package main

import (
	"context"
	"encoding/json"
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
	"github.com/katl-dev/katl/internal/systemdunit"
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
			// With a known-good fallback, a missing network fails fast so the
			// node returns to it. Without one, failing records an environmental
			// fault against the only generation, so boot health waits instead.
			// When the fallback cannot be determined, keep the bounded wait so
			// rollback is never delayed.
			bounded, err := generation.HasBootFallback(*root, selected)
			if err != nil {
				fmt.Fprintf(stdout, "katl-boot-health: cannot determine a known-good fallback, so the management network wait is bounded: %v\n", err)
				bounded = true
			}
			if err := waitForManagementNetwork(ctx, bounded, stdout); err != nil {
				requestedResult = generation.BootHealthFailure
				requestedReason = generation.ManagementNetworkFailure + ": " + err.Error()
				forceFailure = true
			}
		}
	}
	bootHealthClockValue := bootHealthClock()
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
	}
	if record.KeptKnownGood && requestedResult == generation.BootHealthFailure && (*requestReboot || forceFailure) {
		return fmt.Errorf("running generation %s stays known-good. The boot default is generation %s. "+
			"Generation %s is selected for the next boot. No automatic reboot. "+
			"See 'katlctl node status <node>' and 'systemctl --failed'. "+
			"Use the node console or out-of-band access if remote management is unreachable. "+
			"Fix the cause, for example with 'katlctl cluster apply', then run 'katlctl node reboot <node>'",
			record.GenerationID, record.DefaultGeneration, record.NextBootGeneration)
	}
	if *requestReboot && !record.RebootRequested {
		if selection, err := generation.ReadBootSelection(*root); err == nil {
			if guidance := generation.RecoveryGuidance(*root, selection); guidance != "" {
				return fmt.Errorf("automatic reboot refused: %s", guidance)
			}
		}
		return fmt.Errorf("automatic reboot refused: this is not an armed trial with a validated known-good fallback; preserve boot diagnostics and recover from the console")
	}
	if forceFailure && requestedResult == generation.BootHealthFailure && !record.RebootRequested {
		return errors.New(requestedReason)
	}
	return nil
}

type networkctlState struct {
	Interfaces []networkctlLink `json:"Interfaces"`
}

type networkctlLink struct {
	Name              string  `json:"Name"`
	OperationalState  string  `json:"OperationalState"`
	OnlineState       *string `json:"OnlineState"`
	NetworkFile       string  `json:"NetworkFile"`
	RequiredForOnline bool    `json:"RequiredForOnline"`
}

func managementNetworkReady(data []byte) (bool, string, error) {
	var state networkctlState
	if err := json.Unmarshal(data, &state); err != nil {
		return false, "", fmt.Errorf("decode networkctl state: %w", err)
	}

	var required, offline []string
	routable := false
	for _, link := range state.Interfaces {
		if link.NetworkFile == "" || !link.RequiredForOnline {
			continue
		}
		required = append(required, link.Name)
		if link.OnlineState == nil || *link.OnlineState != "online" {
			offline = append(offline, link.Name+": "+linkWait(link.OperationalState))
		}
		if link.OperationalState == "routable" {
			routable = true
		}
	}
	if len(required) == 0 {
		return false, "no systemd-networkd link is required for online", nil
	}
	if len(offline) == 1 {
		return false, "required link " + offline[0], nil
	}
	if len(offline) > 1 {
		return false, "required links " + strings.Join(offline, "; "), nil
	}
	if !routable {
		return false, "no required link is routable", nil
	}
	return true, "", nil
}

// linkWait describes what a required link that is not online is waiting for.
func linkWait(operationalState string) string {
	switch operationalState {
	case "no-carrier":
		return "no carrier; connect a cable or check the switch port"
	case "off":
		return "link is administratively down"
	case "", "missing":
		return "link is missing"
	default:
		return operationalState + ", not online yet"
	}
}

// managementNetworkDeadline bounds the wait when a failure falls back to a
// known-good generation.
var (
	managementNetworkDeadline = 90 * time.Second
	managementNetworkPoll     = time.Second
)

// waitForManagementNetwork returns when the configured management network is
// ready. A bounded wait fails after managementNetworkDeadline; an unbounded one
// waits until ctx ends. Each change in what it waits for is written to progress,
// which reaches the console, and to the unit's systemd status, which node
// status reports.
var waitForManagementNetwork = func(ctx context.Context, bounded bool, progress io.Writer) error {
	if bounded {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, managementNetworkDeadline)
		defer cancel()
	}
	if progress == nil {
		progress = io.Discard
	}
	ticker := time.NewTicker(managementNetworkPoll)
	defer ticker.Stop()
	lastReason := ""
	notReady := func() error {
		err := ctx.Err()
		if bounded && errors.Is(err, context.DeadlineExceeded) {
			err = fmt.Errorf("configured management network is not ready after %s", managementNetworkDeadline)
		}
		if lastReason == "" {
			return err
		}
		return fmt.Errorf("%w: %s", err, lastReason)
	}
	for {
		data, err := readNetworkctlState(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return notReady()
			}
			return err
		}
		ready, reason, err := managementNetworkReady(data)
		if err != nil {
			return err
		}
		if ready {
			if lastReason != "" {
				fmt.Fprintln(progress, "katl-boot-health: management network is ready")
				notifyStatus("")
			}
			return nil
		}
		if reason != lastReason {
			waiting := "waiting for the management network: " + reason
			fmt.Fprintf(progress, "katl-boot-health: %s\n", waiting)
			notifyStatus(waiting)
			lastReason = reason
		}
		select {
		case <-ctx.Done():
			return notReady()
		case <-ticker.C:
		}
	}
}

// notifyStatus sets the unit's systemd status text, which node status reads
// while boot health runs. It does nothing outside systemd.
var notifyStatus = func(status string) {
	_ = systemdunit.Notify("STATUS=" + status)
}

var readNetworkctlState = func(ctx context.Context) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "networkctl", "list", "--json=short", "--no-pager")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("networkctl list: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return output, nil
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
