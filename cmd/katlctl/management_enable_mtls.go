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
	"github.com/katl-dev/katl/internal/installer/controlplaneendpoint"
	"github.com/katl-dev/katl/internal/installer/operation"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/katlc/transport"
	"github.com/katl-dev/katl/internal/katlctl/workstation"
	"github.com/katl-dev/katl/internal/managementidentity"
	"github.com/katl-dev/katl/internal/nodeidentity"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	grpcstatus "google.golang.org/grpc/status"
)

const enableMTLSActor = "katlctl management enable-mtls"
const upgradeManagementTrialAgent = "upgrade the node with `katlctl node upgrade`, then rerun"

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
	endpoint     string
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

	nodes, err := enableMTLSNodes(ctx, configPath, source)
	if err != nil {
		return err
	}
	fmt.Fprintf(stderr, "Preflight: checking %d node(s); nothing changes until every check passes\n", len(nodes))
	// A config that says mtls fixes the secrets path, so there --output
	// cannot name another file.
	otherSecrets := "pass the secrets file it was moved to with --output"
	if mode == managementidentity.MutualTLS {
		otherSecrets = "put the secrets file it was moved to at " + secretsPath + ", where the ClusterConfig expects it"
	}
	var problems []string
	for i := range nodes {
		if err := observeEnableMTLSNode(ctx, &nodes[i], client, authority, otherSecrets, opts.timeout); err != nil {
			problems = append(problems, err.Error())
		}
	}
	pending := 0
	for _, node := range nodes {
		if node.phase != nodeOnMTLS {
			pending++
		}
	}
	if len(problems) == 0 {
		for _, node := range nodes {
			if err := enableMTLSReady(ctx, node, nodeClient(node, client), opts.timeout); err != nil {
				problems = append(problems, err.Error())
			}
			if node.phase == nodeOnMTLS {
				if err := verifyEnableMTLSPlaintextRefused(ctx, node, opts.timeout); err != nil {
					problems = append(problems, fmt.Sprintf("%s: %v", node.name, err))
				}
			}
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("preflight failed; no node was changed:\n  %s", strings.Join(problems, "\n  "))
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
	refreshed, err := refreshEnableMTLSContext(ctx, configPath, source.Metadata.Name, *client, opts.timeout, stderr)
	if err != nil {
		return fmt.Errorf("every node now uses mTLS management and %s references %s, but the saved context could not be refreshed: %w; run %s", configPath, secretsPath, err, contextSaveInvocation(configPath))
	}
	alreadyMTLS := pending == 0 && mode == managementidentity.MutualTLS
	if alreadyMTLS {
		fmt.Fprintf(stdout, "Cluster %s already uses mTLS management on %d node(s); authority %s\n", source.Metadata.Name, len(nodes), authority)
	} else {
		fmt.Fprintf(stdout, "Cluster %s now uses mTLS management on %d node(s); authority %s\n", source.Metadata.Name, len(nodes), authority)
		fmt.Fprintf(stdout, "%s now references %s; keep that file backed up.\n", configPath, secretsPath)
	}
	if refreshed {
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

func enableMTLSNodes(ctx context.Context, configPath string, source configbundle.SourceConfig) ([]enableMTLSNode, error) {
	inv, err := readManagementInventory(configPath)
	if err != nil {
		return nil, err
	}
	topology, err := workstation.ResolveTopology(workstation.ResolveRequest{ExplicitInventory: &inv})
	if err != nil {
		return nil, err
	}
	configuredEndpoint := source.Spec.ControlPlaneEndpoint
	managedVIP := configuredEndpoint != nil && controlplaneendpoint.Managed(*configuredEndpoint)
	nodes := make([]enableMTLSNode, 0, len(topology.Nodes))
	for _, node := range topology.Nodes {
		if node.ManagementEndpoint == "" {
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
			externalAPI:  topology.ExternalControlPlane, managedVIP: managedVIP,
		})
	}
	return nodes, nil
}

// observeEnableMTLSNode derives a node's phase from what it answers: the new
// authority means it is on trial or confirmed, plaintext means it is still on
// trusted-network. A node in a live trial does not answer plaintext.
// otherSecrets tells the operator how to supply the secrets a node was moved
// to when they are not the ones in use.
func observeEnableMTLSNode(ctx context.Context, node *enableMTLSNode, client *managementidentity.ClientCredentials, authority, otherSecrets string, timeout time.Duration) error {
	probe := min(timeout, 15*time.Second)
	if client != nil {
		probeCtx, observation := transport.ObserveManagementConnection(ctx)
		var remote peer.Peer
		status, err := enableMTLSStatus(probeCtx, *node, *client, probe, grpc.Peer(&remote))
		observedAt := time.Now()
		if err == nil {
			if err := node.bind(status, observedAt); err != nil {
				return err
			}
			management := status.GetManagement()
			transition := management.GetTransition()
			switch {
			case management.GetAuthentication() == string(managementidentity.MutualTLS) && management.GetAuthority() == authority:
				node.phase = nodeOnMTLS
			case transition.GetState() == "trial" && transition.GetTargetAuthority() == authority:
				node.phase = nodeOnTrial
				node.trialDeadline = managementTrialDeadline(transition, node.clockOffset)
				if node.trialDeadline.IsZero() {
					return fmt.Errorf("%s did not report a valid mTLS trial deadline; inspect it with 'katlctl node status' before rerunning", node.name)
				}
			default:
				return fmt.Errorf("%s answered with the new authority but reports %q management; inspect it with 'katlctl node status' before rerunning", node.name, management.GetAuthentication())
			}
			return nil
		}
		switch protocol := observation.Protocol(); {
		case remote.Addr != nil || protocol == transport.TLS:
			return fmt.Errorf("%s could not be verified over mTLS at %s: %w; check its address and node name. If it uses a different authority, %s, then rerun", node.name, node.endpoint, err, otherSecrets)
		case protocol != transport.PlaintextHTTP2:
			return fmt.Errorf("%s: could not classify the management transport at %s. Check that katlc-agent is listening and any proxy passes TLS and the HTTP/2 server preface through, or connect directly to the agent, then rerun", node.name, node.endpoint)
		}
	}

	status, err := enableMTLSStatus(ctx, *node, trustedNetworkClient, probe)
	observedAt := time.Now()
	if err != nil {
		if transport.TLSAtPlaintext(err) {
			return fmt.Errorf("%s no longer answers plaintext management; if an earlier enable-mtls trial is active, wait for its deadline or resume with the same secrets; if it was already moved, %s, then rerun: %w", node.name, otherSecrets, err)
		}
		return fmt.Errorf("%s is unreachable at %s: %v; fix it or remove it from the config, then rerun", node.name, node.endpoint, err)
	}
	if err := node.bind(status, observedAt); err != nil {
		return err
	}
	management := status.GetManagement()
	if management.GetAuthentication() != string(managementidentity.TrustedNetwork) {
		return fmt.Errorf("%s answered plaintext management but reports %q mode; inspect it with 'katlctl node status' before rerunning", node.name, management.GetAuthentication())
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
		return fmt.Errorf("%s is not healthy: %w; resolve it (see 'katlctl node status'), then rerun", node.name, err)
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

// migrateEnableMTLSNode moves one node: begin its trial over plaintext, prove
// the new channel from this workstation, confirm, then check that plaintext is
// refused and the node is healthy. Every step before confirm leaves the node
// to return to trusted-network by itself.
func migrateEnableMTLSNode(ctx context.Context, node *enableMTLSNode, bundle managementidentity.Bundle, client managementidentity.ClientCredentials, authority string, opts enableMTLSOptions, stderr io.Writer) error {
	timeout := opts.timeout
	deadline := node.trialDeadline
	if node.phase == nodeOnTrustedNetwork {
		if node.lastRevert != "" {
			fmt.Fprintf(stderr, "%s: an earlier trial reverted (%s); starting a new one\n", node.name, node.lastRevert)
		}
		credentials, ok := bundle.Nodes[node.name]
		if !ok {
			return fmt.Errorf("management secrets lack node %q", node.name)
		}
		var trial *agentapi.ManagementTransition
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
		// The node starts its window when Begin reaches it, no later than
		// now, so now plus the window bounds the deadline from above. An
		// interrupted Begin may still have started the trial.
		if err != nil && (ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) || grpcstatus.Code(err) == codes.DeadlineExceeded) {
			return enableMTLSInterrupted(time.Now().Add(opts.trialWindow))
		}
		if err != nil {
			return enableMTLSBeginError(err)
		}
		deadline = time.Now().Add(opts.trialWindow)
		if trial.GetState() != "committed" {
			fmt.Fprintf(stderr, "%s: serving the new authority on trial; it returns to trusted-network by %s unless confirmed\n", node.name, deadline.Local().Format(time.TimeOnly))
		}
	} else {
		if !time.Now().Before(deadline) {
			return fmt.Errorf("its earlier trial passed its deadline at %s but it has not returned to trusted-network yet; wait for it to revert, then rerun this command", deadline.Local().Format(time.TimeOnly))
		}
		fmt.Fprintf(stderr, "%s: resuming its trial of the new authority\n", node.name)
	}
	reverts := enableMTLSReverts(deadline)

	trialStatus, err := waitEnableMTLSTrial(ctx, *node, client, authority, min(time.Until(deadline), timeout))
	if err != nil {
		if ctx.Err() != nil {
			return enableMTLSInterrupted(deadline)
		}
		return fmt.Errorf("did not answer over mTLS with the new authority: %w; %s. Check that management.address reaches the node from this workstation, then rerun", err, reverts)
	}
	if _, err := managementClockOffset(trialStatus.GetManagement(), time.Now()); err != nil {
		return fmt.Errorf("%w. %s", err, reverts)
	}

	confirmErr := enableMTLSCall(ctx, *node, client, timeout, func(ctx context.Context, agent agentapi.KatlcAgentClient) error {
		_, err := agent.ConfirmManagementTransition(ctx, &agentapi.ConfirmManagementTransitionRequest{
			ApiVersion: operation.APIVersion, Kind: "ConfirmManagementTransitionRequest", Actor: enableMTLSActor,
			ExpectedEnrollmentId: node.status.GetEnrollmentId(), ExpectedInventoryNodeName: node.name, ExpectedMachineId: node.status.GetMachineId(),
			TargetAuthority: authority,
		})
		return err
	})
	// A confirm whose reply was lost may still have committed, so the node's
	// own state decides the outcome.
	status, statusErr := enableMTLSStatus(ctx, *node, client, timeout)
	if statusErr != nil || !committedTo(status, authority) {
		switch {
		case ctx.Err() != nil:
			return fmt.Errorf("interrupted while checking confirmation; the node may already have committed the new authority. Rerun this command with the same secrets to check its state and resume")
		case confirmErr != nil && strings.Contains(confirmErr.Error(), "reverted"):
			return fmt.Errorf("its trial ended before it was confirmed (%v) and it is back on trusted-network; rerun this command", grpcstatus.Convert(confirmErr).Message())
		case confirmErr != nil:
			return fmt.Errorf("could not verify confirmation of the new authority: %w; the node may already have committed it. Rerun this command with the same secrets to check its state and resume", confirmErr)
		case statusErr != nil:
			return fmt.Errorf("confirmed, but the node did not answer over mTLS afterwards: %w; rerun this command to check it", statusErr)
		default:
			return fmt.Errorf("confirmed, but the node reports %q management with authority %q; inspect it with 'katlctl node status'", status.GetManagement().GetAuthentication(), status.GetManagement().GetAuthority())
		}
	}
	if err := node.identity.Verify(status); err != nil {
		return err
	}

	if err := verifyEnableMTLSPlaintextRefused(ctx, *node, timeout); err != nil {
		return err
	}

	if err := waitEnableMTLSHealthy(ctx, *node, client, timeout); err != nil {
		return fmt.Errorf("uses mTLS management now but is not healthy after %s: %w; check it with 'katlctl node status', then rerun to continue with the remaining nodes", timeout, err)
	}
	return nil
}

// A timeout or unrelated connection failure does not establish that the
// listener refused plaintext. Require the endpoint that already answered as
// this installation over authenticated mTLS to close without a response.
func verifyEnableMTLSPlaintextRefused(ctx context.Context, node enableMTLSNode, timeout time.Duration) error {
	ctx, observation := transport.ObserveManagementConnection(ctx)
	var remote peer.Peer
	_, err := enableMTLSStatus(ctx, node, trustedNetworkClient, min(timeout, 5*time.Second), grpc.Peer(&remote))
	switch {
	case err == nil || remote.Addr != nil:
		return fmt.Errorf("uses mTLS management but still answers plaintext management; inspect it with 'katlctl node status' and rerun")
	case !observation.PlaintextClosed():
		return fmt.Errorf("could not verify that it refuses plaintext management: %w; check the endpoint and rerun", err)
	default:
		return nil
	}
}

func enableMTLSReverts(deadline time.Time) string {
	return fmt.Sprintf("it returns to trusted-network by itself by %s, and 'katlc agent management-trial revert' on its console returns it at once", deadline.Local().Format(time.TimeOnly))
}

// enableMTLSInterrupted reports a run stopped while the node may be on trial.
func enableMTLSInterrupted(deadline time.Time) error {
	return fmt.Errorf("interrupted before the new authority was confirmed; %s. Rerun this command before then to finish it", enableMTLSReverts(deadline))
}

func committedTo(status *agentapi.NodeStatus, authority string) bool {
	management := status.GetManagement()
	return management.GetAuthentication() == string(managementidentity.MutualTLS) && management.GetAuthority() == authority
}

func enableMTLSBeginError(err error) error {
	message := grpcstatus.Convert(err).Message()
	switch {
	case grpcstatus.Code(err) == codes.Unimplemented:
		return fmt.Errorf("its KatlOS release cannot move management trust; upgrade it first with 'katlctl node upgrade', then rerun")
	case strings.Contains(message, "is not valid at"):
		// The node checks the new certificates against its own clock before
		// staging them; their validity starts five minutes before this
		// workstation's time.
		return fmt.Errorf("refused the new certificates because its clock is more than 5 minutes behind this workstation (%s); correct the node's time, for example by fixing its NTP source, then rerun. The node was not changed", message)
	default:
		return fmt.Errorf("start the mTLS trial: %w; resolve it, then rerun", err)
	}
}

// waitEnableMTLSTrial polls until the node answers over mTLS with the new
// authority as the same installation. The node restarts its agent to start the
// trial, so refusals are expected at first.
func waitEnableMTLSTrial(ctx context.Context, node enableMTLSNode, client managementidentity.ClientCredentials, authority string, wait time.Duration) (*agentapi.NodeStatus, error) {
	deadline, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	for {
		status, err := enableMTLSStatus(deadline, node, client, min(wait, 5*time.Second))
		if err == nil {
			if verifyErr := node.identity.Verify(status); verifyErr != nil {
				return nil, verifyErr
			}
			transition := status.GetManagement().GetTransition()
			if committedTo(status, authority) || (transition.GetState() == "trial" && transition.GetTargetAuthority() == authority) {
				return status, nil
			}
			err = fmt.Errorf("node reports trial state %q", transition.GetState())
		}
		select {
		case <-deadline.Done():
			return nil, fmt.Errorf("no answer within %s; last error: %v", wait.Round(time.Second), err)
		case <-time.After(enableMTLSPoll):
		}
	}
}

func waitEnableMTLSHealthy(ctx context.Context, node enableMTLSNode, client managementidentity.ClientCredentials, timeout time.Duration) error {
	deadline, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var lastErr error
	for {
		status, err := enableMTLSStatus(deadline, node, client, min(timeout, 5*time.Second))
		if err == nil {
			err = enableMTLSHealth(deadline, node, client, status, min(timeout, 5*time.Second))
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

func enableMTLSStatus(ctx context.Context, node enableMTLSNode, credentials managementidentity.ClientCredentials, timeout time.Duration, opts ...grpc.CallOption) (*agentapi.NodeStatus, error) {
	var status *agentapi.NodeStatus
	err := enableMTLSCall(ctx, node, credentials, timeout, func(ctx context.Context, agent agentapi.KatlcAgentClient) (err error) {
		status, err = agent.GetNodeStatus(ctx, &agentapi.GetNodeStatusRequest{}, opts...)
		return err
	})
	return status, err
}

// enableMTLSCall makes one call with exactly the given credentials. A refused
// connection fails at once because callers poll for themselves.
func enableMTLSCall(ctx context.Context, node enableMTLSNode, credentials managementidentity.ClientCredentials, timeout time.Duration, call func(context.Context, agentapi.KatlcAgentClient) error) error {
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := dialKatlcAgent(transport.WithoutManagementWait(withManagementDial(requestCtx, node.name, &credentials)), node.endpoint)
	if err != nil {
		return err
	}
	defer conn.Close()
	return call(requestCtx, conn.Client)
}

// refreshEnableMTLSContext updates a saved context for the cluster so it uses
// the new credentials, keeping the current context selection. A workstation
// without one is left alone.
func refreshEnableMTLSContext(ctx context.Context, configPath, clusterName string, client managementidentity.ClientCredentials, timeout time.Duration, stderr io.Writer) (bool, error) {
	path, err := workstation.ConfigPath()
	if err != nil {
		return false, err
	}
	saved, err := workstation.Load(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	contextName := ""
	for _, saved := range saved.Contexts {
		if strings.TrimSpace(saved.Cluster) == clusterName {
			contextName = saved.Name
			break
		}
	}
	if contextName == "" {
		return false, nil
	}
	for _, cluster := range saved.Clusters {
		if cluster.Name == clusterName && cluster.Management != nil && *cluster.Management == client {
			return false, nil
		}
	}
	cfg, _, err := prepareContext(ctx, contextSaveOptions{configInput: configPath, contextPath: path, contextName: contextName, timeout: timeout, output: "text"}, stderr)
	if err != nil {
		return false, err
	}
	cfg.CurrentContext = saved.CurrentContext
	return true, workstation.Save(path, cfg)
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
