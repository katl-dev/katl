package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"strings"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/installer/clusterplan"
	"github.com/katl-dev/katl/internal/installer/configbundle"
	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/katlctl/workstation"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

const (
	defaultKubernetesVersion = "v1.36.1"
	defaultKubernetesBundle  = "ghcr.io/katl-dev/kubernetes:v1.36.1-katl.1"
)

type configInitOptions struct {
	outputPath        string
	clusterName       string
	controlPlane      string
	kubernetesVersion string
	kubernetesBundle  string
	sshKeyPath        string
	nodes             initNodeSpecs
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

func newConfigInitCommand(stdout, stderr io.Writer) *cobra.Command {
	opts := defaultConfigInitOptions()
	cmd := &cobra.Command{
		Use:   "init [PATH]",
		Short: "Create an editable starter ClusterConfig",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) == 1 {
				opts.outputPath = args[0]
			}
			return runConfigInit(opts, stdout, stderr)
		},
	}
	addConfigInitFlags(cmd, &opts)
	cmd.Flags().Var(&opts.nodes, "node", "node as name=role,address,/dev/disk/by-id/... (repeatable)")
	return cmd
}

func defaultConfigInitOptions() configInitOptions {
	return configInitOptions{
		clusterName:       "katl-lab",
		kubernetesVersion: defaultKubernetesVersion,
		kubernetesBundle:  defaultKubernetesBundle,
	}
}

func addConfigInitFlags(cmd *cobra.Command, opts *configInitOptions) {
	cmd.Flags().StringVar(&opts.clusterName, "name", opts.clusterName, "cluster name")
	cmd.Flags().StringVar(&opts.controlPlane, "control-plane-endpoint", "", "stable Kubernetes API endpoint host:port; defaults to the first control-plane address")
	cmd.Flags().StringVar(&opts.kubernetesVersion, "kubernetes-version", opts.kubernetesVersion, "Kubernetes payload version")
	cmd.Flags().StringVar(&opts.kubernetesBundle, "kubernetes-bundle", opts.kubernetesBundle, "Kubernetes bundle image")
	cmd.Flags().StringVar(&opts.sshKeyPath, "ssh-authorized-key", "", "public SSH key file; defaults to ~/.ssh/id_ed25519.pub")
	cmd.Flags().BoolVar(&opts.force, "force", false, "replace an existing output file")
}

func runConfigInit(opts configInitOptions, stdout, stderr io.Writer) error {
	_ = stderr
	if len(opts.nodes) == 0 {
		return fmt.Errorf("at least one --node is required")
	}
	configPath, err := workstation.ConfigPath()
	if err != nil {
		return err
	}
	sshKeyPath := strings.TrimSpace(opts.sshKeyPath)
	if sshKeyPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("locate default SSH public key: %w", err)
		}
		sshKeyPath = home + "/.ssh/id_ed25519.pub"
	}
	keyData, err := os.ReadFile(sshKeyPath)
	if err != nil {
		return fmt.Errorf("read SSH public key %s: %w (use --ssh-authorized-key)", sshKeyPath, err)
	}
	sshKey := strings.TrimSpace(string(keyData))
	if sshKey == "" || strings.ContainsAny(sshKey, "\r\n") {
		return fmt.Errorf("SSH public key %s must contain one non-empty key", sshKeyPath)
	}

	controlPlane := strings.TrimSpace(opts.controlPlane)
	if controlPlane == "" {
		for _, node := range opts.nodes {
			if node.role == inventory.RoleControlPlane {
				controlPlane = net.JoinHostPort(node.address, "6443")
				break
			}
		}
	}
	if controlPlane == "" {
		return fmt.Errorf("at least one control-plane --node is required")
	}

	wipe := true
	source := configbundle.SourceConfig{
		APIVersion: configbundle.APIVersion,
		Kind:       configbundle.Kind,
		Metadata:   configbundle.Metadata{Name: strings.TrimSpace(opts.clusterName)},
		Spec: configbundle.SourceSpec{
			ControlPlaneEndpoint: controlPlane,
			Kubernetes: configbundle.SourceKubernetesCluster{
				Version: strings.TrimSpace(opts.kubernetesVersion),
				Bundle:  strings.TrimSpace(opts.kubernetesBundle),
			},
			Defaults: configbundle.SourceNodeLayer{
				Identity: configbundle.SourceIdentity{SSH: manifest.SSHIdentity{AuthorizedKeys: []string{sshKey}}},
				Install:  configbundle.SourceInstallLayer{WipeTarget: &wipe},
				Networkd: manifest.NetworkdConfig{Files: []manifest.NetworkdFile{{Name: "10-lan.network", Content: "[Match]\nType=ether\n\n[Network]\nDHCP=yes\n"}}},
			},
			SystemRoleDefaults: map[inventory.SystemRole]configbundle.SourceNodeLayer{
				inventory.RoleControlPlane: {Kubernetes: configbundle.SourceKubernetesLayer{Kubeadm: configbundle.SourceKubeadmRef{ConfigRef: "control-plane"}}},
				inventory.RoleWorker:       {Kubernetes: configbundle.SourceKubernetesLayer{Kubeadm: configbundle.SourceKubeadmRef{ConfigRef: "worker"}}},
			},
			KubeadmConfigs: map[string]configbundle.SourceKubeadmConfig{
				"control-plane": {Config: kubeadmInitConfig(opts.kubernetesVersion)},
				"worker":        {Config: kubeadmJoinConfig()},
			},
		},
	}
	seen := map[string]struct{}{}
	for _, node := range opts.nodes {
		if _, ok := seen[node.name]; ok {
			return fmt.Errorf("duplicate node name %q", node.name)
		}
		seen[node.name] = struct{}{}
		credentialPath, err := workstation.CredentialPath(configPath, source.Metadata.Name, node.name)
		if err != nil {
			return err
		}
		targetDisk := node.disk
		source.Spec.Nodes = append(source.Spec.Nodes, configbundle.SourceNode{
			Name: node.name, SystemRole: node.role,
			Overrides: configbundle.SourceNodeLayer{
				Identity:  configbundle.SourceIdentity{Hostname: node.name},
				Install:   configbundle.SourceInstallLayer{TargetDisk: &targetDisk},
				Bootstrap: clusterplan.BootstrapLayer{Address: node.address, Access: inventory.Access{Method: "agent", CredentialRef: "file:" + credentialPath}},
			},
		})
	}
	data, err := yaml.Marshal(source)
	if err != nil {
		return fmt.Errorf("encode starter ClusterConfig: %w", err)
	}
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

func kubeadmInitConfig(version string) string {
	return "apiVersion: kubeadm.k8s.io/v1beta4\nkind: InitConfiguration\nnodeRegistration:\n  criSocket: unix:///run/containerd/containerd.sock\n---\napiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\nkubernetesVersion: " + strings.TrimSpace(version) + "\n"
}

func kubeadmJoinConfig() string {
	return "apiVersion: kubeadm.k8s.io/v1beta4\nkind: JoinConfiguration\nnodeRegistration:\n  criSocket: unix:///run/containerd/containerd.sock\n"
}
