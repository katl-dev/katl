package vmtest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallerBoot(t *testing.T) {
	root := t.TempDir()
	uki := writeFixture(t, root, "katl-installer.efi", "uki")
	runtime := writeFixture(t, root, "katl-runtime-root.squashfs", "runtime")
	_, vmConfig := vmFixture(t)
	vmConfig.Expect = "Katl installer ready"
	runner := NewRunner(Options{
		StateRoot: root,
		RunID:     "run-1",
	})
	result, err := RunInstallerBoot(context.Background(), runner, Scenario{Name: "installer-boot"}, InstallerBootConfig{
		InstallerUKI:    uki,
		RuntimeArtifact: runtime,
		VM:              vmConfig,
	}, VMRunner{
		Executor: vmExec{write: "Katl installer ready"},
		probe: probe{
			lookPath: func(string) (string, error) { return "/usr/bin/virsh", nil },
			stat:     os.Stat,
			access:   func(string) error { return nil },
		},
	})
	if err != nil {
		t.Fatalf("RunInstallerBoot() error = %v", err)
	}
	if result.Status != StatusPassed {
		t.Fatalf("Status = %q, failure = %q", result.Status, result.FailureSummary)
	}
	if serial, err := os.ReadFile(result.Artifacts.InstallerSerial); err != nil || !strings.Contains(string(serial), "Katl installer ready") {
		t.Fatalf("installer serial = %q, err = %v", serial, err)
	}
	domainXML := readDomainXML(t, result)
	if !strings.Contains(domainXML, `<source file="`+filepath.Join(result.VMDir, "efi.img")+`"></source>`) {
		t.Fatalf("installer domain XML missing EFI image:\n%s", domainXML)
	}
	if _, err := os.Stat(filepath.Join(result.VMDir, "efi", "EFI", "BOOT", "BOOTX64.EFI")); err != nil {
		t.Fatalf("installer UKI copy missing: %v", err)
	}
	loaded := readResult(t, result.Artifacts.Result)
	if loaded.Status != StatusPassed || loaded.Artifacts.InstallerSerial == "" {
		t.Fatalf("persisted result = %#v", loaded)
	}
}

func TestInstallerBootDirectKernel(t *testing.T) {
	root := t.TempDir()
	kernel := writeFixture(t, root, "katl-installer.vmlinuz", "kernel")
	initrd := writeFixture(t, root, "katl-installer.initrd", "initrd")
	runtime := writeFixture(t, root, "katl-runtime-root.squashfs", "runtime")
	_, vmConfig := vmFixture(t)
	vmConfig.Expect = "Katl installer ready"
	runner := NewRunner(Options{
		StateRoot: root,
		RunID:     "run-1",
	})
	result, err := RunInstallerBoot(context.Background(), runner, Scenario{Name: "installer-boot-direct"}, InstallerBootConfig{
		InstallerKernel: kernel,
		InstallerInitrd: initrd,
		CommandLine:     []string{"console=ttyS0,115200n8"},
		RuntimeArtifact: runtime,
		VM:              vmConfig,
	}, VMRunner{
		Executor: vmExec{write: "Katl installer ready"},
		probe: probe{
			lookPath: func(string) (string, error) { return "/usr/bin/virsh", nil },
			stat:     os.Stat,
			access:   func(string) error { return nil },
		},
	})
	if err != nil {
		t.Fatalf("RunInstallerBoot() error = %v", err)
	}
	if result.Status != StatusPassed {
		t.Fatalf("Status = %q, failure = %q", result.Status, result.FailureSummary)
	}
	domainXML := readDomainXML(t, result)
	if !strings.Contains(domainXML, `<kernel>`+kernel+`</kernel>`) || strings.Contains(domainXML, "katl-efi") {
		t.Fatalf("installer direct kernel domain XML = %s", domainXML)
	}
}

func TestInstallerBootFailure(t *testing.T) {
	root := t.TempDir()
	runner := NewRunner(Options{
		StateRoot: root,
		RunID:     "run-1",
	})
	result, err := RunInstallerBoot(context.Background(), runner, Scenario{Name: "installer-boot"}, InstallerBootConfig{}, VMRunner{})
	if err != nil {
		t.Fatalf("RunInstallerBoot() error = %v", err)
	}
	if result.Status != StatusFailed {
		t.Fatalf("Status = %q", result.Status)
	}
	if !strings.Contains(result.FailureSummary, "installer UKI or kernel/initrd") {
		t.Fatalf("FailureSummary = %q", result.FailureSummary)
	}
	loaded := readResult(t, result.Artifacts.Result)
	if loaded.Status != StatusFailed {
		t.Fatalf("persisted Status = %q", loaded.Status)
	}
}

func writeFixture(t *testing.T, root, name, content string) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}
