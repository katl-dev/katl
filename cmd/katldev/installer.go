package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/installer/configbundle"
	"github.com/katl-dev/katl/internal/vmtest"
	"github.com/spf13/cobra"
)

const (
	installerStateAPIVersion = "katl.dev/v1alpha1"
	installerStateKind       = "KatlDevWorldNode"
	legacyStateKind          = "KatlDevInstallerVM"
	legacyDomainMetadata     = "katl/katldev-installer"
	vmtestMetadataURI        = "https://katlos.io/xmlns/vmtest/1"
	katldevTargetDisk        = "/dev/disk/by-id/virtio-katl-root"
)

type installerOptions struct {
	MemoryMiB   int
	CPUs        int
	DiskSize    string
	Timeout     time.Duration
	NoBuild     bool
	ForceConfig bool
}

// installerState records one world VM so later sessions can find and resume
// it. IPAddress and Endpoint are the last observed address, which differs
// from the node's reservation after readdress until the VM reboots.
type installerState struct {
	APIVersion   string    `json:"apiVersion"`
	Kind         string    `json:"kind"`
	RepoRoot     string    `json:"repoRoot"`
	World        string    `json:"world,omitempty"`
	Node         string    `json:"node,omitempty"`
	Revision     string    `json:"revision,omitempty"`
	Dirty        bool      `json:"dirty,omitempty"`
	DomainName   string    `json:"domainName"`
	LibvirtURI   string    `json:"libvirtURI"`
	Network      string    `json:"network"`
	MACAddress   string    `json:"macAddress"`
	IPAddress    string    `json:"ipAddress,omitempty"`
	Endpoint     string    `json:"endpoint,omitempty"`
	InstallerISO string    `json:"installerISO"`
	ISOSHA256    string    `json:"isoSHA256"`
	TargetDisk   string    `json:"targetDisk"`
	SerialLog    string    `json:"serialLog"`
	DomainXML    string    `json:"domainXML"`
	MemoryMiB    int       `json:"memoryMiB"`
	CPUs         int       `json:"cpus"`
	DiskSize     string    `json:"diskSize"`
	StartedAt    time.Time `json:"startedAt"`
}

type installerManager struct {
	repoRoot string
	stdout   io.Writer
	stderr   io.Writer
	client   *http.Client
	run      buildCommandRunner
	runVirsh func(context.Context, string, ...string) ([]byte, error)
	// guestsConfig reads libvirt-guests.service settings; nil uses systemctl.
	guestsConfig func() (libvirtGuestsConfig, error)
}

// newInstallerCommand keeps the single-VM workflow as a one-node world.
func newInstallerCommand(ctx context.Context, stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "installer",
		Short: "Manage a one-node katldev world (see katldev world for more VMs)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newWorldStartCommand(ctx, stdout, stderr, "start", "Start or resume the persistent installer VM"))
	cmd.AddCommand(newWorldStartCommand(ctx, stdout, stderr, "reset", "Wipe the world and boot a fresh installer"))
	cmd.AddCommand(newWorldStatusCommand(ctx, stdout, stderr))
	cmd.AddCommand(newWorldStopCommand(ctx, stdout, stderr, "stop"))
	cmd.AddCommand(newWorldConsoleCommand(ctx, stdout, stderr, "console"))
	return cmd
}

func loadInstallerManager(stdout, stderr io.Writer) (installerManager, error) {
	repoRoot, err := repositoryRoot()
	if err != nil {
		return installerManager{}, err
	}
	return installerManager{
		repoRoot: repoRoot,
		stdout:   stdout,
		stderr:   stderr,
		client:   &http.Client{Timeout: 2 * time.Second},
		run:      runBuildCommand,
	}, nil
}

func repositoryRoot() (string, error) {
	// jj workspaces can be nested beneath a different Git checkout.
	// Resolve the nearest workspace before falling back to plain Git.
	if output, err := exec.Command("jj", "root").Output(); err == nil {
		if root := strings.TrimSpace(string(output)); root != "" {
			return filepath.Abs(root)
		}
	}
	output, err := exec.Command("git", "rev-parse", "--show-toplevel").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("find Katl checkout: %w: %s", err, strings.TrimSpace(string(output)))
	}
	root := strings.TrimSpace(string(output))
	if root == "" {
		return "", errors.New("find Katl checkout: git returned an empty path")
	}
	return filepath.Abs(root)
}

func validateInstallerOptions(opts installerOptions) error {
	if opts.MemoryMiB < 1024 {
		return errors.New("--memory must be at least 1024 MiB")
	}
	if opts.CPUs < 1 {
		return errors.New("--cpus must be positive")
	}
	if strings.TrimSpace(opts.DiskSize) == "" {
		return errors.New("--disk-size is required")
	}
	if opts.Timeout <= 0 {
		return errors.New("--timeout must be positive")
	}
	return nil
}

func (manager installerManager) prepareInstaller(ctx context.Context, noBuild bool) (string, string, error) {
	if !noBuild {
		fmt.Fprintln(manager.stderr, "katldev world: checking current checkout installer artifacts")
		command := exec.CommandContext(ctx, filepath.Join(manager.repoRoot, "scripts", "mkosi"), "build-installer-iso")
		command.Dir = manager.repoRoot
		command.Stdout = manager.stderr
		command.Stderr = manager.stderr
		if err := command.Run(); err != nil {
			return "", "", fmt.Errorf("build current installer ISO: %w", err)
		}
	}
	iso := filepath.Join(manager.repoRoot, "_build", "mkosi", "katl-installer.iso")
	digest, err := sha256File(iso)
	if err != nil {
		if noBuild && errors.Is(err, os.ErrNotExist) {
			return "", "", fmt.Errorf("existing installer ISO is unavailable; rerun without --no-build: %w", err)
		}
		return "", "", fmt.Errorf("read installer ISO: %w", err)
	}
	return iso, digest, nil
}

// createNode defines and boots a fresh installer VM for node, replacing any
// domain of the same name that this world owns.
func (manager installerManager) createNode(ctx context.Context, world katldevWorld, node vmtest.Node, opts installerOptions, iso, digest string) (installerState, error) {
	result, err := world.Scenario.PlanNodeVM(node, []vmtest.DiskFixture{vmtest.TargetDisk("root", string(vmtest.DiskQCOW2), opts.DiskSize)}, vmtest.KVMAuto)
	if err != nil {
		return installerState{}, fmt.Errorf("plan VM %s: %w", node.Name, err)
	}
	uri := world.World.Libvirt.URI
	config := vmtest.VMConfig{
		Boot:             vmtest.VMBoot{ISO: iso, DiskFirst: true},
		VirshPath:        firstNonEmpty(os.Getenv("KATL_VMTEST_VIRSH"), "virsh"),
		ImageTool:        firstNonEmpty(os.Getenv("KATL_VMTEST_IMAGE_TOOL"), "qemu-img"),
		LibvirtURI:       uri,
		LibvirtNetwork:   world.World.Network.Name,
		OVMFCode:         os.Getenv("KATL_OVMF_CODE"),
		OVMFVars:         os.Getenv("KATL_OVMF_VARS"),
		PreserveNVRAM:    true,
		KVM:              vmtest.KVMAuto,
		RAMMiB:           opts.MemoryMiB,
		CPUs:             opts.CPUs,
		Phase:            "installer",
		Network:          vmtest.VMNetworkConfig{Mode: vmtest.VMNetworkUser, MAC: node.MACAddress},
		DomainMetadata:   world.owner(),
		PersistentSerial: true,
		HotplugPCISlots:  3,
	}
	plan, err := vmtest.PlanVM(result, config)
	if err != nil {
		return installerState{}, fmt.Errorf("plan libvirt VM %s: %w", node.Name, err)
	}
	if err := manager.removeDomainIfPresent(ctx, uri, plan.DomainName, world.owner()); err != nil {
		return installerState{}, err
	}
	if err := os.RemoveAll(node.DiskDir); err != nil {
		return installerState{}, err
	}
	if err := os.MkdirAll(node.DiskDir, 0o755); err != nil {
		return installerState{}, err
	}
	if err := vmtest.CreateDisks(ctx, vmtest.ExecDiskRunner{}, result.Disks); err != nil {
		return installerState{}, fmt.Errorf("create VM %s disk: %w", node.Name, err)
	}
	if err := vmtest.PrepareVM(plan, config); err != nil {
		return installerState{}, fmt.Errorf("prepare libvirt VM %s: %w", node.Name, err)
	}
	serial, err := os.OpenFile(plan.SerialLog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return installerState{}, fmt.Errorf("prepare persistent serial log: %w", err)
	}
	if err := serial.Close(); err != nil {
		return installerState{}, fmt.Errorf("prepare persistent serial log: %w", err)
	}
	if _, err := manager.virsh(ctx, uri, "define", plan.DomainXMLFile); err != nil {
		return installerState{}, err
	}
	if _, err := manager.virsh(ctx, uri, "start", plan.DomainName); err != nil {
		return installerState{}, err
	}
	revision, dirty := checkoutRevision(manager.repoRoot)
	state := installerState{
		APIVersion:   installerStateAPIVersion,
		Kind:         installerStateKind,
		RepoRoot:     manager.repoRoot,
		World:        world.World.RunID,
		Node:         node.Name,
		Revision:     revision,
		Dirty:        dirty,
		DomainName:   plan.DomainName,
		LibvirtURI:   uri,
		Network:      world.World.Network.Name,
		MACAddress:   node.MACAddress,
		InstallerISO: iso,
		ISOSHA256:    digest,
		TargetDisk:   result.Disks[0].HostPath,
		SerialLog:    plan.SerialLog,
		DomainXML:    plan.DomainXMLFile,
		MemoryMiB:    opts.MemoryMiB,
		CPUs:         opts.CPUs,
		DiskSize:     opts.DiskSize,
		StartedAt:    time.Now().UTC(),
	}
	if err := manager.writeState(node, state); err != nil {
		return installerState{}, err
	}
	return state, nil
}

// resume powers on a recorded VM; report waits for it to answer.
func (manager installerManager) resume(ctx context.Context, state installerState) error {
	domainState, err := manager.domainState(ctx, state.LibvirtURI, state.DomainName)
	if err != nil {
		return err
	}
	switch domainState {
	case "running", "idle", "in shutdown":
		return nil
	case "paused":
		_, err = manager.virsh(ctx, state.LibvirtURI, "resume", state.DomainName)
	case "shut off", "crashed", "pmsuspended":
		_, err = manager.virsh(ctx, state.LibvirtURI, "start", state.DomainName)
	default:
		return fmt.Errorf("VM %s is in unsupported state %q", state.DomainName, domainState)
	}
	return err
}

// nodeObservation is one read-only look at a world VM.
type nodeObservation struct {
	Node        vmtest.Node
	State       installerState
	Exists      bool
	DomainState string
	// Installer is the installer API state, empty when it does not answer.
	Installer string
	// KatlOS reports that the KatlOS management API answers instead.
	KatlOS bool
	// ConfigNode names the cluster.yaml node at the observed address.
	ConfigNode string
}

func (observation nodeObservation) waiting() bool {
	return installerAcceptingConfig(observation.Installer)
}

func (observation nodeObservation) reachable() bool {
	return observation.Installer != "" || observation.KatlOS
}

func (observation nodeObservation) summary() string {
	switch {
	case !observation.Exists:
		return "missing"
	case observation.Installer != "":
		return "installer " + observation.Installer
	case observation.KatlOS:
		return "KatlOS management API reachable"
	case observation.DomainState == "running":
		return "unreachable"
	default:
		return observation.DomainState
	}
}

// observe reads the VM's domain, DHCP lease, and APIs. When the VM answers at
// a new address, the recorded state follows it.
func (manager installerManager) observe(ctx context.Context, world katldevWorld, node vmtest.Node) (nodeObservation, error) {
	observation := nodeObservation{Node: node}
	state, err := manager.readState(node)
	if errors.Is(err, os.ErrNotExist) {
		return observation, nil
	}
	if err != nil {
		return observation, err
	}
	observation.State = state
	observation.Exists, err = manager.domainExists(ctx, state.LibvirtURI, state.DomainName)
	if err != nil || !observation.Exists {
		return observation, err
	}
	if err := manager.requireOwnedDomain(ctx, state.LibvirtURI, state.DomainName, world.owner()); err != nil {
		return observation, err
	}
	if observation.DomainState, err = manager.domainState(ctx, state.LibvirtURI, state.DomainName); err != nil {
		return observation, err
	}
	if observation.DomainState != "running" {
		return observation, nil
	}

	addresses := []string{}
	if lease, err := vmtest.DiscoverLibvirtLease(ctx, firstNonEmpty(os.Getenv("KATL_VMTEST_VIRSH"), "virsh"), state.LibvirtURI, state.Network, state.MACAddress); err == nil {
		addresses = append(addresses, lease.IPAddress)
	}
	addresses = append(addresses, state.IPAddress, node.Address)
	for _, address := range addresses {
		if address == "" {
			continue
		}
		endpoint := "http://" + net.JoinHostPort(address, "8080")
		if current, err := manager.installerStatus(ctx, endpoint); err == nil {
			observation.Installer = current
		} else if tcpReachable(ctx, net.JoinHostPort(address, "9443"), 500*time.Millisecond) {
			observation.KatlOS = true
		} else {
			continue
		}
		if state.IPAddress != address || state.Endpoint != endpoint {
			state.IPAddress, state.Endpoint = address, endpoint
			if err := manager.writeState(node, state); err != nil {
				return observation, err
			}
		}
		observation.State = state
		break
	}
	return observation, nil
}

// waitReachable waits for a running VM to answer as an installer or KatlOS.
func (manager installerManager) waitReachable(ctx context.Context, world katldevWorld, node vmtest.Node, timeout time.Duration) (nodeObservation, error) {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		observation, err := manager.observe(waitCtx, world, node)
		if err == nil && (observation.reachable() || !observation.Exists) {
			return observation, nil
		}
		if err != nil && waitCtx.Err() == nil {
			return observation, err
		}
		select {
		case <-waitCtx.Done():
			return observation, fmt.Errorf("VM %s is running%s but neither the installer nor KatlOS management API became reachable within %s; inspect %s or run katldev world console %s", node.Name, addressDetail(firstNonEmpty(observation.State.IPAddress, node.Address)), timeout, observation.State.SerialLog, node.Name)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func addressDetail(address string) string {
	if strings.TrimSpace(address) == "" {
		return " without a discovered address"
	}
	return " at " + address
}

func installerAcceptingConfig(state string) bool {
	switch strings.TrimSpace(state) {
	case "waiting", "waiting-for-config":
		return true
	default:
		return false
	}
}

func tcpReachable(ctx context.Context, address string, timeout time.Duration) bool {
	dialer := net.Dialer{Timeout: timeout}
	connection, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return false
	}
	_ = connection.Close()
	return true
}

func (manager installerManager) installerStatus(ctx context.Context, endpoint string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(endpoint, "/")+"/v1/status", nil)
	if err != nil {
		return "", err
	}
	client := manager.client
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("installer status returned %s", response.Status)
	}
	var status struct {
		State string `json:"state"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&status); err != nil {
		return "", err
	}
	if strings.TrimSpace(status.State) == "" {
		return "", errors.New("installer status has no state")
	}
	return strings.TrimSpace(status.State), nil
}

func (manager installerManager) status(ctx context.Context) error {
	manager.warnHostShutdown()
	world, err := manager.requireWorld()
	if err != nil {
		return err
	}
	observations := make([]nodeObservation, 0, len(world.Scenario.Nodes))
	for _, node := range world.Scenario.Nodes {
		observation, err := manager.observe(ctx, world, node)
		if err != nil {
			return err
		}
		observations = append(observations, observation)
	}
	manager.labelConfigNodes(observations)
	manager.printWorld(world, observations)
	fmt.Fprintln(manager.stdout)
	manager.printFooter()
	return nil
}

func (manager installerManager) clusterConfigPath() string {
	return filepath.Join(manager.repoRoot, "_build", "katldev", "cluster.yaml")
}

func (manager installerManager) labelConfigNodes(observations []nodeObservation) {
	for i := range observations {
		address := firstNonEmpty(observations[i].State.IPAddress, observations[i].Node.Address)
		if name, err := matchingKatldevConfig(manager.clusterConfigPath(), address); err == nil {
			observations[i].ConfigNode = name
		}
	}
}

// report waits for every VM to answer, keeps one cluster.yaml for the world,
// and prints how to install waiting VMs. Only a new or forced config is
// generated; nodes added later go through the operator's add-node procedure.
func (manager installerManager) report(ctx context.Context, world katldevWorld, opts installerOptions, added []string) error {
	manager.warnHostShutdown()
	observations := make([]nodeObservation, 0, len(world.Scenario.Nodes))
	for _, node := range world.Scenario.Nodes {
		observation, err := manager.waitReachable(ctx, world, node, opts.Timeout)
		if err != nil {
			return err
		}
		observations = append(observations, observation)
	}
	configStatus, err := manager.ensureClusterConfig(ctx, observations, opts.ForceConfig)
	if err != nil {
		return err
	}
	manager.labelConfigNodes(observations)

	fmt.Fprintln(manager.stdout, "KatlOS world is ready.")
	manager.printWorld(world, observations)
	fmt.Fprintf(manager.stdout, "\nCluster config: %s (%s)\n", manager.clusterConfigPath(), configStatus)
	for _, observation := range observations {
		if !observation.waiting() {
			continue
		}
		fmt.Fprintln(manager.stdout)
		if observation.ConfigNode != "" {
			fmt.Fprintf(manager.stdout, "Install %s:\n", observation.Node.Name)
			fmt.Fprintf(manager.stdout, "  katlctl install apply --config %s --node %s\n", manager.clusterConfigPath(), observation.ConfigNode)
			continue
		}
		manager.printAddNode(observation)
	}
	for _, name := range added {
		if observation := findObservation(observations, name); observation != nil && !observation.waiting() {
			fmt.Fprintf(manager.stdout, "\n%s is %s rather than waiting for configuration.\n", name, observation.summary())
		}
	}
	fmt.Fprintln(manager.stdout)
	manager.printFooter()
	return nil
}

func findObservation(observations []nodeObservation, name string) *nodeObservation {
	for i := range observations {
		if observations[i].Node.Name == name {
			return &observations[i]
		}
	}
	return nil
}

func (manager installerManager) printWorld(world katldevWorld, observations []nodeObservation) {
	fmt.Fprintf(manager.stdout, "World: %s\n", world.World.RunID)
	fmt.Fprintf(manager.stdout, "Network: %s (%s)\n", world.World.Network.Name, world.World.Network.CIDR)
	for _, observation := range observations {
		node := observation.Node
		fmt.Fprintf(manager.stdout, "\n%s: %s\n", node.Name, observation.summary())
		if observation.State.DomainName != "" {
			fmt.Fprintf(manager.stdout, "  VM: %s (%s)\n", observation.State.DomainName, firstNonEmpty(observation.DomainState, "absent"))
		}
		address := firstNonEmpty(observation.State.IPAddress, "not observed")
		if node.Address != observation.State.IPAddress {
			address += ", reserved " + node.Address + " for its next boot"
		}
		fmt.Fprintf(manager.stdout, "  Address: %s\n", address)
		if observation.Installer != "" {
			fmt.Fprintf(manager.stdout, "  Endpoint: %s\n", observation.State.Endpoint)
		}
		fmt.Fprintf(manager.stdout, "  Cluster node: %s\n", firstNonEmpty(observation.ConfigNode, "not in cluster.yaml"))
		if observation.State.Revision != "" {
			fmt.Fprintf(manager.stdout, "  Installer checkout: %s%s\n", observation.State.Revision, dirtySuffix(observation.State.Dirty))
		}
		if observation.State.SerialLog != "" {
			fmt.Fprintf(manager.stdout, "  Serial log: %s\n", observation.State.SerialLog)
		}
	}
}

func (manager installerManager) printAddNode(observation nodeObservation) {
	node := observation.Node
	address := firstNonEmpty(observation.State.IPAddress, node.Address)
	fmt.Fprintf(manager.stdout, "Add %s to cluster.yaml (docs/operations/change-cluster-nodes.md), for example:\n", node.Name)
	fmt.Fprintf(manager.stdout, "  - name: %s\n", node.Name)
	if node.Role == vmtest.ControlPlane {
		fmt.Fprintln(manager.stdout, "    controlPlane: true")
	}
	fmt.Fprintln(manager.stdout, "    install:")
	fmt.Fprintln(manager.stdout, "      systemDisk:")
	fmt.Fprintf(manager.stdout, "        byID: %s\n", katldevTargetDisk)
	fmt.Fprintln(manager.stdout, "    management:")
	fmt.Fprintf(manager.stdout, "      address: %s\n", address)
	fmt.Fprintln(manager.stdout, "Then validate, install, and join it to the bootstrapped cluster:")
	fmt.Fprintf(manager.stdout, "  katlctl config validate %s\n", manager.clusterConfigPath())
	fmt.Fprintf(manager.stdout, "  katlctl install apply --config %s --node %s\n", manager.clusterConfigPath(), node.Name)
	fmt.Fprintf(manager.stdout, "  katlctl node join %s --config %s\n", node.Name, manager.clusterConfigPath())
}

func (manager installerManager) printFooter() {
	fmt.Fprintln(manager.stdout, "Console: katldev world console NODE")
	fmt.Fprintln(manager.stdout, "Add VM:  katldev world add-node [--control-plane]")
	fmt.Fprintln(manager.stdout, "Reset:   katldev world reset")
}

func (manager installerManager) ensureClusterConfig(ctx context.Context, observations []nodeObservation, force bool) (string, error) {
	configPath := manager.clusterConfigPath()
	var waiting []string
	listed := false
	for _, observation := range observations {
		address := firstNonEmpty(observation.State.IPAddress, observation.Node.Address)
		if observation.waiting() {
			waiting = append(waiting, observation.State.Endpoint)
		}
		if _, err := matchingKatldevConfig(configPath, address); err == nil {
			listed = true
		} else if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, errNodeUnlisted) {
			return "", fmt.Errorf("read ClusterConfig %s: %w", configPath, err)
		}
	}
	_, statErr := os.Stat(configPath)
	switch {
	case statErr != nil && !errors.Is(statErr, os.ErrNotExist):
		return "", statErr
	case !force && statErr == nil && listed:
		return "existing", nil
	case !force && statErr == nil && len(waiting) > 0:
		return "", fmt.Errorf("the world's installers are ready, but existing ClusterConfig %s lists none of its VMs; rerun with --force-config to replace it", configPath)
	case !force && statErr == nil:
		return "existing", nil
	case len(waiting) == 0:
		return "not created: no VM is waiting for configuration", nil
	}

	args := []string{"run", "./cmd/katlctl", "config", "init", configPath}
	for _, endpoint := range waiting {
		args = append(args, "--installer", endpoint)
	}
	if force {
		args = append(args, "--force")
	}
	run := manager.run
	if run == nil {
		run = runBuildCommand
	}
	if err := run(ctx, manager.repoRoot, "go", args, nil, manager.stdout, manager.stderr); err != nil {
		return "", fmt.Errorf("generate ClusterConfig for waiting installers: %w", err)
	}
	if _, err := os.Stat(configPath); err != nil {
		return "", fmt.Errorf("katlctl did not create ClusterConfig %s: %w", configPath, err)
	}
	return "created", nil
}

var errNodeUnlisted = errors.New("no node uses this address and the katldev target disk")

// matchingKatldevConfig returns the config node that uses address and the
// katldev target disk.
func matchingKatldevConfig(path, address string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	source, err := configbundle.DecodeSource(file)
	if err != nil {
		return "", fmt.Errorf("decode config: %w", err)
	}
	for _, node := range source.Spec.Nodes {
		if node.Management.Address != address {
			continue
		}
		layer, err := configbundle.ResolveNodeLayer(source.Spec.Defaults, node)
		if err != nil {
			return "", fmt.Errorf("resolve node %s: %w", node.Name, err)
		}
		if layer.Install.TargetDisk != nil && layer.Install.TargetDisk.ByID == katldevTargetDisk {
			return node.Name, nil
		}
	}
	return "", fmt.Errorf("%w: %s", errNodeUnlisted, address)
}

func (manager installerManager) stop(ctx context.Context, names []string, timeout time.Duration, force bool) error {
	if timeout <= 0 {
		return errors.New("--timeout must be positive")
	}
	world, err := manager.requireWorld()
	if err != nil {
		return err
	}
	nodes := world.Scenario.Nodes
	if len(names) > 0 {
		nodes = nil
		for _, name := range names {
			node, err := manager.worldNode(world, name)
			if err != nil {
				return err
			}
			nodes = append(nodes, node)
		}
	}
	for _, node := range nodes {
		state, err := manager.readState(node)
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(manager.stdout, "%s has no VM.\n", node.Name)
			continue
		}
		if err != nil {
			return err
		}
		if err := manager.stopDomain(ctx, state, world.owner(), timeout, force); err != nil {
			return err
		}
	}
	return nil
}

func (manager installerManager) stopDomain(ctx context.Context, state installerState, owner string, timeout time.Duration, force bool) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	exists, err := manager.domainExists(ctx, state.LibvirtURI, state.DomainName)
	if err != nil {
		return err
	}
	if !exists {
		fmt.Fprintf(manager.stdout, "VM %s is already absent.\n", state.DomainName)
		return nil
	}
	if err := manager.requireOwnedDomain(ctx, state.LibvirtURI, state.DomainName, owner); err != nil {
		return err
	}
	domainState, err := manager.domainState(ctx, state.LibvirtURI, state.DomainName)
	if err != nil {
		return err
	}
	if domainState != "shut off" && domainState != "crashed" {
		if force {
			if _, err := manager.virsh(ctx, state.LibvirtURI, "destroy", state.DomainName); err != nil {
				return err
			}
			fmt.Fprintf(manager.stdout, "Cut power to %s. Resume it with katldev world start.\n", state.DomainName)
			return nil
		}
		switch domainState {
		case "running", "idle":
			// ACPI lets logind honor kubelet's delay inhibitor before service teardown.
			if _, err := manager.virsh(ctx, state.LibvirtURI, "shutdown", state.DomainName, "--mode", "acpi"); err != nil {
				return err
			}
		case "in shutdown":
		default:
			return fmt.Errorf("VM %s is %s; run katldev world start to resume it before stopping, or use stop --force to cut power", state.DomainName, domainState)
		}

		fmt.Fprintf(manager.stderr, "katldev world: waiting up to %s for %s to shut down\n", timeout, state.DomainName)
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for domainState != "shut off" {
			select {
			case <-ctx.Done():
				err = ctx.Err()
			case <-ticker.C:
				domainState, err = manager.domainState(ctx, state.LibvirtURI, state.DomainName)
			}
			if err != nil {
				return fmt.Errorf("shutdown of %s was not confirmed: %w; inspect katldev world console %s and status, then retry stop with a longer --timeout or use --force to cut power", state.DomainName, err, state.Node)
			}
			if domainState == "crashed" {
				return fmt.Errorf("VM %s crashed during shutdown; inspect katldev world console %s", state.DomainName, state.Node)
			}
		}
	}
	fmt.Fprintf(manager.stdout, "Stopped %s. Resume it with katldev world start or wipe the world with katldev world reset.\n", state.DomainName)
	return nil
}

func (manager installerManager) console(ctx context.Context, name string) error {
	world, err := manager.requireWorld()
	if err != nil {
		return err
	}
	node, err := manager.worldNode(world, name)
	if err != nil {
		return err
	}
	state, err := manager.readState(node)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%s has no VM; run katldev world start", node.Name)
		}
		return err
	}
	if err := manager.requireOwnedDomain(ctx, state.LibvirtURI, state.DomainName, world.owner()); err != nil {
		return err
	}
	virsh := firstNonEmpty(os.Getenv("KATL_VMTEST_VIRSH"), "virsh")
	command := exec.CommandContext(ctx, virsh, "-c", state.LibvirtURI, "console", state.DomainName, "--force")
	command.Stdin = os.Stdin
	command.Stdout = manager.stdout
	command.Stderr = manager.stderr
	return command.Run()
}

func (manager installerManager) removeDomainIfPresent(ctx context.Context, uri, domain, owner string) error {
	exists, err := manager.domainExists(ctx, uri, domain)
	if err != nil || !exists {
		return err
	}
	return manager.removeDomain(ctx, uri, domain, owner)
}

// removeDomain destroys and undefines a domain only after confirming owner.
func (manager installerManager) removeDomain(ctx context.Context, uri, domain, owner string) error {
	if err := manager.requireOwnedDomain(ctx, uri, domain, owner); err != nil {
		return err
	}
	domainState, err := manager.domainState(ctx, uri, domain)
	if err != nil {
		return err
	}
	if domainState != "shut off" && domainState != "crashed" {
		if _, err := manager.virsh(ctx, uri, "destroy", domain); err != nil {
			return err
		}
	}
	hasManagedSave, err := manager.domainHasManagedSave(ctx, uri, domain)
	if err != nil {
		return err
	}
	if hasManagedSave {
		if _, err := manager.virsh(ctx, uri, "managedsave-remove", domain); err != nil {
			return err
		}
	}
	_, err = manager.virsh(ctx, uri, "undefine", domain, "--nvram")
	return err
}

func (manager installerManager) domainHasManagedSave(ctx context.Context, uri, domain string) (bool, error) {
	output, err := manager.virsh(ctx, uri, "dominfo", domain)
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(output), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(strings.TrimSpace(key), "Managed save") {
			return strings.EqualFold(strings.TrimSpace(value), "yes"), nil
		}
	}
	return false, nil
}

func (manager installerManager) domainExists(ctx context.Context, uri, domain string) (bool, error) {
	output, err := manager.virsh(ctx, uri, "list", "--all", "--name")
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(output), "\n") {
		if strings.TrimSpace(line) == domain {
			return true, nil
		}
	}
	return false, nil
}

func (manager installerManager) domainState(ctx context.Context, uri, domain string) (string, error) {
	output, err := manager.virsh(ctx, uri, "domstate", domain)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

func (manager installerManager) requireOwnedDomain(ctx context.Context, uri, domain, owner string) error {
	output, err := manager.virsh(ctx, uri, "metadata", domain, "--uri", vmtestMetadataURI)
	if err != nil {
		return fmt.Errorf("refusing to manage libvirt domain %s without katldev ownership metadata: %w", domain, err)
	}
	got, err := domainOwner(output)
	if err != nil {
		return fmt.Errorf("decode libvirt domain %s ownership metadata: %w", domain, err)
	}
	if got != owner {
		return fmt.Errorf("refusing to manage libvirt domain %s owned by %q", domain, got)
	}
	return nil
}

func domainOwner(data []byte) (string, error) {
	var metadata struct {
		Value string `xml:",chardata"`
	}
	if err := xml.Unmarshal(data, &metadata); err != nil {
		return "", err
	}
	return strings.TrimSpace(metadata.Value), nil
}

func (manager installerManager) virsh(ctx context.Context, uri string, args ...string) ([]byte, error) {
	if manager.runVirsh != nil {
		return manager.runVirsh(ctx, uri, args...)
	}
	virsh := firstNonEmpty(os.Getenv("KATL_VMTEST_VIRSH"), "virsh")
	fullArgs := []string{"-c", uri}
	fullArgs = append(fullArgs, args...)
	output, err := exec.CommandContext(ctx, virsh, fullArgs...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("virsh %s: %w: %s", strings.Join(fullArgs, " "), err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

func (manager installerManager) statePath(node vmtest.Node) string {
	return filepath.Join(node.ArtifactDir, "state.json")
}

func (manager installerManager) readState(node vmtest.Node) (installerState, error) {
	path := manager.statePath(node)
	data, err := os.ReadFile(path)
	if err != nil {
		return installerState{}, err
	}
	var state installerState
	if err := json.Unmarshal(data, &state); err != nil {
		return installerState{}, fmt.Errorf("decode %s: %w", path, err)
	}
	if state.APIVersion != installerStateAPIVersion || state.Kind != installerStateKind || state.RepoRoot != manager.repoRoot ||
		state.World != worldRunID(manager.repoRoot) || state.Node != node.Name || state.DomainName == "" {
		return installerState{}, fmt.Errorf("%s is not valid state for VM %s of this checkout's world", path, node.Name)
	}
	return state, nil
}

func (manager installerManager) writeState(node vmtest.Node, state installerState) error {
	return writeJSONFile(manager.statePath(node), state)
}

func (manager installerManager) legacyStateRoot() string {
	return filepath.Join(manager.repoRoot, "_build", "katldev", "installer")
}

// readLegacyState reads the single-VM state that katldev wrote before worlds.
func (manager installerManager) readLegacyState() (installerState, error) {
	path := filepath.Join(manager.legacyStateRoot(), "state.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return installerState{}, err
	}
	var state installerState
	if err := json.Unmarshal(data, &state); err != nil {
		return installerState{}, fmt.Errorf("decode %s: %w", path, err)
	}
	if state.Kind != legacyStateKind || state.RepoRoot != manager.repoRoot || state.DomainName == "" {
		return installerState{}, fmt.Errorf("%s is not valid state for this checkout", path)
	}
	return state, nil
}

func writeJSONFile(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func sha256File(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func checkoutRevision(repoRoot string) (string, bool) {
	revisionOutput, revisionErr := exec.Command("git", "-C", repoRoot, "rev-parse", "--short=12", "HEAD").Output()
	statusOutput, statusErr := exec.Command("git", "-C", repoRoot, "status", "--porcelain", "--untracked-files=no").Output()
	revision := ""
	if revisionErr == nil {
		revision = strings.TrimSpace(string(revisionOutput))
	}
	return revision, statusErr == nil && strings.TrimSpace(string(statusOutput)) != ""
}

func dirtySuffix(dirty bool) string {
	if dirty {
		return " (with local changes)"
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
