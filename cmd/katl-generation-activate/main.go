package main

import (
	"cmp"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/katl-dev/katl/internal/deadline"
	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/katlc/agent"
	"golang.org/x/sys/unix"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "katl-generation-activate: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("katl-generation-activate", flag.ContinueOnError)
	root := flags.String("root", "/", "runtime root containing /var/lib/katl")
	generationID := flags.String("generation", "", "selected generation id; defaults to katl.generation from cmdline")
	cmdline := flags.String("cmdline", "/proc/cmdline", "kernel command line path")
	preflightImage := flags.String("prepare-upgrade-image-root", "", "mounted upgrade image for isolated generation preparation")
	preflightSource := flags.String("prepare-upgrade-source", "", "source generation for isolated generation preparation")
	preflightOperation := flags.String("prepare-upgrade-operation", "", "operation ID for isolated generation preparation")
	preflightConfig := flags.String("prepare-upgrade-config", "", "configuration document for generation preparation")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if stdout == nil {
		stdout = io.Discard
	}
	if *preflightImage != "" {
		if *generationID == "" || *preflightSource == "" || *preflightOperation == "" {
			return fmt.Errorf("experimental preflight requires generation, source, and operation ID")
		}
		spec, err := agent.PreflightHostUpgrade(context.Background(), *root, *preflightImage, *generationID, *preflightSource, *preflightOperation, *preflightConfig)
		if err != nil {
			return fmt.Errorf("target preflight: %w", err)
		}
		fmt.Fprintf(stdout, "target preparation generation=%s version=%s sysexts=%d confexts=%d\n", spec.GenerationID, spec.RuntimeVersion, len(spec.Sysexts), len(spec.Confexts))
		return nil
	}

	if *generationID != "" {
		// Without kernel evidence no boot state is read or written.
		plan, err := generation.ActivateGeneration(*root, *generationID)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "katl-generation-activate generation=%s sysexts=%d confexts=%d trialBound=false reconciled=none\n", plan.GenerationID, len(plan.Sysexts), len(plan.Confexts))
		return nil
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
	mover, err := newBootMover(*root, generation.BootMoverOptions{CommandLine: commandLine})
	if err != nil {
		return err
	}
	plan, outcome, err := mover.Activate(selected)
	if err != nil {
		return err
	}
	if filepath.Clean(*root) == "/" {
		hostname, err := activateHostname(*root, plan, unix.Sethostname)
		if err != nil {
			return err
		}
		if hostname != "" {
			fmt.Fprintf(stdout, "katl-generation-activate hostname=%s\n", hostname)
		}
	}
	reconciled := cmp.Or(outcome.Reconciled, "none")
	switch {
	case outcome.Deferred():
		reconciled = outcome.ReconcileErr.Error()
	case outcome.ReconcileErr != nil:
		// Activation never rejects an accepted generation; boot health
		// reconciles again and refuses moves until the error clears.
		fmt.Fprintf(stdout, "katl-generation-activate could not reconcile boot state for generation %s: %v\n", selected, outcome.ReconcileErr)
	}
	fmt.Fprintf(stdout, "katl-generation-activate generation=%s sysexts=%d confexts=%d trialBound=%t reconciled=%s\n",
		plan.GenerationID, len(plan.Sysexts), len(plan.Confexts), outcome.TrialBound, reconciled)
	if outcome.TrialBound {
		// The deadline is the only bound on a candidate that hangs before boot
		// health, so a trial never runs without it.
		if err := armBootTrial(ctx); err != nil {
			return fmt.Errorf("could not start the boot deadline for the trial of generation %s: %w; failing this boot so the node returns to its fallback", selected, err)
		}
	}
	return nil
}

var (
	newBootMover = generation.NewBootMover
	armBootTrial = deadline.ArmBootTrial
)

func activateHostname(root string, plan generation.ActivationPlan, setHostname func([]byte) error) (string, error) {
	var hostname string
	for _, confext := range plan.Confexts {
		path := filepath.Join(filepath.Clean(root), strings.TrimPrefix(confext.SourcePath, "/"), "etc/hostname")
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return "", fmt.Errorf("read selected hostname: %w", err)
		}
		candidate := strings.TrimSpace(string(data))
		if candidate == "" || strings.ContainsAny(candidate, " \t\r\n") {
			return "", fmt.Errorf("selected hostname in %s is invalid", confext.SourcePath)
		}
		if hostname != "" && hostname != candidate {
			return "", fmt.Errorf("selected confexts disagree on hostname: %q and %q", hostname, candidate)
		}
		hostname = candidate
	}
	if hostname == "" {
		// Generations written before hostname became native configuration remain
		// bootable; their base-image hostname is retained until config is applied.
		return "", nil
	}
	if err := setHostname([]byte(hostname)); err != nil {
		return "", fmt.Errorf("activate hostname %q: %w", hostname, err)
	}
	return hostname, nil
}
