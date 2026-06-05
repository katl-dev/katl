package vmtest

import (
	"context"
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
			Disk:         disk,
			DiskFormat:   DiskRaw,
			ESPArtifacts: esp,
			VM:           vmConfig,
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
	if err := node.Stop(); err != context.Canceled {
		t.Fatalf("Stop() error = %v, want context.Canceled", err)
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
