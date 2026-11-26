package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/installer/configbundle"
	"github.com/katl-dev/katl/internal/installer/operation"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/katlc/transport"
	"github.com/katl-dev/katl/internal/katlctl/workstation"
	"github.com/katl-dev/katl/internal/managementidentity"
	"github.com/katl-dev/katl/internal/nodeidentity"
	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

const (
	enableMTLSActor             = "katlctl management enable-mtls"
	upgradeManagementTrialAgent = "upgrade the node with `katlctl node upgrade`, then rerun"
)

// enableMTLSPoll is how often a node is polled while its trial starts and
// while it returns to health.
var enableMTLSPoll = time.Second

var trustedNetworkClient = managementidentity.ClientCredentials{Authentication: managementidentity.TrustedNetwork}

type enableMTLSOptions struct {
	configPath string
	outputPath string
	timeout    time.Duration
	// trialWindow is how long each node serves the new authority on trial. A
	// node that is not confirmed within it returns to trusted-network by
	// itself.
	trialWindow time.Duration
}

// enableMTLSPhase is a node's management state as observed from the
// workstation; it is never stored.
type enableMTLSPhase int

const (
	nodeOnTrustedNetwork enableMTLSPhase = iota
	nodeOnTrial
	nodeOnMTLS
)

type enableMTLSNode struct {
	name         string
	endpoint     inventory.HostPort
	savedMachine string
	controlPlane bool
	externalAPI  bool
	managedVIP   bool

	phase enableMTLSPhase
	// status and identity are the preflight observation; later requests must
	// reach the same installation.
	status      *agentapi.NodeStatus
	identity    transport.NodeIdentity
	requirement nodeRecoveryRequirement
	// A node that has reported served trust must keep reporting it for
	// every later verification in this run.
	reportedTrust bool
	// trialDeadline is the live trial's deadline on this workstation's clock, when
	// resuming.
	trialDeadline time.Time
	lastRevert    string
	// clockOffset is how far the node clock is ahead of this workstation,
	// erring towards behind.
	clockOffset time.Duration
}

func newManagementEnableMTLSCommand(stdout, stderr io.Writer) *cobra.Command {
	opts := enableMTLSOptions{timeout: 2 * time.Minute, trialWindow: 5 * time.Minute}
	command := &cobra.Command{
		Use:   "enable-mtls",
		Short: "Move an installed trusted-network cluster to mTLS management",
		Long: `Check every node over trusted-network management, create or reuse the
mTLS management secrets, then move the nodes to them one at a time in config
order. Each node serves the new authority on trial and returns to
trusted-network by itself unless katlctl reaches it over mTLS and confirms the
change. The ClusterConfig and any saved context switch to mTLS only after every
node is confirmed. Rerun the same command to resume after an interruption or a
failed node; nodes already on mTLS are checked without starting a new trial.`,
		Example: "katlctl management enable-mtls --config ./cluster.yaml",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runManagementEnableMTLS(cmd.Context(), opts, stdout, stderr)
		},
	}
	command.Flags().StringVar(&opts.configPath, "config", "", "ClusterConfig YAML of the installed cluster")
	command.Flags().StringVar(&opts.outputPath, "output", "", "management secrets file to create or reuse; .sops.yaml encrypts with SOPS (default management-secrets.yaml beside the config)")
	command.Flags().DurationVar(&opts.timeout, "timeout", opts.timeout, "time allowed for each node connection and health check")
	command.Flags().DurationVar(&opts.trialWindow, "trial-window", opts.trialWindow, "how long each node keeps the new authority unconfirmed before it returns to trusted-network by itself")
	_ = command.MarkFlagRequired("config")
	return command
}

func runManagementEnableMTLS(ctx context.Context, opts enableMTLSOptions, stdout, stderr io.Writer) error {
	if opts.timeout <= 0 {
		return fmt.Errorf("--timeout must be positive")
	}
	// The node refuses a window outside its bounds and receives whole seconds,
	// so check here before preflight creates anything.
	if window := opts.trialWindow; window < nodeidentity.MinimumManagementTrialWindow || window > nodeidentity.MaximumManagementTrialWindow || window%time.Second != 0 {
		return fmt.Errorf("--trial-window must be a whole number of seconds from %s to %s, the range a node accepts", nodeidentity.MinimumManagementTrialWindow, nodeidentity.MaximumManagementTrialWindow)
	}
	configPath, err := filepath.Abs(opts.configPath)
	if err != nil {
		return err
	}
	sourceData, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read ClusterConfig: %w", err)
	}
	source, err := configbundle.DecodeSource(bytes.NewReader(sourceData))
	if err != nil {
		return err
	}
	mode, err := source.ManagementAuthentication()
	if err != nil {
		return err
	}
	secretsPath, err := enableMTLSSecretsPath(configPath, source, mode, opts.outputPath)
	if err != nil {
		return err
	}

	// Preflight reads existing secrets but creates none, so a failed
	// preflight leaves the workstation and every node unchanged. Without
	// secrets, preflight passes only if every node answers plaintext
	// trusted-network management, so creating them strands no node even when
	// the config already says mtls.
	var bundle *managementidentity.Bundle
	if _, err := os.Stat(secretsPath); err == nil {
		existing, err := replacementManagementIdentity(configPath, secretsPath, source)
		if err != nil {
			return err
		}
		bundle = &existing
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var authority string
	var client *managementidentity.ClientCredentials
	if bundle != nil {
		if authority, client, err = enableMTLSClient(*bundle); err != nil {
			return err
		}
	}

	nodes, err := enableMTLSNodes(ctx, source)
	if err != nil {
		return err
	}
	fmt.Fprintf(stderr, "Preflight: checking %d node(s); nothing changes until every check passes\n", len(nodes))
	pending := 0
	for i := range nodes {
		node := &nodes[i]
		err := observeEnableMTLSNode(ctx, node, client, authority, opts.timeout)
		if err == nil {
			err = enableMTLSReady(ctx, *node, nodeClient(*node, client), opts.timeout)
		}
		if err != nil {
			return fmt.Errorf("preflight failed; no node was changed: %w. Rerun %s to continue", err, enableMTLSInvocation(configPath, secretsPath))
		}
		if node.phase != nodeOnMTLS {
			pending++
		}
	}
	if bundle == nil {
		// The secrets are durable before any node can depend on them.
		created, err := replacementManagementIdentity(configPath, secretsPath, source)
		if err != nil {
			return fmt.Errorf("create management secrets: %w; no node was changed", err)
		}
		bundle = &created
		if authority, client, err = enableMTLSClient(created); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Created management secrets at %s; back this file up now: once nodes move to mTLS it is the only way to manage them\n", secretsPath)
	} else {
		fmt.Fprintf(stderr, "Using management secrets at %s (authority %s)\n", secretsPath, authority)
	}

	for i := range nodes {
		node := &nodes[i]
		progress := fmt.Sprintf("(%d/%d)", i+1, len(nodes))
		if node.phase == nodeOnMTLS {
			fmt.Fprintf(stderr, "%s: already mTLS %s\n", node.name, progress)
			continue
		}
		if err := migrateEnableMTLSNode(ctx, node, *bundle, *client, authority, opts, stderr); err != nil {
			return fmt.Errorf("%s: %w%s", node.name, err, enableMTLSResumeHint(nodes[:i], mode, enableMTLSInvocation(configPath, secretsPath)))
		}
		fmt.Fprintf(stderr, "%s: mTLS confirmed %s\n", node.name, progress)
	}

	saved, savedPath, err := prepareEnableMTLSContext(ctx, configPath, source.Metadata.Name, nodes, *client, authority, opts.timeout)
	if err != nil {
		return fmt.Errorf("saved context could not be prepared: %w. Rerun %s to continue", err, enableMTLSInvocation(configPath, secretsPath))
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if mode != managementidentity.MutualTLS {
		if err := referenceManagementSecrets(configPath, secretsPath, sourceData); err != nil {
			relative, relErr := filepath.Rel(filepath.Dir(configPath), secretsPath)
			if relErr != nil {
				relative = secretsPath
			}
			return fmt.Errorf("every node now uses mTLS management, but %s could not be updated: %w; fix that and rerun %s, or set these fields under spec, then run %s:\n  managementAuthentication: mtls\n  managementIdentity: %s", configPath, err, enableMTLSInvocation(configPath, secretsPath), contextSaveInvocation(configPath), filepath.ToSlash(relative))
		}
	}
	if saved != nil {
		if err := workstation.Save(savedPath, *saved); err != nil {
			return fmt.Errorf("every node now uses mTLS management and %s references %s, but the saved context could not be refreshed: %w; run %s", configPath, secretsPath, err, contextSaveInvocation(configPath))
		}
	}
	alreadyMTLS := pending == 0 && mode == managementidentity.MutualTLS
	if alreadyMTLS {
		fmt.Fprintf(stdout, "Cluster %s already uses mTLS management on %d node(s); authority %s\n", source.Metadata.Name, len(nodes), authority)
	} else {
		fmt.Fprintf(stdout, "Cluster %s now uses mTLS management on %d node(s); authority %s\n", source.Metadata.Name, len(nodes), authority)
		fmt.Fprintf(stdout, "%s now references %s; keep that file backed up.\n", configPath, secretsPath)
	}
	if saved != nil {
		fmt.Fprintf(stdout, "Refreshed the saved context for %s.\n", source.Metadata.Name)
	} else if !alreadyMTLS {
		fmt.Fprintf(stdout, "Save a shortcut for later commands with %s.\n", contextSaveInvocation(configPath))
	}
	return nil
}

// enableMTLSSecretsPath resolves the secrets file a run creates or resumes
// with. A config that already says mtls names its own file, so a different
// --output would split the cluster between two authorities.
func enableMTLSSecretsPath(configPath string, source configbundle.SourceConfig, mode managementidentity.Authentication, output string) (string, error) {
	referenced := strings.TrimSpace(source.Spec.ManagementIdentity)
	if referenced != "" && !filepath.IsAbs(referenced) {
		referenced = filepath.Join(filepath.Dir(configPath), referenced)
	}
	if output != "" {
		var err error
		if output, err = filepath.Abs(output); err != nil {
			return "", err
		}
	}
	if mode == managementidentity.MutualTLS {
		if referenced == "" {
			path, err := managementIdentityPath(source.Metadata.Name)
			if err != nil {
				return "", err
			}
			referenced = path
		}
		referenced = filepath.Clean(referenced)
		if output != "" && output != referenced {
			return "", fmt.Errorf("ClusterConfig already uses mTLS management with secrets at %s; rerun without --output to finish with that file", referenced)
		}
		return referenced, nil
	}
	switch {
	case output != "":
		return output, nil
	case referenced != "":
		return filepath.Clean(referenced), nil
	default:
		return filepath.Join(filepath.Dir(configPath), defaultManagementSecrets), nil
	}
}

func enableMTLSClient(bundle managementidentity.Bundle) (string, *managementidentity.ClientCredentials, error) {
	now := time.Now().UTC()
	info, err := managementidentity.Validate(bundle, now)
	if err != nil {
		return "", nil, err
	}
	client, err := managementidentity.Client(bundle, now)
	if err != nil {
		return "", nil, err
	}
	return info.Fingerprint, &client, nil
}

func enableMTLSNodes(ctx context.Context, source configbundle.SourceConfig) ([]enableMTLSNode, error) {
	inv, err := configbundle.SourceTopology(source)
	if err != nil {
		return nil, err
	}
	topology, err := workstation.ResolveTopology(workstation.ResolveRequest{ExplicitInventory: &inv})
	if err != nil {
		return nil, err
	}
	nodes := make([]enableMTLSNode, 0, len(topology.Nodes))
	for _, node := range topology.Nodes {
		if node.ManagementEndpoint.IsZero() {
			return nil, fmt.Errorf("node %q needs management.address before moving to mTLS", node.Name)
		}
		// The config names each node's address; a saved context only adds the
		// machine the node was enrolled as.
		saved := ""
		if target, ok := enrolledTarget(ctx, "", "", source.Metadata.Name, node.Name); ok {
			saved = target.machineID
		}
		nodes = append(nodes, enableMTLSNode{
			name: node.Name, endpoint: node.ManagementEndpoint, savedMachine: saved,
			controlPlane: node.SystemRole == inventory.RoleControlPlane,
			externalAPI:  topology.ExternalControlPlane, managedVIP: inv.ControlPlaneEndpointManaged,
		})
	}
	return nodes, nil
}

// observeEnableMTLSNode derives a node's phase from what it answers: the new
// authority means it is on trial or confirmed, plaintext means it is still on
// trusted-network. A node in a live trial does not answer plaintext.
func observeEnableMTLSNode(ctx context.Context, node *enableMTLSNode, client *managementidentity.ClientCredentials, authority string, timeout time.Duration) error {
	probe := min(timeout, 15*time.Second)
	if client != nil {
		probeCtx, observation := transport.ObserveManagementConnection(ctx)
		started := time.Now()
		status, err := enableMTLSStatus(probeCtx, *node, *client, probe)
		observedAt := time.Now()
		if transport.ClassifyManagementEvidence(err, nil, nil) == transport.ManagementConfirmed {
			if err := node.bind(status, observedAt); err != nil {
				return err
			}
			deadline, err := verifyEnableMTLSPhase(status, authority, started)
			if err != nil {
				return fmt.Errorf("%s: %w; inspect it with 'katlctl node status' before rerunning", node.name,
					managementConfirmationError("the node accepts the replacement authority on trial or committed", "its previous management authority", err))
			}
			if deadline.IsZero() {
				node.phase = nodeOnMTLS
			} else {
				node.phase = nodeOnTrial
				node.trialDeadline = deadline
			}
			return nil
		}
		// Only a timely, complete plaintext server preface warrants a
		// plaintext status request. A failed TLS connection alone never does.
		if grpcstatus.Code(err) != codes.Unavailable || !observation.PlaintextHTTP2() {
			return fmt.Errorf("%s at %s: %w; check the management address and node reachability", node.name, node.endpoint,
				managementConfirmationError("the node accepts the replacement authority", "its previous management authority", err))
		}
	}

	status, err := enableMTLSStatus(ctx, *node, trustedNetworkClient, probe)
	observedAt := time.Now()
	if transport.ClassifyManagementEvidence(err, nil, nil) != transport.ManagementConfirmed {
		return fmt.Errorf("%s at %s: %w; check the management address and node reachability", node.name, node.endpoint,
			managementConfirmationError("the node uses trusted-network management", "an mTLS authority", err))
	}
	if err := node.bind(status, observedAt); err != nil {
		return err
	}
	management := status.GetManagement()
	if management.GetAuthentication() != string(managementidentity.TrustedNetwork) {
		return fmt.Errorf("%s answered plaintext management but reports %q mode; inspect it with 'katlctl node status' before rerunning", node.name, management.GetAuthentication())
	}
	if err := checkServedClientTrust(management.GetServedClientTrust()); err != nil {
		return managementConfirmationError("the node uses trusted-network management", "an mTLS authority", err)
	}
	node.phase = nodeOnTrustedNetwork
	if transition := management.GetTransition(); transition.GetState() == "reverted" {
		node.lastRevert = transition.GetRevertReason()
	}
	return nil
}

// bind checks that the endpoint answered as the configured node and records
// that installation for every later request. observedAt is when the status
// arrived, so the recorded clock offset errs towards the node being behind.
func (node *enableMTLSNode) bind(status *agentapi.NodeStatus, observedAt time.Time) error {
	var err error
	node.clockOffset, err = managementClockOffset(status.GetManagement(), observedAt)
	if err != nil {
		return fmt.Errorf("%s: %w", node.name, err)
	}
	identity, err := transport.ObserveNode(node.name, status)
	if err != nil {
		return fmt.Errorf("%s: %w; check its management.address", node.name, err)
	}
	if node.savedMachine != "" && status.GetMachineId() != node.savedMachine {
		return fmt.Errorf("%s answered with machine ID %s, but the saved context recorded %s; check that its management.address points at that machine", node.name, status.GetMachineId(), node.savedMachine)
	}
	node.status, node.identity = status, identity
	node.reportedTrust = status.GetManagement().GetServedClientTrust() != nil
	// The agent reports a Kubernetes role from installed configuration even
	// when kubelet or the API is unhealthy. Fresh installs can migrate before
	// bootstrap. Joined nodes must meet the same requirements on every run.
	kubernetes := status.GetKubernetes()
	if kubernetes.GetRole() != "" || kubernetes.GetState() != "not-configured" {
		// The agent reports the configured VIP both when ownership is disabled
		// and when an enabled owner has released it. A released VIP still gates retries.
		endpoint := status.GetControlPlaneEndpoint()
		requireVIP := (node.controlPlane && node.managedVIP) || (endpoint.GetVip() != "" && endpoint.GetState() != "disabled")
		node.requirement = nodeRecoveryRequirement{
			KubernetesConfigured: true,
			NodeReady:            true,
			APIProxyReady:        !node.externalAPI || status.GetApiProxy() != nil,
			LocalAPIEligible:     node.controlPlane && !node.externalAPI,
			ManagedEndpointReady: requireVIP,
		}
	}
	return nil
}

func (node *enableMTLSNode) verifyBoundStatus(status *agentapi.NodeStatus) error {
	if err := node.identity.Verify(status); err != nil {
		return err
	}
	if status.GetManagement().GetServedClientTrust() != nil {
		node.reportedTrust = true
	} else if node.reportedTrust {
		return fmt.Errorf("listener did not report its client CA trust after reporting it earlier")
	}
	return nil
}

func (node *enableMTLSNode) verifyCommittedStatus(status *agentapi.NodeStatus, authority string) error {
	if err := node.verifyBoundStatus(status); err != nil {
		return err
	}
	return checkCommittedManagement(status, authority)
}

func managementClockOffset(status *agentapi.ManagementStatus, observedAt time.Time) (time.Duration, error) {
	if status == nil {
		return 0, fmt.Errorf("the installed KatlOS release cannot move management trust. To continue, %s", upgradeManagementTrialAgent)
	}
	nodeTime, err := time.Parse(time.RFC3339Nano, status.GetNodeTime())
	if err != nil || nodeTime.IsZero() {
		return 0, fmt.Errorf("management status did not report a valid node time. To continue, %s", upgradeManagementTrialAgent)
	}
	return nodeTime.Sub(observedAt), nil
}

func managementTrialDeadline(transition *agentapi.ManagementTransition, offset time.Duration) time.Time {
	deadline, err := time.Parse(time.RFC3339, transition.GetDeadline())
	if err != nil {
		return time.Time{}
	}
	return deadline.Add(-offset)
}

// verifyEnableMTLSPhase returns zero for committed trust or the deadline of
// the answering listener's live trial.
func verifyEnableMTLSPhase(status *agentapi.NodeStatus, authority string, requestStarted time.Time) (time.Time, error) {
	management := status.GetManagement()
	// The node sampled its clock after the request started. This estimate
	// errs towards an earlier local deadline, even if the reply was delayed.
	offset, err := managementClockOffset(management, requestStarted)
	if err != nil {
		return time.Time{}, err
	}
	if management.GetAuthentication() == string(managementidentity.MutualTLS) {
		return time.Time{}, checkCommittedManagement(status, authority)
	}
	trust := management.GetServedClientTrust()
	if err := checkServedClientTrust(trust, authority); err != nil {
		return time.Time{}, err
	}
	transition := management.GetTransition()
	if management.GetAuthentication() != string(managementidentity.TrustedNetwork) ||
		transition.GetState() != string(nodeidentity.ManagementTrialLive) || transition.GetTargetAuthority() != authority {
		return time.Time{}, fmt.Errorf("node reports installed mode %q and trial state %q for authority %q", management.GetAuthentication(), transition.GetState(), transition.GetTargetAuthority())
	}
	if trust != nil && (trust.GetTrialId() == "" || trust.GetTrialId() != transition.GetTrialId() || trust.GetTrialAuthority() != authority) {
		return time.Time{}, fmt.Errorf("answering listener did not select credentials from the reported trial")
	}
	deadline := managementTrialDeadline(transition, offset)
	if deadline.IsZero() {
		return time.Time{}, fmt.Errorf("node did not report a valid mTLS trial deadline")
	}
	if !time.Now().Before(deadline) {
		return time.Time{}, fmt.Errorf("the mTLS trial passed its deadline at %s but has not returned to trusted-network yet; wait for it to revert, then rerun", deadline.Local().Format(time.TimeOnly))
	}
	return deadline, nil
}

func nodeClient(node enableMTLSNode, client *managementidentity.ClientCredentials) managementidentity.ClientCredentials {
	if node.phase == nodeOnTrustedNetwork || client == nil {
		return trustedNetworkClient
	}
	return *client
}

// enableMTLSReady refuses a node that is busy or unhealthy before any node
// changes, so a migration never starts on a cluster that is already degraded.
func enableMTLSReady(ctx context.Context, node enableMTLSNode, credentials managementidentity.ClientCredentials, timeout time.Duration) error {
	status := node.status
	if active := status.GetActiveOperationIds(); len(active) > 0 {
		return fmt.Errorf("%s has operation %s active; wait for it to finish, then rerun", node.name, strings.Join(active, ", "))
	}
	if status.GetOperationLockHeld() {
		return fmt.Errorf("%s holds its operation lock; wait for the running change to finish, then rerun", node.name)
	}
	// A node checks the new certificates against its own clock before it
	// stages them, and they become valid only CertificateBackdate before issue.
	if node.phase == nodeOnTrustedNetwork && node.clockOffset < -managementidentity.CertificateBackdate {
		return fmt.Errorf("%s's clock is %s behind this workstation, more than the %s the new certificates allow; correct its time, for example its NTP source, then rerun", node.name, (-node.clockOffset).Round(time.Second), managementidentity.CertificateBackdate)
	}
	if err := enableMTLSHealth(ctx, node, credentials, status, timeout); err != nil {
		possibleTrust := "the replacement authority"
		if node.phase == nodeOnTrustedNetwork {
			possibleTrust = "the trusted network"
		}
		return fmt.Errorf("%s: %w", node.name, managementConfirmationError("the node is healthy", possibleTrust, err))
	}
	return nil
}

// enableMTLSHealth checks the node's boot health and Kubernetes recovery
// against the node's configured role, and etcd quorum on a control plane.
func enableMTLSHealth(ctx context.Context, node enableMTLSNode, credentials managementidentity.ClientCredentials, status *agentapi.NodeStatus, timeout time.Duration) error {
	if state := status.GetBootHealthState(); state != bootHealthHealthy {
		return fmt.Errorf("boot health is %s: %s", firstNonEmpty(state, "not reported"), firstNonEmpty(status.GetBootHealthDiagnostic(), "no diagnostic"))
	}
	if recovery := nodeUpgradeRecovery(status, node.requirement); !recovery.Ready {
		return fmt.Errorf("Kubernetes is %s: %s", recovery.State, recovery.Reason)
	}
	kubernetes := status.GetKubernetes()
	if !strings.EqualFold(kubernetes.GetRole(), "control-plane") || kubernetes.GetState() == "not-configured" {
		return nil
	}
	var etcd *agentapi.EtcdStatus
	if err := enableMTLSCall(ctx, node, credentials, timeout, func(ctx context.Context, client agentapi.KatlcAgentClient) (err error) {
		etcd, err = client.GetEtcdStatus(ctx, &agentapi.GetEtcdStatusRequest{})
		return err
	}); err != nil {
		return fmt.Errorf("inspect etcd: %w", err)
	}
	if len(etcd.GetMembers()) == 0 {
		return fmt.Errorf("etcd reported no members")
	}
	if err := etcdBelowQuorum(etcd); err != nil {
		return err
	}
	for _, member := range etcd.GetMembers() {
		if member.GetId() == etcd.GetLocalMemberId() && !member.GetHealthy() {
			return fmt.Errorf("local etcd member %s is unhealthy: %s", member.GetName(), firstNonEmpty(member.GetHealthError(), "no error reported"))
		}
	}
	return nil
}

// migrateEnableMTLSNode starts or resumes one trial, then checks the committed
// authority and health over mTLS before allowing the rollout to continue.
func migrateEnableMTLSNode(ctx context.Context, node *enableMTLSNode, bundle managementidentity.Bundle, client managementidentity.ClientCredentials, authority string, opts enableMTLSOptions, stderr io.Writer) error {
	timeout := opts.timeout
	deadline := node.trialDeadline
	trialID := node.status.GetManagement().GetTransition().GetTrialId()
	statusCommand := "katlctl node status " + shellQuote(node.name) + " --config " + shellQuote(opts.configPath)
	if node.phase == nodeOnTrustedNetwork {
		if node.lastRevert != "" {
			fmt.Fprintf(stderr, "%s: an earlier trial reverted (%s); starting a new one\n", node.name, node.lastRevert)
		}
		credentials, ok := bundle.Nodes[node.name]
		if !ok {
			return fmt.Errorf("management secrets lack node %q", node.name)
		}
		var trial *agentapi.ManagementTransition
		started := time.Now()
		err := enableMTLSCall(ctx, *node, trustedNetworkClient, timeout, func(ctx context.Context, agent agentapi.KatlcAgentClient) (err error) {
			trial, err = agent.BeginManagementTransition(ctx, &agentapi.BeginManagementTransitionRequest{
				ApiVersion: operation.APIVersion, Kind: "BeginManagementTransitionRequest", Actor: enableMTLSActor,
				ExpectedEnrollmentId: node.status.GetEnrollmentId(), ExpectedInventoryNodeName: node.name, ExpectedMachineId: node.status.GetMachineId(),
				ExpectedAuthentication: string(managementidentity.TrustedNetwork),
				CaCertificate:          bundle.CertificateAuthority.Certificate, ServerCertificate: credentials.Certificate, ServerPrivateKey: credentials.PrivateKey,
				TrialSeconds: uint32(opts.trialWindow / time.Second),
			})
			return err
		})
		if transport.ClassifyManagementEvidence(err, nil, nil) != transport.ManagementConfirmed {
			return enableMTLSBeginError(err)
		}
		deadline = started.Add(opts.trialWindow)
		trialID = trial.GetTrialId()
		if trial.GetState() != "committed" {
			fmt.Fprintf(stderr, "%s: serving the new authority on trial; it returns to trusted-network by %s unless confirmed\n", node.name, deadline.Local().Format(time.TimeOnly))
		}
	} else {
		if !time.Now().Before(deadline) {
			return fmt.Errorf("its earlier trial passed its deadline at %s but it has not returned to trusted-network yet; wait for it to revert, then rerun this command", deadline.Local().Format(time.TimeOnly))
		}
		fmt.Fprintf(stderr, "%s: resuming its trial of the new authority\n", node.name)
	}
	observedDeadline, err := waitEnableMTLSTrial(ctx, node, client, authority, trialID, min(time.Until(deadline), timeout))
	if err != nil {
		return fmt.Errorf("%w; check management.address", managementConfirmationError("the node accepts the replacement authority", "the trusted network", err))
	}
	if !observedDeadline.IsZero() {
		confirmErr := enableMTLSCall(ctx, *node, client, min(timeout, time.Until(deadline), time.Until(observedDeadline)), func(ctx context.Context, agent agentapi.KatlcAgentClient) error {
			_, err := agent.ConfirmManagementTransition(ctx, &agentapi.ConfirmManagementTransitionRequest{
				ApiVersion: operation.APIVersion, Kind: "ConfirmManagementTransitionRequest", Actor: enableMTLSActor,
				ExpectedEnrollmentId: node.status.GetEnrollmentId(), ExpectedInventoryNodeName: node.name, ExpectedMachineId: node.status.GetMachineId(),
				TargetAuthority: authority, ExpectedTrialId: &trialID,
			})
			return err
		})
		switch grpcstatus.Code(confirmErr) {
		case codes.FailedPrecondition, codes.InvalidArgument, codes.PermissionDenied:
			return fmt.Errorf("%w; check status with %s",
				managementConfirmationError("mTLS confirmation", "the replacement authority", confirmErr), statusCommand)
		}
		// A lost confirm reply is resolved by a fresh status over the replacement credentials.
		status, statusErr := enableMTLSStatus(ctx, *node, client, timeout)
		if statusErr == nil {
			statusErr = node.verifyCommittedStatus(status, authority)
		}
		if statusErr != nil {
			return fmt.Errorf("%w; check status with %s",
				managementConfirmationError("mTLS confirmation", "the replacement authority", errors.Join(confirmErr, statusErr)), statusCommand)
		}
	}

	if err := waitEnableMTLSHealthy(ctx, node, client, authority, timeout); err != nil {
		return fmt.Errorf("%w; check status with %s",
			managementConfirmationError("the node is healthy over mTLS", "the replacement authority", err), statusCommand)
	}
	return nil
}

func enableMTLSBeginError(err error) error {
	switch grpcstatus.Code(err) {
	case codes.Unimplemented:
		return fmt.Errorf("its KatlOS release cannot move management trust; upgrade it first with 'katlctl node upgrade', then rerun")
	case codes.InvalidArgument, codes.FailedPrecondition:
		return fmt.Errorf("start the mTLS trial: %w; resolve it, then rerun", err)
	default:
		return managementConfirmationError("the mTLS trial started", "the trusted network", err)
	}
}

// waitEnableMTLSTrial returns the deadline of a verified trial, or a zero
// deadline if the same installation already committed the authority.
func waitEnableMTLSTrial(ctx context.Context, node *enableMTLSNode, client managementidentity.ClientCredentials, authority, trialID string, wait time.Duration) (time.Time, error) {
	deadline, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	for {
		started := time.Now()
		status, err := enableMTLSStatus(deadline, *node, client, min(wait, 5*time.Second))
		if transport.ClassifyManagementEvidence(err, nil, nil) == transport.ManagementConfirmed {
			if verifyErr := node.verifyBoundStatus(status); verifyErr != nil {
				return time.Time{}, verifyErr
			}
			trialDeadline, err := verifyEnableMTLSPhase(status, authority, started)
			if err != nil {
				return time.Time{}, err
			}
			if !trialDeadline.IsZero() {
				if trialID == "" {
					return time.Time{}, fmt.Errorf("node did not report the mTLS trial ID")
				}
				if status.GetManagement().GetTransition().GetTrialId() != trialID {
					return time.Time{}, fmt.Errorf("node reports a different mTLS trial")
				}
			}
			return trialDeadline, nil
		}
		select {
		case <-deadline.Done():
			return time.Time{}, fmt.Errorf("no usable status within %s: %w", wait.Round(time.Second), err)
		case <-time.After(enableMTLSPoll):
		}
	}
}

func waitEnableMTLSHealthy(ctx context.Context, node *enableMTLSNode, client managementidentity.ClientCredentials, authority string, timeout time.Duration) error {
	deadline, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var lastErr error
	for {
		status, err := enableMTLSStatus(deadline, *node, client, min(timeout, 5*time.Second))
		if transport.ClassifyManagementEvidence(err, nil, nil) == transport.ManagementConfirmed {
			if err := node.verifyCommittedStatus(status, authority); err != nil {
				return err
			}
			err = enableMTLSHealth(deadline, *node, client, status, min(timeout, 5*time.Second))
			if err == nil {
				return nil
			}
		}
		// The last RPC can exhaust the remaining time. Keep an earlier
		// diagnosis when that RPC only reports a deadline error.
		if lastErr == nil || (!errors.Is(err, context.DeadlineExceeded) && grpcstatus.Code(err) != codes.DeadlineExceeded) {
			lastErr = err
		}
		select {
		case <-deadline.Done():
			return lastErr
		case <-time.After(enableMTLSPoll):
		}
	}
}

func enableMTLSStatus(ctx context.Context, node enableMTLSNode, credentials managementidentity.ClientCredentials, timeout time.Duration) (*agentapi.NodeStatus, error) {
	var status *agentapi.NodeStatus
	err := enableMTLSCall(ctx, node, credentials, timeout, func(ctx context.Context, agent agentapi.KatlcAgentClient) (err error) {
		status, err = agent.GetNodeStatus(ctx, &agentapi.GetNodeStatusRequest{})
		return err
	})
	return status, err
}

// enableMTLSCall makes one call with exactly the given credentials. A refused
// connection fails at once because callers poll for themselves.
func enableMTLSCall(ctx context.Context, node enableMTLSNode, credentials managementidentity.ClientCredentials, timeout time.Duration, call func(context.Context, agentapi.KatlcAgentClient) error) error {
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := transport.ManagementContextError(requestCtx); err != nil {
		return err
	}
	requestCtx = transport.WithoutManagementAdvice(requestCtx)
	conn, err := dialKatlcAgent(transport.WithoutManagementWait(withManagementDial(requestCtx, node.name, &credentials)), node.endpoint.String())
	if err != nil {
		return err
	}
	defer conn.Close()
	return call(requestCtx, conn.Client)
}

// Prepare the refreshed context before publishing the mTLS configuration so
// failed verification leaves both files as they were.
func prepareEnableMTLSContext(ctx context.Context, configPath, clusterName string, nodes []enableMTLSNode, client managementidentity.ClientCredentials, authority string, timeout time.Duration) (*workstation.Config, string, error) {
	path, err := workstation.ConfigPath()
	if err != nil {
		return nil, "", err
	}
	saved, err := workstation.Load(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	contextName := ""
	for _, saved := range saved.Contexts {
		if strings.TrimSpace(saved.Cluster) == clusterName {
			contextName = saved.Name
			break
		}
	}
	if contextName == "" {
		return nil, "", nil
	}
	for _, cluster := range saved.Clusters {
		if cluster.Name == clusterName && cluster.Management != nil && *cluster.Management == client {
			return nil, "", nil
		}
	}
	bound := make(map[string]*enableMTLSNode, len(nodes))
	endpoints := make(map[string]inventory.HostPort, len(nodes))
	for i := range nodes {
		node := &nodes[i]
		bound[node.name], endpoints[node.name] = node, node.endpoint
	}
	opts := contextSaveOptions{configInput: configPath, contextPath: path, contextName: contextName, timeout: timeout, endpoints: endpoints}
	opts.observeStatus = func(ctx context.Context, name string, endpoint inventory.HostPort) (*agentapi.NodeStatus, error) {
		node, ok := bound[name]
		if !ok {
			return nil, fmt.Errorf("node %q was not in the migration preflight", name)
		}
		status, err := contextEndpointStatus(withManagementDial(ctx, name, &client), endpoint)
		if err != nil {
			return nil, err
		}
		return status, node.verifyCommittedStatus(status, authority)
	}
	cfg, _, err := prepareContextWithCredentials(ctx, opts, clusterName, client, func(err error) error {
		return managementConfirmationError("the saved context can reach the nodes over mTLS", "the replacement authority", err)
	})
	if err != nil {
		return nil, "", err
	}
	cfg.CurrentContext = saved.CurrentContext
	return &cfg, path, nil
}

func enableMTLSResumeHint(done []enableMTLSNode, mode managementidentity.Authentication, invocation string) string {
	names := make([]string, 0, len(done))
	for _, node := range done {
		names = append(names, node.name)
	}
	moved := "no other node was changed"
	if len(names) > 0 {
		moved = "already on mTLS: " + strings.Join(names, ", ")
	}
	config := ""
	if mode != managementidentity.MutualTLS {
		config = " The ClusterConfig keeps trusted-network until every node is confirmed."
	}
	return fmt.Sprintf("\n%s.%s Rerun %s to continue", moved, config, invocation)
}

// enableMTLSInvocation is the command that resumes this run with the same
// secrets file. An empty secretsPath uses the file selected by the config.
func enableMTLSInvocation(configPath, secretsPath string) string {
	command := "katlctl management enable-mtls --config " + shellQuote(configPath)
	if secretsPath != "" && secretsPath != filepath.Join(filepath.Dir(configPath), defaultManagementSecrets) {
		command += " --output " + shellQuote(secretsPath)
	}
	return command
}
