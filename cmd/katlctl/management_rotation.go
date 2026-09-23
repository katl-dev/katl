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
	"google.golang.org/grpc"
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
	if source.ManagementAuthentication() != managementidentity.MutualTLS {
		return fmt.Errorf("management identity rotation requires an mTLS cluster")
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
				return fmt.Errorf("ClusterConfig already references %s, but node %s does not accept it: %w; restore the previous secrets reference and rerun rotation", outputPath, node.name, err)
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
	for _, node := range nodes {
		fmt.Fprintf(stderr, "Preflight root SSH and management identity on %s\n", node.name)
		if _, err := runManagementRotationSSH(ctx, node, opts, true); err != nil {
			return fmt.Errorf("preflight %s: %w; no node credentials were changed; keep %s to resume", node.name, err, outputPath)
		}
	}
	newClient, err := managementidentity.Client(next, time.Now().UTC())
	if err != nil {
		return err
	}
	oldClient, err := managementidentity.Client(old, time.Now().UTC())
	if err != nil {
		return err
	}
	for _, node := range nodes {
		fmt.Fprintf(stderr, "Rotating management identity on %s\n", node.name)
		if _, err := runManagementRotationSSH(ctx, node, opts, false); err != nil {
			return fmt.Errorf("rotate %s: %w; keep %s and rerun the same command after recovery", node.name, err, outputPath)
		}
		if err := verifyRotatedManagement(ctx, node, newClient, oldClient, opts.timeout); err != nil {
			return fmt.Errorf("verify %s: %w; the replacement file at %s is needed to resume", node.name, err, outputPath)
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

func replacementManagementIdentity(configPath, outputPath string, source configbundle.SourceConfig) (managementidentity.Bundle, error) {
	if _, err := os.Stat(outputPath); err == nil {
		bundle, err := readManagementIdentity(outputPath)
		if err != nil {
			return managementidentity.Bundle{}, err
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

func managementRotationNodes(ctx context.Context, configPath string, source configbundle.SourceConfig, oldFingerprint string, next managementidentity.Bundle) ([]managementRotationNode, error) {
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
		credentials, ok := next.Nodes[node.Name]
		if !ok {
			return nil, fmt.Errorf("replacement management identity lacks node %q", node.Name)
		}
		machineID := ""
		if saved, ok := enrolledTarget(ctx, "", "", source.Metadata.Name, node.Name); ok {
			machineID = saved.machineID
		}
		nodes = append(nodes, managementRotationNode{name: node.Name, host: host, endpoint: endpoint, machineID: machineID,
			request: nodeidentity.ManagementRotation{NodeName: node.Name, ExpectedMachineID: machineID, ExpectedAuthority: oldFingerprint,
				Replacement: managementidentity.NodeCredentials{CACertificate: next.CertificateAuthority.Certificate, ServerCertificate: credentials.Certificate, ServerPrivateKey: credentials.PrivateKey}},
		})
	}
	return nodes, nil
}

func sshManagementRotation(ctx context.Context, node managementRotationNode, opts managementRotationOptions, dryRun bool) (nodeidentity.ManagementRotationStatus, error) {
	request, err := json.Marshal(node.request)
	if err != nil {
		return nodeidentity.ManagementRotationStatus{}, err
	}
	args := []string{"-T", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "ConnectTimeout=10", "-p", strconv.Itoa(opts.sshPort)}
	if opts.sshKey != "" {
		args = append(args, "-i", opts.sshKey, "-o", "IdentitiesOnly=yes")
	}
	if opts.knownHosts != "" {
		args = append(args, "-o", "UserKnownHostsFile="+opts.knownHosts)
	}
	args = append(args, "root@"+node.host, "/usr/bin/katlc", "agent", "rotate-management")
	if dryRun {
		args = append(args, "--dry-run")
	}
	requestCtx, cancel := context.WithTimeout(ctx, opts.timeout)
	defer cancel()
	command := exec.CommandContext(requestCtx, "ssh", args...)
	command.Stdin = bytes.NewReader(request)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		return nodeidentity.ManagementRotationStatus{}, fmt.Errorf("root SSH to %s: %w: %s; verify its host key and root SSH access", node.name, err, strings.TrimSpace(stderr.String()))
	}
	var status nodeidentity.ManagementRotationStatus
	if err := json.Unmarshal(stdout.Bytes(), &status); err != nil {
		return nodeidentity.ManagementRotationStatus{}, fmt.Errorf("read node rotation result: %w", err)
	}
	if status.NodeName != node.name || (node.machineID != "" && status.MachineID != node.machineID) {
		return nodeidentity.ManagementRotationStatus{}, fmt.Errorf("SSH endpoint does not match node %s and its saved machine identity", node.name)
	}
	return status, nil
}

func verifyRotatedManagement(ctx context.Context, node managementRotationNode, next, old managementidentity.ClientCredentials, timeout time.Duration) error {
	if err := waitManagementCredential(ctx, node, next, timeout); err != nil {
		return fmt.Errorf("replacement authority cannot manage the node: %w", err)
	}
	// Trust the new server certificate while presenting the old client leaf.
	// This proves the node rejects the exposed client credential itself.
	if err := checkExposedManagementCredential(ctx, node, next.CACertificate, old, min(timeout, 5*time.Second)); err == nil {
		return fmt.Errorf("exposed authority still has management access")
	}
	if err := checkManagementCredential(ctx, node, next, min(timeout, 5*time.Second)); err != nil {
		return fmt.Errorf("replacement authority lost access during old-client rejection check: %w", err)
	}
	return nil
}

func checkExposedManagementCredential(ctx context.Context, node managementRotationNode, currentCA string, exposed managementidentity.ClientCredentials, timeout time.Duration) error {
	certificate, err := tls.X509KeyPair([]byte(exposed.ClientCertificate), []byte(exposed.ClientPrivateKey))
	if err != nil {
		return fmt.Errorf("load exposed client credential: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(currentCA)) {
		return fmt.Errorf("replacement management authority has no certificate")
	}
	config := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, Certificates: []tls.Certificate{certificate}, ServerName: node.name}
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := grpc.DialContext(requestCtx, node.endpoint, katlcAgentDialOptions(transport.NewClientCredentials(config))...)
	if err != nil {
		return err
	}
	defer conn.Close()
	status, err := agentapi.NewKatlcAgentClient(conn).GetNodeStatus(requestCtx, &agentapi.GetNodeStatusRequest{})
	if err != nil {
		return err
	}
	if status.GetInventoryNodeName() != node.name || (node.machineID != "" && status.GetMachineId() != node.machineID) {
		return fmt.Errorf("management endpoint reported a different node or machine")
	}
	return nil
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
	requestCtx = withManagementDial(requestCtx, node.name, &credentials)
	conn, err := dialKatlcAgent(requestCtx, node.endpoint)
	if err != nil {
		return err
	}
	defer conn.Close()
	status, err := conn.Client.GetNodeStatus(requestCtx, &agentapi.GetNodeStatusRequest{})
	if err != nil {
		return err
	}
	if status.GetInventoryNodeName() != node.name || (node.machineID != "" && status.GetMachineId() != node.machineID) {
		return fmt.Errorf("management endpoint reported a different node or machine")
	}
	return nil
}
