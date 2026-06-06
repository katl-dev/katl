package vmtest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStartInstalledRuntimeNodeKeepsVMRunningWithNodeArtifacts(t *testing.T) {
	root := t.TempDir()
	disk := filepath.Join(root, "installed.raw")
	if err := os.WriteFile(disk, []byte("disk"), 0o644); err != nil {
		t.Fatalf("write disk: %v", err)
	}
	esp := espFixture(t)
	nodeMetadata := filepath.Join(root, "node.json")
	if err := os.WriteFile(nodeMetadata, []byte(`{"kind":"NodeMetadata"}`), 0o644); err != nil {
		t.Fatalf("write node metadata: %v", err)
	}
	fixtureManifest := writeInstalledFixtureManifest(t, root, disk, esp, nodeMetadata)
	parent, err := NewRunner(Options{
		StateRoot: root,
		RunID:     "run-1",
	}).Plan(Scenario{Name: "two-node"})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	_, vmConfig := vmFixture(t)
	vmConfig.Expect = "Katl state projection ready"
	vmConfig.Timeout = time.Minute
	vmConfig.VSock = VSockConfig{Enabled: true, GuestCID: 62000}
	runner := VMRunner{
		Executor: longRunningVMExec{ready: "Katl state projection ready"},
		AgentConnector: func(context.Context, VSockPlan, string) (AgentHealthClient, error) {
			return fakeHealthClient{}, nil
		},
		probe: probe{
			lookPath: func(string) (string, error) { return "/usr/bin/qemu-system-x86_64", nil },
			stat:     os.Stat,
			access:   func(string) error { return nil },
			output: func(string, ...string) ([]byte, error) {
				return []byte("vhost-vsock-pci guest-cid=<uint32>"), nil
			},
		},
	}

	node, err := StartInstalledRuntimeNode(context.Background(), parent, InstalledRuntimeNodeConfig{
		Name: "cp-1",
		Runtime: InstalledRuntimeConfig{
			Disk:            disk,
			DiskFormat:      DiskRaw,
			ESPArtifacts:    esp,
			FixtureManifest: fixtureManifest,
			NodeMetadata:    nodeMetadata,
			VM:              vmConfig,
		},
	}, runner)
	if err != nil {
		t.Fatalf("StartInstalledRuntimeNode() error = %v", err)
	}
	if node.VSock.GuestCID != 62000 || node.VSock.Port != 10240 {
		t.Fatalf("vsock = %#v", node.VSock)
	}
	if node.Result.RunDir != filepath.Join(parent.RunDir, "nodes", "cp-1") {
		t.Fatalf("node run dir = %q", node.Result.RunDir)
	}
	if _, err := os.Stat(filepath.Join(node.Result.RunDir, "esp", "loader", "entries", filepath.Base(loaderEntry(t, esp)))); err != nil {
		t.Fatalf("ESP copy missing: %v", err)
	}
	serial, err := os.ReadFile(node.Result.Artifacts.RuntimeSerial)
	if err != nil || !strings.Contains(string(serial), "Katl state projection ready") {
		t.Fatalf("runtime serial = %q, err = %v", serial, err)
	}
	command, err := os.ReadFile(node.Result.Artifacts.QEMUCommand)
	if err != nil {
		t.Fatalf("read qemu command: %v", err)
	}
	if !strings.Contains(string(command), "guest-cid=62000") || !strings.Contains(string(command), "fat:rw:"+filepath.Join(node.Result.RunDir, "esp")) {
		t.Fatalf("qemu command = %s", command)
	}
	entry, err := os.ReadFile(filepath.Join(node.Result.RunDir, "esp", "loader", "entries", filepath.Base(loaderEntry(t, esp))))
	if err != nil {
		t.Fatalf("read copied loader entry: %v", err)
	}
	if !strings.Contains(string(entry), "katl.vmtest_agent=1") {
		t.Fatalf("vmtest agent flag missing from copied loader entry: %s", entry)
	}
	input := readInstalledRuntimeInput(t, node.Result.Artifacts.InstalledRuntime)
	if input.FixtureManifest != fixtureManifest || input.NodeMetadata != nodeMetadata {
		t.Fatalf("installed runtime input provenance = %#v", input)
	}
	if input.Fixture == nil || input.Fixture.NodeName != "node-1" || input.Fixture.SystemRole != "control-plane" {
		t.Fatalf("fixture metadata = %#v", input.Fixture)
	}
	result := readNodeResult(t, node.Result.Artifacts.Result)
	if result.Status != StatusPassed || result.FailureSummary != "" || len(result.Phases) != 1 || result.Phases[0].Name != "installed-runtime-node-start" {
		t.Fatalf("node result = %#v", result)
	}
	if err := node.Stop(); err != context.Canceled {
		t.Fatalf("Stop() error = %v, want context.Canceled", err)
	}
}

func TestStartInstalledRuntimeNodeWritesFailureResult(t *testing.T) {
	root := t.TempDir()
	disk := filepath.Join(root, "installed.raw")
	if err := os.WriteFile(disk, []byte("disk"), 0o644); err != nil {
		t.Fatalf("write disk: %v", err)
	}
	esp := espFixture(t)
	nodeMetadata := filepath.Join(root, "node.json")
	if err := os.WriteFile(nodeMetadata, []byte(`{"kind":"NodeMetadata"}`), 0o644); err != nil {
		t.Fatalf("write node metadata: %v", err)
	}
	fixtureManifest := writeInstalledFixtureManifest(t, root, disk, esp, nodeMetadata)
	parent, err := NewRunner(Options{
		StateRoot: root,
		RunID:     "run-1",
	}).Plan(Scenario{Name: "two-node"})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	_, vmConfig := vmFixture(t)
	vmConfig.Expect = "never-ready"
	vmConfig.Timeout = time.Minute
	vmConfig.VSock = VSockConfig{Enabled: true, GuestCID: 62001}
	runner := VMRunner{
		Executor: failingVMExec{},
		probe: probe{
			lookPath: func(string) (string, error) { return "/usr/bin/qemu-system-x86_64", nil },
			stat:     os.Stat,
			access:   func(string) error { return nil },
			output: func(string, ...string) ([]byte, error) {
				return []byte("vhost-vsock-pci guest-cid=<uint32>"), nil
			},
		},
	}

	_, err = StartInstalledRuntimeNode(context.Background(), parent, InstalledRuntimeNodeConfig{
		Name: "worker-1",
		Runtime: InstalledRuntimeConfig{
			Disk:            disk,
			DiskFormat:      DiskRaw,
			ESPArtifacts:    esp,
			FixtureManifest: fixtureManifest,
			NodeMetadata:    nodeMetadata,
			VM:              vmConfig,
		},
	}, runner)
	if err == nil {
		t.Fatal("StartInstalledRuntimeNode() error = nil")
	}
	planned, planErr := PlannedInstalledRuntimeNodeResult(parent, "worker-1")
	if planErr != nil {
		t.Fatalf("plan worker-1: %v", planErr)
	}
	result := readNodeResult(t, planned.Artifacts.Result)
	if result.Status != StatusFailed || !strings.Contains(result.FailureSummary, "qemu exited before serial signal") {
		t.Fatalf("failure result = %#v", result)
	}
	if len(result.Phases) != 1 || result.Phases[0].Status != StatusFailed || result.Phases[0].Name != "installed-runtime-node-start" {
		t.Fatalf("failure phases = %#v", result.Phases)
	}
}

func TestPlannedInstalledRuntimeNodeResult(t *testing.T) {
	parent, err := NewRunner(Options{
		StateRoot: "/tmp/katl-vmtest",
		RunID:     "run-1",
	}).Plan(Scenario{Name: "two-node"})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	result, err := PlannedInstalledRuntimeNodeResult(parent, " cp 1 ")
	if err != nil {
		t.Fatalf("PlannedInstalledRuntimeNodeResult() error = %v", err)
	}
	wantRunDir := filepath.Join(parent.RunDir, "nodes", "cp-1")
	if result.RunID != "run-1-cp-1" || result.RunDir != wantRunDir {
		t.Fatalf("planned node result runID=%q runDir=%q", result.RunID, result.RunDir)
	}
	if result.Artifacts.QEMUCommand != filepath.Join(wantRunDir, "qemu", "qemu-command.txt") {
		t.Fatalf("planned qemu command = %q", result.Artifacts.QEMUCommand)
	}
	if result.Artifacts.RuntimeSerial != filepath.Join(wantRunDir, "qemu", "runtime-serial.log") {
		t.Fatalf("planned runtime serial = %q", result.Artifacts.RuntimeSerial)
	}
	if result.VSock.Enabled || result.Phases != nil {
		t.Fatalf("planned result = %#v", result)
	}
	if _, err := PlannedInstalledRuntimeNodeResult(parent, " "); err == nil {
		t.Fatal("PlannedInstalledRuntimeNodeResult() error = nil, want empty name rejection")
	}
}

type longRunningVMExec struct {
	ready string
}

func (e longRunningVMExec) Run(ctx context.Context, _ string, _ []string, serial io.Writer) error {
	if e.ready != "" {
		_, _ = io.WriteString(serial, e.ready)
	}
	<-ctx.Done()
	return ctx.Err()
}

type failingVMExec struct{}

func (f failingVMExec) Run(context.Context, string, []string, io.Writer) error {
	return errors.New("qemu failed")
}

func readNodeResult(t *testing.T, path string) Result {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read result %s: %v", path, err)
	}
	var result Result
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("decode result %s: %v", path, err)
	}
	return result
}
