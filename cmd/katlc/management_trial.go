package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/katl-dev/katl/internal/nodeidentity"
)

func runAgentManagementTrial(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] != "revert" {
		return fmt.Errorf("management-trial command must be revert")
	}
	flags := flag.NewFlagSet("katlc agent management-trial revert", flag.ContinueOnError)
	flags.SetOutput(stderr)
	root := flags.String("root", "/", "installed runtime root")
	trial := flags.String("trial", "", "revert only this trial; by default revert any live trial")
	reason := flags.String("reason", "reverted by the operator", "reason recorded with the revert")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if filepath.Clean(*root) == "/" && os.Geteuid() != 0 {
		return fmt.Errorf("reverting a management trial requires root on the node")
	}
	return revertManagementTrial(ctx, *root, *trial, *reason, nodeidentity.SystemTrialClock{}, stdout, systemdAgentService)
}

// revertManagementTrial restores the installed management mode and restarts
// katlc-agent so it stops serving the trial credentials.
func revertManagementTrial(ctx context.Context, root, id, reason string, clock nodeidentity.TrialClock, stdout io.Writer, service rotationServiceRunner) error {
	trial, reverted, err := nodeidentity.RevertManagementTrial(root, id, reason, clock)
	if err != nil {
		return err
	}
	if !reverted {
		fmt.Fprintln(stdout, "no management trial is in progress; the installed management mode is unchanged")
		return nil
	}
	if err := service(ctx, "restart"); err != nil {
		return fmt.Errorf("management trial %s reverted, but katlc-agent did not restart: %w; restart katlc-agent.service so it serves the installed management mode", trial.ID, err)
	}
	fmt.Fprintf(stdout, "reverted management trial %s for authority %s; katlc-agent restarted with the installed management mode\n", trial.ID, trial.TargetAuthority)
	return nil
}
