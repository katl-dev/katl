package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/installer/configbundle"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/katlc/transport"
	"github.com/katl-dev/katl/internal/managementidentity"
	"github.com/katl-dev/katl/internal/nodeidentity"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

type managementRotationOptions struct {
	configPath string
	outputPath string
	sshPort    int
	sshKey     string
	knownHosts string
	timeout    time.Duration
}

type managementRotationNode struct {
	name      string
	host      string
	endpoint  string
	machineID string
	request   nodeidentity.ManagementRotation
}

var runManagementRotationSSH = sshManagementRotation
var runManagementSSHCommand = (*exec.Cmd).Run

func newManagementIdentityRotateCommand(stdout, stderr io.Writer) *cobra.Command {
	opts := managementRotationOptions{sshPort: 22, timeout: 2 * time.Minute}
	command := &cobra.Command{
		Use:   "rotate",
		Short: "Replace an installed cluster's management mTLS authority through root SSH",
		Long: `Generate or resume a replacement authority, preflight every node through
host-key-verified root SSH, switch nodes one at a time, and verify that each
accepts the replacement and rejects the old authority. The ClusterConfig
reference changes only after every node succeeds. Reuse the same --output
path to resume a partial rotation.`,
		Example: "katlctl management identity rotate --config ./cluster.yaml --output ./management-secrets-next.sops.yaml",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runManagementIdentityRotate(cmd.Context(), opts, stdout, stderr)
		},
	}
	command.Flags().StringVar(&opts.configPath, "config", "", "ClusterConfig YAML with the installed mTLS authority")
	command.Flags().StringVar(&opts.outputPath, "output", "", "new secrets file; .sops.yaml encrypts with SOPS before any node changes")
	command.Flags().IntVar(&opts.sshPort, "ssh-port", opts.sshPort, "root SSH port on each node")
	command.Flags().StringVar(&opts.sshKey, "ssh-key", "", "SSH private key for root access")
	command.Flags().StringVar(&opts.knownHosts, "ssh-known-hosts", "", "trusted SSH host keys file")
	command.Flags().DurationVar(&opts.timeout, "timeout", opts.timeout, "time allowed for each SSH or management connection")
	_ = command.MarkFlagRequired("config")
	_ = command.MarkFlagRequired("output")
	return command
}

func runManagementIdentityRotate(ctx context.Context, opts managementRotationOptions, stdout, stderr io.Writer) error {
	if opts.sshPort < 1 || opts.sshPort > 65535 {
		return fmt.Errorf("--ssh-port must be between 1 and 65535")
	}
	if opts.timeout <= 0 {
		return fmt.Errorf("--timeout must be positive")
	}
	configPath, err := filepath.Abs(opts.configPath)
	if err != nil {
		return err
	}
	outputPath, err := filepath.Abs(opts.outputPath)
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
	authentication, err := source.ManagementAuthentication()
	if err != nil {
		return err
	}
	if authentication != managementidentity.MutualTLS {
		return fmt.Errorf("management identity rotation requires an mTLS cluster; run `katlctl management enable-mtls --config %s` to move this trusted-network cluster to mTLS first", opts.configPath)
	}
	oldPath := strings.TrimSpace(source.Spec.ManagementIdentity)
	if oldPath == "" {
		oldPath, err = managementIdentityPath(source.Metadata.Name)
		if err != nil {
			return err
		}
	} else if !filepath.IsAbs(oldPath) {
		oldPath = filepath.Join(filepath.Dir(configPath), oldPath)
	}
	old, err := managementIdentityForSource(configPath, source)
	if err != nil {
		return err
	}
	oldInfo, err := managementidentity.Validate(old, time.Now().UTC())
	if err != nil {
		return err
	}
	if filepath.Clean(oldPath) == outputPath {
		client, err := managementidentity.Client(old, time.Now().UTC())
		if err != nil {
			return err
		}
		nodes, err := managementRotationNodes(ctx, configPath, source, oldInfo.Fingerprint, old)
		if err != nil {
			return err
		}
		for _, node := range nodes {
			if err := checkManagementCredential(ctx, node, client, opts.timeout); err != nil {
				return fmt.Errorf("%s at %s: %w; keep %s, check the endpoint and rerun the same command", node.name, node.endpoint,
					managementConfirmationError("the node accepts the configured authority", "the previous authority", err), outputPath)
			}
		}
		fmt.Fprintf(stdout, "Management identity for %s already uses %s on %d node(s)\n", source.Metadata.Name, oldInfo.Fingerprint, len(nodes))
		return nil
	}
	next, err := replacementManagementIdentity(configPath, outputPath, source)
	if err != nil {
		return err
	}
	nextInfo, err := managementidentity.Validate(next, time.Now().UTC())
	if err != nil {
		return err
	}
	if nextInfo.Fingerprint == oldInfo.Fingerprint {
		return fmt.Errorf("replacement secrets use the installed authority; choose a new --output path")
	}
	nodes, err := managementRotationNodes(ctx, configPath, source, oldInfo.Fingerprint, next)
	if err != nil {
		return err
	}
	oldProbe, err := managementidentity.IssueRotationProbe(old.CertificateAuthority, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("issue a verification credential from the old management authority: %w", err)
	}
	for _, node := range nodes {
		fmt.Fprintf(stderr, "Preflight root SSH and management identity on %s\n", node.name)
		if _, err := runManagementRotationSSH(ctx, node, opts, true); err != nil {
			return fmt.Errorf("preflight %s: %w; no node credentials were changed; keep %s and rerun the same command", node.name,
				managementConfirmationError("the node is ready to rotate its management identity", "the old authority", err), outputPath)
		}
	}
	newClient, err := managementidentity.Client(next, time.Now().UTC())
	if err != nil {
		return err
	}
	for _, node := range nodes {
		fmt.Fprintf(stderr, "Rotating management identity on %s\n", node.name)
		if _, err := runManagementRotationSSH(ctx, node, opts, false); err != nil {
			return fmt.Errorf("rotate %s: %w; keep %s and rerun the same command", node.name,
				managementConfirmationError("the replacement was installed", "the old authority", err), outputPath)
		}
		if err := verifyRotatedManagement(ctx, node, newClient, oldProbe, opts.timeout); err != nil {
			return fmt.Errorf("verify %s: %w; keep %s and rerun with the same --output", node.name, err, outputPath)
		}
		fmt.Fprintf(stderr, "Verified replacement authority and old-authority rejection on %s\n", node.name)
	}
	if err := referenceManagementSecrets(configPath, outputPath, sourceData); err != nil {
		return fmt.Errorf("all nodes rotated, but ClusterConfig still references the old authority: %w; set spec.managementIdentity to %s", err, outputPath)
	}
	fmt.Fprintf(stdout, "Rotated management identity for %s on %d node(s); new authority %s\n", source.Metadata.Name, len(nodes), nextInfo.Fingerprint)
	fmt.Fprintf(stdout, "ClusterConfig now references %s. Refresh any saved shortcut with %s.\n", outputPath, contextSaveInvocation(configPath))
	return nil
}

// unreadableSecretsError marks an existing replacement secrets file that could
// not be read, as opposed to one that was read and rejected.
type unreadableSecretsError struct{ err error }

func (e *unreadableSecretsError) Error() string { return e.err.Error() }
func (e *unreadableSecretsError) Unwrap() error { return e.err }

func replacementManagementIdentity(configPath, outputPath string, source configbundle.SourceConfig) (managementidentity.Bundle, error) {
	if _, err := os.Stat(outputPath); err == nil {
		bundle, err := readManagementIdentity(outputPath)
		if err != nil {
			return managementidentity.Bundle{}, &unreadableSecretsError{err: err}
		}
		if bundle.ClusterName != source.Metadata.Name {
			return managementidentity.Bundle{}, fmt.Errorf("replacement secrets belong to cluster %q, not %q", bundle.ClusterName, source.Metadata.Name)
		}
		for _, node := range source.Spec.Nodes {
			if _, ok := bundle.Nodes[node.Name]; !ok {
				return managementidentity.Bundle{}, fmt.Errorf("replacement secrets lack node %q", node.Name)
			}
		}
		return bundle, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return managementidentity.Bundle{}, err
	}
	bundle, err := managementidentity.Generate(managementidentity.GenerateOptions{ClusterName: source.Metadata.Name})
	if err != nil {
		return managementidentity.Bundle{}, err
	}
	for _, node := range source.Spec.Nodes {
		if _, _, err := managementidentity.EnsureNode(&bundle, node.Name, time.Now().UTC(), nil); err != nil {
			return managementidentity.Bundle{}, err
		}
	}
	if strings.HasSuffix(outputPath, ".sops.yaml") {
		if err := writeEncryptedManagementIdentity(configPath, outputPath, bundle); err != nil {
			return managementidentity.Bundle{}, err
		}
		loaded, err := readManagementIdentity(outputPath)
		if err != nil {
			return managementidentity.Bundle{}, fmt.Errorf("verify encrypted replacement secrets: %w", err)
		}
		if loaded.CertificateAuthority != bundle.CertificateAuthority || loaded.Operator != bundle.Operator || len(loaded.Nodes) != len(bundle.Nodes) {
			return managementidentity.Bundle{}, fmt.Errorf("SOPS round-trip changed the replacement credentials")
		}
		for name, node := range bundle.Nodes {
			if loaded.Nodes[name] != node {
				return managementidentity.Bundle{}, fmt.Errorf("SOPS round-trip changed replacement credentials for node %q", name)
			}
		}
	} else if err := managementidentity.Write(outputPath, bundle); err != nil {
		return managementidentity.Bundle{}, err
	}
	return bundle, nil
}

func writeEncryptedManagementIdentity(configPath, outputPath string, bundle managementidentity.Bundle) error {
	plain, err := managementidentity.Marshal(bundle)
	if err != nil {
		return err
	}
	command := exec.Command("sops", "encrypt", "--filename-override", outputPath, "--input-type", "yaml", "--output-type", "yaml")
	command.Dir = filepath.Dir(configPath)
	command.Stdin = bytes.NewReader(plain)
	ciphertext, err := command.Output()
	if err != nil {
		return fmt.Errorf("encrypt replacement management secrets with SOPS: %w; ensure sops and its age recipient are configured", err)
	}
	var encrypted struct {
		CertificateAuthority struct {
			PrivateKey string `yaml:"privateKey"`
		} `yaml:"certificateAuthority"`
		Operator struct {
			PrivateKey string `yaml:"privateKey"`
		} `yaml:"operator"`
		Nodes map[string]struct {
			PrivateKey string `yaml:"privateKey"`
		} `yaml:"nodes"`
		SOPS *yaml.Node `yaml:"sops"`
	}
	if err := yaml.Unmarshal(ciphertext, &encrypted); err != nil || encrypted.SOPS == nil || !strings.HasPrefix(encrypted.CertificateAuthority.PrivateKey, "ENC[") || !strings.HasPrefix(encrypted.Operator.PrivateKey, "ENC[") {
		return fmt.Errorf("SOPS output must encrypt every management private key")
	}
	for name, node := range encrypted.Nodes {
		if !strings.HasPrefix(node.PrivateKey, "ENC[") {
			return fmt.Errorf("SOPS output leaves management private key for node %q unencrypted", name)
		}
	}
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(outputPath), ".management-identity-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if _, err := temp.Write(ciphertext); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Link(temp.Name(), outputPath); err != nil {
		return fmt.Errorf("publish replacement management secrets without overwrite: %w", err)
	}
	dir, err := os.Open(filepath.Dir(outputPath))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func managementRotationTargets(ctx context.Context, configPath string, source configbundle.SourceConfig) ([]managementRotationNode, error) {
	inv, err := readManagementInventory(configPath)
	if err != nil {
		return nil, err
	}
	nodes := make([]managementRotationNode, 0, len(inv.Nodes))
	for _, node := range inv.Nodes {
		endpoint, err := normalizeManagementAddress(node.Address)
		if err != nil || endpoint == "" {
			return nil, fmt.Errorf("node %q needs a valid management.address before credential rotation: %v", node.Name, err)
		}
		host, _, err := net.SplitHostPort(endpoint)
		if err != nil {
			return nil, err
		}
		machineID := ""
		if saved, ok := enrolledTarget(ctx, "", "", source.Metadata.Name, node.Name); ok {
			machineID = saved.machineID
		}
		nodes = append(nodes, managementRotationNode{name: node.Name, host: host, endpoint: endpoint, machineID: machineID})
	}
	return nodes, nil
}

func managementRotationNodes(ctx context.Context, configPath string, source configbundle.SourceConfig, oldFingerprint string, next managementidentity.Bundle) ([]managementRotationNode, error) {
	nodes, err := managementRotationTargets(ctx, configPath, source)
	if err != nil {
		return nil, err
	}
	for i := range nodes {
		node := &nodes[i]
		credentials, ok := next.Nodes[node.name]
		if !ok {
			return nil, fmt.Errorf("replacement management identity lacks node %q", node.name)
		}
		node.request = nodeidentity.ManagementRotation{NodeName: node.name, ExpectedMachineID: node.machineID, ExpectedAuthority: oldFingerprint,
			Replacement: managementidentity.NodeCredentials{CACertificate: next.CertificateAuthority.Certificate, ServerCertificate: credentials.Certificate, ServerPrivateKey: credentials.PrivateKey}}
	}
	return nodes, nil
}

func sshManagementRotation(ctx context.Context, node managementRotationNode, opts managementRotationOptions, dryRun bool) (nodeidentity.ManagementRotationStatus, error) {
	request, err := json.Marshal(node.request)
	if err != nil {
		return nodeidentity.ManagementRotationStatus{}, err
	}
	args := []string{"rotate-management"}
	if dryRun {
		args = append(args, "--dry-run")
	}
	output, err := runKatlcAgentSSH(ctx, node, opts, request, args...)
	if err != nil {
		return nodeidentity.ManagementRotationStatus{}, err
	}
	var status nodeidentity.ManagementRotationStatus
	if err := json.Unmarshal(output, &status); err != nil {
		return nodeidentity.ManagementRotationStatus{}, fmt.Errorf("read node rotation result: %w", err)
	}
	if status.NodeName != node.name || (node.machineID != "" && status.MachineID != node.machineID) {
		return nodeidentity.ManagementRotationStatus{}, fmt.Errorf("SSH endpoint does not match node %s and its saved machine identity", node.name)
	}
	return status, nil
}

// runKatlcAgentSSH runs `katlc agent <args>` as root over SSH with the host
// key pinned in known_hosts; an unknown key is refused, never accepted.
func runKatlcAgentSSH(ctx context.Context, node managementRotationNode, opts managementRotationOptions, stdin []byte, agentArgs ...string) ([]byte, error) {
	args := []string{"-T", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "ConnectTimeout=10", "-p", strconv.Itoa(opts.sshPort)}
	if opts.sshKey != "" {
		args = append(args, "-i", opts.sshKey, "-o", "IdentitiesOnly=yes")
	}
	if opts.knownHosts != "" {
		// Only the supplied file may vouch for a host key; the system-wide
		// list must not widen what the operator pinned.
		args = append(args, "-o", "UserKnownHostsFile="+opts.knownHosts, "-o", "GlobalKnownHostsFile=/dev/null")
	}
	args = append(args, "root@"+node.host, "/usr/bin/katlc", "agent")
	args = append(args, agentArgs...)
	requestCtx, cancel := context.WithTimeout(ctx, opts.timeout)
	defer cancel()
	command := exec.CommandContext(requestCtx, "ssh", args...)
	command.Stdin = bytes.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := runManagementSSHCommand(command)
	if ctxErr := transport.ManagementContextError(requestCtx); ctxErr != nil {
		return nil, fmt.Errorf("root SSH to %s: %w", node.name, ctxErr)
	}
	if err != nil {
		return nil, fmt.Errorf("root SSH to %s: %w: %s; verify its host key and root SSH access", node.name, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func verifyRotatedManagement(ctx context.Context, node managementRotationNode, next managementidentity.ClientCredentials, oldProbe managementidentity.CertificateKey, timeout time.Duration) error {
	if err := waitManagementCredential(ctx, node, next, timeout); err != nil {
		return managementConfirmationError("the node accepts the replacement authority", "the old authority", err)
	}
	if err := verifyOldManagementAuthorityRejected(ctx, node, next.CACertificate, oldProbe, min(timeout, 5*time.Second)); err != nil {
		return err
	}
	if err := checkManagementCredential(ctx, node, next, min(timeout, 5*time.Second)); err != nil {
		return managementConfirmationError("the node still accepts the replacement authority", "the old authority", err)
	}
	return nil
}

func verifyOldManagementAuthorityRejected(ctx context.Context, node managementRotationNode, currentCA string, oldProbe managementidentity.CertificateKey, timeout time.Duration) error {
	certificate, err := tls.X509KeyPair([]byte(oldProbe.Certificate), []byte(oldProbe.PrivateKey))
	if err != nil {
		return fmt.Errorf("load the old authority's verification credential: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(currentCA)) {
		return fmt.Errorf("replacement management authority has no certificate")
	}
	config := &tls.Config{
		MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: node.name,
	}
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	creds, unknownAuthority := transport.ObserveUnknownClientAuthority(requestCtx, config, certificate)
	conn, err := dialKatlcAgentWithTransport(transport.WithoutManagementWait(requestCtx), node.name, node.endpoint, creds)
	if err != nil {
		return managementConfirmationError("the node stopped trusting the old authority", "the old authority", err)
	}
	defer conn.Close()
	status, err := conn.Client.GetNodeStatus(requestCtx, &agentapi.GetNodeStatusRequest{})
	switch transport.ClassifyManagementEvidence(err, unknownAuthority) {
	case transport.ManagementRefusedExplicitly:
		return nil
	case transport.ManagementConfirmed:
		if err := checkManagementStatusIdentity(node, status); err != nil {
			return err
		}
		return fmt.Errorf("old authority still has management access; check the agent's client CA trust")
	default:
		return managementConfirmationError("the node stopped trusting the old authority", "the old authority", err)
	}
}

func waitManagementCredential(ctx context.Context, node managementRotationNode, credentials managementidentity.ClientCredentials, timeout time.Duration) error {
	deadline, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		err := checkManagementCredential(deadline, node, credentials, min(timeout, 5*time.Second))
		if err == nil {
			return nil
		}
		select {
		case <-deadline.Done():
			return fmt.Errorf("%w; last connection error: %v", deadline.Err(), err)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func checkManagementCredential(ctx context.Context, node managementRotationNode, credentials managementidentity.ClientCredentials, timeout time.Duration) error {
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	requestCtx = transport.WithoutManagementAdvice(withManagementDial(requestCtx, node.name, &credentials))
	conn, err := dialKatlcAgent(transport.WithoutManagementWait(requestCtx), node.endpoint)
	if err != nil {
		return err
	}
	defer conn.Close()
	status, err := conn.Client.GetNodeStatus(requestCtx, &agentapi.GetNodeStatusRequest{})
	if transport.ClassifyManagementEvidence(err, nil) != transport.ManagementConfirmed {
		return err
	}
	return checkManagementStatusIdentity(node, status)
}

func checkManagementStatusIdentity(node managementRotationNode, status *agentapi.NodeStatus) error {
	if status.GetInventoryNodeName() != node.name || (node.machineID != "" && status.GetMachineId() != node.machineID) {
		return fmt.Errorf("management endpoint reported a different node or machine")
	}
	return nil
}

func managementConfirmationError(check, possibleTrust string, err error) error {
	if err == nil {
		return fmt.Errorf("could not confirm %s; the node may still trust %s", check, possibleTrust)
	}
	return fmt.Errorf("could not confirm %s; the node may still trust %s: %w", check, possibleTrust, err)
}
