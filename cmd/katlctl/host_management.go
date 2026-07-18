package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/katl-dev/katl/internal/installer/generation"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/spf13/cobra"
)

const (
	hostOutputText = "text"
	hostOutputJSON = "json"
)

type hostStatusOptions struct {
	target  managementTargetOptions
	timeout time.Duration
	output  string
}

type hostRebootOptions struct {
	target  managementTargetOptions
	timeout time.Duration
	noWait  bool
	output  string
}

type hostShutdownOptions struct {
	target  managementTargetOptions
	timeout time.Duration
	noWait  bool
	output  string
}

type hostStatusReport struct {
	Node          string `json:"node"`
	Endpoint      string `json:"endpoint"`
	Health        string `json:"health"`
	Generation    string `json:"generation"`
	KatlOSVersion string `json:"katlosVersion,omitempty"`
	NextBoot      string `json:"nextBoot,omitempty"`
	Activity      string `json:"activity"`
}

type hostRebootReport struct {
	Node       string `json:"node"`
	Result     string `json:"result"`
	Generation string `json:"generation"`
	Health     string `json:"health,omitempty"`
}

type hostShutdownReport struct {
	Node   string `json:"node"`
	Result string `json:"result"`
}

var hostShutdownPollInterval = 500 * time.Millisecond

func newHostStatusCommand(ctx context.Context, stdout, stderr io.Writer) *cobra.Command {
	opts := hostStatusOptions{timeout: 15 * time.Second, output: hostOutputText}
	cmd := &cobra.Command{
		Use:   "status [NODE]",
		Short: "Show the current state of one KatlOS node",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := selectHostNode(&opts.target.nodeName, args); err != nil {
				return err
			}
			return runHostStatus(ctx, opts, stdout, stderr)
		},
	}
	addManagementTargetFlags(cmd, &opts.target)
	cmd.Flags().DurationVar(&opts.timeout, "timeout", opts.timeout, "management request timeout")
	cmd.Flags().StringVarP(&opts.output, "output", "o", opts.output, "output format: text or json")
	return cmd
}

func newHostRebootCommand(ctx context.Context, stdout, stderr io.Writer) *cobra.Command {
	opts := hostRebootOptions{timeout: 10 * time.Minute, output: hostOutputText}
	cmd := &cobra.Command{
		Use:   "reboot [NODE]",
		Short: "Reboot one KatlOS node and wait for it to return healthy",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := selectHostNode(&opts.target.nodeName, args); err != nil {
				return err
			}
			return runHostReboot(ctx, opts, stdout, stderr)
		},
	}
	addManagementTargetFlags(cmd, &opts.target)
	cmd.Flags().DurationVar(&opts.timeout, "timeout", opts.timeout, "time to wait for the host to return healthy")
	cmd.Flags().BoolVar(&opts.noWait, "no-wait", false, "return after the host schedules the reboot")
	cmd.Flags().StringVarP(&opts.output, "output", "o", opts.output, "output format: text or json")
	return cmd
}

func newHostShutdownCommand(ctx context.Context, stdout, stderr io.Writer) *cobra.Command {
	opts := hostShutdownOptions{timeout: 2 * time.Minute, output: hostOutputText}
	cmd := &cobra.Command{
		Use:   "shutdown [NODE]",
		Short: "Shut down one KatlOS node",
		Long: `Shut down one KatlOS node and wait for its management API to stop.

Use the same ClusterConfig used to install the node. --endpoint can override its recorded address when DHCP or local routing changed.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := selectHostNode(&opts.target.nodeName, args); err != nil {
				return err
			}
			return runHostShutdown(ctx, opts, stdout, stderr)
		},
	}
	addManagementTargetFlags(cmd, &opts.target)
	cmd.Flags().DurationVar(&opts.timeout, "timeout", opts.timeout, "time to wait for the management API to stop")
	cmd.Flags().BoolVar(&opts.noWait, "no-wait", false, "return after the host schedules the shutdown")
	cmd.Flags().StringVarP(&opts.output, "output", "o", opts.output, "output format: text or json")
	return cmd
}

func selectHostNode(selected *string, args []string) error {
	if len(args) == 0 {
		return nil
	}
	if strings.TrimSpace(*selected) != "" {
		return fmt.Errorf("NODE cannot be combined with --node")
	}
	*selected = args[0]
	return nil
}

func runHostStatus(ctx context.Context, opts hostStatusOptions, stdout, stderr io.Writer) error {
	_ = stderr
	if err := validateHostOutput(opts.output); err != nil {
		return err
	}
	if opts.timeout <= 0 {
		return fmt.Errorf("--timeout must be positive")
	}
	target, err := resolveManagementTarget(opts.target)
	if err != nil {
		return err
	}
	node := hostTargetName(target)
	requestCtx, cancel := context.WithTimeout(ctx, opts.timeout)
	defer cancel()
	conn, err := dialKatlcAgent(requestCtx, target.endpoint)
	if err != nil {
		return fmt.Errorf("connect to %s at %s: %w", node, target.endpoint, err)
	}
	defer conn.Close()

	status, current, err := readHostState(requestCtx, conn.Client, node)
	if err != nil {
		return err
	}
	report := newHostStatusReport(node, target.endpoint, status, current)
	return writeHostStatus(stdout, opts.output, report)
}

func runHostReboot(ctx context.Context, opts hostRebootOptions, stdout, stderr io.Writer) error {
	if err := validateHostOutput(opts.output); err != nil {
		return err
	}
	if opts.timeout <= 0 {
		return fmt.Errorf("--timeout must be positive")
	}
	target, err := resolveManagementTarget(opts.target)
	if err != nil {
		return err
	}
	node := hostTargetName(target)
	requestCtx, cancelRequest := context.WithTimeout(ctx, opts.timeout)
	conn, err := dialKatlcAgent(requestCtx, target.endpoint)
	if err != nil {
		cancelRequest()
		return fmt.Errorf("connect to %s at %s: %w", node, target.endpoint, err)
	}

	status, _, err := readHostState(requestCtx, conn.Client, node)
	if err != nil {
		_ = conn.Close()
		cancelRequest()
		return err
	}
	generationID := strings.TrimSpace(status.GetBootTargetGenerationId())
	if generationID == "" {
		generationID = strings.TrimSpace(status.GetCurrentGenerationId())
	}
	previousAgentStart := status.GetAgentStartId()
	if err := requestNodeReboot(requestCtx, conn.Client, "katlctl node reboot", status.GetMachineId(), generationID); err != nil {
		_ = conn.Close()
		cancelRequest()
		return fmt.Errorf("schedule reboot for %s: %w", node, err)
	}
	_ = conn.Close()
	cancelRequest()

	report := hostRebootReport{Node: node, Result: "scheduled", Generation: generationID}
	if opts.noWait {
		return writeHostReboot(stdout, opts.output, report)
	}
	_, _ = fmt.Fprintf(stderr, "Reboot scheduled for %s; waiting for KatlOS to return healthy...\n", node)
	waitCtx, cancelWait := context.WithTimeout(ctx, opts.timeout)
	verifiedConn, verified, err := waitNodeBootHealth(waitCtx, node, target.endpoint, previousAgentStart, generationID, io.Discard)
	cancelWait()
	if err != nil {
		return err
	}
	_ = verifiedConn.Close()
	report.Result = "rebooted"
	report.Health = displayHostHealth(verified.Generation)
	return writeHostReboot(stdout, opts.output, report)
}

func runHostShutdown(ctx context.Context, opts hostShutdownOptions, stdout, stderr io.Writer) error {
	if err := validateHostOutput(opts.output); err != nil {
		return err
	}
	if opts.timeout <= 0 {
		return fmt.Errorf("--timeout must be positive")
	}
	target, err := resolveManagementTarget(opts.target)
	if err != nil {
		return err
	}
	node := hostTargetName(target)
	requestCtx, cancelRequest := context.WithTimeout(ctx, opts.timeout)
	conn, err := dialKatlcAgent(requestCtx, target.endpoint)
	if err != nil {
		cancelRequest()
		return fmt.Errorf("connect to %s at %s: %w", node, target.endpoint, err)
	}
	status, err := conn.Client.GetNodeStatus(requestCtx, &agentapi.GetNodeStatusRequest{})
	if err != nil {
		_ = conn.Close()
		cancelRequest()
		return fmt.Errorf("read status from %s: %w", node, err)
	}
	accepted, err := conn.Client.Shutdown(requestCtx, &agentapi.ShutdownRequest{
		ApiVersion:        generation.APIVersion,
		Kind:              "ShutdownRequest",
		Actor:             "katlctl node shutdown",
		ExpectedMachineId: status.GetMachineId(),
	})
	_ = conn.Close()
	cancelRequest()
	if err != nil {
		return fmt.Errorf("schedule shutdown for %s: %w", node, err)
	}
	if !accepted.GetScheduled() {
		return fmt.Errorf("%s did not schedule shutdown", node)
	}

	report := hostShutdownReport{Node: node, Result: "scheduled"}
	if opts.noWait {
		return writeHostShutdown(stdout, opts.output, report)
	}
	_, _ = fmt.Fprintf(stderr, "Shutdown scheduled for %s; waiting for its management API to stop...\n", node)
	waitCtx, cancelWait := context.WithTimeout(ctx, opts.timeout)
	err = waitNodeOffline(waitCtx, target.endpoint)
	cancelWait()
	if err != nil {
		return fmt.Errorf("%s did not shut down: %w", node, err)
	}
	report.Result = "offline"
	return writeHostShutdown(stdout, opts.output, report)
}

func waitNodeOffline(ctx context.Context, endpoint string) error {
	for {
		attemptCtx, cancelAttempt := context.WithTimeout(ctx, 2*time.Second)
		conn, err := dialKatlcAgent(attemptCtx, endpoint)
		if err == nil {
			_, err = conn.Client.GetNodeStatus(attemptCtx, &agentapi.GetNodeStatusRequest{})
			_ = conn.Close()
		}
		cancelAttempt()
		if err != nil {
			return nil
		}

		timer := time.NewTimer(hostShutdownPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func readHostState(ctx context.Context, client agentapi.KatlcAgentClient, node string) (*agentapi.NodeStatus, *agentapi.Generation, error) {
	status, err := client.GetNodeStatus(ctx, &agentapi.GetNodeStatusRequest{})
	if err != nil {
		return nil, nil, fmt.Errorf("read status from %s: %w", node, err)
	}
	generationID := strings.TrimSpace(status.GetCurrentGenerationId())
	if generationID == "" {
		return nil, nil, fmt.Errorf("%s did not report a current KatlOS generation", node)
	}
	current, err := client.GetGeneration(ctx, &agentapi.GetGenerationRequest{GenerationId: generationID})
	if err != nil {
		return nil, nil, fmt.Errorf("read current generation from %s: %w", node, err)
	}
	return status, current, nil
}

func newHostStatusReport(node, endpoint string, status *agentapi.NodeStatus, current *agentapi.Generation) hostStatusReport {
	activity := "idle"
	if status.GetOperationLockHeld() {
		activity = "busy"
	}
	report := hostStatusReport{
		Node:          node,
		Endpoint:      endpoint,
		Health:        displayHostHealth(current),
		Generation:    current.GetGenerationId(),
		KatlOSVersion: strings.TrimSpace(current.GetRuntimeVersion()),
		Activity:      activity,
	}
	if target := strings.TrimSpace(status.GetBootTargetGenerationId()); target != "" && target != current.GetGenerationId() {
		report.NextBoot = target
	}
	return report
}

func displayHostHealth(current *agentapi.Generation) string {
	if current.GetCommitState() == generation.CommitStateCommitted && current.GetBootState() == generation.BootStateGood && current.GetHealthState() == generation.HealthStateHealthy {
		return "OK"
	}
	if health := strings.TrimSpace(current.GetHealthState()); health != "" {
		return health
	}
	return "unknown"
}

func writeHostStatus(stdout io.Writer, output string, report hostStatusReport) error {
	if output == hostOutputJSON {
		return writeJSON(stdout, report)
	}
	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "NODE\tHEALTH\tKATLOS\tGENERATION\tNEXT BOOT\tACTIVITY"); err != nil {
		return err
	}
	version := report.KatlOSVersion
	if version == "" {
		version = "unknown"
	}
	nextBoot := report.NextBoot
	if nextBoot == "" {
		nextBoot = "-"
	}
	if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", report.Node, report.Health, version, report.Generation, nextBoot, report.Activity); err != nil {
		return err
	}
	return w.Flush()
}

func writeHostReboot(stdout io.Writer, output string, report hostRebootReport) error {
	if output == hostOutputJSON {
		return writeJSON(stdout, report)
	}
	if report.Result == "scheduled" {
		_, err := fmt.Fprintf(stdout, "%s reboot scheduled\n", report.Node)
		return err
	}
	_, err := fmt.Fprintf(stdout, "%s rebooted successfully; health %s\n", report.Node, report.Health)
	return err
}

func writeHostShutdown(stdout io.Writer, output string, report hostShutdownReport) error {
	if output == hostOutputJSON {
		return writeJSON(stdout, report)
	}
	if report.Result == "scheduled" {
		_, err := fmt.Fprintf(stdout, "%s shutdown scheduled\n", report.Node)
		return err
	}
	_, err := fmt.Fprintf(stdout, "%s shut down; management API is offline\n", report.Node)
	return err
}

func validateHostOutput(output string) error {
	switch output {
	case hostOutputText, hostOutputJSON:
		return nil
	default:
		return fmt.Errorf("--output = %q, want text or json", output)
	}
}

func hostTargetName(target managementTarget) string {
	if node := strings.TrimSpace(target.nodeName); node != "" {
		return node
	}
	return target.endpoint
}
