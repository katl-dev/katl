package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/installer/configbundle"
	"github.com/katl-dev/katl/internal/installer/controlplaneendpoint"
	"github.com/katl-dev/katl/internal/installer/handoff"
	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/managementidentity"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var sshAgentPublicKeys = func() ([]byte, error) {
	return exec.Command("ssh-add", "-L").Output()
}

type configInitOptions struct {
	managementAuthentication string
	outputPath               string
	clusterName              string
	controlPlane             string
	kubernetesVersion        string
	sshKeyPath               string
	nodes                    initNodeSpecs
	installers               stringList
	installerTimeout         time.Duration
	force                    bool
}

type initNodeSpec struct {
	name    string
	role    inventory.SystemRole
	address string
	disk    manifest.DiskSelector
}

type initNodeSpecs []initNodeSpec

func (values *initNodeSpecs) String() string {
	var entries []string
	for _, node := range *values {
		entries = append(entries, node.name+"="+string(node.role)+","+node.address+","+systemDiskArgumentString(node.disk))
	}
	return strings.Join(entries, "; ")
}
func (values *initNodeSpecs) Type() string { return "node" }
func (values *initNodeSpecs) Set(value string) error {
	name, fields, ok := strings.Cut(strings.TrimSpace(value), "=")
	parts := strings.Split(fields, ",")
	if !ok || strings.TrimSpace(name) == "" || len(parts) != 3 {
		return fmt.Errorf("node must be name=role,address,/dev/disk/by-id/...")
	}
	role := inventory.SystemRole(strings.TrimSpace(parts[0]))
	if role != inventory.RoleControlPlane && role != inventory.RoleWorker {
		return fmt.Errorf("node %q role must be %q or %q", name, inventory.RoleControlPlane, inventory.RoleWorker)
	}
	address := strings.TrimSpace(parts[1])
	if net.ParseIP(address) == nil && !validHostname(address) {
		return fmt.Errorf("node %q address %q is not an IP address or hostname", name, address)
	}
	disk, err := systemDiskArgument(strings.TrimSpace(parts[2]))
	if err != nil {
		return fmt.Errorf("node %q %w", name, err)
	}
	*values = append(*values, initNodeSpec{name: strings.TrimSpace(name), role: role, address: address, disk: disk})
	return nil
}

func newConfigInitCommand(ctx context.Context, stdout, stderr io.Writer) *cobra.Command {
	opts := configInitOptions{
		clusterName:       "katl-lab",
		kubernetesVersion: configbundle.DefaultKubernetesVersion,
		installerTimeout:  15 * time.Second,
	}
	cmd := &cobra.Command{
		Use:   "init [PATH]",
		Short: "Create an editable starter ClusterConfig",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) == 1 {
				opts.outputPath = args[0]
			}
			return runConfigInit(ctx, opts, stdout, stderr)
		},
	}
	cmd.Flags().StringVar(&opts.managementAuthentication, "management-authentication", "", "trusted-network (default) or mtls; --force preserves the existing mode unless specified")
	cmd.Flags().StringVar(&opts.clusterName, "name", opts.clusterName, "cluster name")
	cmd.Flags().StringVar(&opts.controlPlane, "control-plane-endpoint", "", "stable Kubernetes API endpoint host:port; defaults to the first control-plane address")
	cmd.Flags().StringVar(&opts.kubernetesVersion, "kubernetes-version", opts.kubernetesVersion, "override the default Kubernetes payload version")
	cmd.Flags().StringVar(&opts.sshKeyPath, "ssh-authorized-key", "", "public SSH key file; otherwise uses the active SSH agent or ~/.ssh/id_ed25519.pub")
	cmd.Flags().BoolVar(&opts.force, "force", false, "replace an existing output file")
	cmd.Flags().Var(&opts.nodes, "node", "node as name=role,address,DISK where DISK is a /dev/disk/by-id path (recommended), /dev/disk/by-path path, or /dev/NAME (repeatable)")
	cmd.Flags().Var(&opts.installers, "installer", "waiting installer IP, hostname, host:port, or base URL (repeatable)")
	cmd.Flags().DurationVar(&opts.installerTimeout, "installer-timeout", opts.installerTimeout, "overall timeout for reading installer status")
	return cmd
}

func runConfigInit(ctx context.Context, opts configInitOptions, stdout, stderr io.Writer) error {
	kubernetesVersion := strings.TrimSpace(opts.kubernetesVersion)
	if kubernetesVersion == "" {
		return fmt.Errorf("--kubernetes-version is required and must be an exact version such as %s", configbundle.DefaultKubernetesVersion)
	}
	if len(opts.nodes) > 0 && len(opts.installers.values) > 0 {
		return fmt.Errorf("--node and --installer cannot be used together")
	}
	if len(opts.installers.values) > 0 {
		var err error
		opts.nodes, err = initNodesFromInstallers(ctx, opts.installers.values, opts.installerTimeout)
		if err != nil {
			return err
		}
	}
	if len(opts.nodes) == 0 {
		return fmt.Errorf("at least one --node or --installer is required")
	}
	sshKeys, notices, err := configSSHKeys(opts.sshKeyPath)
	if err != nil {
		return err
	}
	for _, notice := range notices {
		fmt.Fprintln(stderr, notice)
	}

	hasControlPlane := false
	for _, node := range opts.nodes {
		if node.role == inventory.RoleControlPlane {
			hasControlPlane = true
			break
		}
	}
	if !hasControlPlane {
		return fmt.Errorf("at least one control-plane --node is required")
	}
	controlPlaneEndpoint, err := configInitControlPlaneEndpoint(opts.controlPlane)
	if err != nil {
		return err
	}

	source := configbundle.SourceConfig{
		APIVersion: configbundle.APIVersion,
		Kind:       configbundle.Kind,
		Metadata:   configbundle.Metadata{Name: strings.TrimSpace(opts.clusterName)},
		Spec: configbundle.SourceSpec{
			ManagementAuthentication: managementidentity.Authentication(opts.managementAuthentication),
			ControlPlaneEndpoint:     controlPlaneEndpoint,
			Kubernetes: configbundle.SourceKubernetesCluster{
				Version: kubernetesVersion,
			},
			Defaults: configbundle.SourceNodeLayer{
				Access: configbundle.SourceAccess{SSH: configbundle.SourceSSHAccess{
					AuthorizedKeys: configbundle.Some(sshKeys),
				}},
			},
		},
	}
	seen := map[string]struct{}{}
	for _, node := range opts.nodes {
		if _, ok := seen[node.name]; ok {
			return fmt.Errorf("duplicate node name %q", node.name)
		}
		seen[node.name] = struct{}{}
		source.Spec.Nodes = append(source.Spec.Nodes, sourceNode(node))
		if warning := weakSystemDiskWarning(node.name, node.disk); warning != "" {
			fmt.Fprintln(stderr, warning)
		}
	}
	if source.Spec.ManagementAuthentication == "" {
		source.Spec.ManagementAuthentication = managementidentity.TrustedNetwork
	}
	existingReference := false
	existingTLS := false
	if opts.force && strings.TrimSpace(opts.outputPath) != "" {
		previous, readErr := os.ReadFile(opts.outputPath)
		if readErr == nil {
			previousSource, decodeErr := configbundle.DecodeSource(strings.NewReader(string(previous)))
			if decodeErr != nil {
				return fmt.Errorf("read existing cluster configuration: %w", decodeErr)
			}
			if opts.managementAuthentication == "" {
				source.Spec.ManagementAuthentication = previousSource.ManagementAuthentication()
			}
			existingTLS = previousSource.ManagementAuthentication() == managementidentity.MutualTLS
			if reference := previousSource.Spec.ManagementIdentity; reference != "" && source.ManagementAuthentication() == managementidentity.MutualTLS {
				source.Spec.ManagementIdentity = reference
				existingReference = true
			}
		} else if !errors.Is(readErr, os.ErrNotExist) {
			return readErr
		}
	}
	if source.ManagementAuthentication() == managementidentity.MutualTLS && source.Spec.ManagementIdentity == "" {
		source.Spec.ManagementIdentity = defaultManagementSecrets
	}
	data, err := yaml.Marshal(source)
	if err != nil {
		return fmt.Errorf("encode starter ClusterConfig: %w", err)
	}
	data = annotateStarterConfig(data, len(sshKeys) == 0)
	if _, err := configbundle.DecodeSource(strings.NewReader(string(data))); err != nil {
		return fmt.Errorf("validate generated ClusterConfig: %w", err)
	}
	if strings.TrimSpace(opts.outputPath) == "" {
		_, err = stdout.Write(data)
		return err
	}
	_, outputErr := os.Stat(opts.outputPath)
	if outputErr == nil && !opts.force {
		return fmt.Errorf("ClusterConfig %s already exists; use --force to replace the configuration", opts.outputPath)
	}
	if outputErr != nil && !errors.Is(outputErr, os.ErrNotExist) {
		return outputErr
	}
	if source.ManagementAuthentication() != managementidentity.TrustedNetwork {
		secretsPath := source.Spec.ManagementIdentity
		if !filepath.IsAbs(secretsPath) {
			secretsPath = filepath.Join(filepath.Dir(opts.outputPath), secretsPath)
		}
		identity, err := readManagementIdentity(secretsPath)
		if errors.Is(err, os.ErrNotExist) {
			if existingReference {
				return missingManagementIdentity(source.Metadata.Name, secretsPath)
			}
			// Reuse an existing authority when moving a cluster out of the legacy
			// workstation store. Only explicit cluster setup creates new trust.
			legacyPath, pathErr := managementIdentityPath(source.Metadata.Name)
			if pathErr != nil {
				return pathErr
			}
			identity, err = readManagementIdentity(legacyPath)
			if errors.Is(err, os.ErrNotExist) {
				if existingTLS {
					return missingManagementIdentity(source.Metadata.Name, secretsPath)
				}
				identity, err = managementidentity.Generate(managementidentity.GenerateOptions{ClusterName: source.Metadata.Name})
			}
			if err == nil && identity.ClusterName != source.Metadata.Name {
				return fmt.Errorf("management secrets belong to cluster %q, not %q", identity.ClusterName, source.Metadata.Name)
			}
			if err == nil {
				for _, node := range source.Spec.Nodes {
					if _, _, err = managementidentity.EnsureNode(&identity, node.Name, time.Now().UTC(), nil); err != nil {
						return err
					}
				}
				err = managementidentity.Write(secretsPath, identity)
			}
		}
		if err != nil {
			return fmt.Errorf("prepare cluster secrets %s: %w", secretsPath, err)
		}
		if identity.ClusterName != source.Metadata.Name {
			return fmt.Errorf("management secrets %s belong to cluster %q, not %q", secretsPath, identity.ClusterName, source.Metadata.Name)
		}
		fmt.Fprintf(stdout, "management secrets: %s (keep across reinstalls; supports SOPS encryption)\n", secretsPath)
	}
	flags := os.O_WRONLY | os.O_CREATE
	if opts.force {
		flags |= os.O_TRUNC
	} else {
		flags |= os.O_EXCL
	}
	file, err := os.OpenFile(opts.outputPath, flags, 0o600)
	if err != nil {
		return fmt.Errorf("create ClusterConfig %s: %w", opts.outputPath, err)
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return fmt.Errorf("write ClusterConfig %s: %w", opts.outputPath, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close ClusterConfig %s: %w", opts.outputPath, err)
	}
	fmt.Fprintf(stdout, "created %s\n", opts.outputPath)
	return nil
}

func sourceNode(node initNodeSpec) configbundle.SourceNode {
	targetDisk := configbundle.SourceSystemDiskSelector{}
	for _, field := range []struct {
		target *configbundle.Optional[string]
		value  string
	}{
		{&targetDisk.ByID, node.disk.ByID}, {&targetDisk.WWN, node.disk.WWN}, {&targetDisk.Serial, node.disk.Serial},
		{&targetDisk.ByPath, node.disk.ByPath}, {&targetDisk.GPTGUID, node.disk.GPTGUID}, {&targetDisk.Name, node.disk.Name},
		{&targetDisk.Model, node.disk.Model}, {&targetDisk.Size, node.disk.Size}, {&targetDisk.Type, node.disk.Type},
	} {
		if field.value != "" {
			*field.target = configbundle.Some(field.value)
		}
	}
	if node.disk.MinSizeMiB != 0 {
		targetDisk.MinSizeMiB = configbundle.Some(node.disk.MinSizeMiB)
	}
	return configbundle.SourceNode{
		Name:         node.name,
		ControlPlane: node.role == inventory.RoleControlPlane,
		Install:      configbundle.SourceInstallLayer{SystemDisk: &targetDisk},
		Management:   configbundle.SourceManagementLayer{Address: node.address},
	}
}

func annotateStarterConfig(data []byte, missingSSHKeys bool) []byte {
	comments := "spec:\n" +
		"    # Stable Kubernetes API VIP or DNS name; without it, the first control plane's address is used and cannot be changed later.\n" +
		"    # controlPlaneEndpoint:\n" +
		"    #     host: api.home.arpa\n" +
		"    #     # port: 6443\n" +
		"    #     # To let Katl own a health-gated API VIP, add advertisement.vip.\n" +
		"    # Set controlPlane: true on nodes that join the Kubernetes control plane.\n" +
		"    # Omission means worker.\n" +
		"    # Nodes use DHCP by default; add native systemd-networkd files through hostConfiguration.fileSets.\n"
	comments += "    # Optional kernel arguments can be set under defaults.kernel.commandLine or a node.\n"
	if missingSSHKeys {
		comments += "    # Add an SSH public key here if console-only access is not sufficient.\n" +
			"    # defaults:\n" +
			"    #     access:\n" +
			"    #         ssh:\n" +
			"    #             authorizedKeys:\n" +
			"    #                 - ssh-ed25519 AAAA... operator@home\n"
	}
	comments += "\n"
	return []byte(strings.Replace(string(data), "spec:\n", comments, 1))
}

func configInitControlPlaneEndpoint(value string) (*controlplaneendpoint.Config, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	host, portText, err := net.SplitHostPort(value)
	if err != nil {
		return nil, fmt.Errorf("--control-plane-endpoint must be host:port: %w", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return nil, fmt.Errorf("--control-plane-endpoint port must be a number")
	}
	config := controlplaneendpoint.Config{Host: host, Port: port}
	plan, err := controlplaneendpoint.Normalize(config)
	if err != nil {
		return nil, fmt.Errorf("--control-plane-endpoint: %w", err)
	}
	return &plan.Config, nil
}

func initNodesFromInstallers(ctx context.Context, addresses []string, timeout time.Duration) (initNodeSpecs, error) {
	if timeout <= 0 {
		return nil, fmt.Errorf("--installer-timeout must be positive")
	}
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	client := &http.Client{Timeout: requestTimeout(timeout)}
	nodes := make(initNodeSpecs, 0, len(addresses))
	seen := make(map[string]struct{}, len(addresses))
	for i, address := range addresses {
		endpoint, host, disks, err := readWaitingInstaller(requestCtx, client, address)
		if err != nil {
			return nil, err
		}
		if _, ok := seen[endpoint]; ok {
			return nil, fmt.Errorf("duplicate --installer endpoint %s", endpoint)
		}
		seen[endpoint] = struct{}{}
		disk, err := installerTargetDisk(endpoint, disks, "", "Use katlctl config init --node NAME=ROLE,ADDRESS,DISK to choose the intended disk")
		if err != nil {
			return nil, err
		}
		name, role := "cp-1", inventory.RoleControlPlane
		if i > 0 {
			name, role = fmt.Sprintf("worker-%d", i), inventory.RoleWorker
		}
		nodes = append(nodes, initNodeSpec{name: name, role: role, address: host, disk: disk})
	}
	return nodes, nil
}

// readWaitingInstaller returns the installer's normalized endpoint, the host
// it answers at, and the disks it reports while waiting for configuration.
func readWaitingInstaller(ctx context.Context, client *http.Client, address string) (string, string, []handoff.HandoffDisk, error) {
	endpoint, err := normalizeInstallerAddress(address)
	if err != nil {
		return "", "", nil, fmt.Errorf("--installer %q: %w", address, err)
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Hostname() == "" {
		return "", "", nil, fmt.Errorf("installer endpoint %q has no usable address", endpoint)
	}
	status, err := fetchInstallStatus(ctx, client, endpoint)
	if err != nil {
		return "", "", nil, fmt.Errorf("read installer %s: %w", endpoint, err)
	}
	if status.State != handoff.HandoffWaiting {
		return "", "", nil, fmt.Errorf("installer %s is not waiting for config: state=%s", endpoint, status.State)
	}
	return endpoint, parsed.Hostname(), status.Disks, nil
}

// installerTargetDisk selects the installer's only selectable disk, or the
// selectable disk named by chosen. hint tells the operator how to choose when
// the selection fails.
func installerTargetDisk(endpoint string, disks []handoff.HandoffDisk, chosen, hint string) (manifest.DiskSelector, error) {
	var named manifest.DiskSelector
	if chosen != "" {
		var err error
		if named, err = systemDiskArgument(chosen); err != nil {
			return manifest.DiskSelector{}, err
		}
	}
	var selectable []handoff.HandoffDisk
	for _, disk := range disks {
		if disk.Selectable && (chosen == "" || handoffDiskNames(disk, chosen)) {
			selectable = append(selectable, disk)
		}
	}
	if len(selectable) == 1 {
		// Record the strongest identity of the chosen disk. Without a choice,
		// only a hardware identity may stand in for one: an attachment path or
		// kernel name must be named by the operator.
		strongest := installerSystemDisk(selectable[0])
		switch {
		case named.Target().Stable():
			return named, nil
		case strongest.Target().Stable():
			return strongest, nil
		case chosen != "":
			return named, nil
		default:
			return manifest.DiskSelector{}, fmt.Errorf("installer %s reports one selectable disk, %s, but it has no hardware by-id path, WWN, or serial, so Katl will not choose it implicitly. %s", endpoint, describeHandoffDisk(selectable[0]), hint)
		}
	}
	if chosen != "" && len(selectable) == 0 {
		return manifest.DiskSelector{}, fmt.Errorf("installer %s reports no selectable disk %s. %s", endpoint, chosen, hint)
	}
	detail := "none"
	if len(selectable) > 0 {
		descriptions := make([]string, 0, len(selectable))
		for _, disk := range selectable {
			descriptions = append(descriptions, describeHandoffDisk(disk))
		}
		detail = strings.Join(descriptions, "; ")
	}
	return manifest.DiskSelector{}, fmt.Errorf("installer %s reports %d selectable disks: %s. %s", endpoint, len(selectable), detail, hint)
}

func configSSHKeys(explicitPath string) ([]string, []string, error) {
	explicitPath = strings.TrimSpace(explicitPath)
	if explicitPath != "" {
		key, err := readConfigSSHKey(explicitPath)
		if err != nil {
			return nil, nil, err
		}
		return []string{key}, nil, nil
	}

	var notices []string
	if data, err := sshAgentPublicKeys(); err == nil {
		keys, ignored := supportedSSHKeys(data)
		if len(keys) > 0 {
			notices = append(notices, fmt.Sprintf("using %d SSH public key(s) from the active SSH agent", len(keys)))
			if ignored > 0 {
				notices = append(notices, fmt.Sprintf("warning: ignored %d SSH agent key(s) not supported by KatlOS", ignored))
			}
			return keys, notices, nil
		}
		if ignored > 0 {
			notices = append(notices, fmt.Sprintf("warning: ignored %d SSH agent key(s) not supported by KatlOS", ignored))
		}
	}

	if home, err := os.UserHomeDir(); err == nil {
		path := home + "/.ssh/id_ed25519.pub"
		if _, err := os.Stat(path); err == nil {
			key, err := readConfigSSHKey(path)
			if err == nil {
				return []string{key}, append(notices, "using SSH public key "+path), nil
			}
			return nil, append(
				notices,
				"warning: "+err.Error(),
				"warning: generated ClusterConfig has no SSH authorized keys; add one before install apply",
			), nil
		}
	}

	return nil, append(notices, "warning: generated ClusterConfig has no SSH authorized keys; add one before install apply"), nil
}

func readConfigSSHKey(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read SSH public key %s: %w", path, err)
	}
	key := strings.TrimSpace(string(data))
	if key == "" || strings.ContainsAny(key, "\r\n") || !manifest.ValidAuthorizedKey(key) {
		return "", fmt.Errorf("SSH public key %s must contain one supported public key", path)
	}
	return key, nil
}

func supportedSSHKeys(data []byte) ([]string, int) {
	var keys []string
	seen := map[string]struct{}{}
	ignored := 0
	for _, line := range strings.Split(string(data), "\n") {
		key := strings.TrimSpace(line)
		if key == "" {
			continue
		}
		if !manifest.ValidAuthorizedKey(key) {
			ignored++
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	return keys, ignored
}

func validHostname(value string) bool {
	if value == "" || len(value) > 253 || strings.ContainsAny(value, " /:") {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if label == "" || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
	}
	return true
}
