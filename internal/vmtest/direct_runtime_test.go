package vmtest

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDirectRuntimeBootsRuntimeRootSquashFS(t *testing.T) {
	root := t.TempDir()
	runtimeRoot := writeDirectRuntimeFixture(t, root)
	result, err := NewRunner(Options{
		StateRoot: root,
		RunID:     "run-1",
	}).Plan(Scenario{Name: "direct-runtime"})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	checked := false
	runner := VMRunner{
		Executor: vmExec{write: runtimeBootSignal},
		AgentConnector: func(_ context.Context, plan VSockPlan, transcript string) (AgentHealthClient, error) {
			if plan.GuestCID == 0 || plan.Port != DefaultAgentPort {
				t.Fatalf("vsock plan = %#v", plan)
			}
			if transcript != result.Artifacts.VSockTranscript {
				t.Fatalf("transcript = %q, want %q", transcript, result.Artifacts.VSockTranscript)
			}
			checked = true
			return fakeHealthClient{}, nil
		},
		probe: probe{
			lookPath: func(string) (string, error) { return "/usr/bin/virsh", nil },
			stat:     os.Stat,
			access:   func(string) error { return nil },
			output: func(string, ...string) ([]byte, error) {
				return []byte("vhost-vsock-pci guest-cid=<uint32>"), nil
			},
		},
	}
	result = RunDirectRuntime(context.Background(), result, DirectRuntimeConfig{
		RuntimeRoot:        runtimeRoot,
		RequireVMTestAgent: true,
		VM: VMConfig{
			Timeout:   time.Second,
			VSock:     VSockConfig{GuestCID: 4096},
			Agent:     AgentControlConfig{Timeout: time.Second},
			Expect:    runtimeBootSignal,
			Phase:     "caller-phase-is-overridden",
			RAMMiB:    1024,
			CPUs:      1,
			KVM:       KVMOff,
			ImageTool: filepath.Join(root, "unused-qemu-img"),
		},
	}, runner)
	if result.Status != StatusPassed {
		t.Fatalf("Status = %q, failure = %q", result.Status, result.FailureSummary)
	}
	if !checked {
		t.Fatal("agent health was not checked")
	}
	domainXML := readDomainXML(t, result)
	for _, want := range []string{
		"<kernel>" + filepath.Join(root, "katl-runtime-root", "boot", "fedora", "6.12.0", "linux") + "</kernel>",
		"<initrd>" + filepath.Join(root, "katl-runtime-root", "boot", "fedora", "6.12.0", "initrd") + "</initrd>",
		`<source file="` + runtimeRoot + `"></source>`,
		"root=/dev/vda",
		"rootfstype=squashfs",
		"systemd.volatile=state",
		"systemd.mask=katlc-agent.service",
		"katl.vmtest_agent=1",
	} {
		if !strings.Contains(domainXML, want) {
			t.Fatalf("domain XML missing %q:\n%s", want, domainXML)
		}
	}
	if strings.Contains(domainXML, "<loader") || strings.Contains(domainXML, "<nvram") {
		t.Fatalf("direct runtime boot unexpectedly used OVMF:\n%s", domainXML)
	}
	record := readDirectRuntimeRecord(t, result.Artifacts.DirectRuntime)
	if record.RuntimeRoot != runtimeRoot || !record.RequireVMTestAgent {
		t.Fatalf("direct runtime record = %#v", record)
	}
	for _, want := range []string{"root=/dev/vda", "systemd.volatile=state", "katl.vmtest_agent=1"} {
		if !contains(record.KernelCommandLine, want) {
			t.Fatalf("direct runtime command line missing %q: %#v", want, record.KernelCommandLine)
		}
	}
}

func TestDirectRuntimeRequiresRuntimeRoot(t *testing.T) {
	result, err := NewRunner(Options{
		StateRoot: t.TempDir(),
		RunID:     "run-1",
	}).Plan(Scenario{Name: "direct-runtime"})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	result = RunDirectRuntime(context.Background(), result, DirectRuntimeConfig{}, VMRunner{})
	if result.Status != StatusFailed || !strings.Contains(result.FailureSummary, "direct runtime root squashfs is required") {
		t.Fatalf("result = %#v", result)
	}
}

func TestDirectRuntimeAddsDebugShellOptionWhenEnabled(t *testing.T) {
	t.Setenv("KATL_VMTEST_DEBUG_ON_FAILURE", "1")
	root := t.TempDir()
	runtimeRoot := writeDirectRuntimeFixture(t, root)
	result, err := NewRunner(Options{
		StateRoot: root,
		RunID:     "run-debug-shell",
	}).Plan(Scenario{Name: "direct-runtime"})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	result = RunDirectRuntime(context.Background(), result, DirectRuntimeConfig{
		RuntimeRoot: runtimeRoot,
		VM: VMConfig{
			KVM:       KVMOff,
			ImageTool: filepath.Join(root, "unused-qemu-img"),
		},
	}, VMRunner{
		Executor: vmExec{write: runtimeBootSignal},
		probe: probe{
			lookPath: func(string) (string, error) { return "/usr/bin/virsh", nil },
			stat:     os.Stat,
			access:   func(string) error { return nil },
		},
	})
	if result.Status != StatusPassed {
		t.Fatalf("Status = %q, failure = %q", result.Status, result.FailureSummary)
	}
	record := readDirectRuntimeRecord(t, result.Artifacts.DirectRuntime)
	if !contains(record.KernelCommandLine, runtimeDebugShellOption) {
		t.Fatalf("direct runtime command line missing debug shell option: %#v", record.KernelCommandLine)
	}
}

func writeDirectRuntimeFixture(t *testing.T, root string) string {
	t.Helper()
	runtimeRoot := filepath.Join(root, "katl-runtime-root.squashfs")
	for path, content := range map[string]string{
		runtimeRoot: "runtime",
		filepath.Join(root, "katl-runtime-root", "boot", "fedora", "6.12.0", "linux"):  "kernel",
		filepath.Join(root, "katl-runtime-root", "boot", "fedora", "6.12.0", "initrd"): "initrd",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s) error = %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", path, err)
		}
	}
	return runtimeRoot
}

func readDirectRuntimeRecord(t *testing.T, path string) directRuntimeRecord {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, err)
	}
	var record directRuntimeRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatalf("Unmarshal(%s) error = %v", path, err)
	}
	return record
}
