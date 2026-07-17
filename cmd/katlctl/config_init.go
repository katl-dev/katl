package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/installer/configbundle"
	"github.com/katl-dev/katl/internal/installer/handoff"
	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var sshAgentPublicKeys = func() ([]byte, error) {
	return exec.Command("ssh-add", "-L").Output()
}

type configInitOptions struct {
	outputPath        string
	clusterName       string
	controlPlane      string
	kubernetesVersion string
	sshKeyPath        string
	nodes             initNodeSpecs
	installers        stringList
	installerTimeout  time.Duration
	force             bool
}

type initNodeSpec struct {
	name    string
	role    inventory.SystemRole
	address string
	disk    manifest.DiskSelector
}

type initNodeSpecs []initNodeSpec

func (values *initNodeSpecs) String() string { return "name=role,address,/dev/disk/by-id/..." }
func (values *initNodeSpecs) Type() string   { return "node" }
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
	disk := strings.TrimSpace(parts[2])
	if !strings.HasPrefix(disk, "/dev/disk/by-id/") {
		return fmt.Errorf("node %q disk must be a stable /dev/disk/by-id path", name)
	}
	*values = append(*values, initNodeSpec{name: strings.TrimSpace(name), role: role, address: address, disk: manifest.DiskSelector{ByID: disk}})
	return nil
}

func newConfigInitCommand(ctx context.Context, stdout, stderr io.Writer) *cobra.Command {
	opts := defaultConfigInitOptions()
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
	addConfigInitFlags(cmd, &opts)
	cmd.Flags().Var(&opts.nodes, "node", "node as name=role,address,/dev/disk/by-id/... (repeatable)")
	cmd.Flags().Var(&opts.installers, "installer", "waiting installer IP, hostname, host:port, or base URL (repeatable)")
	cmd.Flags().DurationVar(&opts.installerTimeout, "installer-timeout", opts.installerTimeout, "overall timeout for reading installer status")
	return cmd
}

func defaultConfigInitOptions() configInitOptions {
	return configInitOptions{
		clusterName:       "katl-lab",
		kubernetesVersion: configbundle.DefaultKubernetesVersion,
		installerTimeout:  15 * time.Second,
	}
}

func addConfigInitFlags(cmd *cobra.Command, opts *configInitOptions) {
	cmd.Flags().StringVar(&opts.clusterName, "name", opts.clusterName, "cluster name")
	cmd.Flags().StringVar(&opts.controlPlane, "control-plane-endpoint", "", "stable Kubernetes API endpoint host:port; defaults to the first control-plane address")
	cmd.Flags().StringVar(&opts.kubernetesVersion, "kubernetes-version", opts.kubernetesVersion, "override the default Kubernetes payload version")
	cmd.Flags().StringVar(&opts.sshKeyPath, "ssh-authorized-key", "", "public SSH key file; otherwise uses the active SSH agent or ~/.ssh/id_ed25519.pub")
	cmd.Flags().BoolVar(&opts.force, "force", false, "replace an existing output file")
}

func runConfigInit(ctx context.Context, opts configInitOptions, stdout, stderr io.Writer) error {
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

	source := configbundle.SourceConfig{
		APIVersion: configbundle.APIVersion,
		Kind:       configbundle.Kind,
		Metadata:   configbundle.Metadata{Name: strings.TrimSpace(opts.clusterName)},
		Spec: configbundle.SourceSpec{
			ControlPlaneEndpoint: strings.TrimSpace(opts.controlPlane),
			Kubernetes: configbundle.SourceKubernetesCluster{
				Version: strings.TrimSpace(opts.kubernetesVersion),
			},
			Defaults: configbundle.SourceNodeLayer{
				Identity: configbundle.SourceIdentity{SSH: manifest.SSHIdentity{AuthorizedKeys: sshKeys}},
			},
		},
	}
	seen := map[string]struct{}{}
	for _, node := range opts.nodes {
		if _, ok := seen[node.name]; ok {
			return fmt.Errorf("duplicate node name %q", node.name)
		}
		seen[node.name] = struct{}{}
		targetDisk := node.disk
		source.Spec.Nodes = append(source.Spec.Nodes, configbundle.SourceNode{
			Name:         node.name,
			ControlPlane: node.role == inventory.RoleControlPlane,
			Install:      configbundle.SourceInstallLayer{TargetDisk: &targetDisk},
			Bootstrap:    configbundle.SourceBootstrapLayer{Address: node.address},
		})
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

func annotateStarterConfig(data []byte, missingSSHKeys bool) []byte {
	comments := "spec:\n" +
		"    # Stable Kubernetes API endpoint for multi-control-plane clusters.\n" +
		"    # controlPlaneEndpoint: api.home.arpa:6443\n" +
		"    # Set controlPlane: true on nodes that join the Kubernetes control plane.\n" +
		"    # Omission means worker.\n" +
		"    # Nodes use DHCP by default; native systemd-networkd files can be set under defaults or a node.\n"
	if missingSSHKeys {
		comments += "    # Add an SSH public key here if console-only access is not sufficient.\n" +
			"    # defaults:\n" +
			"    #     identity:\n" +
			"    #         ssh:\n" +
			"    #             authorizedKeys:\n" +
			"    #                 - ssh-ed25519 AAAA... operator@home\n"
	}
	comments += "\n"
	return []byte(strings.Replace(string(data), "spec:\n", comments, 1))
}

func initNodesFromInstallers(ctx context.Context, addresses []string, timeout time.Duration) (initNodeSpecs, error) {
	if timeout <= 0 {
		return nil, fmt.Errorf("--installer-timeout must be positive")
	}
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	client := &http.Client{Timeout: requestTimeout(timeout)}
	installers := make([]discoveredInstaller, 0, len(addresses))
	seen := make(map[string]struct{}, len(addresses))
	for _, address := range addresses {
		endpoint, err := normalizeInstallerAddress(address)
		if err != nil {
			return nil, fmt.Errorf("--installer %q: %w", address, err)
		}
		if _, ok := seen[endpoint]; ok {
			return nil, fmt.Errorf("duplicate --installer endpoint %s", endpoint)
		}
		seen[endpoint] = struct{}{}
		status, err := fetchInstallStatus(requestCtx, client, endpoint)
		if err != nil {
			return nil, fmt.Errorf("read installer %s: %w", endpoint, err)
		}
		if status.State != handoff.HandoffWaiting {
			return nil, fmt.Errorf("installer %s is not waiting for config: state=%s", endpoint, status.State)
		}
		installers = append(installers, discoveredInstaller{Endpoint: endpoint, Status: status})
	}
	return discoveredInitNodes(installers)
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
			return nil, append(notices,
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
