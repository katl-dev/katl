package vmtest

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
		lookPath: func(string) (string, error) { return "/usr/bin/virsh", nil },
		stat:     os.Stat,
		access:   func(string) error { return nil },
	})
	if err != nil {
		t.Fatalf("planVM() error = %v", err)
	}
	if plan.Accel != "kvm" {
		t.Fatalf("Accel = %q", plan.Accel)
	}
	if plan.VirshPath != "/usr/bin/virsh" || plan.DomainName != "katl-run-1" {
		t.Fatalf("plan libvirt fields = %#v", plan)
	}
	wantArgs := []string{"-c", "qemu:///system", "define", filepath.Join(result.QEMUDir, "domain.xml")}
	if strings.Join(plan.Args, "\x00") != strings.Join(wantArgs, "\x00") {
		t.Fatalf("Args = %#v, want %#v", plan.Args, wantArgs)
	}
	for _, want := range []string{
		`<domain type="kvm">`,
		`<name>katl-run-1</name>`,
		`<memory unit="MiB">2048</memory>`,
		`<loader readonly="yes" type="pflash">` + config.OVMFCode + `</loader>`,
		`<nvram>` + filepath.Join(result.QEMUDir, "OVMF_VARS.fd") + `</nvram>`,
		`<source network="default"></source>`,
		`<source file="` + filepath.Join(result.QEMUDir, "efi.img") + `"></source>`,
		`<source file="` + filepath.Join(result.QEMUDir, "vdb.snapshot.qcow2") + `"></source>`,
		`<source file="` + filepath.Join(result.DiskDir, "00-root.qcow2") + `"></source>`,
		`<serial>katl-root</serial>`,
		`<serial type="file">`,
	} {
		if !strings.Contains(plan.DomainXML, want) {
			t.Fatalf("domain XML missing %q:\n%s", want, plan.DomainXML)
		}
	}
}

func TestVMLibvirtNetworkFromConfigAndEnv(t *testing.T) {
	result, config := vmFixture(t)
	config.LibvirtNetwork = "katl-net"

	plan, err := planVM(result, config, probe{
		lookPath: func(string) (string, error) { return "/usr/bin/virsh", nil },
		stat:     os.Stat,
		access:   func(string) error { return nil },
	})
	if err != nil {
		t.Fatalf("planVM() error = %v", err)
	}
	if !strings.Contains(plan.DomainXML, `<source network="katl-net"></source>`) {
		t.Fatalf("domain XML missing configured network:\n%s", plan.DomainXML)
	}

	config.LibvirtNetwork = ""
	plan, err = planVM(result, config, probe{
		lookPath: func(string) (string, error) { return "/usr/bin/virsh", nil },
		stat:     os.Stat,
		access:   func(string) error { return nil },
		env: func(name string) string {
			if name == "KATL_VMTEST_LIBVIRT_NETWORK" {
				return "env-net"
			}
			return ""
		},
	})
	if err != nil {
		t.Fatalf("planVM() error = %v", err)
	}
	if !strings.Contains(plan.DomainXML, `<source network="env-net"></source>`) {
		t.Fatalf("domain XML missing env network:\n%s", plan.DomainXML)
	}
}

func TestVMLibvirtRejectsHostForwards(t *testing.T) {
	result, config := vmFixture(t)
	config.HostForwards = []HostForward{{HostPort: 18080, GuestPort: 8080}}
	_, err := planVM(result, config, probe{
		lookPath: func(string) (string, error) { return "/usr/bin/virsh", nil },
		stat:     os.Stat,
		access:   func(string) error { return nil },
	})
	if err == nil || !strings.Contains(err.Error(), "host forwards") {
		t.Fatalf("planVM() error = %v, want host forwards rejection", err)
	}
}

func TestVMPrepare(t *testing.T) {
	result, config := vmFixture(t)
	plan, err := planVM(result, config, probe{
		lookPath: func(string) (string, error) { return "/usr/bin/virsh", nil },
		stat:     os.Stat,
		access: func(string) error {
			return os.ErrNotExist
		},
	})
	if err != nil {
		t.Fatalf("planVM() error = %v", err)
	}
	if plan.Accel != "qemu" {
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
	if !strings.Contains(string(command), "/usr/bin/virsh -c qemu:///system define "+plan.DomainXMLFile) {
		t.Fatalf("command = %q", command)
	}
	domainXML, err := os.ReadFile(plan.DomainXMLFile)
	if err != nil {
		t.Fatalf("read domain XML: %v", err)
	}
	if !strings.Contains(string(domainXML), `<domain type="qemu">`) {
		t.Fatalf("domain XML = %s", domainXML)
	}
}

func TestExecVMExecutorSetsTMPDIR(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "qemu-tmp")
	var serial strings.Builder
	err := (ExecVMExecutor{TempDir: tmp}).Run(context.Background(), "sh", []string{"-c", "printf %s \"$TMPDIR\""}, &serial)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if got := serial.String(); got != tmp {
		t.Fatalf("TMPDIR = %q, want %q", got, tmp)
	}
	if info, err := os.Stat(tmp); err != nil || !info.IsDir() {
		t.Fatalf("TMPDIR was not created: %v", err)
	}
}

func TestLibvirtVMExecutorCleansUpOnTimeout(t *testing.T) {
	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "virsh.log")
	virsh := filepath.Join(tmp, "virsh")
	writeExecutable(t, virsh, `#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$KATL_FAKE_VIRSH_LOG"
if [[ "${1:-}" == "-c" ]]; then
    shift 2
fi
case "${1:-}" in
    define|start|destroy|undefine)
        exit 0
        ;;
    domstate)
        printf 'running\n'
        exit 0
        ;;
    *)
        echo "unexpected virsh args: $*" >&2
        exit 40
        ;;
esac
`)
	t.Setenv("KATL_FAKE_VIRSH_LOG", logPath)
	xmlPath := filepath.Join(tmp, "domain.xml")
	if err := os.WriteFile(xmlPath, []byte("<domain/>"), 0o644); err != nil {
		t.Fatalf("write domain XML: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err := LibvirtVMExecutor{
		TempDir:       filepath.Join(tmp, "run-tmp"),
		VirshPath:     virsh,
		URI:           "qemu:///system",
		DomainName:    "katl-run-1",
		DomainXMLFile: xmlPath,
		PollInterval:  time.Millisecond,
	}.Run(ctx, "", nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run() error = %v, want deadline", err)
	}
	log := readScriptFile(t, logPath)
	for _, want := range []string{
		"-c qemu:///system define " + xmlPath,
		"-c qemu:///system start katl-run-1",
		"-c qemu:///system domstate katl-run-1",
		"-c qemu:///system destroy katl-run-1",
		"-c qemu:///system undefine katl-run-1 --nvram",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("virsh log missing %q:\n%s", want, log)
		}
	}
}

func TestLibvirtVMExecutorFailsCrashedDomain(t *testing.T) {
	tmp := t.TempDir()
	virsh := filepath.Join(tmp, "virsh")
	writeExecutable(t, virsh, `#!/usr/bin/env bash
set -euo pipefail
if [[ "${1:-}" == "-c" ]]; then
    shift 2
fi
case "${1:-}" in
    define|start|destroy|undefine)
        exit 0
        ;;
    domstate)
        printf 'crashed\n'
        exit 0
        ;;
    *)
        exit 40
        ;;
esac
`)
	xmlPath := filepath.Join(tmp, "domain.xml")
	if err := os.WriteFile(xmlPath, []byte("<domain/>"), 0o644); err != nil {
		t.Fatalf("write domain XML: %v", err)
	}

	err := LibvirtVMExecutor{
		VirshPath:     virsh,
		URI:           "qemu:///system",
		DomainName:    "katl-run-1",
		DomainXMLFile: xmlPath,
		PollInterval:  time.Millisecond,
	}.Run(context.Background(), "", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "crashed") {
		t.Fatalf("Run() error = %v, want crashed failure", err)
	}
}

func TestLibvirtVMExecutorBoundsCleanup(t *testing.T) {
	tmp := t.TempDir()
	virsh := filepath.Join(tmp, "virsh")
	writeExecutable(t, virsh, `#!/usr/bin/env bash
set -euo pipefail
if [[ "${1:-}" == "-c" ]]; then
    shift 2
fi
case "${1:-}" in
    define|start|undefine)
        exit 0
        ;;
    domstate)
        printf 'running\n'
        exit 0
        ;;
    destroy)
        sleep 1
        exit 0
        ;;
    *)
        exit 40
        ;;
esac
`)
	xmlPath := filepath.Join(tmp, "domain.xml")
	if err := os.WriteFile(xmlPath, []byte("<domain/>"), 0o644); err != nil {
		t.Fatalf("write domain XML: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := LibvirtVMExecutor{
		VirshPath:      virsh,
		URI:            "qemu:///system",
		DomainName:     "katl-run-1",
		DomainXMLFile:  xmlPath,
		PollInterval:   time.Millisecond,
		CleanupTimeout: 5 * time.Millisecond,
	}.Run(ctx, "", nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run() error = %v, want deadline", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("cleanup was not bounded, elapsed %s", elapsed)
	}
}

func TestVMDiskBoot(t *testing.T) {
	result, config := vmFixture(t)
	config.Boot.UKI = ""
	config.Boot.ImageSnapshot = false
	plan, err := planVM(result, config, probe{
		lookPath: func(string) (string, error) { return "/usr/bin/virsh", nil },
		stat:     os.Stat,
		access:   func(string) error { return nil },
	})
	if err != nil {
		t.Fatalf("planVM() error = %v", err)
	}
	if strings.Contains(plan.DomainXML, "katl-efi") {
		t.Fatalf("disk boot XML contains EFI disk:\n%s", plan.DomainXML)
	}
	if !strings.Contains(plan.DomainXML, `<source file="`+config.Boot.Image+`"></source>`) {
		t.Fatalf("disk boot XML = %s", plan.DomainXML)
	}
}

func TestVMDirectKernelBoot(t *testing.T) {
	result, config := vmFixture(t)
	kernel := writeFixture(t, t.TempDir(), "installer.vmlinuz", "kernel")
	initrd := writeFixture(t, t.TempDir(), "installer.initrd", "initrd")
	config.Boot = VMBoot{
		Kernel:      kernel,
		Initrd:      initrd,
		CommandLine: []string{"console=ttyS0,115200n8", "katl.install.mode=auto"},
	}
	config.OVMFCode = ""
	config.OVMFVars = ""
	plan, err := planVM(result, config, probe{
		lookPath: func(string) (string, error) { return "/usr/bin/virsh", nil },
		stat:     os.Stat,
		access:   func(string) error { return nil },
		env:      func(string) string { return "" },
	})
	if err != nil {
		t.Fatalf("planVM() error = %v", err)
	}
	for _, want := range []string{
		`<kernel>` + kernel + `</kernel>`,
		`<initrd>` + initrd + `</initrd>`,
		`<cmdline>console=ttyS0,115200n8 katl.install.mode=auto</cmdline>`,
	} {
		if !strings.Contains(plan.DomainXML, want) {
			t.Fatalf("direct kernel XML missing %q:\n%s", want, plan.DomainXML)
		}
	}
	if strings.Contains(plan.DomainXML, "pflash") || strings.Contains(plan.DomainXML, "OVMF") {
		t.Fatalf("direct kernel XML includes firmware boot media:\n%s", plan.DomainXML)
	}
	if err := prepareVM(plan, config); err != nil {
		t.Fatalf("prepareVM() error = %v", err)
	}
}

func TestVMEFITreeBoot(t *testing.T) {
	result, config := vmFixture(t)
	efiTree := filepath.Join(t.TempDir(), "esp")
	config.Boot.UKI = ""
	config.Boot.EFITree = efiTree
	config.Boot.ImageSnapshot = false
	plan, err := planVM(result, config, probe{
		lookPath: func(string) (string, error) { return "/usr/bin/virsh", nil },
		stat:     os.Stat,
		access:   func(string) error { return nil },
	})
	if err != nil {
		t.Fatalf("planVM() error = %v", err)
	}
	if plan.EFITree != efiTree {
		t.Fatalf("EFITree = %q, want %q", plan.EFITree, efiTree)
	}
	if !strings.Contains(plan.DomainXML, `<source file="`+filepath.Join(result.QEMUDir, "efi.img")+`"></source>`) {
		t.Fatalf("EFI image disk missing from XML:\n%s", plan.DomainXML)
	}
	if !strings.Contains(plan.DomainXML, `<source file="`+config.Boot.Image+`"></source>`) {
		t.Fatalf("disk drive missing from XML:\n%s", plan.DomainXML)
	}
}

func TestVMUKIEFIImage(t *testing.T) {
	result, config := vmFixture(t)
	config.EFIDiskImage = true
	config.MediaRunner = fakePreseedRunner{}
	plan, err := planVM(result, config, probe{
		lookPath: func(string) (string, error) { return "/usr/bin/virsh", nil },
		stat:     os.Stat,
		access:   func(string) error { return nil },
	})
	if err != nil {
		t.Fatalf("planVM() error = %v", err)
	}
	efiImage := filepath.Join(result.QEMUDir, "efi.img")
	if !strings.Contains(plan.DomainXML, `<source file="`+efiImage+`"></source>`) {
		t.Fatalf("EFI image drive missing from XML:\n%s", plan.DomainXML)
	}
	if strings.Contains(plan.DomainXML, "fat:rw:") {
		t.Fatalf("EFI image plan still uses fat:rw:\n%s", plan.DomainXML)
	}
	if err := prepareVM(plan, config); err != nil {
		t.Fatalf("prepareVM() error = %v", err)
	}
	if _, err := os.Stat(efiImage); err != nil {
		t.Fatalf("EFI image missing: %v", err)
	}
}

func TestVMPreseedDrive(t *testing.T) {
	result, config := vmFixture(t)
	result.Disks = []DiskPlan{{
		Name:     "root",
		Format:   DiskQCOW2,
		HostPath: filepath.Join(result.DiskDir, "00-root.qcow2"),
	}}
	preseed := filepath.Join(t.TempDir(), "preseed")
	if err := os.MkdirAll(preseed, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	config.PreseedDir = preseed
	config.MediaRunner = fakePreseedRunner{}

	plan, err := planVM(result, config, probe{
		lookPath: func(string) (string, error) { return "/usr/bin/virsh", nil },
		stat:     os.Stat,
		access:   func(string) error { return nil },
	})
	if err != nil {
		t.Fatalf("planVM() error = %v", err)
	}
	preseedImage := filepath.Join(result.QEMUDir, "preseed.img")
	if !strings.Contains(plan.DomainXML, `<source file="`+preseedImage+`"></source>`) || !strings.Contains(plan.DomainXML, `<serial>katl-seed</serial>`) {
		t.Fatalf("preseed disk missing from XML:\n%s", plan.DomainXML)
	}
	if err := prepareVM(plan, config); err != nil {
		t.Fatalf("prepareVM() error = %v", err)
	}
}

func TestVMPreseedImage(t *testing.T) {
	result, config := vmFixture(t)
	result.Disks = []DiskPlan{{
		Name:     "root",
		Format:   DiskQCOW2,
		HostPath: filepath.Join(result.DiskDir, "00-root.qcow2"),
	}}
	image := filepath.Join(t.TempDir(), "preseed.img")
	if err := os.WriteFile(image, []byte("seed"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	config.PreseedImage = image

	plan, err := planVM(result, config, probe{
		lookPath: func(string) (string, error) { return "/usr/bin/virsh", nil },
		stat:     os.Stat,
		access:   func(string) error { return nil },
	})
	if err != nil {
		t.Fatalf("planVM() error = %v", err)
	}
	if !strings.Contains(plan.DomainXML, `<source file="`+image+`"></source>`) || !strings.Contains(plan.DomainXML, `<serial>katl-seed</serial>`) {
		t.Fatalf("preseed image disk missing from XML:\n%s", plan.DomainXML)
	}
	if err := prepareVM(plan, config); err != nil {
		t.Fatalf("prepareVM() error = %v", err)
	}
}

func TestVMVSock(t *testing.T) {
	result, config := vmFixture(t)
	config.VSock.Enabled = true
	config.VSock.GuestCID = 2048
	plan, err := planVM(result, config, probe{
		lookPath: func(string) (string, error) { return "/usr/bin/virsh", nil },
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
	if plan.VSock.Device != "virtio-vsock,cid=2048" {
		t.Fatalf("vsock device = %q", plan.VSock.Device)
	}
	if !strings.Contains(plan.DomainXML, `<vsock model="virtio">`) || !strings.Contains(plan.DomainXML, `<cid auto="no" address="2048"></cid>`) {
		t.Fatalf("vsock missing from XML:\n%s", plan.DomainXML)
	}
	runner := VMRunner{
		Executor: vmExec{write: "serial ready"},
		probe: probe{
			lookPath: func(string) (string, error) { return "/usr/bin/virsh", nil },
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
		lookPath: func(string) (string, error) { return "/usr/bin/virsh", nil },
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
		lookPath: func(string) (string, error) { return "/usr/bin/virsh", nil },
		stat:     os.Stat,
		access:   func(string) error { return nil },
	})
	if err != nil {
		t.Fatalf("planVM() error = %v", err)
	}
}

func TestVMRun(t *testing.T) {
	result, config := vmFixture(t)
	runner := VMRunner{
		Executor: vmExec{write: "serial ready"},
		probe: probe{
			lookPath: func(string) (string, error) { return "/usr/bin/virsh", nil },
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
			lookPath: func(string) (string, error) { return "/usr/bin/virsh", nil },
			stat:     os.Stat,
			access:   func(string) error { return nil },
		},
	}
	result = runner.Run(context.Background(), result, config)
	if result.Status != StatusPassed {
		t.Fatalf("Status = %q, failure = %q", result.Status, result.FailureSummary)
	}
}

func TestVMSerialHook(t *testing.T) {
	result, config := vmFixture(t)
	config.Expect = "runtime ready"
	called := false
	config.SerialHooks = []SerialHook{{
		Name:   "handoff",
		Signal: "waiting for config",
		Run: func(_ context.Context, event SerialHookEvent) error {
			called = true
			if !strings.Contains(event.SerialText, "runtime ready") {
				t.Fatalf("SerialText = %q, want final signal", event.SerialText)
			}
			return nil
		},
	}}
	runner := VMRunner{
		Executor: vmExec{write: "waiting for config\nruntime ready\n"},
		probe: probe{
			lookPath: func(string) (string, error) { return "/usr/bin/virsh", nil },
			stat:     os.Stat,
			access:   func(string) error { return nil },
		},
	}
	result = runner.Run(context.Background(), result, config)
	if result.Status != StatusPassed {
		t.Fatalf("Status = %q, failure = %q", result.Status, result.FailureSummary)
	}
	if !called {
		t.Fatal("serial hook was not called")
	}
}

func TestVMFailure(t *testing.T) {
	result, config := vmFixture(t)
	runner := VMRunner{
		Executor: vmExec{err: errors.New("exit status 1")},
		probe: probe{
			lookPath: func(string) (string, error) { return "/usr/bin/virsh", nil },
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
		Executor: vmExec{write: "boot line 1\nboot line 2\n", err: context.DeadlineExceeded},
		probe: probe{
			lookPath: func(string) (string, error) { return "/usr/bin/virsh", nil },
			stat:     os.Stat,
			access:   func(string) error { return nil },
		},
	}
	result = runner.Run(context.Background(), result, config)
	if result.Status != StatusFailed {
		t.Fatalf("Status = %q", result.Status)
	}
	if !strings.Contains(result.FailureSummary, "libvirt domain timed out; serial tail:") || !strings.Contains(result.FailureSummary, "boot line 2") {
		t.Fatalf("FailureSummary = %q", result.FailureSummary)
	}
}

func TestVMSerialIdleTimeout(t *testing.T) {
	result, config := vmFixture(t)
	config.Expect = "runtime ready"
	config.PollInterval = time.Millisecond
	config.SerialIdleTimeout = 5 * time.Millisecond
	runner := VMRunner{
		Executor: vmExec{write: "Boot0002\n", waitForCancel: true},
		probe: probe{
			lookPath: func(string) (string, error) { return "/usr/bin/virsh", nil },
			stat:     os.Stat,
			access:   func(string) error { return nil },
		},
	}

	result = runner.Run(context.Background(), result, config)
	if result.Status != StatusFailed {
		t.Fatalf("Status = %q", result.Status)
	}
	if !strings.Contains(result.FailureSummary, "libvirt domain serial idle timed out") || !strings.Contains(result.FailureSummary, "Boot0002") {
		t.Fatalf("FailureSummary = %q", result.FailureSummary)
	}
}

func TestQEMUTimeoutSummaryWithoutSerial(t *testing.T) {
	if got := qemuTimeoutSummary(filepath.Join(t.TempDir(), "missing.log")); got != "libvirt domain timed out" {
		t.Fatalf("qemuTimeoutSummary() = %q", got)
	}
}

type vmExec struct {
	write         string
	err           error
	waitForCancel bool
}

func (e vmExec) Run(ctx context.Context, _ string, _ []string, serial io.Writer) error {
	if e.write != "" {
		_, _ = io.WriteString(serial, e.write)
	}
	if e.waitForCancel {
		<-ctx.Done()
		return ctx.Err()
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

func hasArgPrefix(args []string, want string) bool {
	for _, arg := range args {
		if strings.HasPrefix(arg, want) {
			return true
		}
	}
	return false
}

func readDomainXML(t *testing.T, result Result) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(result.QEMUDir, "domain.xml"))
	if err != nil {
		t.Fatalf("read domain XML: %v", err)
	}
	return string(data)
}

func vmFixture(t *testing.T) (Result, VMConfig) {
	t.Helper()
	root := t.TempDir()
	code := filepath.Join(root, "OVMF_CODE.fd")
	vars := filepath.Join(root, "OVMF_VARS.fd")
	uki := filepath.Join(root, "installer.efi")
	image := filepath.Join(root, "root.raw")
	imageTool := filepath.Join(root, "qemu-img")
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
	writeExecutable(t, imageTool, `#!/usr/bin/env bash
set -euo pipefail
touch "${@: -1}"
`)
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
		OVMFCode:  code,
		OVMFVars:  vars,
		ImageTool: imageTool,
		KVM:       KVMAuto,
	}
}
