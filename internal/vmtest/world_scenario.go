package vmtest

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

type NodeRole string

const (
	ControlPlane NodeRole = "control-plane"
	Worker       NodeRole = "worker"
)

type NodeSpec struct {
	Name    string   `json:"name"`
	Role    NodeRole `json:"role"`
	Address string   `json:"address,omitempty"`
}

type WorldScenario struct {
	World        World
	Name         string
	ID           string
	Dir          string
	ManifestPath string
	ResultPath   string
	NodeDir      string
	Nodes        []Node
	Fixtures     []FixtureRecord
}

type Node struct {
	Name        string   `json:"name"`
	Role        NodeRole `json:"role"`
	Address     string   `json:"address"`
	MACAddress  string   `json:"macAddress"`
	ArtifactDir string   `json:"artifactDir"`
	ManifestDir string   `json:"manifestDir"`
	DiskDir     string   `json:"diskDir"`
	VMDir       string   `json:"vmDir"`
}

type scenarioManifest struct {
	APIVersion string          `json:"apiVersion"`
	Kind       string          `json:"kind"`
	WorldRunID string          `json:"worldRunID"`
	Name       string          `json:"name"`
	ID         string          `json:"id"`
	Dir        string          `json:"dir"`
	ResultPath string          `json:"resultPath"`
	Nodes      []Node          `json:"nodes"`
	Fixtures   []FixtureRecord `json:"fixtures,omitempty"`
}

type scenarioResult struct {
	APIVersion     string      `json:"apiVersion"`
	Kind           string      `json:"kind"`
	ScenarioName   string      `json:"scenarioName"`
	Status         WorldStatus `json:"status"`
	FailureSummary string      `json:"failureSummary,omitempty"`
	ManifestPath   string      `json:"manifestPath"`
	ResultPath     string      `json:"resultPath"`
	Nodes          []Node      `json:"nodes,omitempty"`
}

type leaseFile struct {
	APIVersion string       `json:"apiVersion,omitempty"`
	Kind       string       `json:"kind,omitempty"`
	Leases     []leaseEntry `json:"leases,omitempty"`
}

type leaseEntry struct {
	Address    string `json:"address"`
	MACAddress string `json:"macAddress,omitempty"`
	Scenario   string `json:"scenario"`
	Node       string `json:"node"`
}

func (world World) NewScenario(t interface {
	Helper()
	Fatalf(format string, args ...any)
}, name string,
) *WorldScenario {
	t.Helper()
	scenario, err := world.PlanScenario(name)
	if err != nil {
		t.Fatalf("plan VM test scenario: %v", err)
	}
	if err := scenario.WriteManifest(); err != nil {
		t.Fatalf("write VM test scenario manifest: %v", err)
	}
	return scenario
}

func (world World) PlanScenario(name string) (*WorldScenario, error) {
	id := clean(name)
	if id == "" {
		return nil, errors.New("scenario name is required")
	}
	dir := filepath.Join(world.ScenarioDir, id)
	scenario := &WorldScenario{
		World:        world,
		Name:         name,
		ID:           id,
		Dir:          dir,
		ManifestPath: filepath.Join(dir, "scenario.json"),
		ResultPath:   filepath.Join(dir, "result.json"),
		NodeDir:      filepath.Join(dir, "nodes"),
	}
	if err := os.MkdirAll(scenario.NodeDir, 0o755); err != nil {
		return nil, err
	}
	return scenario, nil
}

func (scenario *WorldScenario) NewNode(t interface {
	Helper()
	Fatalf(format string, args ...any)
}, spec NodeSpec,
) Node {
	t.Helper()
	node, err := scenario.AddNode(spec)
	if err != nil {
		t.Fatalf("add VM test node: %v", err)
	}
	return node
}

func (scenario *WorldScenario) AddNode(spec NodeSpec) (Node, error) {
	if strings.TrimSpace(spec.Name) == "" {
		return Node{}, errors.New("node name is required")
	}
	if spec.Role == "" {
		return Node{}, fmt.Errorf("node %q role is required", spec.Name)
	}
	nodeID := clean(spec.Name)
	if nodeID == "" {
		return Node{}, fmt.Errorf("node %q has no usable artifact path", spec.Name)
	}
	address, err := scenario.allocateAddress(spec)
	if err != nil {
		return Node{}, err
	}
	artifactDir := filepath.Join(scenario.NodeDir, nodeID)
	node := Node{
		Name:        spec.Name,
		Role:        spec.Role,
		Address:     address,
		MACAddress:  nodeMACAddress(scenario.World.RunID, scenario.ID, spec.Name),
		ArtifactDir: artifactDir,
		ManifestDir: filepath.Join(artifactDir, "manifests"),
		DiskDir:     filepath.Join(artifactDir, "disks"),
		VMDir:       filepath.Join(artifactDir, "vm"),
	}
	for _, dir := range []string{node.ManifestDir, node.DiskDir, node.VMDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return Node{}, err
		}
	}
	scenario.Nodes = append(scenario.Nodes, node)
	if err := scenario.WriteManifest(); err != nil {
		return Node{}, err
	}
	return node, nil
}

func (scenario *WorldScenario) WriteManifest() error {
	if err := os.MkdirAll(scenario.Dir, 0o755); err != nil {
		return err
	}
	return writeJSON(scenario.ManifestPath, scenarioManifest{
		APIVersion: WorldAPIVersion,
		Kind:       "VMTestScenario",
		WorldRunID: scenario.World.RunID,
		Name:       scenario.Name,
		ID:         scenario.ID,
		Dir:        scenario.Dir,
		ResultPath: scenario.ResultPath,
		Nodes:      scenario.Nodes,
		Fixtures:   scenario.Fixtures,
	})
}

func (scenario *WorldScenario) WriteSetupFailure(err error) error {
	failure := ""
	if err != nil {
		failure = err.Error()
	}
	return scenario.WriteResult(WorldStatusSetupFailed, failure)
}

func (scenario *WorldScenario) WriteResult(status WorldStatus, failure string) error {
	if scenario == nil {
		return errors.New("world scenario is required")
	}
	switch status {
	case WorldStatusPassed, WorldStatusFailed, WorldStatusSetupFailed, WorldStatusHostSkipped, WorldStatusDisabled:
	default:
		return fmt.Errorf("invalid world scenario result status %q", status)
	}
	if writeErr := scenario.WriteManifest(); writeErr != nil {
		return writeErr
	}
	return writeJSON(scenario.ResultPath, scenarioResult{
		APIVersion:     WorldAPIVersion,
		Kind:           "VMTestScenarioResult",
		ScenarioName:   scenario.Name,
		Status:         status,
		FailureSummary: failure,
		ManifestPath:   scenario.ManifestPath,
		ResultPath:     scenario.ResultPath,
		Nodes:          scenario.Nodes,
	})
}

func (scenario *WorldScenario) allocateAddress(spec NodeSpec) (string, error) {
	unlock, err := lockLeaseFile(scenario.World.Network.LeaseFile)
	if err != nil {
		return "", err
	}
	defer unlock()
	leases, err := readLeases(scenario.World.Network.LeaseFile)
	if err != nil {
		return "", err
	}
	address := strings.TrimSpace(spec.Address)
	if address == "" {
		address, err = firstAvailableAddress(scenario.World, leases)
		if err != nil {
			return "", err
		}
	} else if err := validateRequestedAddress(scenario.World, leases, address); err != nil {
		return "", err
	}
	leases.Leases = append(leases.Leases, leaseEntry{
		Address:    address,
		MACAddress: nodeMACAddress(scenario.World.RunID, scenario.ID, spec.Name),
		Scenario:   scenario.ID,
		Node:       spec.Name,
	})
	return address, writeLeases(scenario.World.Network.LeaseFile, leases)
}

func nodeMACAddress(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return fmt.Sprintf("52:54:%02x:%02x:%02x:%02x", sum[0], sum[1], sum[2], sum[3])
}

func readLeases(path string) (leaseFile, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return leaseFile{APIVersion: WorldAPIVersion, Kind: "VMTestLeases"}, nil
	}
	if err != nil {
		return leaseFile{}, err
	}
	if strings.TrimSpace(string(data)) == "" {
		return leaseFile{APIVersion: WorldAPIVersion, Kind: "VMTestLeases"}, nil
	}
	var leases leaseFile
	if err := json.Unmarshal(data, &leases); err != nil {
		return leaseFile{}, err
	}
	if leases.APIVersion == "" {
		leases.APIVersion = WorldAPIVersion
	}
	if leases.Kind == "" {
		leases.Kind = "VMTestLeases"
	}
	return leases, nil
}

func writeLeases(path string, leases leaseFile) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return writeJSON(path, leases)
}

func lockLeaseFile(path string) (func(), error) {
	lockPath := path + ".lock"
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for i := 0; i < 100; i++ {
		err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return func() {
				_ = unix.Flock(int(lock.Fd()), unix.LOCK_UN)
				_ = lock.Close()
			}, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			_ = lock.Close()
			return nil, err
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = lock.Close()
	return nil, fmt.Errorf("timed out waiting for lease lock %s", lockPath)
}

func firstAvailableAddress(world World, leases leaseFile) (string, error) {
	networkIP, network, err := parseWorldNetwork(world)
	if err != nil {
		return "", err
	}
	gateway := net.ParseIP(world.Network.Gateway).To4()
	leased := leasedAddresses(leases)
	networkStart := ipToUint32(networkIP)
	networkEnd := broadcast(network)
	if networkEnd <= networkStart+1 {
		return "", fmt.Errorf("no free addresses in %s", world.Network.CIDR)
	}
	start := networkStart + 1
	end := networkEnd - 1
	for candidate := start; candidate <= end; candidate++ {
		address := uint32ToIP(candidate).String()
		if gateway != nil && address == gateway.String() {
			continue
		}
		if leased[address] {
			continue
		}
		return address, nil
	}
	return "", fmt.Errorf("no free addresses in %s", world.Network.CIDR)
}

func validateRequestedAddress(world World, leases leaseFile, address string) error {
	ip := net.ParseIP(address).To4()
	if ip == nil {
		return fmt.Errorf("node address %q is invalid", address)
	}
	_, network, err := parseWorldNetwork(world)
	if err != nil {
		return err
	}
	if !network.Contains(ip) {
		return fmt.Errorf("node address %q is outside %s", address, world.Network.CIDR)
	}
	if address == network.IP.String() {
		return fmt.Errorf("node address %q is reserved as network address", address)
	}
	gateway := net.ParseIP(world.Network.Gateway).To4()
	if gateway == nil {
		return fmt.Errorf("world gateway %q is invalid", world.Network.Gateway)
	}
	if address == gateway.String() {
		return fmt.Errorf("node address %q is reserved as gateway", address)
	}
	if address == uint32ToIP(broadcast(network)).String() {
		return fmt.Errorf("node address %q is reserved as broadcast address", address)
	}
	if leasedAddresses(leases)[address] {
		return fmt.Errorf("node address %q is already leased", address)
	}
	return nil
}

func parseWorldNetwork(world World) (net.IP, *net.IPNet, error) {
	ip, network, err := net.ParseCIDR(world.Network.CIDR)
	if err != nil {
		return nil, nil, err
	}
	ip = ip.To4()
	if ip == nil {
		return nil, nil, fmt.Errorf("world CIDR %q is not IPv4", world.Network.CIDR)
	}
	network.IP = ip
	return ip, network, nil
}

func leasedAddresses(leases leaseFile) map[string]bool {
	leased := make(map[string]bool, len(leases.Leases))
	for _, lease := range leases.Leases {
		leased[lease.Address] = true
	}
	return leased
}

func ipToUint32(ip net.IP) uint32 {
	ip = ip.To4()
	return uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
}

func uint32ToIP(value uint32) net.IP {
	return net.IPv4(byte(value>>24), byte(value>>16), byte(value>>8), byte(value))
}

func broadcast(network *net.IPNet) uint32 {
	ones, bits := network.Mask.Size()
	if bits != 32 {
		return ipToUint32(network.IP)
	}
	hostBits := 32 - ones
	return ipToUint32(network.IP) | (1<<hostBits - 1)
}

// LoadScenario reopens a scenario whose manifest an earlier process wrote, so
// a persistent world can keep adding to and managing its nodes.
func (world World) LoadScenario(name string) (*WorldScenario, error) {
	scenario, err := world.PlanScenario(name)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(scenario.ManifestPath)
	if err != nil {
		return nil, err
	}
	var manifest scenarioManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("decode %s: %w", scenario.ManifestPath, err)
	}
	if manifest.WorldRunID != world.RunID || manifest.ID != scenario.ID {
		return nil, fmt.Errorf("%s belongs to world %q scenario %q, not %q %q", scenario.ManifestPath, manifest.WorldRunID, manifest.ID, world.RunID, scenario.ID)
	}
	scenario.Nodes = manifest.Nodes
	scenario.Fixtures = manifest.Fixtures
	return scenario, nil
}

// Node returns the named node.
func (scenario *WorldScenario) Node(name string) (Node, bool) {
	for _, node := range scenario.Nodes {
		if node.Name == name {
			return node, true
		}
	}
	return Node{}, false
}

// ReassignAddress moves a node's lease to address, or to the first free
// address when address is empty, and records it in the scenario manifest.
func (scenario *WorldScenario) ReassignAddress(name, address string) (Node, error) {
	index := -1
	for i, node := range scenario.Nodes {
		if node.Name == name {
			index = i
		}
	}
	if index < 0 {
		return Node{}, fmt.Errorf("node %q is not in scenario %q", name, scenario.Name)
	}
	unlock, err := lockLeaseFile(scenario.World.Network.LeaseFile)
	if err != nil {
		return Node{}, err
	}
	defer unlock()
	leases, err := readLeases(scenario.World.Network.LeaseFile)
	if err != nil {
		return Node{}, err
	}
	// The node's own lease must not block the search, but its current address
	// stays reserved so a reassignment always moves the node.
	others := leaseFile{APIVersion: leases.APIVersion, Kind: leases.Kind}
	for _, lease := range leases.Leases {
		if lease.Scenario == scenario.ID && lease.Node == name {
			continue
		}
		others.Leases = append(others.Leases, lease)
	}
	current := scenario.Nodes[index].Address
	reserved := others
	reserved.Leases = append(append([]leaseEntry(nil), others.Leases...), leaseEntry{Address: current})
	address = strings.TrimSpace(address)
	if address == "" {
		address, err = firstAvailableAddress(scenario.World, reserved)
	} else {
		err = validateRequestedAddress(scenario.World, reserved, address)
	}
	if err != nil {
		return Node{}, err
	}
	node := scenario.Nodes[index]
	node.Address = address
	others.Leases = append(others.Leases, leaseEntry{
		Address:    address,
		MACAddress: node.MACAddress,
		Scenario:   scenario.ID,
		Node:       name,
	})
	if err := writeLeases(scenario.World.Network.LeaseFile, others); err != nil {
		return Node{}, err
	}
	scenario.Nodes[index] = node
	if err := scenario.WriteManifest(); err != nil {
		return Node{}, err
	}
	return node, nil
}

// PlanNodeVM plans a libvirt VM whose disks and artifacts live in the node's
// world directories rather than a per-run directory, so the VM can outlive the
// process that created it. Its domain name carries the world's run ID.
func (scenario *WorldScenario) PlanNodeVM(node Node, disks []DiskFixture, kvm KVMPolicy) (Result, error) {
	plans, err := planDisks(node.DiskDir, disks)
	if err != nil {
		return Result{}, err
	}
	return Result{
		ScenarioName: nodeScenarioName(scenario.Name, node.Name),
		Status:       StatusPlanned,
		RunID:        scenario.World.RunID + "-" + clean(node.Name),
		RunDir:       node.ArtifactDir,
		VMDir:        node.VMDir,
		DiskDir:      node.DiskDir,
		ManifestDir:  node.ManifestDir,
		Keep:         KeepAlways,
		KVM:          kvm,
		Artifacts:    pathsFor(node.ArtifactDir),
		Disks:        plans,
	}, nil
}
