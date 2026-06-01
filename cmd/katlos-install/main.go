package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"git.cbannister.xyz/chris/katl/internal/installer"
)

var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "katlos-install: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("katlos-install", flag.ContinueOnError)
	flags.SetOutput(stderr)

	manifestPath := flags.String("manifest", "", "path to install manifest")
	stateDir := flags.String("state-dir", "/var/lib/katl/install", "installer state directory")
	listStates := flags.Bool("list-states", false, "print the installer state order and exit")
	showVersion := flags.Bool("version", false, "print build metadata and exit")

	if err := flags.Parse(args); err != nil {
		return err
	}

	if *showVersion {
		fmt.Fprintf(stdout, "katlos-install version=%s commit=%s date=%s\n", version, commit, date)
		return nil
	}

	plan := installer.DefaultPlan()
	if *listStates {
		for _, id := range plan.IDs() {
			fmt.Fprintln(stdout, id)
		}
		return nil
	}

	if strings.TrimSpace(*manifestPath) == "" {
		return fmt.Errorf("--manifest is required unless --list-states is set")
	}

	runner := installer.NewRunner(installer.PreseededManifestPlan(), &installer.Context{
		ManifestPath: strings.TrimSpace(*manifestPath),
		StateDir:     *stateDir,
		Commands:     installer.NewExecCommandRunner(),
		Store:        installer.NewFileStateStore(*stateDir),
	})

	return runner.Run(ctx)
}
