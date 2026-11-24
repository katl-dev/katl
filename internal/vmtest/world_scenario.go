package vmtest

import (
	"context"
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
	// loaded marks a scenario reopened from its manifest. Other processes
	// may share it, so allocations reload its nodes first.
	loaded bool
	// owned marks a scenario that created its own libvirt network, which it
	// keeps reserving its nodes' addresses in.
	owned bool
}

type Node struct {
	Name    string   `json:"name"`
	Role    NodeRole `json:"role"`
	Address string   `json:"address"`
	// PreviousAddress is the address a reassigned node held before
	// ReassignAddress. The VM keeps it until it next boots, so it stays
	// reserved until ConfirmAddress observes the node at Address.
	PreviousAddress string `json:"previousAddress,omitempty"`
	MACAddress      string `json:"macAddress"`
	// Network is the libvirt network the node's VM attaches to.
	Network string `json:"network,omitempty"`
	// Reserved marks a node whose network gives its MAC the planned address
	// through a DHCP reservation.
	Reserved    bool   `json:"reserved,omitempty"`
	ArtifactDir string `json:"artifactDir"`
	ManifestDir string `json:"manifestDir"`
	DiskDir     string `json:"diskDir"`
	VMDir       string `json:"vmDir"`
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

// NewScenario plans and records a scenario. In a world that a run lease owns,
// the scenario gets its own libvirt network, which t's cleanup removes.
func (world World) NewScenario(t interface {
	Helper()
	Fatalf(format string, args ...any)
	Cleanup(func())
}, name string,
) *WorldScenario {
	t.Helper()
	scenario, err := world.PlanScenario(name)
	if err != nil {
		t.Fatalf("plan VM test scenario: %v", err)
	}
	t.Cleanup(scenario.Release)
	if err := scenario.WriteManifest(); err != nil {
		t.Fatalf("write VM test scenario manifest: %v", err)
	}
	return scenario
}

// PlanScenario plans a scenario. In a world with libvirt that a run lease owns,
// it also creates the scenario's own libvirt network, which Release removes.
func (world World) PlanScenario(name string) (*WorldScenario, error) {
	scenario, err := world.planScenario(name)
	if err != nil {
		return nil, err
	}
	if world.Lease != "" && world.Capabilities["libvirt"] == WorldStatusPassed {
		if err := scenario.createNetwork(context.Background()); err != nil {
			return nil, fmt.Errorf("create scenario network: %w", err)
		}
	}
	return scenario, nil
}

func (world World) planScenario(name string) (*WorldScenario, error) {
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
	artifactDir := filepath.Join(scenario.NodeDir, nodeID)
	node := Node{
		Name:        spec.Name,
		Role:        spec.Role,
		MACAddress:  nodeMACAddress(scenario.World.RunID, scenario.ID, spec.Name),
		Network:     scenario.World.Network.Name,
		Reserved:    scenario.owned,
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
	err := scenario.allocate(func(reserved map[string]bool) (*leaseEntry, error) {
		if scenario.nodeIndex(spec.Name) >= 0 {
			return nil, fmt.Errorf("scenario %q already has node %q", scenario.Name, spec.Name)
		}
		address := strings.TrimSpace(spec.Address)
		var err error
		if address == "" {
			address, err = firstAvailableAddress(scenario.World, reserved)
		} else {
			err = validateRequestedAddress(scenario.World, reserved, address)
		}
		if err != nil {
			return nil, err
		}
		node.Address = address
		scenario.Nodes = append(scenario.Nodes, node)
		return &leaseEntry{Address: address, MACAddress: node.MACAddress, Scenario: scenario.ID, Node: node.Name}, nil
	})
	if err != nil {
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

// allocate applies change to the scenario's nodes under the world lease lock
// and records the result. The scenario manifest is the record of this
// scenario's addresses: change sees every address its nodes hold, including
// the previous addresses of reassigned nodes, plus other scenarios' leases.
// The lease file only claims addresses against other scenarios, so its
// entries for this scenario are ignored here and may over-claim after a
// reassignment. The lease is written before the manifest, so an interruption
// between them can only over-claim. A loaded scenario's nodes are reloaded
// from the manifest first so that processes sharing it do not overwrite each
// other's nodes.
func (scenario *WorldScenario) allocate(change func(reserved map[string]bool) (*leaseEntry, error)) error {
	unlock, err := lockLeaseFile(scenario.World.Network.LeaseFile)
	if err != nil {
		return err
	}
	defer unlock()
	if scenario.loaded {
		if err := scenario.reloadNodes(); err != nil {
			return err
		}
	}
	leases, err := readLeases(scenario.World.Network.LeaseFile)
	if err != nil {
		return err
	}
	reserved := map[string]bool{}
	for _, lease := range leases.Leases {
		if lease.Scenario != scenario.ID {
			reserved[lease.Address] = true
		}
	}
	for _, node := range scenario.Nodes {
		for _, address := range []string{node.Address, node.PreviousAddress} {
			if address != "" {
				reserved[address] = true
			}
		}
	}

	lease, err := change(reserved)
	if err != nil {
		return err
	}
	if lease != nil {
		leases.Leases = append(leases.Leases, *lease)
		if err := writeLeases(scenario.World.Network.LeaseFile, leases); err != nil {
			return err
		}
	}
	if err := scenario.WriteManifest(); err != nil {
		return err
	}
	return scenario.syncNetwork(context.Background())
}

// reloadNodes replaces the scenario's nodes with those of its manifest.
func (scenario *WorldScenario) reloadNodes() error {
	data, err := os.ReadFile(scenario.ManifestPath)
	if err != nil {
		return err
	}
	var manifest scenarioManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return fmt.Errorf("decode %s: %w", scenario.ManifestPath, err)
	}
	scenario.Nodes = manifest.Nodes
	return nil
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
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	return lockFile(ctx, path)
}

// lockFile takes an exclusive flock on path+".lock", waiting until ctx ends.
func lockFile(ctx context.Context, path string) (func(), error) {
	lockPath := path + ".lock"
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
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
		select {
		case <-ctx.Done():
			_ = lock.Close()
			return nil, fmt.Errorf("timed out waiting for lock %s: %w", lockPath, ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// worldAddressPlan returns how the world's network divides its addresses.
func worldAddressPlan(world World) (addressPlan, error) {
	_, network, err := parseWorldNetwork(world)
	if err != nil {
		return addressPlan{}, err
	}
	gateway := net.ParseIP(world.Network.Gateway).To4()
	if gateway == nil {
		return addressPlan{}, fmt.Errorf("world gateway %q is invalid", world.Network.Gateway)
	}
	return planAddresses(network, gateway)
}

// firstAvailableAddress returns the first reservable address that leased does
// not hold.
func firstAvailableAddress(world World, leased map[string]bool) (string, error) {
	plan, err := worldAddressPlan(world)
	if err != nil {
		return "", err
	}
	for candidate := plan.reservableStart; candidate <= plan.reservableEnd; candidate++ {
		address := uint32ToIP(candidate).String()
		if plan.reservable(candidate) && !leased[address] {
			return address, nil
		}
	}
	return "", fmt.Errorf("no free addresses in %s", world.Network.CIDR)
}

// validateRequestedAddress accepts a reservable address that leased does not
// hold. Addresses of the dynamic DHCP range are refused because dnsmasq may
// give them to any guest.
func validateRequestedAddress(world World, leased map[string]bool, address string) error {
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
	plan, err := planAddresses(network, gateway)
	if err != nil {
		return err
	}
	if !plan.reservable(ipToUint32(ip)) {
		return fmt.Errorf("node address %q is in the dynamic DHCP range %s-%s of %s", address, uint32ToIP(plan.dynamicStart), uint32ToIP(plan.dynamicEnd), world.Network.CIDR)
	}
	if leased[address] {
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
	return ip, network, nil
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
	scenario, err := world.planScenario(name)
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
	scenario.loaded = true
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

// ReassignAddress moves a node's reservation to address, or to the first free
// address when address is empty. The address the VM holds until its next boot
// stays reserved as its PreviousAddress until ConfirmAddress releases it.
func (scenario *WorldScenario) ReassignAddress(name, address string) (Node, error) {
	var moved Node
	err := scenario.allocate(func(reserved map[string]bool) (*leaseEntry, error) {
		index := scenario.nodeIndex(name)
		if index < 0 {
			return nil, fmt.Errorf("node %q is not in scenario %q", name, scenario.Name)
		}
		node := scenario.Nodes[index]
		live := first(node.PreviousAddress, node.Address)
		// The node may move back to the address its VM still holds, but a
		// reassignment always moves its reservation.
		delete(reserved, live)
		reserved[node.Address] = true
		requested := strings.TrimSpace(address)
		var err error
		if requested == "" {
			requested, err = firstAvailableAddress(scenario.World, reserved)
		} else {
			err = validateRequestedAddress(scenario.World, reserved, requested)
		}
		if err != nil {
			return nil, err
		}

		node.Address, node.PreviousAddress = requested, live
		if requested == live {
			node.PreviousAddress = ""
		}
		scenario.Nodes[index] = node
		moved = node
		return &leaseEntry{Address: requested, MACAddress: node.MACAddress, Scenario: scenario.ID, Node: name}, nil
	})
	if err != nil {
		return Node{}, err
	}
	return moved, nil
}

// ConfirmAddress records that the node answered at address. When that is its
// reservation, its PreviousAddress is released for other nodes.
func (scenario *WorldScenario) ConfirmAddress(name, address string) error {
	return scenario.allocate(func(map[string]bool) (*leaseEntry, error) {
		if index := scenario.nodeIndex(name); index >= 0 && scenario.Nodes[index].Address == address {
			scenario.Nodes[index].PreviousAddress = ""
		}
		return nil, nil
	})
}

func (scenario *WorldScenario) nodeIndex(name string) int {
	for i, node := range scenario.Nodes {
		if node.Name == name {
			return i
		}
	}
	return -1
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
