package vmtest

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
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
	Address  string `json:"address"`
	Scenario string `json:"scenario"`
	Node     string `json:"node"`
}

func (world World) NewScenario(t interface {
	Helper()
	Fatalf(format string, args ...any)
}, name string) *WorldScenario {
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
}, spec NodeSpec) Node {
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
	if writeErr := scenario.WriteManifest(); writeErr != nil {
		return writeErr
	}
	return writeJSON(scenario.ResultPath, scenarioResult{
		APIVersion:     WorldAPIVersion,
		Kind:           "VMTestScenarioResult",
		ScenarioName:   scenario.Name,
		Status:         WorldStatusSetupFailed,
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
		Address:  address,
		Scenario: scenario.ID,
		Node:     spec.Name,
	})
	return address, writeLeases(scenario.World.Network.LeaseFile, leases)
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
	var lock *os.File
	var err error
	for i := 0; i < 100; i++ {
		lock, err = os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if err == nil {
			return func() {
				_ = lock.Close()
				_ = os.Remove(lockPath)
			}, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		time.Sleep(10 * time.Millisecond)
	}
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
	return ipToUint32(network.IP) | (1 << hostBits) - 1
}
