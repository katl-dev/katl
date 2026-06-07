package vmtest

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
)

func TestWorldScenarioAllocatesLeases(t *testing.T) {
	world := testWorld(t)
	scenario := world.NewScenario(t, "two node kubeadm")

	cp := scenario.NewNode(t, NodeSpec{Name: "cp-1", Role: ControlPlane})
	worker := scenario.NewNode(t, NodeSpec{Name: "worker-1", Role: Worker})

	if cp.Address != "10.77.0.2" || worker.Address != "10.77.0.3" {
		t.Fatalf("addresses = %s, %s", cp.Address, worker.Address)
	}
	if cp.ArtifactDir != filepath.Join(world.ScenarioDir, "two-node-kubeadm", "nodes", "cp-1") {
		t.Fatalf("cp artifact dir = %q", cp.ArtifactDir)
	}
	for _, dir := range []string{cp.ManifestDir, cp.DiskDir, cp.VMDir, worker.ManifestDir, worker.DiskDir, worker.VMDir} {
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			t.Fatalf("artifact dir %s missing or not dir: info=%v err=%v", dir, info, err)
		}
	}

	leases := readLeaseFileForTest(t, world.Network.LeaseFile)
	gotLeases := leaseAddresses(leases)
	wantLeases := []string{"10.77.0.2", "10.77.0.3"}
	if !reflect.DeepEqual(gotLeases, wantLeases) {
		t.Fatalf("leases = %#v, want %#v", gotLeases, wantLeases)
	}
	manifest := readScenarioManifest(t, scenario.ManifestPath)
	if manifest.Name != "two node kubeadm" || manifest.ID != "two-node-kubeadm" || len(manifest.Nodes) != 2 {
		t.Fatalf("manifest = %#v", manifest)
	}
}

func TestWorldScenarioRejectsReservedAndDuplicateAddresses(t *testing.T) {
	world := testWorld(t)
	scenario, err := world.PlanScenario("reserved")
	if err != nil {
		t.Fatalf("PlanScenario() error = %v", err)
	}
	tests := []struct {
		name    string
		address string
		want    string
	}{
		{name: "network", address: "10.77.0.0", want: "network address"},
		{name: "gateway", address: "10.77.0.1", want: "gateway"},
		{name: "broadcast", address: "10.77.0.255", want: "broadcast"},
		{name: "outside", address: "10.78.0.2", want: "outside"},
		{name: "invalid", address: "not-an-ip", want: "invalid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := scenario.AddNode(NodeSpec{Name: tt.name, Role: Worker, Address: tt.address})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("AddNode() error = %v, want %q", err, tt.want)
			}
		})
	}

	first, err := scenario.AddNode(NodeSpec{Name: "first", Role: Worker, Address: "10.77.0.10"})
	if err != nil {
		t.Fatalf("AddNode(first) error = %v", err)
	}
	if first.Address != "10.77.0.10" {
		t.Fatalf("first address = %q", first.Address)
	}
	other, err := world.PlanScenario("other")
	if err != nil {
		t.Fatalf("PlanScenario(other) error = %v", err)
	}
	_, err = other.AddNode(NodeSpec{Name: "duplicate", Role: Worker, Address: "10.77.0.10"})
	if err == nil || !strings.Contains(err.Error(), "already leased") {
		t.Fatalf("AddNode(duplicate) error = %v, want duplicate rejection", err)
	}
}

func TestWorldScenarioReportsExhaustedCIDR(t *testing.T) {
	world := testWorld(t)
	world.Network.CIDR = "10.77.0.1/32"
	world.Network.Gateway = "10.77.0.1"
	scenario, err := world.PlanScenario("exhausted")
	if err != nil {
		t.Fatalf("PlanScenario() error = %v", err)
	}
	_, err = scenario.AddNode(NodeSpec{Name: "node", Role: Worker})
	if err == nil || !strings.Contains(err.Error(), "no free addresses") {
		t.Fatalf("AddNode() error = %v, want exhausted CIDR", err)
	}
}

func TestWorldScenarioParallelIsolation(t *testing.T) {
	world := testWorld(t)
	const scenarios = 12
	var wg sync.WaitGroup
	addresses := make(chan string, scenarios)
	errs := make(chan error, scenarios)

	for i := 0; i < scenarios; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			scenario, err := world.PlanScenario("parallel scenario " + string(rune('a'+i)))
			if err != nil {
				errs <- err
				return
			}
			node, err := scenario.AddNode(NodeSpec{Name: "node", Role: Worker})
			if err != nil {
				errs <- err
				return
			}
			addresses <- node.Address
		}()
	}
	wg.Wait()
	close(addresses)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("parallel scenario error = %v", err)
		}
	}

	seen := map[string]bool{}
	for address := range addresses {
		if seen[address] {
			t.Fatalf("duplicate address allocated: %s", address)
		}
		seen[address] = true
	}
	if len(seen) != scenarios {
		t.Fatalf("allocated %d addresses, want %d", len(seen), scenarios)
	}
	leases := readLeaseFileForTest(t, world.Network.LeaseFile)
	if len(leases.Leases) != scenarios {
		t.Fatalf("lease count = %d, want %d", len(leases.Leases), scenarios)
	}
}

func TestWorldScenarioWritesSetupFailureArtifacts(t *testing.T) {
	world := testWorld(t)
	scenario, err := world.PlanScenario("setup stops")
	if err != nil {
		t.Fatalf("PlanScenario() error = %v", err)
	}
	if err := scenario.WriteSetupFailure(errors.New("fixture missing")); err != nil {
		t.Fatalf("WriteSetupFailure() error = %v", err)
	}
	if _, err := os.Stat(scenario.ManifestPath); err != nil {
		t.Fatalf("scenario manifest missing: %v", err)
	}
	var result scenarioResult
	readJSONForTest(t, scenario.ResultPath, &result)
	if result.Status != WorldStatusSetupFailed || result.FailureSummary != "fixture missing" {
		t.Fatalf("result = %#v", result)
	}
	if result.ManifestPath != scenario.ManifestPath || result.ResultPath != scenario.ResultPath {
		t.Fatalf("result paths = %#v", result)
	}
}

func TestWorldScenarioRequiresNodeIdentity(t *testing.T) {
	world := testWorld(t)
	scenario, err := world.PlanScenario("identity")
	if err != nil {
		t.Fatalf("PlanScenario() error = %v", err)
	}
	if _, err := scenario.AddNode(NodeSpec{Role: Worker}); err == nil || !strings.Contains(err.Error(), "node name is required") {
		t.Fatalf("AddNode(empty name) error = %v", err)
	}
	if _, err := scenario.AddNode(NodeSpec{Name: "worker"}); err == nil || !strings.Contains(err.Error(), "role is required") {
		t.Fatalf("AddNode(empty role) error = %v", err)
	}
}

func testWorld(t *testing.T) World {
	t.Helper()
	root := t.TempDir()
	world := validWorld()
	world.RunID = "run-1"
	world.RunDir = root
	world.ArtifactDir = filepath.Join(root, "artifacts")
	world.ScenarioDir = filepath.Join(root, "scenarios")
	world.Network.CIDR = "10.77.0.0/24"
	world.Network.Gateway = "10.77.0.1"
	world.Network.LeaseFile = filepath.Join(root, "network", "leases.json")
	if err := os.MkdirAll(filepath.Dir(world.Network.LeaseFile), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(world.Network.LeaseFile, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return world
}

func readLeaseFileForTest(t *testing.T, path string) leaseFile {
	t.Helper()
	var leases leaseFile
	readJSONForTest(t, path, &leases)
	return leases
}

func readScenarioManifest(t *testing.T, path string) scenarioManifest {
	t.Helper()
	var manifest scenarioManifest
	readJSONForTest(t, path, &manifest)
	return manifest
}

func readJSONForTest(t *testing.T, path string, value any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, err)
	}
	if err := json.Unmarshal(data, value); err != nil {
		t.Fatalf("Unmarshal(%s) error = %v", path, err)
	}
}

func leaseAddresses(leases leaseFile) []string {
	addresses := make([]string, 0, len(leases.Leases))
	for _, lease := range leases.Leases {
		addresses = append(addresses, lease.Address)
	}
	sort.Strings(addresses)
	return addresses
}
