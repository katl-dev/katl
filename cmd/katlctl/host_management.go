package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/configbundle"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/katlc/transport"
	"github.com/katl-dev/katl/internal/managementidentity"
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
	verbose bool
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
	Management           *managementStatusReport     `json:"management,omitempty"`
}

// managementStatusReport is the node's installed management mode and any
// recorded trust transition; it is absent for a node too old to report them.
type managementStatusReport struct {
	Authentication string                      `json:"authentication"`
	Authority      string                      `json:"authority,omitempty"`
	Transition     *managementTransitionReport `json:"transition,omitempty"`
	NextStep       string                      `json:"nextStep,omitempty"`
	trialDeadline  string
}

type managementTransitionReport struct {
	State           string `json:"state"`
	TargetAuthority string `json:"targetAuthority,omitempty"`
	Deadline        string `json:"deadline,omitempty"`
	RevertReason    string `json:"revertReason,omitempty"`
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
	ExternalEndpoint            string                   `json:"externalEndpoint,omitempty"`
	State                       string                   `json:"state"`
	Role                        string                   `json:"role"`
	NodeName                    string                   `json:"nodeName"`
	KubeletActive               bool                     `json:"kubeletActive"`
	NodeReady                   bool                     `json:"nodeReady"`
	ControlPlaneComponentsReady bool                     `json:"controlPlaneComponentsReady,omitempty"`
	FailureReason               string                   `json:"failureReason,omitempty"`
	CertificateExpiry           *certificateExpiryReport `json:"certificateExpiry,omitempty"`
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
	Node           string `json:"node"`
	Result         string `json:"result"`
	Generation     string `json:"generation"`
	Health         string `json:"health,omitempty"`
	Endpoint       string `json:"endpoint,omitempty"`
	AddressChanged bool   `json:"addressChanged,omitempty"`
}

type hostShutdownReport struct {
	Node   string `json:"node"`
	Result string `json:"result"`
}

var hostShutdownPollInterval = 500 * time.Millisecond

func newHostStatusCommand(ctx context.Context, stdout, stderr io.Writer) *cobra.Command {
	opts := hostStatusOptions{timeout: defaultManagementTimeout, output: hostOutputText}
	cmd := &cobra.Command{
		Use:   "status [NODE]",
		Short: "Show the current state of one KatlOS node",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			if err := selectHostNode(command, &opts.target.nodeName, args); err != nil {
				return err
			}
			return runHostStatus(ctx, opts, stdout, stderr)
		},
	}
	addManagementTargetFlags(cmd, &opts.target)
	cmd.Flags().DurationVar(&opts.timeout, "timeout", opts.timeout, "management request timeout")
	cmd.Flags().BoolVar(&opts.verbose, "verbose", false, "include healthy API proxy details and listener addresses")
	addOutputFlag(cmd, &opts.output, opts.output, "text", "json")
	return cmd
}

func newHostRebootCommand(ctx context.Context, stdout, stderr io.Writer) *cobra.Command {
	opts := hostRebootOptions{timeout: 15 * time.Minute, output: hostOutputText}
	cmd := &cobra.Command{
		Use:   "reboot [NODE]",
		Short: "Reboot one KatlOS node and wait for it to return healthy",
		Long: `Reboot one KatlOS node and wait for it to return healthy.

When cluster apply has staged a worker's move to a new management.address, pass
its current address with --endpoint. The reboot is scheduled there, and the
command waits for the same installation at either address.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			if err := selectHostNode(command, &opts.target.nodeName, args); err != nil {
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
		RunE: func(command *cobra.Command, args []string) error {
			if err := selectHostNode(command, &opts.target.nodeName, args); err != nil {
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

func selectHostNode(command *cobra.Command, selected *string, args []string) error {
	if len(args) == 0 {
		return nil
	}
	if command.Flags().Changed("node") {
		return fmt.Errorf("NODE cannot be combined with --node")
	}
	if strings.TrimSpace(args[0]) == "" {
		return fmt.Errorf("NODE must not be empty; choose a node name from your configuration")
	}
	*selected = args[0]
	return nil
}

func runHostStatus(ctx context.Context, opts hostStatusOptions, stdout, stderr io.Writer) error {
	_ = stderr
	if err := validateHostOutput(opts.output); err != nil {
		return err
	}
	ctx, observation := transport.ObserveManagementConnection(ctx)
	session, err := openManagementSession(ctx, opts.target, opts.timeout)
	if err != nil {
		return err
	}
	defer session.close()
	target := session.target
	node := hostTargetName(target)

	report, _, err := readStatusReport(session.ctx, session.client, node, target.endpoint, opts.target.clusterConfigPath, target.credentials, observation)
	if err != nil {
		return err
	}
	return writeHostStatus(stdout, opts.output, report, opts.verbose)
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

	// With --config, --endpoint names where the node answers now and
	// management.address names where its configuration places it after boot.
	desiredEndpoint := ""
	if strings.TrimSpace(opts.target.clusterConfigPath) != "" && strings.TrimSpace(opts.target.endpoint) != "" {
		configured, err := configuredManagementEndpoint(opts.target.clusterConfigPath, node)
		if err != nil {
			return err
		}
		if configured != "" && configured != target.endpoint {
			desiredEndpoint = configured
		}
	}

	status, _, _, err := readHostState(session.ctx, session.client, node)
	if err != nil {
		if desiredEndpoint != "" && managementUnreachable(err) {
			return fmt.Errorf("%w; %s", err, movedHint(node, desiredEndpoint, opts.target.clusterConfigPath))
		}
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
	wait := bootWaitTarget{nodeName: node, endpoints: []string{target.endpoint}, machineID: status.GetMachineId(), enrollmentID: status.GetEnrollmentId(), flags: opts.target.guidanceFlags()}
	if desiredEndpoint != "" {
		wait.endpoints = []string{desiredEndpoint, target.endpoint}
		_, _ = fmt.Fprintf(stderr, "Reboot scheduled for %s; waiting for KatlOS to return healthy at %s (or %s)...\n", node, desiredEndpoint, target.endpoint)
	} else {
		_, _ = fmt.Fprintf(stderr, "Reboot scheduled for %s; waiting for KatlOS to return healthy...\n", node)
	}
	waitCtx, cancelWait := context.WithTimeout(ctx, opts.timeout)
	waitCtx = withManagementTarget(waitCtx, target)
	verifiedConn, verified, err := waitNodeBootHealth(waitCtx, wait, previousAgentStart, generationID, recoveryRequirement, "reboot node="+node, stderr)
	cancelWait()
	if err != nil {
		if desiredEndpoint != "" {
			return addressChangeRebootError(node, target.endpoint, desiredEndpoint, verified.Endpoint, opts.target.clusterConfigPath, err)
		}
		return err
	}
	_ = verifiedConn.Close()
	if desiredEndpoint != "" && verified.Endpoint != desiredEndpoint {
		return addressChangeRebootError(node, target.endpoint, desiredEndpoint, verified.Endpoint, opts.target.clusterConfigPath, fmt.Errorf("node %s returned healthy at %s instead of %s", node, verified.Endpoint, desiredEndpoint))
	}
	report.Result = "rebooted"
	report.Health = displayHostHealth(verified.Generation)
	report.Endpoint = verified.Endpoint
	report.AddressChanged = desiredEndpoint != ""
	return writeHostReboot(stdout, opts.output, report)
}

func addressChangeRebootError(node, currentEndpoint, desiredEndpoint, answered, configPath string, err error) error {
	current, _, _ := net.SplitHostPort(currentEndpoint)
	if answered == currentEndpoint {
		return fmt.Errorf("%w; %s still answers at its previous address %s, so its new network configuration did not take effect. Correct its network configuration in %s, then rerun 'katlctl cluster apply --config %s --node %s --endpoint %s', or restore management.address to %s to keep the previous address", err, node, current, configPath, configPath, node, current, current)
	}
	return fmt.Errorf("%w; %s did not answer as the same installation at %s or %s. If its new address is wrong but still routable, the node can accept it and become unreachable: use the node console to inspect its network, or choose the previous generation from the boot menu, then correct %s", err, node, desiredEndpoint, currentEndpoint, configPath)
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
		conn, err := dialKatlcAgent(transport.WithoutManagementWait(attemptCtx), target.endpoint)
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

func readStatusReport(ctx context.Context, client agentapi.KatlcAgentClient, node, endpoint, configPath string, credentials *managementidentity.ClientCredentials, observation *transport.ManagementObservation) (hostStatusReport, *agentapi.NodeStatus, error) {
	status, current, observedAt, err := readHostState(ctx, client, node)
	if err == nil {
		return newHostStatusReport(node, endpoint, status, current, observedAt), status, nil
	}

	// The fallback belongs to status. A successful plaintext status read or a
	// failed generation lookup after it gives no reason to change credentials.
	configPath = strings.TrimSpace(configPath)
	if status != nil || credentials == nil || credentials.Authentication != managementidentity.TrustedNetwork ||
		configPath == "" || ctx.Err() != nil || !observation.PlaintextClosed() {
		return hostStatusReport{}, nil, err
	}
	data, readErr := os.ReadFile(configPath)
	if readErr != nil {
		return hostStatusReport{}, nil, err
	}
	source, readErr := configbundle.DecodeSource(bytes.NewReader(data))
	if readErr != nil {
		// Only a ClusterConfig can be passed to enable-mtls.
		return hostStatusReport{}, nil, err
	}
	invocation := enableMTLSInvocation(configPath, "")
	// Status can find only the default or referenced secrets. The operator
	// needs the original --output path if they chose an unreferenced file.
	err = fmt.Errorf("%w; the node may be on an interrupted enable-mtls trial. To resume it, rerun the original command for %s. If you used a custom secrets path, run %s and replace the placeholder with the original path. An unconfirmed trial returns to trusted-network by itself at its deadline",
		err, shellQuote(configPath), enableMTLSInvocation(configPath, "<original-secrets-path>"))
	path, readErr := enableMTLSSecretsPath(configPath, source, managementidentity.TrustedNetwork, "")
	if readErr != nil {
		return hostStatusReport{}, nil, fmt.Errorf("%w; resolve enable-mtls secrets: %v", err, readErr)
	}
	bundle, readErr := readManagementIdentityForCluster(path, source.Metadata.Name)
	if errors.Is(readErr, os.ErrNotExist) {
		return hostStatusReport{}, nil, err
	}
	if readErr != nil {
		return hostStatusReport{}, nil, fmt.Errorf("%w; read enable-mtls secrets at %s: %v", err, path, readErr)
	}
	identity, readErr := managementidentity.Client(bundle, time.Now().UTC())
	if readErr != nil {
		return hostStatusReport{}, nil, fmt.Errorf("%w; use enable-mtls secrets at %s: %v", err, path, readErr)
	}

	conn, retryErr := dialKatlcAgent(withManagementDial(ctx, node, &identity), endpoint)
	if retryErr != nil {
		return hostStatusReport{}, nil, fmt.Errorf("%w; connect using enable-mtls secrets: %v", err, retryErr)
	}
	defer conn.Close()
	status, current, observedAt, retryErr = readHostState(ctx, conn.Client, node)
	if retryErr != nil {
		return hostStatusReport{}, nil, fmt.Errorf("%w; using enable-mtls secrets: %v", err, retryErr)
	}
	report := newHostStatusReport(node, endpoint, status, current, observedAt)
	report.Management.addEnableMTLSGuidance(node, invocation)
	return report, status, nil
}

func readHostState(ctx context.Context, client agentapi.KatlcAgentClient, node string) (*agentapi.NodeStatus, *agentapi.Generation, time.Time, error) {
	status, err := client.GetNodeStatus(ctx, &agentapi.GetNodeStatusRequest{})
	if err != nil {
		return nil, nil, time.Time{}, fmt.Errorf("read status from %s: %w", node, err)
	}
	observedAt := time.Now()
	generationID := strings.TrimSpace(status.GetCurrentGenerationId())
	if status.GetBootHealthState() == "failed" && strings.TrimSpace(status.GetSelectedGenerationId()) != "" {
		generationID = strings.TrimSpace(status.GetSelectedGenerationId())
	}
	if generationID == "" {
		return status, nil, observedAt, fmt.Errorf("%s did not report a current KatlOS generation", node)
	}
	current, err := client.GetGeneration(ctx, &agentapi.GetGenerationRequest{GenerationId: generationID})
	if err != nil {
		return status, nil, observedAt, fmt.Errorf("read current generation from %s: %w", node, err)
	}
	return status, current, observedAt, nil
}

func newHostStatusReport(node, endpoint string, status *agentapi.NodeStatus, current *agentapi.Generation, observedAt time.Time) hostStatusReport {
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
		Management:           newManagementStatusReport(status.GetManagement(), observedAt),
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
		ExternalEndpoint:            status.GetExternalEndpoint(),
		State:                       status.GetState(),
		Role:                        status.GetRole(),
		NodeName:                    status.GetNodeName(),
		KubeletActive:               status.GetKubeletActive(),
		NodeReady:                   status.GetNodeReady(),
		ControlPlaneComponentsReady: status.GetControlPlaneComponentsReady(),
		FailureReason:               status.GetFailureReason(),
		CertificateExpiry:           newCertificateExpiryReport(status.GetCertificateExpiry()),
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

func writeHostStatus(stdout io.Writer, output string, report hostStatusReport, verbose bool) error {
	if output == hostOutputJSON {
		return writeJSON(stdout, report)
	}
	w := newTable(stdout)
	w.row("NODE", "HEALTH", "KUBERNETES", "KATLOS", "GENERATION", "NEXT BOOT", "ACTIVITY", "MANAGEMENT")
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
	w.row(report.Node, report.Health, kubernetes, version, report.Generation, nextBoot, report.Activity, report.Management.mode())
	if detail := report.Management.detail(); detail != "" {
		w.row("", detail)
	}
	if report.Management != nil && report.Management.NextStep != "" {
		w.row("", report.Management.NextStep)
	}
	if report.BootHealthDiagnostic != "" {
		w.row("", report.BootHealthDiagnostic)
	}
	if report.Kubernetes != nil && report.Kubernetes.FailureReason != "" {
		w.row("", "", report.Kubernetes.FailureReason)
	}
	if report.Kubernetes != nil && report.Kubernetes.CertificateExpiry != nil {
		w.row("", "", report.Kubernetes.CertificateExpiry.summary())
	}
	if err := w.flush(); err != nil {
		return err
	}
	if report.APIProxy != nil {
		proxy := report.APIProxy
		waitingForBootstrap := report.Kubernetes != nil && report.Kubernetes.State == "not-configured" && proxy.State == "no-eligible-backends"
		if verbose || !waitingForBootstrap && (proxy.State != "ready" || proxy.CanonicalState != "reachable" || proxy.FailureReason != "") {
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
			if proxy.CanonicalFailureReason != "" && proxy.CanonicalFailureReason != proxy.FailureReason {
				w.row("", proxy.CanonicalFailureReason)
			}
			if verbose {
				for _, listener := range proxy.Listeners {
					w.row("listener", listener.Exposure, listener.Address)
				}
			}
			if err := w.flush(); err != nil {
				return err
			}
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
	if report.AddressChanged {
		host, _, _ := net.SplitHostPort(report.Endpoint)
		_, err := fmt.Fprintf(stdout, "%s rebooted successfully at %s; health %s\n", report.Node, host, report.Health)
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

func newManagementStatusReport(status *agentapi.ManagementStatus, observedAt time.Time) *managementStatusReport {
	if status == nil {
		return nil
	}
	report := &managementStatusReport{Authentication: status.GetAuthentication(), Authority: status.GetAuthority()}
	if transition := status.GetTransition(); transition != nil {
		report.Transition = &managementTransitionReport{
			State: transition.GetState(), TargetAuthority: transition.GetTargetAuthority(),
			RevertReason: transition.GetRevertReason(),
		}
		if transition.GetState() == "trial" {
			offset, err := managementClockOffset(status, observedAt)
			if err != nil {
				report.NextStep = err.Error()
			} else if deadline := managementTrialDeadline(transition, offset); !deadline.IsZero() {
				deadline = deadline.Round(time.Second).Local()
				report.Transition.Deadline = deadline.Format(time.RFC3339)
				report.trialDeadline = deadline.Format(time.TimeOnly)
			}
		}
	}
	return report
}

func (report *managementStatusReport) addEnableMTLSGuidance(node, invocation string) {
	if report == nil || report.NextStep != "" {
		return
	}
	if transition := report.Transition; transition != nil && transition.State == "trial" {
		report.NextStep = fmt.Sprintf("%s is on an enable-mtls trial; rerun %s before %s to confirm it, or wait for it to return to trusted-network", node, invocation, firstNonEmpty(report.trialDeadline, "its deadline"))
	} else if report.Authentication == string(managementidentity.MutualTLS) {
		report.NextStep = fmt.Sprintf("%s already uses mTLS management; rerun %s to finish moving the cluster and update the config", node, invocation)
	}
}

// mode is the management column: the installed mode, marked while a trial of
// mTLS is live.
func (report *managementStatusReport) mode() string {
	if report == nil {
		return "-"
	}
	mode := firstNonEmpty(report.Authentication, "unknown")
	if report.Transition != nil && report.Transition.State == "trial" {
		mode += " (mtls trial)"
	}
	return mode
}

// detail explains a transition the operator may need to act on, if any.
func (report *managementStatusReport) detail() string {
	if report == nil || report.Transition == nil {
		return ""
	}
	switch transition := report.Transition; transition.State {
	case "trial":
		detail := "management: mTLS trial is active"
		if report.trialDeadline != "" {
			detail = "management: mTLS trial until " + report.trialDeadline
		}
		if report.NextStep != "" {
			return detail
		}
		return fmt.Sprintf("%s; it returns to %s unless katlctl management enable-mtls confirms it", detail, firstNonEmpty(report.Authentication, "its installed mode"))
	case "reverted":
		if report.Authentication == string(managementidentity.MutualTLS) {
			return ""
		}
		return "management: last mTLS trial reverted: " + firstNonEmpty(transition.RevertReason, "no reason recorded")
	case "unreadable":
		return "management: the transition record is unreadable; run katlc agent management-trial revert on the node"
	}
	return ""
}
