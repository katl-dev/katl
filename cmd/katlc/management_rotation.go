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

	"github.com/katl-dev/katl/internal/installer/operation"
	"github.com/katl-dev/katl/internal/nodeidentity"
)

const managementRotationLimit = 1 << 20

type rotationServiceRunner func(context.Context, string) error

func runAgentRotateManagement(ctx context.Context, args []string, input io.Reader, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("katlc agent rotate-management", flag.ContinueOnError)
	flags.SetOutput(stderr)
	root := flags.String("root", "/", "installed runtime root")
	dryRun := flags.Bool("dry-run", false, "validate the replacement without changing node state")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if filepath.Clean(*root) == "/" && os.Geteuid() != 0 {
		return fmt.Errorf("management rotation requires root SSH access")
	}
	return rotateManagementOnNode(ctx, *root, *dryRun, input, stdout, systemdAgentService)
}

func rotateManagementOnNode(ctx context.Context, root string, dryRun bool, input io.Reader, stdout io.Writer, service rotationServiceRunner) error {
	decoder := json.NewDecoder(io.LimitReader(input, managementRotationLimit+1))
	decoder.DisallowUnknownFields()
	var request nodeidentity.ManagementRotation
	if err := decoder.Decode(&request); err != nil {
		return fmt.Errorf("read management rotation request: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return fmt.Errorf("management rotation request must contain one JSON document")
	}
	status, err := nodeidentity.InspectManagementRotation(root, request)
	if err != nil {
		return err
	}
	if dryRun {
		return json.NewEncoder(stdout).Encode(status)
	}
	// Share the executor's mutation lock so an in-flight node mutation cannot
	// overlap the trust switch or agent restart.
	operationRoot := filepath.Join(root, "var/lib/katl/operations")
	if err := os.MkdirAll(operationRoot, 0o700); err != nil {
		return err
	}
	store, err := operation.NewStore(operationRoot)
	if err != nil {
		return err
	}
	lock, err := store.AcquireMutationLock()
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := refuseActiveNodeOperations(root); err != nil {
		return err
	}
	if !status.AlreadyRotated {
		status, err = nodeidentity.RotateManagementIdentity(root, request)
		if err != nil {
			return err
		}
	}
	// The old process keeps serving until systemd starts the replacement.
	// Repeated calls restart a previously switched identity after interruption.
	if err := service(ctx, "restart"); err != nil {
		return fmt.Errorf("restart katlc agent with replacement identity: %w; retry this SSH rotation command after repairing the service", err)
	}
	if err := service(ctx, "is-active"); err != nil {
		return fmt.Errorf("katlc agent did not remain active after rotation: %w; retry this SSH rotation command after inspecting its journal", err)
	}
	return json.NewEncoder(stdout).Encode(status)
}

func refuseActiveNodeOperations(root string) error {
	store, err := operation.NewStore(filepath.Join(root, "var/lib/katl/operations"))
	if err != nil {
		return fmt.Errorf("inspect node operations before credential rotation: %w", err)
	}
	records, err := store.List()
	if err != nil {
		return fmt.Errorf("inspect node operations before credential rotation: %w", err)
	}
	for _, record := range records {
		if !record.Terminal {
			return fmt.Errorf("node operation %s is still active; wait for it or recover it before rotating credentials", record.OperationID)
		}
	}
	return nil
}

func systemdAgentService(ctx context.Context, action string) error {
	args := []string{action, "katlc-agent.service"}
	if action == "is-active" {
		args = []string{"is-active", "--quiet", "katlc-agent.service"}
	}
	output, err := exec.CommandContext(ctx, "systemctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s katlc-agent.service: %w: %s", action, err, strings.TrimSpace(string(output)))
	}
	return nil
}
