package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/generation"
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
	Node                 string                      `json:"node"`
	Endpoint             string                      `json:"endpoint"`
	Health               string                      `json:"health"`
	Generation           string                      `json:"generation"`
	KatlOSVersion        string                      `json:"katlosVersion,omitempty"`
	KatlOSFlavour        string                      `json:"katlosFlavour,omitempty"`
	NextBoot             string                      `json:"nextBoot,omitempty"`
	Activity             string                      `json:"activity"`
	BootHealthDiagnostic string                      `json:"bootHealthDiagnostic,omitempty"`
	Kubernetes           *kubernetesStatusReport     `json:"kubernetes,omitempty"`
	APIProxy             *apiProxyStatusReport       `json:"apiProxy,omitempty"`
	ControlPlaneEndpoint *controlPlaneEndpointReport `json:"controlPlaneEndpoint,omitempty"`
	Volumes              []volumeStatusReport        `json:"volumes,omitempty"`
}

type apiProxyStatusReport struct {
	State                  string                   `json:"state"`
	Listeners              []apiProxyListenerReport `json:"listeners,omitempty"`
	Backends               []apiProxyBackendReport  `json:"backends,omitempty"`
	LocalAPIEligible       bool                     `json:"localAPIEligible"`
	CanonicalEndpoint      string                   `json:"canonicalEndpoint,omitempty"`
	CanonicalState         string                   `json:"canonicalState"`
	CanonicalFailureReason string                   `json:"canonicalFailureReason,omitempty"`
	UpdatedAt              string                   `json:"updatedAt,omitempty"`
	FailureReason          string                   `json:"failureReason,omitempty"`
}

type apiProxyListenerReport struct {
	Address  string `json:"address"`
	Exposure string `json:"exposure"`
}

type apiProxyBackendReport struct {
	Name        string `json:"name"`
	Address     string `json:"address"`
	Local       bool   `json:"local"`
	Eligible    bool   `json:"eligible"`
	Reason      string `json:"reason,omitempty"`
	LastChecked string `json:"lastChecked,omitempty"`
}

type volumeStatusReport struct {
	Name              string `json:"name"`
	TargetKind        string `json:"targetKind"`
	MountPath         string `json:"mountPath"`
	Filesystem        string `json:"filesystem"`
	LoadState         string `json:"loadState"`
	ActiveState       string `json:"activeState"`
	SubState          string `json:"subState"`
	Result            string `json:"result"`
	FailureDiagnostic string `json:"failureDiagnostic,omitempty"`
	MountSource       string `json:"mountSource,omitempty"`
}

type kubernetesStatusReport struct {
	State                       string `json:"state"`
	Role                        string `json:"role"`
	NodeName                    string `json:"nodeName"`
	KubeletActive               bool   `json:"kubeletActive"`
	NodeReady                   bool   `json:"nodeReady"`
	ControlPlaneComponentsReady bool   `json:"controlPlaneComponentsReady,omitempty"`
	FailureReason               string `json:"failureReason,omitempty"`
}

type controlPlaneEndpointReport struct {
	Endpoint           string `json:"endpoint"`
	VIP                string `json:"vip"`
	State              string `json:"state"`
	LocalAPIReady      bool   `json:"localAPIReady"`
	LocalVIPOwned      bool   `json:"localVIPOwned"`
	LastTransitionTime string `json:"lastTransitionTime,omitempty"`
	FailureReason      string `json:"failureReason,omitempty"`
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
	addOutputFlag(cmd, &opts.output, opts.output, "text", "json")
	return cmd
}

func newHostRebootCommand(ctx context.Context, stdout, stderr io.Writer) *cobra.Command {
	opts := hostRebootOptions{timeout: 15 * time.Minute, output: hostOutputText}
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
	addOutputFlag(cmd, &opts.output, opts.output, "text", "json")
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
	addOutputFlag(cmd, &opts.output, opts.output, "text", "json")
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
	session, err := openManagementSession(ctx, opts.target, opts.timeout)
	if err != nil {
		return err
	}
	defer session.close()
	target := session.target
	node := hostTargetName(target)

	status, current, err := readHostState(session.ctx, session.client, node)
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
	session, err := openManagementSession(ctx, opts.target, opts.timeout)
	if err != nil {
		return err
	}
	defer session.close()
	target := session.target
	node := hostTargetName(target)

	status, _, err := readHostState(session.ctx, session.client, node)
	if err != nil {
		return err
	}
	if err := bindManagementStatus(&target, status); err != nil {
		return err
	}
	generationID := strings.TrimSpace(status.GetBootTargetGenerationId())
	if generationID == "" {
		generationID = strings.TrimSpace(status.GetCurrentGenerationId())
	}
	previousAgentStart := status.GetAgentStartId()
	recoveryRequirement := nodeRecoveryRequirementFor(status)
	if err := requestNodeReboot(session.ctx, session.client, "katlctl node reboot", status, generationID); err != nil {
		return fmt.Errorf("schedule reboot for %s: %w", node, err)
	}
	session.close()

	report := hostRebootReport{Node: node, Result: "scheduled", Generation: generationID}
	if opts.noWait {
		return writeHostReboot(stdout, opts.output, report)
	}
	_, _ = fmt.Fprintf(stderr, "Reboot scheduled for %s; waiting for KatlOS to return healthy...\n", node)
	waitCtx, cancelWait := context.WithTimeout(ctx, opts.timeout)
	waitCtx = withManagementTarget(waitCtx, target)
	verifiedConn, verified, err := waitNodeBootHealth(waitCtx, node, target.endpoint, previousAgentStart, generationID, recoveryRequirement, io.Discard)
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
	session, err := openManagementSession(ctx, opts.target, opts.timeout)
	if err != nil {
		return err
	}
	defer session.close()
	target := session.target
	node := hostTargetName(target)

	status, err := session.client.GetNodeStatus(session.ctx, &agentapi.GetNodeStatusRequest{})
	if err != nil {
		return fmt.Errorf("read status from %s: %w", node, err)
	}
	if err := bindManagementStatus(&target, status); err != nil {
		return err
	}
	accepted, err := session.client.Shutdown(session.ctx, &agentapi.ShutdownRequest{
		ApiVersion:                  generation.APIVersion,
		Kind:                        "ShutdownRequest",
		Actor:                       "katlctl node shutdown",
		ExpectedEnrollmentId:        status.GetEnrollmentId(),
		ExpectedInventoryNodeName:   status.GetInventoryNodeName(),
		ExpectedMachineId:           status.GetMachineId(),
		ExpectedCurrentGenerationId: status.GetCurrentGenerationId(),
	})
	session.close()
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
	err = waitNodeOffline(waitCtx, target)
	cancelWait()
	if err != nil {
		return fmt.Errorf("%s did not shut down: %w", node, err)
	}
	report.Result = "offline"
	return writeHostShutdown(stdout, opts.output, report)
}

func waitNodeOffline(ctx context.Context, target managementTarget) error {
	for {
		attemptCtx, cancelAttempt := context.WithTimeout(ctx, 2*time.Second)
		attemptCtx = withManagementTarget(attemptCtx, target)
		conn, err := dialKatlcAgent(attemptCtx, target.endpoint)
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
	if status.GetBootHealthState() == "failed" && strings.TrimSpace(status.GetSelectedGenerationId()) != "" {
		generationID = strings.TrimSpace(status.GetSelectedGenerationId())
	}
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
	health := displayHostHealth(current)
	if bootHealth := strings.TrimSpace(status.GetBootHealthState()); bootHealth != "" && bootHealth != "healthy" {
		health = bootHealth
	}
	report := hostStatusReport{
		Node:                 node,
		Endpoint:             endpoint,
		Health:               health,
		Generation:           current.GetGenerationId(),
		KatlOSVersion:        strings.TrimSpace(current.GetRuntimeVersion()),
		KatlOSFlavour:        current.GetRuntimeFlavour(),
		Activity:             activity,
		BootHealthDiagnostic: strings.TrimSpace(status.GetBootHealthDiagnostic()),
		Kubernetes:           newKubernetesStatusReport(status.GetKubernetes()),
		APIProxy:             newAPIProxyStatusReport(status.GetApiProxy()),
		ControlPlaneEndpoint: newControlPlaneEndpointReport(status.GetControlPlaneEndpoint()),
	}
	if target := strings.TrimSpace(status.GetBootTargetGenerationId()); target != "" && target != current.GetGenerationId() {
		report.NextBoot = target
	}
	for _, volume := range status.GetVolumes() {
		report.Volumes = append(report.Volumes, volumeStatusReport{
			Name: volume.GetName(), TargetKind: volume.GetTargetKind(), MountPath: volume.GetMountPath(),
			Filesystem: volume.GetFilesystem(), LoadState: volume.GetLoadState(), ActiveState: volume.GetActiveState(),
			SubState: volume.GetSubState(), Result: volume.GetResult(), FailureDiagnostic: volume.GetFailureDiagnostic(),
			MountSource: volume.GetMountSource(),
		})
	}
	return report
}

func newAPIProxyStatusReport(status *agentapi.APIProxyStatus) *apiProxyStatusReport {
	if status == nil {
		return nil
	}
	report := &apiProxyStatusReport{
		State: status.GetState(), LocalAPIEligible: status.GetLocalApiEligible(),
		CanonicalEndpoint: status.GetCanonicalEndpoint(), CanonicalState: status.GetCanonicalState(),
		CanonicalFailureReason: status.GetCanonicalFailureReason(), UpdatedAt: status.GetUpdatedAt(),
		FailureReason: status.GetFailureReason(),
	}
	for _, listener := range status.GetListeners() {
		report.Listeners = append(report.Listeners, apiProxyListenerReport{
			Address: listener.GetAddress(), Exposure: listener.GetExposure(),
		})
	}
	for _, backend := range status.GetBackends() {
		report.Backends = append(report.Backends, apiProxyBackendReport{
			Name: backend.GetName(), Address: backend.GetAddress(), Local: backend.GetLocal(),
			Eligible: backend.GetEligible(), Reason: backend.GetReason(), LastChecked: backend.GetLastChecked(),
		})
	}
	return report
}

func newKubernetesStatusReport(status *agentapi.KubernetesStatus) *kubernetesStatusReport {
	if status == nil {
		return nil
	}
	return &kubernetesStatusReport{
		State:                       status.GetState(),
		Role:                        status.GetRole(),
		NodeName:                    status.GetNodeName(),
		KubeletActive:               status.GetKubeletActive(),
		NodeReady:                   status.GetNodeReady(),
		ControlPlaneComponentsReady: status.GetControlPlaneComponentsReady(),
		FailureReason:               status.GetFailureReason(),
	}
}

func newControlPlaneEndpointReport(status *agentapi.ControlPlaneEndpointStatus) *controlPlaneEndpointReport {
	if status == nil {
		return nil
	}
	report := &controlPlaneEndpointReport{
		Endpoint:           status.GetEndpoint(),
		VIP:                status.GetVip(),
		State:              status.GetState(),
		LocalAPIReady:      status.GetLocalApiReady(),
		LocalVIPOwned:      status.GetLocalVipOwned(),
		LastTransitionTime: status.GetLastTransitionTime(),
		FailureReason:      status.GetFailureReason(),
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
	w := newTable(stdout)
	w.row("NODE", "HEALTH", "KUBERNETES", "KATLOS", "GENERATION", "NEXT BOOT", "ACTIVITY")
	version := report.KatlOSVersion
	if report.KatlOSFlavour == "lts" {
		version += " (lts)"
	}
	if version == "" {
		version = "unknown"
	}
	nextBoot := report.NextBoot
	if nextBoot == "" {
		nextBoot = "-"
	}
	kubernetes := "-"
	if report.Kubernetes != nil {
		kubernetes = firstNonEmpty(strings.TrimSpace(report.Kubernetes.State), "unknown")
	}
	w.row(report.Node, report.Health, kubernetes, version, report.Generation, nextBoot, report.Activity)
	if report.BootHealthDiagnostic != "" {
		w.row("", report.BootHealthDiagnostic)
	}
	if report.Kubernetes != nil && report.Kubernetes.FailureReason != "" {
		w.row("", "", report.Kubernetes.FailureReason)
	}
	if err := w.flush(); err != nil {
		return err
	}
	if report.APIProxy != nil {
		proxy := report.APIProxy
		eligible := 0
		for _, backend := range proxy.Backends {
			if backend.Eligible {
				eligible++
			}
		}
		w = newTable(stdout)
		w.row()
		w.row("API PROXY", "LOCAL API", "CANONICAL ENDPOINT", "CANONICAL", "BACKENDS")
		w.row(proxy.State, yesNo(proxy.LocalAPIEligible), proxy.CanonicalEndpoint, proxy.CanonicalState, fmt.Sprintf("%d/%d", eligible, len(proxy.Backends)))
		if proxy.FailureReason != "" {
			w.row("", proxy.FailureReason)
		}
		for _, listener := range proxy.Listeners {
			w.row("listener", listener.Exposure, listener.Address)
		}
		if err := w.flush(); err != nil {
			return err
		}
	}
	if len(report.Volumes) > 0 {
		w = newTable(stdout)
		w.row()
		w.row("VOLUME", "TARGET", "MOUNT", "FILESYSTEM", "SOURCE", "STATE")
		for _, volume := range report.Volumes {
			state := firstNonEmpty(volume.ActiveState, "unknown")
			w.row(volume.Name, volume.TargetKind, volume.MountPath, volume.Filesystem, firstNonEmpty(volume.MountSource, "unbound"), state)
			if volume.FailureDiagnostic != "" {
				w.row("", "", "", "", "", volume.FailureDiagnostic)
			}
		}
		if err := w.flush(); err != nil {
			return err
		}
	}
	if report.ControlPlaneEndpoint == nil {
		return nil
	}
	endpoint := report.ControlPlaneEndpoint
	w = newTable(stdout)
	w.row()
	w.row("CONTROL PLANE ENDPOINT", "VIP", "STATE", "LOCAL API", "LOCAL VIP")
	w.row(endpoint.Endpoint, endpoint.VIP, endpoint.State, yesNo(endpoint.LocalAPIReady), yesNo(endpoint.LocalVIPOwned))
	if endpoint.FailureReason != "" {
		w.row("", endpoint.FailureReason)
	}
	return w.flush()
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
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
