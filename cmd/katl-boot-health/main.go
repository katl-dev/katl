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
	forceFailureFlag := flags.Bool("force-failure", false, "record failure even when the running generation was previously healthy")
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
		ForceFailure:   forceFailure,
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
	if *requestReboot && !record.RebootRequested {
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
			offline = append(offline, link.Name)
		}
		if link.OperationalState == "routable" {
			routable = true
		}
	}
	if len(required) == 0 {
		return false, "no systemd-networkd link is required for online", nil
	}
	if len(offline) > 0 {
		return false, "required links are not online: " + strings.Join(offline, ", "), nil
	}
	if !routable {
		return false, "no required link is routable", nil
	}
	return true, "", nil
}

var waitForManagementNetwork = func(ctx context.Context) error {
	healthCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	lastReason := "network state has not been observed"
	for {
		data, err := readNetworkctlState(healthCtx)
		if err != nil {
			if errors.Is(healthCtx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("configured management network is not ready after 90 seconds: %s", lastReason)
			}
			return err
		}
		ready, reason, err := managementNetworkReady(data)
		if err != nil {
			return err
		}
		if ready {
			return nil
		}
		lastReason = reason
		select {
		case <-healthCtx.Done():
			return fmt.Errorf("configured management network is not ready after 90 seconds: %s", lastReason)
		case <-ticker.C:
		}
	}
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
