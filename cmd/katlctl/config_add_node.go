package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/installer/configbundle"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

type configAddNodeOptions struct {
	configPath       string
	installer        string
	name             string
	role             string
	disk             string
	installerTimeout time.Duration
}

func newConfigAddNodeCommand(ctx context.Context, stdout io.Writer) *cobra.Command {
	opts := configAddNodeOptions{installerTimeout: 15 * time.Second}
	cmd := &cobra.Command{
		Use:   "add-node CONFIG --installer ADDRESS",
		Short: "Add a waiting installer's node to an existing ClusterConfig",
		Example: "katlctl config add-node cluster.yaml --installer 192.0.2.22\n" +
			"katlctl config add-node cluster.yaml --installer 192.0.2.13 --role control-plane",
		Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			opts.configPath = args[0]
			return runConfigAddNode(ctx, opts, stdout, command.ErrOrStderr())
		},
	}
	cmd.Flags().StringVar(&opts.installer, "installer", "", "waiting installer IP, hostname, host:port, or base URL")
	cmd.Flags().StringVar(&opts.name, "name", "", "node name; defaults to the next free worker-N or cp-N")
	cmd.Flags().StringVar(&opts.role, "role", "", "worker (default) or control-plane")
	cmd.Flags().StringVar(&opts.disk, "disk", "", "disk to install to when the installer reports several: a /dev/disk/by-id path (recommended), /dev/disk/by-path path, or /dev/NAME")
	cmd.Flags().DurationVar(&opts.installerTimeout, "installer-timeout", opts.installerTimeout, "overall timeout for reading installer status")
	_ = cmd.MarkFlagRequired("installer")
	return cmd
}

func runConfigAddNode(ctx context.Context, opts configAddNodeOptions, stdout, stderr io.Writer) error {
	role := inventory.SystemRole(strings.TrimSpace(opts.role))
	if role != "" && role != inventory.RoleControlPlane && role != inventory.RoleWorker {
		return fmt.Errorf("--role must be %q or %q", inventory.RoleWorker, inventory.RoleControlPlane)
	}
	name := strings.TrimSpace(opts.name)
	if opts.installerTimeout <= 0 {
		return fmt.Errorf("--installer-timeout must be positive")
	}

	original, err := os.ReadFile(opts.configPath)
	if err != nil {
		return fmt.Errorf("read ClusterConfig %s: %w", opts.configPath, err)
	}
	source, err := configbundle.DecodeSource(bytes.NewReader(original))
	if err != nil {
		return fmt.Errorf("read ClusterConfig %s: %w", opts.configPath, err)
	}
	if _, err := source.ManagementAuthentication(); err != nil {
		return fmt.Errorf("read ClusterConfig %s: %w", opts.configPath, err)
	}
	endpoint, err := normalizeInstallerAddress(opts.installer)
	if err != nil {
		return fmt.Errorf("--installer %q: %w", opts.installer, err)
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Hostname() == "" {
		return fmt.Errorf("installer endpoint %q has no usable address", endpoint)
	}
	address := parsed.Hostname()

	// The installer's address identifies the machine, so a repeated command
	// finds its earlier entry even after the installer has stopped waiting.
	for _, node := range source.Spec.Nodes {
		if !strings.EqualFold(strings.TrimSpace(node.Management.Address), address) {
			continue
		}
		existingRole := inventory.RoleWorker
		if node.ControlPlane {
			existingRole = inventory.RoleControlPlane
		}
		if name != "" && name != node.Name {
			return fmt.Errorf("%s already lists node %s at %s; omit --name to keep that entry, or give the new machine a different address", opts.configPath, node.Name, address)
		}
		if role != "" && role != existingRole {
			return fmt.Errorf("%s already lists %s at %s as a %s; a node's role cannot change without wiping and reinstalling it", opts.configPath, node.Name, address, existingRole)
		}
		fmt.Fprintf(stdout, "%s already lists %s (%s) at %s; unchanged\n", opts.configPath, node.Name, existingRole, address)
		fmt.Fprintf(stdout, "If %s is not installed and joined yet, run:\n%s", node.Name, addNodeNextSteps(opts.configPath, opts.installer, address, node.Name))
		return nil
	}

	if role == "" {
		role = inventory.RoleWorker
	}
	if name == "" {
		name = nextNodeName(source, role)
	}
	for _, node := range source.Spec.Nodes {
		if node.Name == name {
			return fmt.Errorf("%s already has a node named %s at %s; choose another --name", opts.configPath, name, node.Management.Address)
		}
	}

	requestCtx, cancel := context.WithTimeout(ctx, opts.installerTimeout)
	defer cancel()
	client := &http.Client{Timeout: requestTimeout(opts.installerTimeout)}
	endpoint, _, disks, err := readWaitingInstaller(requestCtx, client, opts.installer)
	if err != nil {
		return err
	}
	disk, err := installerTargetDisk(endpoint, disks, strings.TrimSpace(opts.disk), "Choose the intended disk with --disk, giving its /dev/disk/by-id path when it has one, otherwise its /dev/disk/by-path path or kernel name")
	if err != nil {
		return err
	}
	node := sourceNode(initNodeSpec{name: name, role: role, address: address, disk: disk})
	if warning := weakSystemDiskWarning(name, disk); warning != "" {
		fmt.Fprintln(stderr, warning)
	}

	updated, err := appendNodeEntry(original, node)
	if err != nil {
		return fmt.Errorf("add %s to %s: %w; add the node entry by hand", name, opts.configPath, err)
	}
	// Text insertion must change nothing but the appended node.
	want := source
	want.Spec.Nodes = append(append([]configbundle.SourceNode(nil), source.Spec.Nodes...), node)
	if got, err := configbundle.DecodeSource(bytes.NewReader(updated)); err != nil || !reflect.DeepEqual(got, want) {
		return fmt.Errorf("add %s to %s: the file layout prevents a safe edit; add the node entry by hand", name, opts.configPath)
	}
	if err := replaceConfigFile(opts.configPath, original, updated, name); err != nil {
		return err
	}

	fmt.Fprintf(stdout, "added %s (%s) at %s to %s\n", name, role, address, opts.configPath)
	fmt.Fprintf(stdout, "Install it and join it to the running cluster:\n%s", addNodeNextSteps(opts.configPath, opts.installer, address, name))
	return nil
}

// addNodeNextSteps names the commands that install and join name, passing
// the installer address to install apply unless it is the default for the
// node's management address.
func addNodeNextSteps(configPath, installer, address, name string) string {
	endpoint := ""
	if normalized, err := normalizeInstallerAddress(installer); err == nil {
		if bare, err := normalizeInstallerAddress(address); err != nil || bare != normalized {
			endpoint = " --endpoint " + installer
		}
	}
	return fmt.Sprintf("  katlctl install apply --config %s --node %s%s\n  katlctl node join %s --config %s\n", configPath, name, endpoint, name, configPath)
}

func nextNodeName(source configbundle.SourceConfig, role inventory.SystemRole) string {
	prefix := "worker-"
	if role == inventory.RoleControlPlane {
		prefix = "cp-"
	}
	used := make(map[string]struct{}, len(source.Spec.Nodes))
	for _, node := range source.Spec.Nodes {
		used[node.Name] = struct{}{}
	}
	for i := 1; ; i++ {
		name := fmt.Sprintf("%s%d", prefix, i)
		if _, ok := used[name]; !ok {
			return name
		}
	}
}

// appendNodeEntry inserts node after the last spec.nodes entry, leaving every
// other line of data as written.
func appendNodeEntry(data []byte, node configbundle.SourceNode) ([]byte, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil, err
	}
	if len(document.Content) == 0 {
		return nil, fmt.Errorf("the file is empty")
	}
	_, spec := yamlMappingEntry(document.Content[0], "spec")
	key, nodes := yamlMappingEntry(spec, "nodes")
	if nodes == nil || nodes.Kind != yaml.SequenceNode || nodes.Style&yaml.FlowStyle != 0 || len(nodes.Content) == 0 {
		return nil, fmt.Errorf("spec.nodes is not a non-empty block list")
	}

	// yaml.v3 reports 1-based columns; the sequence column is its dash.
	dash := nodes.Column - 1
	indent := dash - (key.Column - 1)
	if indent < 2 {
		indent = 2
	}
	var rendered bytes.Buffer
	encoder := yaml.NewEncoder(&rendered)
	encoder.SetIndent(indent)
	if err := encoder.Encode(map[string][]configbundle.SourceNode{"nodes": {node}}); err != nil {
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	entry := strings.SplitAfter(strings.TrimSuffix(rendered.String(), "\n"), "\n")[1:]
	shift := dash - strings.Index(entry[0], "-")
	for i, line := range entry {
		if shift >= 0 {
			entry[i] = strings.Repeat(" ", shift) + line
		} else {
			entry[i] = line[-shift:]
		}
	}

	// The last entry extends over every following line indented past the
	// dash, including its own comments. Lines at or left of the dash that
	// are not content, such as comments, stay where they are.
	lines := strings.SplitAfter(string(data), "\n")
	end := nodes.Content[len(nodes.Content)-1].Line - 1
	for i := end + 1; i < len(lines); i++ {
		text := strings.TrimRight(lines[i], "\r\n")
		trimmed := strings.TrimLeft(text, " ")
		if trimmed == "" {
			continue
		}
		if len(text)-len(trimmed) > dash {
			end = i
			continue
		}
		if !strings.HasPrefix(trimmed, "#") {
			break
		}
	}
	if !strings.HasSuffix(lines[end], "\n") {
		lines[end] += "\n"
	}
	var out strings.Builder
	for _, line := range lines[:end+1] {
		out.WriteString(line)
	}
	for _, line := range entry {
		out.WriteString(line)
	}
	out.WriteString("\n")
	for _, line := range lines[end+1:] {
		out.WriteString(line)
	}
	return []byte(out.String()), nil
}

func yamlMappingEntry(mapping *yaml.Node, key string) (*yaml.Node, *yaml.Node) {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil, nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i], mapping.Content[i+1]
		}
	}
	return nil, nil
}

// replaceConfigFile validates updated as config validate would, in the
// config's directory so relative host-file paths resolve, then atomically
// replaces the config unless it changed since it was read.
func replaceConfigFile(path string, original, updated []byte, name string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".katl-config-*")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	if _, err := temporary.Write(updated); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Chmod(info.Mode().Perm()); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if _, err := planConfigFile(temporary.Name()); err != nil {
		return fmt.Errorf("adding %s would make %s invalid, so it is unchanged: %w", name, path, err)
	}
	current, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(current, original) {
		return fmt.Errorf("%s changed while adding %s; run the command again", path, name)
	}
	return os.Rename(temporary.Name(), path)
}
