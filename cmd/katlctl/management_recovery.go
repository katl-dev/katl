package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/installer/configbundle"
	"github.com/katl-dev/katl/internal/managementidentity"
	"github.com/katl-dev/katl/internal/nodeidentity"
	"github.com/spf13/cobra"
)

var runManagementAuthoritySSH = sshManagementAuthority

func newManagementIdentityRecoverCommand(stdout, stderr io.Writer) *cobra.Command {
	opts := managementRotationOptions{sshPort: 22, timeout: 2 * time.Minute}
	command := &cobra.Command{
		Use:   "recover",
		Short: "Replace an installed mTLS authority after the management secrets are lost",
		Long: `Read the management authority each node trusts through host-key-verified
root SSH, then install a new authority on every node that does not already
trust it. The original secrets are not needed. Nodes stay mTLS; no node is
changed until every node has been reached. Rerun the same command to resume;
the new secrets file is reused when it exists.`,
		Example: "katlctl management identity recover --config ./cluster.yaml",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runManagementIdentityRecover(cmd.Context(), opts, stdout, stderr)
		},
	}
	command.Flags().StringVar(&opts.configPath, "config", "", "ClusterConfig YAML of the mTLS cluster")
	command.Flags().StringVar(&opts.outputPath, "output", "", "new secrets file; defaults to the path the config references. A .sops.yaml name encrypts with SOPS before any node changes")
	command.Flags().IntVar(&opts.sshPort, "ssh-port", opts.sshPort, "root SSH port on each node")
	command.Flags().StringVar(&opts.sshKey, "ssh-key", "", "SSH private key for root access")
	command.Flags().StringVar(&opts.knownHosts, "ssh-known-hosts", "", "trusted SSH host keys file")
	command.Flags().DurationVar(&opts.timeout, "timeout", opts.timeout, "time allowed for each SSH or management connection")
	_ = command.MarkFlagRequired("config")
	return command
}

func runManagementIdentityRecover(ctx context.Context, opts managementRotationOptions, stdout, stderr io.Writer) error {
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
		return fmt.Errorf("cluster %q uses trusted-network management, which has no secrets to recover; use --config with the mTLS ClusterConfig", source.Metadata.Name)
	}
	referenced := strings.TrimSpace(source.Spec.ManagementIdentity)
	if referenced != "" && !filepath.IsAbs(referenced) {
		referenced = filepath.Join(filepath.Dir(configPath), referenced)
	}
	outputPath := strings.TrimSpace(opts.outputPath)
	switch {
	case outputPath == "" && referenced != "":
		outputPath = referenced
	case outputPath == "":
		outputPath = filepath.Join(filepath.Dir(configPath), defaultManagementSecrets)
	}
	outputPath, err = filepath.Abs(outputPath)
	if err != nil {
		return err
	}

	// Read every node's authority before writing anything, so an unreachable
	// node or unknown SSH host key leaves the cluster and workstation unchanged.
	nodes, err := managementRotationTargets(ctx, configPath, source)
	if err != nil {
		return err
	}
	installed := make(map[string]nodeidentity.ManagementAuthorityStatus, len(nodes))
	for i := range nodes {
		node := &nodes[i]
		// A saved machine ID may predate a reinstall; the pinned SSH host key
		// ties inspection to the node, and the machine ID it reports binds the
		// later steps to that same install.
		node.machineID = ""
		fmt.Fprintf(stderr, "Reading the installed management authority on %s\n", node.name)
		status, err := runManagementAuthoritySSH(ctx, *node, opts)
		if err != nil {
			return fmt.Errorf("inspect %s: %w; no node credentials were changed", node.name, err)
		}
		installed[node.name] = status
	}

	_, statErr := os.Stat(outputPath)
	reusing := statErr == nil
	next, err := replacementManagementIdentity(configPath, outputPath, source)
	var unreadable *unreadableSecretsError
	if errors.As(err, &unreadable) && opts.outputPath == "" {
		return fmt.Errorf("secrets at %s cannot be read: %w; to recover with a new authority pass --output <new path>", outputPath, unreadable.err)
	}
	if err != nil {
		return err
	}
	nextInfo, err := managementidentity.Validate(next, time.Now().UTC())
	if err != nil {
		return err
	}
	if reusing {
		fmt.Fprintf(stderr, "Reusing existing secrets at %s (authority %s)\n", outputPath, nextInfo.Fingerprint)
	} else {
		fmt.Fprintf(stderr, "Generated new authority %s at %s\n", nextInfo.Fingerprint, outputPath)
	}
	withNext, err := managementRotationNodes(ctx, configPath, source, "", next)
	if err != nil {
		return err
	}
	var pending []managementRotationNode
	for i, node := range withNext {
		status := installed[node.name]
		withNext[i].machineID, withNext[i].request.ExpectedMachineID = status.MachineID, status.MachineID
		if status.Authority == nextInfo.Fingerprint {
			continue
		}
		withNext[i].request.ExpectedAuthority = status.Authority
		pending = append(pending, withNext[i])
	}
	for _, node := range pending {
		fmt.Fprintf(stderr, "Preflight replacement identity on %s\n", node.name)
		if _, err := runManagementRotationSSH(ctx, node, opts, true); err != nil {
			return fmt.Errorf("preflight %s: %w; no node credentials were changed; keep %s to resume", node.name, err, outputPath)
		}
	}
	client, err := managementidentity.Client(next, time.Now().UTC())
	if err != nil {
		return err
	}
	for _, node := range withNext {
		var err error
		if installed[node.name].Authority == nextInfo.Fingerprint {
			err = checkManagementCredential(ctx, node, client, min(opts.timeout, 5*time.Second))
		} else {
			fmt.Fprintf(stderr, "Installing replacement management authority on %s\n", node.name)
			if _, err := runManagementRotationSSH(ctx, node, opts, false); err != nil {
				return fmt.Errorf("recover %s: %w; keep %s and rerun the same command", node.name,
					managementConfirmationError("the replacement was installed", "the previous authority", err), outputPath)
			}
			err = waitManagementCredential(ctx, node, client, opts.timeout)
		}
		if err != nil {
			return fmt.Errorf("%s: %w; keep %s and rerun the same command", node.name,
				managementConfirmationError("the node accepts the replacement authority", "the previous authority", err), outputPath)
		}
		fmt.Fprintf(stderr, "Verified the new authority on %s\n", node.name)
	}
	if referenced != outputPath {
		if err := referenceManagementSecrets(configPath, outputPath, sourceData); err != nil {
			return fmt.Errorf("all nodes accept the new authority, but ClusterConfig does not reference it: %w; set spec.managementAuthentication to mtls and spec.managementIdentity to %s", err, outputPath)
		}
	}
	if len(pending) == 0 {
		fmt.Fprintf(stdout, "Every node of %s already trusts %s; nothing to recover\n", source.Metadata.Name, nextInfo.Fingerprint)
	} else {
		fmt.Fprintf(stdout, "Recovered management trust for %s on %d of %d node(s); new authority %s\n", source.Metadata.Name, len(pending), len(withNext), nextInfo.Fingerprint)
	}
	fmt.Fprintf(stdout, "Back up %s now; it is the only copy of the new authority. Refresh any saved shortcut with %s.\n", outputPath, contextSaveInvocation(configPath))
	return nil
}

func sshManagementAuthority(ctx context.Context, node managementRotationNode, opts managementRotationOptions) (nodeidentity.ManagementAuthorityStatus, error) {
	output, err := runKatlcAgentSSH(ctx, node, opts, nil, "management-authority")
	if err != nil {
		return nodeidentity.ManagementAuthorityStatus{}, err
	}
	var status nodeidentity.ManagementAuthorityStatus
	if err := json.Unmarshal(output, &status); err != nil {
		return nodeidentity.ManagementAuthorityStatus{}, fmt.Errorf("read node management authority: %w; upgrade %s to a release that supports management recovery", err, node.name)
	}
	if status.NodeName != node.name {
		return nodeidentity.ManagementAuthorityStatus{}, fmt.Errorf("SSH endpoint is node %q, not %s; check management.address and the SSH host key", status.NodeName, node.name)
	}
	return status, nil
}
