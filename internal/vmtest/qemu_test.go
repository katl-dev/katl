package vmtest

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestVMPlan(t *testing.T) {
	result, config := vmFixture(t)
	result.Disks = []DiskPlan{{
		Name:            "root",
		Format:          DiskQCOW2,
		HostPath:        filepath.Join(result.DiskDir, "00-root.qcow2"),
		AttachmentOrder: 0,
	}}
	plan, err := planVM(result, config, probe{
		lookPath: func(string) (string, error) { return "/usr/bin/qemu-system-x86_64", nil },
		stat:     os.Stat,
		access:   func(string) error { return nil },
	})
	if err != nil {
		t.Fatalf("planVM() error = %v", err)
	}
	if plan.Accel != "kvm" {
		t.Fatalf("Accel = %q", plan.Accel)
	}
	want := []string{
		"-machine", "q35,accel=kvm",
		"-cpu", "max",
		"-smp", "2",
		"-m", "2048",
		"-display", "none",
		"-monitor", "none",
		"-serial", "file:" + result.Artifacts.InstallerSerial,
		"-drive", "if=pflash,format=raw,readonly=on,file=" + config.OVMFCode,
		"-drive", "if=pflash,format=raw,file=" + filepath.Join(result.QEMUDir, "OVMF_VARS.fd"),
		"-drive", "if=virtio,index=0,format=raw,file=fat:rw:" + filepath.Join(result.QEMUDir, "efi"),
		"-drive", "if=virtio,index=1,format=qcow2,file=" + config.Boot.Image + ",snapshot=on",
		"-drive", "if=virtio,index=2,format=qcow2,file=" + filepath.Join(result.DiskDir, "00-root.qcow2") + ",serial=katl-root",
		"-device", "virtio-rng-pci",
		"-netdev", "user,id=net0,hostfwd=tcp:127.0.0.1:18080-:8080",
		"-device", "virtio-net-pci,netdev=net0",
	}
	if !reflect.DeepEqual(plan.Args, want) {
		t.Fatalf("Args = %#v", plan.Args)
	}
}

func TestVMBridgeNetwork(t *testing.T) {
	result, config := vmFixture(t)
	config.Network = VMNetworkConfig{Mode: VMNetworkBridge, Bridge: "katlbr0"}
	config.HostForwards = nil

	plan, err := planVM(result, config, probe{
		lookPath: func(string) (string, error) { return "/usr/bin/qemu-system-x86_64", nil },
		stat:     os.Stat,
		access:   func(string) error { return nil },
	})
	if err != nil {
		t.Fatalf("planVM() error = %v", err)
	}
	if !hasArg(plan.Args, "bridge,id=net0,br=katlbr0") {
		t.Fatalf("bridge netdev missing from args: %#v", plan.Args)
	}
}

func TestVMBridgeNetworkFromEnv(t *testing.T) {
	result, config := vmFixture(t)
	config.Network = VMNetworkConfig{Mode: VMNetworkBridge}
	config.HostForwards = nil

	plan, err := planVM(result, config, probe{
		lookPath: func(string) (string, error) { return "/usr/bin/qemu-system-x86_64", nil },
		stat:     os.Stat,
		access:   func(string) error { return nil },
		env: func(name string) string {
			switch name {
			case "KATL_VMTEST_BRIDGE":
				return "katlbr1"
			case "KATL_QEMU_BRIDGE_HELPER":
				return "/usr/lib/qemu/qemu-bridge-helper"
			}
			return ""
		},
	})
	if err != nil {
		t.Fatalf("planVM() error = %v", err)
	}
	if !hasArg(plan.Args, "bridge,id=net0,br=katlbr1,helper=/usr/lib/qemu/qemu-bridge-helper") {
		t.Fatalf("bridge netdev missing from args: %#v", plan.Args)
	}
}

func TestVMBridgeNetworkRejectsHostForwardsAndMissingBridge(t *testing.T) {
	result, config := vmFixture(t)
	config.Network = VMNetworkConfig{Mode: VMNetworkBridge, Bridge: "katlbr0"}
	_, err := planVM(result, config, probe{
		lookPath: func(string) (string, error) { return "/usr/bin/qemu-system-x86_64", nil },
		stat:     os.Stat,
		access:   func(string) error { return nil },
	})
	if err == nil || !strings.Contains(err.Error(), "host forwards") {
		t.Fatalf("planVM() error = %v, want host forwards rejection", err)
	}

	config.HostForwards = nil
	config.Network.Bridge = ""
	_, err = planVM(result, config, probe{
		lookPath: func(string) (string, error) { return "/usr/bin/qemu-system-x86_64", nil },
		stat:     os.Stat,
		access:   func(string) error { return nil },
		env:      func(string) string { return "" },
	})
	if err == nil || !strings.Contains(err.Error(), "KATL_VMTEST_BRIDGE") {
		t.Fatalf("planVM() error = %v, want bridge env rejection", err)
	}

	config.Network.Bridge = "../katlbr0"
	_, err = planVM(result, config, probe{
		lookPath: func(string) (string, error) { return "/usr/bin/qemu-system-x86_64", nil },
		stat:     os.Stat,
		access:   func(string) error { return nil },
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported character") {
		t.Fatalf("planVM() error = %v, want invalid bridge name rejection", err)
	}
}

func TestVMPrepare(t *testing.T) {
	result, config := vmFixture(t)
	plan, err := planVM(result, config, probe{
		lookPath: func(string) (string, error) { return "/usr/bin/qemu-system-x86_64", nil },
		stat:     os.Stat,
		access: func(string) error {
			return os.ErrNotExist
		},
	})
	if err != nil {
		t.Fatalf("planVM() error = %v", err)
	}
	if plan.Accel != "tcg" {
		t.Fatalf("Accel = %q", plan.Accel)
	}
	if err := prepareVM(plan, config); err != nil {
		t.Fatalf("prepareVM() error = %v", err)
	}
	vars, err := os.ReadFile(plan.OVMFVars)
	if err != nil {
		t.Fatalf("read vars copy: %v", err)
	}
	if string(vars) != "vars" {
		t.Fatalf("vars copy = %q", vars)
	}
	if _, err := os.Stat(filepath.Join(plan.EFITree, "EFI", "BOOT", "BOOTX64.EFI")); err != nil {
		t.Fatalf("EFI copy missing: %v", err)
	}
	command, err := os.ReadFile(plan.CommandFile)
	if err != nil {
		t.Fatalf("read command: %v", err)
	}
	if !strings.Contains(string(command), "q35,accel=tcg") {
		t.Fatalf("command = %q", command)
	}
}

func TestVMDiskBoot(t *testing.T) {
	result, config := vmFixture(t)
	config.Boot.UKI = ""
	config.Boot.ImageSnapshot = false
	plan, err := planVM(result, config, probe{
		lookPath: func(string) (string, error) { return "/usr/bin/qemu-system-x86_64", nil },
		stat:     os.Stat,
		access:   func(string) error { return nil },
	})
	if err != nil {
		t.Fatalf("planVM() error = %v", err)
	}
	for _, arg := range plan.Args {
		if strings.Contains(arg, "fat:rw:") {
			t.Fatalf("disk boot args contain EFI tree: %#v", plan.Args)
		}
	}
	if !hasArg(plan.Args, "if=virtio,index=0,format=qcow2,file="+config.Boot.Image) {
		t.Fatalf("disk boot args = %#v", plan.Args)
	}
}

func TestVMEFITreeBoot(t *testing.T) {
	result, config := vmFixture(t)
	efiTree := filepath.Join(t.TempDir(), "esp")
	config.Boot.UKI = ""
	config.Boot.EFITree = efiTree
	config.Boot.ImageSnapshot = false
	plan, err := planVM(result, config, probe{
		lookPath: func(string) (string, error) { return "/usr/bin/qemu-system-x86_64", nil },
		stat:     os.Stat,
		access:   func(string) error { return nil },
	})
	if err != nil {
		t.Fatalf("planVM() error = %v", err)
	}
	if !hasArg(plan.Args, "if=virtio,index=0,format=raw,file=fat:rw:"+efiTree) {
		t.Fatalf("EFI tree drive missing from args: %#v", plan.Args)
	}
	if !hasArg(plan.Args, "if=virtio,index=1,format=qcow2,file="+config.Boot.Image) {
		t.Fatalf("disk drive missing from args: %#v", plan.Args)
	}
}

func TestVMVSock(t *testing.T) {
	result, config := vmFixture(t)
	config.VSock.Enabled = true
	config.VSock.GuestCID = 2048
	plan, err := planVM(result, config, probe{
		lookPath: func(string) (string, error) { return "/usr/bin/qemu-system-x86_64", nil },
		stat:     os.Stat,
		access:   func(string) error { return nil },
		output: func(string, ...string) ([]byte, error) {
			return []byte("vhost-vsock-pci guest-cid=<uint32>"), nil
		},
	})
	if err != nil {
		t.Fatalf("planVM() error = %v", err)
	}
	if plan.VSock.GuestCID != 2048 || plan.VSock.Port != 10240 {
		t.Fatalf("vsock = %#v", plan.VSock)
	}
	if !hasArg(plan.Args, "vhost-vsock-pci,id=vsock0,guest-cid=2048") {
		t.Fatalf("vsock device missing from args: %#v", plan.Args)
	}
	runner := VMRunner{
		Executor: vmExec{write: "serial ready"},
		probe: probe{
			lookPath: func(string) (string, error) { return "/usr/bin/qemu-system-x86_64", nil },
			stat:     os.Stat,
			access:   func(string) error { return nil },
			output: func(string, ...string) ([]byte, error) {
				return []byte("vhost-vsock-pci guest-cid=<uint32>"), nil
			},
		},
	}
	result = runner.Run(context.Background(), result, config)
	if result.VSock.GuestCID != 2048 || result.VSock.Port != 10240 {
		t.Fatalf("result vsock = %#v", result.VSock)
	}
}

func TestCID(t *testing.T) {
	first := cidForRun("run-a")
	second := cidForRun("run-a")
	other := cidForRun("run-b")
	if first != second {
		t.Fatalf("cid not deterministic: %d != %d", first, second)
	}
	if first == other {
		t.Fatalf("cid collision for distinct run ids: %d", first)
	}
	if first < 1024 {
		t.Fatalf("cid = %d", first)
	}
	if _, err := reserveExactCID("owner-a", 55000); err != nil {
		t.Fatalf("reserve owner-a: %v", err)
	}
	if _, err := reserveExactCID("owner-b", 55000); err == nil {
		t.Fatal("reserve owner-b succeeded")
	}
}

func TestVSockHostCheck(t *testing.T) {
	result, config := vmFixture(t)
	config.VSock.Enabled = true
	_, err := planVM(result, config, probe{
		lookPath: func(string) (string, error) { return "/usr/bin/qemu-system-x86_64", nil },
		stat:     os.Stat,
		access: func(path string) error {
			if path == "/dev/vhost-vsock" {
				return os.ErrNotExist
			}
			return nil
		},
		output: func(string, ...string) ([]byte, error) {
			return []byte("vhost-vsock-pci guest-cid=<uint32>"), nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "/dev/vhost-vsock") {
		t.Fatalf("planVM() error = %v", err)
	}

	_, err = planVM(result, config, probe{
		lookPath: func(string) (string, error) { return "/usr/bin/qemu-system-x86_64", nil },
		stat:     os.Stat,
		access:   func(string) error { return nil },
		output: func(string, ...string) ([]byte, error) {
			return nil, errors.New("unsupported")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "vhost-vsock-pci") {
		t.Fatalf("planVM() unsupported error = %v", err)
	}
}

func TestVMRun(t *testing.T) {
	result, config := vmFixture(t)
	runner := VMRunner{
		Executor: vmExec{write: "serial ready"},
		probe: probe{
			lookPath: func(string) (string, error) { return "/usr/bin/qemu-system-x86_64", nil },
			stat:     os.Stat,
			access:   func(string) error { return nil },
		},
	}
	result = runner.Run(context.Background(), result, config)
	if result.Status != StatusPassed {
		t.Fatalf("Status = %q", result.Status)
	}
	serial, err := os.ReadFile(result.Artifacts.InstallerSerial)
	if err != nil {
		t.Fatalf("read serial: %v", err)
	}
	if string(serial) != "serial ready" {
		t.Fatalf("serial = %q", serial)
	}
}

func TestVMExpect(t *testing.T) {
	result, config := vmFixture(t)
	config.Expect = "runtime ready"
	runner := VMRunner{
		Executor: vmExec{write: "runtime ready"},
		probe: probe{
			lookPath: func(string) (string, error) { return "/usr/bin/qemu-system-x86_64", nil },
			stat:     os.Stat,
			access:   func(string) error { return nil },
		},
	}
	result = runner.Run(context.Background(), result, config)
	if result.Status != StatusPassed {
		t.Fatalf("Status = %q, failure = %q", result.Status, result.FailureSummary)
	}
}

func TestVMFailure(t *testing.T) {
	result, config := vmFixture(t)
	runner := VMRunner{
		Executor: vmExec{err: errors.New("exit status 1")},
		probe: probe{
			lookPath: func(string) (string, error) { return "/usr/bin/qemu-system-x86_64", nil },
			stat:     os.Stat,
			access:   func(string) error { return nil },
		},
	}
	result = runner.Run(context.Background(), result, config)
	if result.Status != StatusFailed {
		t.Fatalf("Status = %q", result.Status)
	}
	if !strings.Contains(result.FailureSummary, "exit status 1") {
		t.Fatalf("FailureSummary = %q", result.FailureSummary)
	}
}

func TestVMTimeout(t *testing.T) {
	result, config := vmFixture(t)
	runner := VMRunner{
		Executor: vmExec{err: context.DeadlineExceeded},
		probe: probe{
			lookPath: func(string) (string, error) { return "/usr/bin/qemu-system-x86_64", nil },
			stat:     os.Stat,
			access:   func(string) error { return nil },
		},
	}
	result = runner.Run(context.Background(), result, config)
	if result.Status != StatusFailed {
		t.Fatalf("Status = %q", result.Status)
	}
	if result.FailureSummary != "qemu timed out" {
		t.Fatalf("FailureSummary = %q", result.FailureSummary)
	}
}

type vmExec struct {
	write string
	err   error
}

func (e vmExec) Run(_ context.Context, _ string, _ []string, serial io.Writer) error {
	if e.write != "" {
		_, _ = io.WriteString(serial, e.write)
	}
	return e.err
}

func hasArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func vmFixture(t *testing.T) (Result, VMConfig) {
	t.Helper()
	root := t.TempDir()
	code := filepath.Join(root, "OVMF_CODE.fd")
	vars := filepath.Join(root, "OVMF_VARS.fd")
	uki := filepath.Join(root, "installer.efi")
	image := filepath.Join(root, "root.raw")
	for path, content := range map[string]string{
		code:  "code",
		vars:  "vars",
		uki:   "uki",
		image: "image",
	} {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	result, err := NewRunner(Options{
		StateRoot: root,
		RunID:     "run-1",
	}).Plan(Scenario{Name: "boot"})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	return result, VMConfig{
		Boot: VMBoot{
			UKI:           uki,
			Image:         image,
			ImageFormat:   DiskQCOW2,
			ImageSnapshot: true,
		},
		OVMFCode: code,
		OVMFVars: vars,
		KVM:      KVMAuto,
		HostForwards: []HostForward{{
			HostPort:  18080,
			GuestPort: 8080,
		}},
	}
}
