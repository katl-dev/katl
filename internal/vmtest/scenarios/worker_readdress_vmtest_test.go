package scenarios

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/installer/configbundle"
	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/vmtest"
	"gopkg.in/yaml.v3"
)

// runWorkerReaddress moves the joined worker from its DHCP address to a free
// static address through the public cluster apply and node reboot workflow.
// It observes the result through SSH, the vmtest agent, and Kubernetes rather
// than through katlctl's own report.
func runWorkerReaddress(t *testing.T, ctx context.Context, smoke operationBackedSmokeRun, contextPath string, nodes []vmtest.RunningInstalledRuntimeNode, addresses map[string]string, kubeconfig string, bundle threeControlPlaneKubernetesPayloadBundle, cniFixtures map[string]nodeCNIFixture) error {
	t.Helper()
	dir := filepath.Join(smoke.Result.RunDir, "worker-readdress")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	oldAddress := addresses["worker-1"]
	network := smoke.WorldScenario.World.Network
	newAddress, prefix, err := freeAddress(ctx, smoke.WorldScenario.World.Libvirt, network, addresses)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "addresses.txt"), []byte(fmt.Sprintf("old=%s\nnew=%s/%d\ngateway=%s\n", oldAddress, newAddress, prefix, network.Gateway)), 0o600); err != nil {
		return err
	}

	static := fmt.Sprintf("[Match]\nType=ether\nKind=!*\n\n[Link]\nRequiredForOnline=routable\n\n[Network]\nAddress=%s/%d\nGateway=%s\nDNS=%s\n", newAddress, prefix, network.Gateway, network.Gateway)
	configPath, err := writeWorkerReaddressConfig(smoke, dir, bundle, func(node *configbundle.SourceNode) {
		node.Management.Address = newAddress
		sets, _ := node.HostConfiguration.FileSets.Get()
		next := map[string]configbundle.SourceHostConfigurationFileSet{}
		for name, set := range sets {
			next[name] = set
		}
		// Replace the inherited DHCP profile of the same set name.
		next["vmtest-network"] = configbundle.SourceHostConfigurationFileSet{Files: []manifest.HostConfigurationFile{{Path: "/etc/systemd/network/80-katl-vmtest-static.network", Content: &static}}}
		node.HostConfiguration.FileSets = configbundle.Some(next)
	})
	if err != nil {
		return err
	}

	before, err := readTwoNodeEnrollments(ctx, addresses["cp-1"], oldAddress)
	if err != nil {
		return err
	}
	katlctl := buildKatlctlCommand(t, ctx, katlRepoRoot(t), contextPath)
	run := func(name string, args ...string) ([]byte, error) {
		stdout, stderr, err := runProofKatlctl(ctx, katlctl, dir, name, args...)
		if err != nil {
			return stdout, fmt.Errorf("%s: %w: %s", name, err, stderr)
		}
		return stdout, nil
	}

	// A live re-address would cut the management session mid-operation.
	if _, stderr, err := runProofKatlctl(ctx, katlctl, dir, "refuse-live", "cluster", "apply", "--config", configPath, "--node", "worker-1", "--endpoint", oldAddress, "--mode", "live"); err == nil || !strings.Contains(stderr, "requires a reboot") {
		return fmt.Errorf("live re-address was not refused: %v: %s", err, stderr)
	}

	stdout, err := run("stage", "cluster", "apply", "--config", configPath, "--node", "worker-1", "--endpoint", oldAddress, "-o", "json")
	if err != nil {
		return err
	}
	var staged struct {
		Result         string   `json:"result"`
		RebootRequired bool     `json:"rebootRequired"`
		StagedNodes    []string `json:"stagedNodes"`
		AddressChange  struct {
			Node, Current, Desired string
		} `json:"addressChange"`
	}
	if err := json.Unmarshal(stdout, &staged); err != nil {
		return err
	}
	if staged.Result != "succeeded" || !staged.RebootRequired || len(staged.StagedNodes) != 1 || staged.AddressChange.Current != oldAddress || staged.AddressChange.Desired != newAddress {
		return fmt.Errorf("unexpected staged re-address report: %s", stdout)
	}
	if output, err := readKubeletNode(ctx, smoke.Inputs.SSHPrivateKey, oldAddress, "worker-1", "ip -4 -o addr show scope global"); err != nil || strings.Contains(string(output), newAddress+"/") {
		return fmt.Errorf("staging changed the running network: %v: %s", err, output)
	}

	stdout, err = run("reboot", "node", "reboot", "worker-1", "--config", configPath, "--endpoint", oldAddress, "--timeout", "15m", "-o", "json")
	if err != nil {
		return err
	}
	var rebooted struct {
		Result, Endpoint string
		AddressChanged   bool `json:"addressChanged"`
	}
	if err := json.Unmarshal(stdout, &rebooted); err != nil {
		return err
	}
	if rebooted.Result != "rebooted" || !rebooted.AddressChanged || rebooted.Endpoint != net.JoinHostPort(newAddress, "9443") {
		return fmt.Errorf("unexpected re-address reboot report: %s", stdout)
	}

	// Independent observations: the host network, the old endpoint, the
	// installation identity, and Kubernetes' view of the node.
	worker := nodeByName(nodes, "worker-1")
	result, err := runNodeCommand(ctx, worker, []string{"ip", "-4", "-o", "addr", "show", "scope", "global"}, 16<<10)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "worker-addresses.txt"), result.Stdout, 0o600); err != nil {
		return err
	}
	if !strings.Contains(string(result.Stdout), newAddress+"/") || strings.Contains(string(result.Stdout), oldAddress+"/") {
		return fmt.Errorf("worker host addresses after re-address: %s", result.Stdout)
	}
	if conn, err := net.DialTimeout("tcp", net.JoinHostPort(oldAddress, "9443"), 3*time.Second); err == nil {
		_ = conn.Close()
		return fmt.Errorf("the previous worker address still serves the management API")
	}
	after, err := readTwoNodeEnrollments(ctx, addresses["cp-1"], newAddress)
	if err != nil {
		return err
	}
	if after["worker-1"].GetMachineId() != before["worker-1"].GetMachineId() || after["worker-1"].GetEnrollmentId() != before["worker-1"].GetEnrollmentId() {
		return fmt.Errorf("worker identity changed across re-address")
	}
	if err := waitForNodeInternalIP(ctx, kubeconfig, dir, "worker-1", newAddress, 5*time.Minute); err != nil {
		return err
	}
	// The API server reaches kubelet at the Node's reported address.
	configz, err := kubectlOutput(ctx, kubeconfig, "get", "--raw", "/api/v1/nodes/worker-1/proxy/configz")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "worker-configz.json"), configz, 0o600); err != nil {
		return err
	}

	// Restore the test CNI's static pod routes for the new node address.
	cp := cniFixtures["cp-1"]
	cp.PeerAddress = newAddress
	cniFixtures["cp-1"] = cp
	for _, node := range nodes {
		if err := activateNodeCNIFixture(ctx, node, cniFixtures[node.Name]); err != nil {
			return fmt.Errorf("reactivate %s CNI fixture after re-address: %w", node.Name, err)
		}
	}

	// The saved context still names the old endpoint until it is rebound.
	if _, err := run("rebind", "context", "rebind", "--node", "worker-1", "--endpoint", newAddress); err != nil {
		return err
	}
	stdout, err = run("converge", "cluster", "apply", "--config", configPath, "--node", "worker-1", "-o", "json")
	if err != nil {
		return err
	}
	var converged struct {
		Result         string `json:"result"`
		RebootRequired bool   `json:"rebootRequired"`
		NodePlans      []struct {
			NoChanges bool `json:"noChanges"`
		} `json:"nodePlans"`
	}
	if err := json.Unmarshal(stdout, &converged); err != nil {
		return err
	}
	if converged.Result != "succeeded" || converged.RebootRequired || len(converged.NodePlans) != 1 || !converged.NodePlans[0].NoChanges {
		return fmt.Errorf("apply at the new address did not converge without changes: %s", stdout)
	}
	addresses["worker-1"] = newAddress
	return nil
}

// writeWorkerReaddressConfig writes dir/cluster.katlcfg from the configuration
// last applied to both nodes with move applied to worker-1, so the move is the
// only desired change.
func writeWorkerReaddressConfig(smoke operationBackedSmokeRun, dir string, bundle threeControlPlaneKubernetesPayloadBundle, move func(*configbundle.SourceNode)) (string, error) {
	applied := filepath.Join(smoke.Result.RunDir, "pre-cni-apply")
	for _, name := range []string{"cluster.yaml", "kubeadm.yaml", "kubelet.yaml"} {
		if err := copyFile(filepath.Join(applied, name), filepath.Join(dir, name)); err != nil {
			return "", err
		}
	}
	if err := copyDir(filepath.Join(applied, "patches"), filepath.Join(dir, "patches")); err != nil {
		return "", err
	}
	data, err := os.ReadFile(filepath.Join(dir, "cluster.yaml"))
	if err != nil {
		return "", err
	}
	source, err := configbundle.DecodeSource(bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	index := slices.IndexFunc(source.Spec.Nodes, func(node configbundle.SourceNode) bool { return node.Name == "worker-1" })
	if index < 0 {
		return "", fmt.Errorf("applied configuration does not describe worker-1")
	}
	move(&source.Spec.Nodes[index])
	data, err = yaml.Marshal(source)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "cluster.yaml"), data, 0o600); err != nil {
		return "", err
	}

	management, err := vmtest.VMTestManagementPlanning(vmtest.VMTestManagementClusterName, []string{"cp-1", "worker-1"})
	if err != nil {
		return "", err
	}
	configPath := filepath.Join(dir, "cluster.katlcfg")
	if _, err := configbundle.WriteArchive(configPath, configbundle.BuildRequest{SourcePath: filepath.Join(dir, "cluster.yaml"), Planning: configbundle.PlanningInputs{KubernetesBundle: bundle.Ref, ManagementIdentities: management}}); err != nil {
		return "", err
	}
	return configPath, nil
}

// freeAddress chooses an address in the test network that no lease or host
// currently uses, for a static address or a DHCP reservation. Libvirt's
// dnsmasq probes an address before leasing it, so a configured static
// address is not handed out later.
func freeAddress(ctx context.Context, libvirt vmtest.WorldLibvirt, network vmtest.WorldNetwork, used map[string]string) (string, int, error) {
	prefix, err := netip.ParsePrefix(strings.TrimSpace(network.CIDR))
	if err != nil {
		return "", 0, fmt.Errorf("test network CIDR %q: %w", network.CIDR, err)
	}
	prefix = prefix.Masked()
	args := []string{"net-dhcp-leases", network.Name}
	if uri := strings.TrimSpace(libvirt.URI); uri != "" {
		args = append([]string{"-c", uri}, args...)
	}
	leases, err := exec.CommandContext(ctx, "virsh", args...).CombinedOutput()
	if err != nil {
		return "", 0, fmt.Errorf("virsh %s: %w: %s", strings.Join(args, " "), err, leases)
	}
	taken := map[string]bool{network.Gateway: true}
	for _, address := range used {
		taken[address] = true
	}
	last := prefix.Addr()
	for i := 0; i < 1<<(prefix.Addr().BitLen()-prefix.Bits())-2; i++ {
		last = last.Next()
	}
	for candidate := last; prefix.Contains(candidate) && candidate != prefix.Addr(); candidate = candidate.Prev() {
		address := candidate.String()
		if taken[address] || strings.Contains(string(leases), " "+address+"/") {
			continue
		}
		probe, cancel := context.WithTimeout(ctx, 3*time.Second)
		err := exec.CommandContext(probe, "ping", "-c", "1", "-W", "1", address).Run()
		cancel()
		if err != nil {
			return address, prefix.Bits(), nil
		}
	}
	return "", 0, fmt.Errorf("no free static address in %s", prefix)
}

func waitForNodeInternalIP(ctx context.Context, kubeconfig, dir, node, address string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last []byte
	for time.Now().Before(deadline) {
		output, err := kubectlOutput(ctx, kubeconfig, "get", "node", node, "-o", `jsonpath={range .status.addresses[?(@.type=="InternalIP")]}{.address}{"\n"}{end}{range .status.conditions[?(@.type=="Ready")]}{.status}{end}`)
		if err == nil {
			last = output
			fields := strings.Fields(string(output))
			if len(fields) == 2 && fields[0] == address && fields[1] == "True" {
				return os.WriteFile(filepath.Join(dir, node+"-internal-ip.txt"), output, 0o600)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("node %s did not report Ready InternalIP %s: %s", node, address, last)
}

func copyFile(source, target string) error {
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	return os.WriteFile(target, data, 0o600)
}

func copyDir(source, target string) error {
	entries, err := os.ReadDir(source)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(target, 0o700); err != nil {
		return err
	}
	for _, entry := range entries {
		if err := copyFile(filepath.Join(source, entry.Name()), filepath.Join(target, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

// TestWorkerReaddress moves a joined worker through the documented re-address
// procedure, first to a new DHCP reservation and then to a static address.
func TestWorkerReaddress(t *testing.T) {
	if run, ok := operationBackedWorldSmokeRun(t, "installed-runtime-two-node-worker-readdress"); ok {
		runOperationBackedBootstrapSmoke(t, run, readdressProof)
		return
	}

	options := vmtest.DefaultOptions()
	if !options.Enabled {
		t.Skip("set -katl.vmtest.run or KATL_VMTEST_RUN=1 to run worker re-address")
	}
	_ = vmtest.RequireWorld(t)
}

// runWorkerDHCPReaddress gives the joined worker a new DHCP reservation and
// follows the documented re-address procedure, in which only management.address
// changes in cluster.yaml. It observes the result through the vmtest agent, a
// direct management client, and Kubernetes rather than katlctl's own report.
func runWorkerDHCPReaddress(t *testing.T, ctx context.Context, smoke operationBackedSmokeRun, contextPath string, nodes []vmtest.RunningInstalledRuntimeNode, addresses map[string]string, kubeconfig string, bundle threeControlPlaneKubernetesPayloadBundle, cniFixtures map[string]nodeCNIFixture) error {
	t.Helper()
	dir := filepath.Join(smoke.Result.RunDir, "worker-dhcp-readdress")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	world := smoke.WorldScenario.World
	oldAddress := addresses["worker-1"]
	newAddress, _, err := freeAddress(ctx, world.Libvirt, world.Network, addresses)
	if err != nil {
		return err
	}
	moved, err := smoke.WorldScenario.ReassignAddress("worker-1", newAddress)
	if err != nil {
		return err
	}
	release, err := reserveDHCPAddress(ctx, world, moved)
	if err != nil {
		return err
	}
	t.Cleanup(release)
	if err := os.WriteFile(filepath.Join(dir, "addresses.txt"), []byte(fmt.Sprintf("old=%s\nreserved=%s\nmac=%s\n", oldAddress, moved.Address, moved.MACAddress)), 0o600); err != nil {
		return err
	}

	// A changed DHCP reservation needs no network configuration change.
	configPath, err := writeWorkerReaddressConfig(smoke, dir, bundle, func(node *configbundle.SourceNode) {
		node.Management.Address = newAddress
	})
	if err != nil {
		return err
	}
	before, err := readTwoNodeEnrollments(ctx, addresses["cp-1"], oldAddress)
	if err != nil {
		return err
	}
	katlctl := buildKatlctlCommand(t, ctx, katlRepoRoot(t), contextPath)
	run := func(name string, args ...string) ([]byte, error) {
		stdout, stderr, err := runProofKatlctl(ctx, katlctl, dir, name, args...)
		if err != nil {
			return stdout, fmt.Errorf("%s: %w: %s", name, err, stderr)
		}
		return stdout, nil
	}

	stdout, err := run("stage", "cluster", "apply", "--config", configPath, "--node", "worker-1", "--endpoint", oldAddress, "-o", "json")
	if err != nil {
		return err
	}
	var staged struct {
		Result        string `json:"result"`
		AddressChange struct {
			Node, Current, Desired string
		} `json:"addressChange"`
	}
	if err := json.Unmarshal(stdout, &staged); err != nil {
		return err
	}
	if staged.Result != "succeeded" || staged.AddressChange.Node != "worker-1" || staged.AddressChange.Current != oldAddress || staged.AddressChange.Desired != newAddress {
		return fmt.Errorf("unexpected staged re-address report: %s", stdout)
	}

	worker := nodeByName(nodes, "worker-1")
	bootBefore, err := nodeBootID(ctx, worker)
	if err != nil {
		return err
	}
	stdout, err = run("reboot", "node", "reboot", "worker-1", "--config", configPath, "--endpoint", oldAddress, "--timeout", "15m", "-o", "json")
	if err != nil {
		return err
	}
	var rebooted struct {
		Result, Endpoint string
		AddressChanged   bool `json:"addressChanged"`
	}
	if err := json.Unmarshal(stdout, &rebooted); err != nil {
		return err
	}
	if rebooted.Result != "rebooted" || !rebooted.AddressChanged || rebooted.Endpoint != net.JoinHostPort(newAddress, "9443") {
		return fmt.Errorf("unexpected re-address reboot report: %s", stdout)
	}
	if err := smoke.WorldScenario.ConfirmAddress("worker-1", newAddress); err != nil {
		return err
	}
	if bootAfter, err := nodeBootID(ctx, worker); err != nil || bootAfter == bootBefore {
		return fmt.Errorf("worker did not boot again for the re-address: %v", err)
	}

	// The kernel holds only the reserved address.
	result, err := runNodeCommand(ctx, worker, []string{"ip", "-4", "-o", "addr", "show", "scope", "global"}, 16<<10)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "worker-addresses.txt"), result.Stdout, 0o600); err != nil {
		return err
	}
	if !strings.Contains(string(result.Stdout), " "+newAddress+"/") || strings.Contains(string(result.Stdout), " "+oldAddress+"/") {
		return fmt.Errorf("worker host addresses after re-address: %s", result.Stdout)
	}

	// The management API answers as the same installation at the new address
	// only.
	if conn, err := net.DialTimeout("tcp", net.JoinHostPort(oldAddress, "9443"), 3*time.Second); err == nil {
		_ = conn.Close()
		return fmt.Errorf("the previous worker address still serves the management API")
	}
	after, err := readTwoNodeEnrollments(ctx, addresses["cp-1"], newAddress)
	if err != nil {
		return err
	}
	if after["worker-1"].GetMachineId() != before["worker-1"].GetMachineId() || after["worker-1"].GetEnrollmentId() != before["worker-1"].GetEnrollmentId() {
		return fmt.Errorf("worker identity changed across re-address")
	}

	// Rerunning the earlier steps against the previous address must point
	// at finishing the move rather than leave a bare connection error.
	bootIDs, err := captureNodeBootIDs(ctx, worker)
	if err != nil {
		return err
	}
	finish := fmt.Sprintf("katlctl cluster apply --config %s --node worker-1' without --endpoint", configPath)
	for _, retry := range []struct {
		name string
		args []string
	}{
		{"stale-apply", []string{"cluster", "apply", "--config", configPath, "--node", "worker-1", "--endpoint", oldAddress}},
		{"stale-reboot", []string{"node", "reboot", "worker-1", "--config", configPath, "--endpoint", oldAddress, "--timeout", "1m"}},
	} {
		if _, stderr, err := runProofKatlctl(ctx, katlctl, dir, retry.name, retry.args...); err == nil || !strings.Contains(stderr, finish) {
			return fmt.Errorf("%s at the previous address did not name %q: %v: %s", retry.name, finish, err, stderr)
		}
	}
	if err := assertNodeBootIDsUnchanged(ctx, bootIDs, worker); err != nil {
		return fmt.Errorf("a retry at the previous address rebooted the worker: %w", err)
	}

	if err := waitForNodeInternalIP(ctx, kubeconfig, dir, "worker-1", newAddress, 5*time.Minute); err != nil {
		return err
	}
	if active, err := runNodeCommand(ctx, worker, []string{"systemctl", "is-active", "kubelet.service"}, 4<<10); err != nil || active.ExitStatus != 0 {
		return fmt.Errorf("kubelet is not active after re-address: %v", err)
	}
	// The API server reaches kubelet at the Node's reported address.
	configz, err := kubectlOutput(ctx, kubeconfig, "get", "--raw", "/api/v1/nodes/worker-1/proxy/configz")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "worker-configz.json"), configz, 0o600); err != nil {
		return err
	}

	// Restore the test CNI's static pod routes for the new node address.
	cp := cniFixtures["cp-1"]
	cp.PeerAddress = newAddress
	cniFixtures["cp-1"] = cp
	for _, node := range nodes {
		if err := activateNodeCNIFixture(ctx, node, cniFixtures[node.Name]); err != nil {
			return fmt.Errorf("reactivate %s CNI fixture after re-address: %w", node.Name, err)
		}
	}

	if _, err := run("rebind", "context", "rebind", "--node", "worker-1", "--endpoint", newAddress); err != nil {
		return err
	}
	stdout, err = run("finish", "cluster", "apply", "--config", configPath, "--node", "worker-1", "-o", "json")
	if err != nil {
		return err
	}
	var finished struct {
		Result         string `json:"result"`
		RebootRequired bool   `json:"rebootRequired"`
	}
	if err := json.Unmarshal(stdout, &finished); err != nil {
		return err
	}
	if finished.Result != "succeeded" || finished.RebootRequired {
		return fmt.Errorf("apply at the new address did not finish the move: %s", stdout)
	}
	bootIDs, err = captureNodeBootIDs(ctx, nodes...)
	if err != nil {
		return err
	}
	stdout, err = run("repeat", "cluster", "apply", "--config", configPath, "--node", "worker-1", "-o", "json")
	if err != nil {
		return err
	}
	var repeated struct {
		Result         string `json:"result"`
		RebootRequired bool   `json:"rebootRequired"`
		NodePlans      []struct {
			NoChanges bool `json:"noChanges"`
		} `json:"nodePlans"`
	}
	if err := json.Unmarshal(stdout, &repeated); err != nil {
		return err
	}
	if repeated.Result != "succeeded" || repeated.RebootRequired || len(repeated.NodePlans) != 1 || !repeated.NodePlans[0].NoChanges {
		return fmt.Errorf("repeated apply at the new address was not a no-op: %s", stdout)
	}
	if err := assertNodeBootIDsUnchanged(ctx, bootIDs, nodes...); err != nil {
		return fmt.Errorf("repeated apply rebooted a node: %w", err)
	}

	// A fresh server pod schedules on the worker, and a client on cp-1
	// reaches it through its Service across nodes.
	if _, err := kubectlOutput(ctx, kubeconfig, "-n", "katl-vmtest", "delete", "pod", "-l", "app=net-server", "--wait=true"); err != nil {
		return err
	}
	if _, err := kubectlOutput(ctx, kubeconfig, "-n", "katl-vmtest", "rollout", "status", "deployment/net-server", "--timeout=3m"); err != nil {
		return err
	}
	placed, err := kubectlOutput(ctx, kubeconfig, "-n", "katl-vmtest", "get", "pod", "-l", "app=net-server", "-o", `jsonpath={range .items[*]}{.spec.nodeName} {.status.hostIP} {.status.phase}{"\n"}{end}`)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(placed)) != "worker-1 "+newAddress+" Running" {
		return fmt.Errorf("net-server pod after re-address: %q", placed)
	}
	if _, err := kubectlOutput(ctx, kubeconfig, "-n", "katl-vmtest", "delete", "job", "net-client", "--wait=true"); err != nil {
		return err
	}
	if _, err := kubectlOutput(ctx, kubeconfig, "apply", "-f", filepath.Join(katlRepoRoot(t), "internal/vmtest/scenarios/testdata/bootstrap/cross-node-workload.yaml")); err != nil {
		return err
	}
	if _, err := kubectlOutput(ctx, kubeconfig, "-n", "katl-vmtest", "wait", "--for=condition=Complete", "job/net-client", "--timeout=3m"); err != nil {
		return err
	}
	addresses["worker-1"] = newAddress
	return nil
}

// reserveDHCPAddress adds node's reservation to the live test network, so
// its next DHCP request receives node.Address. The network is shared with
// other runs, so the reservation is not persisted and release removes it.
func reserveDHCPAddress(ctx context.Context, world vmtest.World, node vmtest.Node) (func(), error) {
	host := fmt.Sprintf("<host mac='%s' ip='%s'/>", node.MACAddress, node.Address)
	virsh := func(ctx context.Context, command string) error {
		args := []string{"net-update", world.Network.Name, command, "ip-dhcp-host", host, "--live"}
		if uri := strings.TrimSpace(world.Libvirt.URI); uri != "" {
			args = append([]string{"-c", uri}, args...)
		}
		if output, err := exec.CommandContext(ctx, "virsh", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("virsh %s: %w: %s", strings.Join(args, " "), err, output)
		}
		return nil
	}
	if err := virsh(ctx, "add-last"); err != nil {
		return nil, err
	}
	return func() { _ = virsh(context.Background(), "delete") }, nil
}
