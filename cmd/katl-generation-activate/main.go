package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

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

func run(_ context.Context, args []string, stdout io.Writer) error {
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

	selected := *generationID
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
	metadataPath, err := generation.MetadataPath(*root, selected)
	if err != nil {
		return err
	}
	record, err := generation.ReadRecord(metadataPath)
	if err != nil {
		return err
	}
	if record.GenerationID != selected {
		return fmt.Errorf("metadata generation %q does not match selected generation %q", record.GenerationID, selected)
	}
	plan, err := generation.ApplyActivation(*root, record)
	if err != nil {
		return err
	}
	if filepath.Clean(*root) == "/" {
		hostname, err := activateHostname(*root, plan, unix.Sethostname)
		if err != nil {
			return err
		}
		if stdout != nil && hostname != "" {
			fmt.Fprintf(stdout, "katl-generation-activate hostname=%s\n", hostname)
		}
	}
	if stdout != nil {
		fmt.Fprintf(stdout, "katl-generation-activate generation=%s sysexts=%d confexts=%d\n", plan.GenerationID, len(plan.Sysexts), len(plan.Confexts))
	}
	return nil
}

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
