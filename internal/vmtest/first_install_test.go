package vmtest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/installer/handoff"
)

func TestFirstInstall(t *testing.T) {
	root := t.TempDir()
	uki := writeFixture(t, root, "katl-installer.efi", "uki")
	runtime := writeFixture(t, root, "runtime.squashfs", "runtime")
	_, vmConfig := vmFixture(t)
	result, err := RunFirstInstall(context.Background(), NewRunner(Options{
		StateRoot: root,
		RunID:     "run-1",
	}), Scenario{Name: "first-install"}, FirstInstallConfig{
		Installer: InstallerBootConfig{
			InstallerUKI:    uki,
			RuntimeArtifact: runtime,
			VM:              vmConfig,
		},
		Runtime: InstalledRuntimeConfig{
			ESPArtifacts: espFixture(t),
			VM:           vmConfig,
		},
		Manifest:        []byte(firstManifest()),
		TargetDisk:      TargetDisk("root", string(DiskRaw), "64M"),
		DiskRunner:      fileDiskRunner{},
		InstallerRunner: fakeVM("Katl installer ready"),
		RuntimeRunner:   fakeVM(runtimeBootSignal),
	})
	if err != nil {
		t.Fatalf("RunFirstInstall() error = %v", err)
	}
	if result.Status != StatusPassed {
		t.Fatalf("Status = %q, failure = %q", result.Status, result.FailureSummary)
	}
	if manifest, err := os.ReadFile(result.Artifacts.InstallManifest); err != nil || !strings.Contains(string(manifest), "lab-node-01") {
		t.Fatalf("install manifest = %q, err = %v", manifest, err)
	}
	response := readLog(t, result.Artifacts.HandoffResponse)
	if response.StatusCode != 200 || !strings.Contains(response.Body, "install-starting") {
		t.Fatalf("handoff response = %#v", response)
	}
	if command, err := os.ReadFile(result.Artifacts.InstallerLaunchCommand); err != nil || !strings.Contains(string(command), "virsh -c qemu:///system define") {
		t.Fatalf("installer command = %q, err = %v", command, err)
	}
	domainXML := readDomainXML(t, result)
	if strings.Contains(domainXML, "katl-efi") || !strings.Contains(domainXML, `<source file="`+filepath.Join(result.VMDir, "vda.snapshot.qcow2")+`"></source>`) {
		t.Fatalf("runtime domain XML = %s", domainXML)
	}
	if command, err := os.ReadFile(result.Artifacts.RuntimeLaunchCommand); err != nil || !strings.Contains(string(command), "virsh -c qemu:///system define") {
		t.Fatalf("runtime command = %q, err = %v", command, err)
	}
	if _, err := os.Stat(result.Disks[0].HostPath); !os.IsNotExist(err) {
		t.Fatalf("successful target disk kept: %v", err)
	}
}

func TestFirstInstallObservesInstalledReboot(t *testing.T) {
	root := t.TempDir()
	iso := writeFixture(t, root, "katl-installer.iso", "iso")
	_, vmConfig := vmFixture(t)
	const installedGenerationSignal = "katl.generation=0 katl.root-slot=root-a"
	serial := preseedInstallerEvidence() + installerCompletedSignal + "/run/katl/preseed/install-manifest.json\n" + installedGenerationSignal + "\n"
	result, err := RunFirstInstall(context.Background(), NewRunner(Options{
		StateRoot: root,
		RunID:     "run-1",
	}), Scenario{Name: "first-install-reboot"}, FirstInstallConfig{
		Installer: InstallerBootConfig{
			InstallerISO: iso,
			VM:           vmConfig,
		},
		Runtime: InstalledRuntimeConfig{
			Expect: installedGenerationSignal,
			VM:     vmConfig,
		},
		Manifest:            []byte(firstManifest()),
		PreseedManifest:     true,
		RebootIntoInstalled: true,
		TargetDisk:          TargetDisk("root", string(DiskRaw), "64M"),
		DiskRunner:          fileDiskRunner{},
		PreseedRunner:       fakePreseedRunner{},
		InstallerRunner:     fakeVM(serial),
	})
	if err != nil {
		t.Fatalf("RunFirstInstall() error = %v", err)
	}
	if result.Status != StatusPassed {
		t.Fatalf("Status = %q, failure = %q", result.Status, result.FailureSummary)
	}
	if !hasPhase(result, "installed-reboot") || hasPhase(result, "runtime") {
		t.Fatalf("phases = %#v, want same-domain installed reboot", result.Phases)
	}
	domainXML := readDomainXML(t, result)
	if !strings.Contains(domainXML, `<source file="`+result.Disks[0].HostPath+`"></source>`) ||
		!strings.Contains(domainXML, `<boot order="1"></boot>`) ||
		!strings.Contains(domainXML, `<source file="`+iso+`"></source>`) ||
		!strings.Contains(domainXML, `<boot order="2"></boot>`) {
		t.Fatalf("same-domain boot order missing from domain XML:\n%s", domainXML)
	}
	if serial, err := os.ReadFile(result.Artifacts.RuntimeSerial); err != nil || !strings.Contains(string(serial), installedGenerationSignal) {
		t.Fatalf("runtime serial = %q, err = %v", serial, err)
	}
}

func TestFirstInstallFailure(t *testing.T) {
	root := t.TempDir()
	uki := writeFixture(t, root, "katl-installer.efi", "uki")
	_, vmConfig := vmFixture(t)
	result, err := RunFirstInstall(context.Background(), NewRunner(Options{
		StateRoot: root,
		RunID:     "run-1",
	}), Scenario{Name: "first-install"}, FirstInstallConfig{
		Installer: InstallerBootConfig{
			InstallerUKI: uki,
			VM:           vmConfig,
		},
		Runtime: InstalledRuntimeConfig{
			ESPArtifacts: espFixture(t),
			VM:           vmConfig,
		},
		Manifest:        []byte(firstManifest()),
		TargetDisk:      TargetDisk("root", string(DiskRaw), "64M"),
		DiskRunner:      fileDiskRunner{},
		InstallerRunner: fakeVM("Katl installer ready"),
		RuntimeRunner:   fakeVM(""),
	})
	if err != nil {
		t.Fatalf("RunFirstInstall() error = %v", err)
	}
	if result.Status != StatusFailed {
		t.Fatalf("Status = %q", result.Status)
	}
	if _, err := os.Stat(result.Disks[0].HostPath); err != nil {
		t.Fatalf("failed target disk missing: %v", err)
	}
}

func TestRuntimeConfigAcceptsSnapshotTargetDisk(t *testing.T) {
	result := Result{Disks: []DiskPlan{{
		Name:     "root",
		Kind:     DiskSnapshot,
		Format:   DiskQCOW2,
		HostPath: "/tmp/installed-target.snapshot.qcow2",
	}}}
	config, err := runtimeConfig(result, InstalledRuntimeConfig{})
	if err != nil {
		t.Fatalf("runtimeConfig() error = %v", err)
	}
	if config.Disk != "/tmp/installed-target.snapshot.qcow2" || config.DiskFormat != DiskQCOW2 {
		t.Fatalf("runtimeConfig() = disk %q format %q", config.Disk, config.DiskFormat)
	}
	target, err := firstTargetDisk(result)
	if err != nil {
		t.Fatalf("firstTargetDisk() error = %v", err)
	}
	if target.HostPath != "/tmp/installed-target.snapshot.qcow2" {
		t.Fatalf("firstTargetDisk() host path = %q", target.HostPath)
	}
}

func TestFirstInstallFailsFastOnInstallerServiceFailure(t *testing.T) {
	root := t.TempDir()
	uki := writeFixture(t, root, "katl-installer.efi", "uki")
	_, vmConfig := vmFixture(t)
	result, err := RunFirstInstall(context.Background(), NewRunner(Options{
		StateRoot: root,
		RunID:     "run-1",
	}), Scenario{Name: "first-install"}, FirstInstallConfig{
		Installer: InstallerBootConfig{
			InstallerUKI: uki,
			VM:           vmConfig,
		},
		Runtime: InstalledRuntimeConfig{
			ESPArtifacts: espFixture(t),
			VM:           vmConfig,
		},
		Manifest:        []byte(firstManifest()),
		TargetDisk:      TargetDisk("root", string(DiskRaw), "64M"),
		DiskRunner:      fileDiskRunner{},
		InstallerRunner: fakeVMWithExecutor(vmExec{write: "katlos-install.service: Failed with result 'exit-code'.\ncollect facts failed\n"}),
		RuntimeRunner:   fakeVM(runtimeBootSignal),
	})
	if err != nil {
		t.Fatalf("RunFirstInstall() error = %v", err)
	}
	if result.Status != StatusFailed || !strings.Contains(result.FailureSummary, "installer service failed") || !strings.Contains(result.FailureSummary, "collect facts failed") {
		t.Fatalf("Status = %q, failure = %q", result.Status, result.FailureSummary)
	}
}

func TestFirstInstallGuestHandoffUsesAnnouncementURL(t *testing.T) {
	root := t.TempDir()
	uki := writeFixture(t, root, "katl-installer.efi", "uki")
	runtime := writeFixture(t, root, "runtime.squashfs", "runtime")
	_, vmConfig := vmFixture(t)
	vmConfig.HostForwards = nil
	server := handoff.NewHandoffServer(nil)
	handoffPosted := make(chan struct{})
	installerSerial := stagedVMExec{
		first: server.Announcement("http://10.0.2.15:8080") + "\n",
		wait:  handoffPosted,
		then: guestHandoffAcceptedSignal + "/run/katl/install-manifest.json\n" +
			installerCompletedSignal + "/run/katl/install-manifest.json\n",
	}
	result, err := RunFirstInstall(context.Background(), NewRunner(Options{
		StateRoot: root,
		RunID:     "run-1",
	}), Scenario{Name: "first-install-guest-handoff"}, FirstInstallConfig{
		Installer: InstallerBootConfig{
			InstallerUKI:    uki,
			RuntimeArtifact: runtime,
			VM:              vmConfig,
		},
		Runtime: InstalledRuntimeConfig{
			ESPArtifacts: espFixture(t),
			VM:           vmConfig,
		},
		Manifest:     []byte(firstManifest()),
		GuestHandoff: true,
		HandoffPoster: func(ctx context.Context, url string, manifest []byte) (int, string, error) {
			if url != "http://10.0.2.15:8080/v1/install" {
				t.Fatalf("handoff post URL = %q", url)
			}
			status, body, err := postLocal(ctx, server.Handler(), url, FirstInstallConfig{}, manifest)
			if err == nil {
				close(handoffPosted)
			}
			return status, body, err
		},
		TargetDisk:      TargetDisk("root", string(DiskRaw), "64M"),
		DiskRunner:      fileDiskRunner{},
		PreseedRunner:   fakePreseedRunner{},
		InstallerRunner: fakeVMWithExecutor(installerSerial),
		RuntimeRunner:   fakeVM(runtimeBootSignal),
	})
	if err != nil {
		t.Fatalf("RunFirstInstall() error = %v", err)
	}
	if result.Status != StatusPassed {
		t.Fatalf("Status = %q, failure = %q", result.Status, result.FailureSummary)
	}
	request := readLog(t, result.Artifacts.HandoffRequest)
	if request.URL != "http://10.0.2.15:8080/v1/config-bundle" || request.PostURL != "http://10.0.2.15:8080/v1/install" || request.GuestAddress != "10.0.2.15" || request.DomainName == "" || request.SerialLog == "" || !strings.Contains(request.SerialTail, "10.0.2.15") {
		t.Fatalf("handoff request = %#v", request)
	}
	response := readLog(t, result.Artifacts.HandoffResponse)
	if response.StatusCode != 200 || !strings.Contains(response.Body, "install-starting") {
		t.Fatalf("handoff response = %#v", response)
	}
	if _, err := os.Stat(filepath.Join(result.Artifacts.ManifestsDir, "handoff-seed", "install-input.json")); !os.IsNotExist(err) {
		t.Fatalf("handoff seed should not contain install input: %v", err)
	}
	if _, err := os.Stat(filepath.Join(result.Artifacts.ManifestsDir, "handoff-seed", "install-manifest.json")); !os.IsNotExist(err) {
		t.Fatalf("handoff seed should not contain install manifest: %v", err)
	}
	network, err := os.ReadFile(filepath.Join(result.Artifacts.ManifestsDir, "handoff-seed", "etc/systemd/network/80-katl-vmtest-installer-dhcp.network"))
	if err != nil {
		t.Fatalf("read handoff seed networkd file: %v", err)
	}
	if !strings.Contains(string(network), "Name=en*") || !strings.Contains(string(network), "DHCP=yes") || !strings.Contains(string(network), "UseHostname=no") {
		t.Fatalf("handoff seed networkd file = %q", network)
	}
}

func TestFirstInstallGuestHandoffPostsConfigBundle(t *testing.T) {
	root := t.TempDir()
	uki := writeFixture(t, root, "katl-installer.efi", "uki")
	runtime := writeFixture(t, root, "runtime.squashfs", "runtime")
	bundle := writeFixture(t, root, "config.katlcfg", "bundle")
	_, vmConfig := vmFixture(t)
	vmConfig.HostForwards = nil
	posted := make(chan struct{})
	installerSerial := stagedVMExec{
		first: guestHandoffSignal + "http://10.0.2.15:8080\n",
		wait:  posted,
		then:  guestHandoffAcceptedSignal + "/run/katl/config.katlcfg\n" + bundleCompletedSignal + "/run/katl/config.katlcfg\n",
	}
	result, err := RunFirstInstall(context.Background(), NewRunner(Options{
		StateRoot: root,
		RunID:     "run-1",
	}), Scenario{Name: "first-install-guest-handoff-bundle"}, FirstInstallConfig{
		Installer: InstallerBootConfig{
			InstallerUKI:    uki,
			RuntimeArtifact: runtime,
			VM:              vmConfig,
		},
		Runtime: InstalledRuntimeConfig{
			ESPArtifacts: espFixture(t),
			VM:           vmConfig,
		},
		Manifest:     []byte(firstManifest()),
		ConfigBundle: bundle,
		SelectedNode: "cp-1",
		GuestHandoff: true,
		HandoffPoster: func(_ context.Context, url string, payload []byte) (int, string, error) {
			if url != "http://10.0.2.15:8080/config-bundle?node=cp-1" {
				t.Fatalf("handoff post URL = %q", url)
			}
			if string(payload) != "bundle" {
				t.Fatalf("handoff payload = %q", payload)
			}
			close(posted)
			return 200, `{"installStatus":{"state":"install-starting"}}`, nil
		},
		TargetDisk:      TargetDisk("root", string(DiskRaw), "64M"),
		DiskRunner:      fileDiskRunner{},
		PreseedRunner:   fakePreseedRunner{},
		InstallerRunner: fakeVMWithExecutor(installerSerial),
		RuntimeRunner:   fakeVM(runtimeBootSignal),
	})
	if err != nil {
		t.Fatalf("RunFirstInstall() error = %v", err)
	}
	if result.Status != StatusPassed {
		t.Fatalf("Status = %q, failure = %q", result.Status, result.FailureSummary)
	}
	request := readLog(t, result.Artifacts.HandoffRequest)
	if request.PostURL != "http://10.0.2.15:8080/config-bundle?node=cp-1" || request.GuestAddress != "10.0.2.15" {
		t.Fatalf("handoff request = %#v", request)
	}
	if _, err := os.Stat(filepath.Join(result.Artifacts.ManifestsDir, "handoff-seed", "install-input.json")); !os.IsNotExist(err) {
		t.Fatalf("handoff seed should not contain install input: %v", err)
	}
}

func TestFirstInstallGuestHandoffFailureIncludesDebugContext(t *testing.T) {
	root := t.TempDir()
	uki := writeFixture(t, root, "katl-installer.efi", "uki")
	runtime := writeFixture(t, root, "runtime.squashfs", "runtime")
	_, vmConfig := vmFixture(t)
	vmConfig.HostForwards = nil
	server := handoff.NewHandoffServer(nil)
	result, err := RunFirstInstall(context.Background(), NewRunner(Options{
		StateRoot: root,
		RunID:     "run-1",
	}), Scenario{Name: "first-install-guest-handoff-failure"}, FirstInstallConfig{
		Installer: InstallerBootConfig{
			InstallerUKI:    uki,
			RuntimeArtifact: runtime,
			VM:              vmConfig,
		},
		Runtime: InstalledRuntimeConfig{
			ESPArtifacts: espFixture(t),
			VM:           vmConfig,
		},
		Manifest:     []byte(firstManifest()),
		GuestHandoff: true,
		HandoffPoster: func(context.Context, string, []byte) (int, string, error) {
			return 0, "", fmt.Errorf("network unreachable")
		},
		TargetDisk:      TargetDisk("root", string(DiskRaw), "64M"),
		DiskRunner:      fileDiskRunner{},
		PreseedRunner:   fakePreseedRunner{},
		InstallerRunner: fakeVM(server.Announcement("http://10.0.2.15:8080") + "\n"),
		RuntimeRunner:   fakeVM(runtimeBootSignal),
	})
	if err != nil {
		t.Fatalf("RunFirstInstall() error = %v", err)
	}
	if result.Status != StatusFailed {
		t.Fatalf("Status = %q, failure = %q", result.Status, result.FailureSummary)
	}
	for _, want := range []string{"guest handoff post failed", "network unreachable", "guest=10.0.2.15", "domain=katl-run-1", "serial=", "serial tail:"} {
		if !strings.Contains(result.FailureSummary, want) {
			t.Fatalf("failure summary missing %q: %s", want, result.FailureSummary)
		}
	}
	request := readLog(t, result.Artifacts.HandoffRequest)
	if request.GuestAddress != "10.0.2.15" || request.DomainName != "katl-run-1" || request.SerialLog == "" || !strings.Contains(request.SerialTail, "10.0.2.15") {
		t.Fatalf("handoff request = %#v", request)
	}
}

func TestFirstInstallPreseedManifest(t *testing.T) {
	root := t.TempDir()
	uki := writeFixture(t, root, "katl-installer.efi", "uki")
	runtime := writeFixture(t, root, "runtime.squashfs", "runtime")
	_, vmConfig := vmFixture(t)
	vmConfig.HostForwards = nil
	result, err := RunFirstInstall(context.Background(), NewRunner(Options{
		StateRoot: root,
		RunID:     "run-1",
	}), Scenario{Name: "first-install-preseed"}, FirstInstallConfig{
		Installer: InstallerBootConfig{
			InstallerUKI:    uki,
			RuntimeArtifact: runtime,
			VM:              vmConfig,
		},
		Runtime: InstalledRuntimeConfig{
			ESPArtifacts: espFixture(t),
			VM:           vmConfig,
		},
		Manifest:        []byte(firstManifest()),
		PreseedManifest: true,
		TargetDisk:      TargetDisk("root", string(DiskRaw), "64M"),
		DiskRunner:      fileDiskRunner{},
		PreseedRunner:   fakePreseedRunner{},
		InstallerRunner: fakeVM(preseedInstallerEvidence() + installerCompletedSignal + "/run/katl/install-manifest.json\n"),
		RuntimeRunner:   fakeVM(runtimeBootSignal),
	})
	if err != nil {
		t.Fatalf("RunFirstInstall() error = %v", err)
	}
	if result.Status != StatusPassed {
		t.Fatalf("Status = %q, failure = %q", result.Status, result.FailureSummary)
	}
	if _, err := os.Stat(result.Artifacts.HandoffResponse); !os.IsNotExist(err) {
		t.Fatalf("handoff response was written for preseed flow: %v", err)
	}
	if !hasPhase(result, "preseed") {
		t.Fatalf("preseed phase missing: %#v", result.Phases)
	}
	command, err := os.ReadFile(result.Artifacts.InstallerLaunchCommand)
	if err != nil {
		t.Fatalf("read installer command: %v", err)
	}
	if !strings.Contains(string(command), "virsh -c qemu:///system define") {
		t.Fatalf("installer command = %s", command)
	}
	if _, err := os.Stat(filepath.Join(result.Artifacts.ManifestsDir, "preseed.img")); err != nil {
		t.Fatalf("preseed image missing: %v", err)
	}
	input, err := os.ReadFile(filepath.Join(result.Artifacts.ManifestsDir, "preseed", "install-input.json"))
	if err != nil {
		t.Fatalf("read preseed input: %v", err)
	}
	if !strings.Contains(string(input), "/run/katl/preseed/install-manifest.json") {
		t.Fatalf("preseed input = %s", input)
	}
}

func TestFirstInstallPreseedConfigBundle(t *testing.T) {
	root := t.TempDir()
	uki := writeFixture(t, root, "katl-installer.efi", "uki")
	runtime := writeFixture(t, root, "runtime.squashfs", "runtime")
	bundle := writeFixture(t, root, "config.katlcfg", "bundle")
	_, vmConfig := vmFixture(t)
	vmConfig.HostForwards = nil
	result, err := RunFirstInstall(context.Background(), NewRunner(Options{
		StateRoot: root,
		RunID:     "run-1",
	}), Scenario{Name: "first-install-preseed-bundle"}, FirstInstallConfig{
		Installer: InstallerBootConfig{
			InstallerUKI:    uki,
			RuntimeArtifact: runtime,
			VM:              vmConfig,
		},
		Runtime: InstalledRuntimeConfig{
			ESPArtifacts: espFixture(t),
			VM:           vmConfig,
		},
		Manifest:        []byte(firstManifest()),
		ConfigBundle:    bundle,
		SelectedNode:    "cp-1",
		PreseedManifest: true,
		TargetDisk:      TargetDisk("root", string(DiskRaw), "64M"),
		DiskRunner:      fileDiskRunner{},
		PreseedRunner:   fakePreseedRunner{},
		InstallerRunner: fakeVM(preseedConfigBundleEvidence() + bundleCompletedSignal + "/run/katl/preseed/config.katlcfg\n"),
		RuntimeRunner:   fakeVM(runtimeBootSignal),
	})
	if err != nil {
		t.Fatalf("RunFirstInstall() error = %v", err)
	}
	if result.Status != StatusPassed {
		t.Fatalf("Status = %q, failure = %q", result.Status, result.FailureSummary)
	}
	if !hasPhase(result, "preseed") {
		t.Fatalf("preseed phase missing: %#v", result.Phases)
	}
	input, err := os.ReadFile(filepath.Join(result.Artifacts.ManifestsDir, "preseed", "install-input.json"))
	if err != nil {
		t.Fatalf("read preseed input: %v", err)
	}
	if !strings.Contains(string(input), `"/run/katl/preseed/config.katlcfg"`) ||
		!strings.Contains(string(input), `"nodeName": "cp-1"`) ||
		strings.Contains(string(input), "install-manifest.json") {
		t.Fatalf("preseed input = %s", input)
	}
	copied, err := os.ReadFile(filepath.Join(result.Artifacts.ManifestsDir, "preseed", "config.katlcfg"))
	if err != nil {
		t.Fatalf("read copied config bundle: %v", err)
	}
	if string(copied) != "bundle" {
		t.Fatalf("copied config bundle = %q", copied)
	}
	if _, err := os.Stat(filepath.Join(result.Artifacts.ManifestsDir, "preseed", "install-manifest.json")); !os.IsNotExist(err) {
		t.Fatalf("preseed bundle media should not contain install manifest: %v", err)
	}
}

func TestFirstInstallPreseedManifestDoesNotDependOnSerialFormatting(t *testing.T) {
	root := t.TempDir()
	uki := writeFixture(t, root, "katl-installer.efi", "uki")
	runtime := writeFixture(t, root, "runtime.squashfs", "runtime")
	_, vmConfig := vmFixture(t)
	vmConfig.HostForwards = nil
	result, err := RunFirstInstall(context.Background(), NewRunner(Options{
		StateRoot: root,
		RunID:     "run-1",
	}), Scenario{Name: "first-install-preseed"}, FirstInstallConfig{
		Installer: InstallerBootConfig{
			InstallerUKI:    uki,
			RuntimeArtifact: runtime,
			VM:              vmConfig,
		},
		Runtime: InstalledRuntimeConfig{
			ESPArtifacts: espFixture(t),
			VM:           vmConfig,
		},
		Manifest:        []byte(firstManifest()),
		PreseedManifest: true,
		TargetDisk:      TargetDisk("root", string(DiskRaw), "64M"),
		DiskRunner:      fileDiskRunner{},
		PreseedRunner:   fakePreseedRunner{},
		InstallerRunner: fakeVM("katl input: [audit message interleaved]\ncopied /run/katl/preseed/install-input.json to /run/katl/install-input.json\n" + installerCompletedSignal + "/run/katl/install-manifest.json\n"),
		RuntimeRunner:   fakeVM(runtimeBootSignal),
	})
	if err != nil {
		t.Fatalf("RunFirstInstall() error = %v", err)
	}
	if result.Status != StatusPassed {
		t.Fatalf("Status = %q, want passed", result.Status)
	}
	if !hasPhase(result, "preseed") {
		t.Fatalf("phases = %+v, want passed preseed phase", result.Phases)
	}
}

func TestFirstInstallPreseedLocalRef(t *testing.T) {
	root := t.TempDir()
	sourceDir := filepath.Join(root, "manifest")
	payload := filepath.Join(sourceDir, "images", "katlos.squashfs")
	if err := os.MkdirAll(filepath.Dir(payload), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(payload, []byte("image"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	manifestPath := filepath.Join(sourceDir, "install-manifest.json")
	manifest := []byte(strings.Replace(firstManifest(), `"url": "https://example.invalid/katlos-install.squashfs",`, `"localRef": "images/katlos.squashfs",`, 1))
	if err := os.WriteFile(manifestPath, manifest, 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	result, _ := vmFixture(t)
	result.Artifacts.ManifestsDir = filepath.Join(root, "run", "manifests")
	result.Artifacts.InstallManifest = filepath.Join(result.Artifacts.ManifestsDir, "install-manifest.json")

	preseed, err := writePreseedMedia(context.Background(), result, FirstInstallConfig{
		ManifestPath:  manifestPath,
		PreseedRunner: fakePreseedRunner{},
	}, manifest)
	if err != nil {
		t.Fatalf("writePreseedMedia() error = %v", err)
	}
	copied, err := os.ReadFile(filepath.Join(preseed.Dir, "images", "katlos.squashfs"))
	if err != nil {
		t.Fatalf("read copied localRef: %v", err)
	}
	if string(copied) != "image" {
		t.Fatalf("copied localRef = %q", copied)
	}
	if _, err := os.Stat(preseed.Image); err != nil {
		t.Fatalf("preseed image missing: %v", err)
	}
}

func TestFirstInstallPreseedYAMLWithoutLocalRef(t *testing.T) {
	root := t.TempDir()
	result, _ := vmFixture(t)
	result.Artifacts.ManifestsDir = filepath.Join(root, "run", "manifests")
	result.Artifacts.InstallManifest = filepath.Join(result.Artifacts.ManifestsDir, "install-manifest.yaml")
	manifest := []byte("apiVersion: install.katl.dev/v1alpha1\nkind: InstallManifest\n")
	preseed, err := writePreseedMedia(context.Background(), result, FirstInstallConfig{PreseedRunner: fakePreseedRunner{}}, manifest)
	if err != nil {
		t.Fatalf("writePreseedMedia() error = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(preseed.Dir, "install-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(manifest) {
		t.Fatalf("preseed manifest = %q", data)
	}
}

func TestFirstInstallUsesInstalledESPExtractor(t *testing.T) {
	root := t.TempDir()
	uki := writeFixture(t, root, "katl-installer.efi", "uki")
	runtime := writeFixture(t, root, "runtime.squashfs", "runtime")
	sourceESP := espFixture(t)
	_, vmConfig := vmFixture(t)
	var extractedDisk DiskPlan
	result, err := RunFirstInstall(context.Background(), NewRunner(Options{
		StateRoot: root,
		RunID:     "run-1",
		Keep:      KeepAlways,
	}), Scenario{Name: "first-install-installed-esp"}, FirstInstallConfig{
		Installer: InstallerBootConfig{
			InstallerUKI:    uki,
			RuntimeArtifact: runtime,
			VM:              vmConfig,
		},
		Runtime: InstalledRuntimeConfig{
			VM: vmConfig,
		},
		UseInstalledESP: true,
		ESPExtractor: func(_ context.Context, disk DiskPlan, outputDir string) (string, error) {
			extractedDisk = disk
			if err := copyDir(sourceESP, outputDir); err != nil {
				return "", err
			}
			return outputDir, nil
		},
		Manifest:        []byte(firstManifest()),
		TargetDisk:      TargetDisk("root", string(DiskRaw), "64M"),
		DiskRunner:      fileDiskRunner{},
		InstallerRunner: fakeVM("Katl installer ready"),
		RuntimeRunner:   fakeVM(runtimeBootSignal),
	})
	if err != nil {
		t.Fatalf("RunFirstInstall() error = %v", err)
	}
	if result.Status != StatusPassed {
		t.Fatalf("Status = %q, failure = %q", result.Status, result.FailureSummary)
	}
	if extractedDisk.Kind != DiskTarget || extractedDisk.HostPath == "" {
		t.Fatalf("extractor disk = %#v", extractedDisk)
	}
	if _, err := os.Stat(filepath.Join(result.Artifacts.InstalledESP, "loader", "entries")); err != nil {
		t.Fatalf("installed ESP artifacts missing: %v", err)
	}
	input := readInstalledRuntimeInput(t, result.Artifacts.InstalledRuntime)
	if input.ESPArtifacts != result.Artifacts.InstalledESP {
		t.Fatalf("runtime ESP artifacts = %q, want %q", input.ESPArtifacts, result.Artifacts.InstalledESP)
	}
	if !hasPhase(result, "installed-esp") {
		t.Fatalf("installed-esp phase missing: %#v", result.Phases)
	}
}

func TestInstalledESPPartitionSelectsNamedESP(t *testing.T) {
	data := []byte(`{
  "partitiontable": {
    "sectorsize": 4096,
    "partitions": [
      {"name": "root", "type": "4f68bce3-e8cd-4db1-96e7-fbcaf984b709", "start": 10, "size": 20},
      {"name": "KATL_ESP", "type": "21686148-6449-6e6f-744e-656564454649", "start": 30, "size": 40}
    ]
  }
}`)
	partition, sectorSize, err := installedESPPartition(data)
	if err != nil {
		t.Fatalf("installedESPPartition() error = %v", err)
	}
	if partition.Start != 30 || partition.Size != 40 || sectorSize != 4096 {
		t.Fatalf("partition = %#v sectorSize=%d", partition, sectorSize)
	}
}

func TestInstalledESPPartitionSelectsEFIType(t *testing.T) {
	data := []byte(`{
  "partitiontable": {
    "partitions": [
      {"name": "ESP", "type": "C12A7328-F81F-11D2-BA4B-00A0C93EC93B", "start": 2048, "size": 4096}
    ]
  }
}`)
	partition, sectorSize, err := installedESPPartition(data)
	if err != nil {
		t.Fatalf("installedESPPartition() error = %v", err)
	}
	if partition.Start != 2048 || partition.Size != 4096 || sectorSize != 512 {
		t.Fatalf("partition = %#v sectorSize=%d", partition, sectorSize)
	}
}

func TestInstalledESPPartitionRejectsMissingESP(t *testing.T) {
	_, _, err := installedESPPartition([]byte(`{"partitiontable":{"partitions":[{"name":"root","start":1,"size":2}]}}`))
	if err == nil || !strings.Contains(err.Error(), "no KATL_ESP partition") {
		t.Fatalf("installedESPPartition() error = %v, want missing ESP", err)
	}
}

func TestCheckExtractedESPArtifacts(t *testing.T) {
	root := t.TempDir()
	if err := checkExtractedESPArtifacts(root); err == nil || !strings.Contains(err.Error(), "loader/entries") {
		t.Fatalf("checkExtractedESPArtifacts() error = %v, want missing entries", err)
	}
	entry := filepath.Join(root, "loader", "entries", "katl.conf")
	if err := os.MkdirAll(filepath.Dir(entry), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(entry, []byte("title Katl\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := checkExtractedESPArtifacts(root); err != nil {
		t.Fatalf("checkExtractedESPArtifacts() error = %v", err)
	}
}

func TestFirstInstallIgnoresSwitchRootFailureAfterCompletion(t *testing.T) {
	root := t.TempDir()
	uki := writeFixture(t, root, "katl-installer.efi", "uki")
	runtime := writeFixture(t, root, "runtime.squashfs", "runtime")
	_, vmConfig := vmFixture(t)
	result, err := RunFirstInstall(context.Background(), NewRunner(Options{
		StateRoot: root,
		RunID:     "run-1",
	}), Scenario{Name: "first-install-preseed-switch-root-after-complete"}, FirstInstallConfig{
		Installer: InstallerBootConfig{
			InstallerUKI:    uki,
			RuntimeArtifact: runtime,
			VM:              vmConfig,
		},
		Runtime: InstalledRuntimeConfig{
			ESPArtifacts: espFixture(t),
			VM:           vmConfig,
		},
		Manifest:        []byte(firstManifest()),
		PreseedManifest: true,
		TargetDisk:      TargetDisk("root", string(DiskRaw), "64M"),
		DiskRunner:      fileDiskRunner{},
		PreseedRunner:   fakePreseedRunner{},
		InstallerRunner: fakeVM(preseedInstallerEvidence() + installerCompletedSignal + "/run/katl/install-manifest.json\ninitrd-switch-root.service: Failed with result 'exit-code'.\n"),
		RuntimeRunner:   fakeVM(runtimeBootSignal),
	})
	if err != nil {
		t.Fatalf("RunFirstInstall() error = %v", err)
	}
	if result.Status != StatusPassed {
		t.Fatalf("Status = %q, failure = %q", result.Status, result.FailureSummary)
	}
}

func TestFirstInstallGuestHandoffRequiresHook(t *testing.T) {
	root := t.TempDir()
	uki := writeFixture(t, root, "katl-installer.efi", "uki")
	runtime := writeFixture(t, root, "runtime.squashfs", "runtime")
	_, vmConfig := vmFixture(t)
	vmConfig.HostForwards = nil
	result, err := RunFirstInstall(context.Background(), NewRunner(Options{
		StateRoot: root,
		RunID:     "run-1",
	}), Scenario{Name: "first-install-guest-handoff-missing"}, FirstInstallConfig{
		Installer: InstallerBootConfig{
			InstallerUKI:    uki,
			RuntimeArtifact: runtime,
			Expect:          "Katl installer ready",
			VM:              vmConfig,
		},
		Runtime: InstalledRuntimeConfig{
			ESPArtifacts: espFixture(t),
			VM:           vmConfig,
		},
		Manifest:        []byte(firstManifest()),
		GuestHandoff:    true,
		TargetDisk:      TargetDisk("root", string(DiskRaw), "64M"),
		DiskRunner:      fileDiskRunner{},
		InstallerRunner: fakeVM("Katl installer ready"),
		RuntimeRunner:   fakeVM(runtimeBootSignal),
	})
	if err != nil {
		t.Fatalf("RunFirstInstall() error = %v", err)
	}
	if result.Status != StatusFailed || !strings.Contains(result.FailureSummary, "guest handoff response artifact is missing") {
		t.Fatalf("Status = %q, failure = %q", result.Status, result.FailureSummary)
	}
}

func hasPhase(result Result, name string) bool {
	for _, phase := range result.Phases {
		if phase.Name == name {
			return true
		}
	}
	return false
}

type fileDiskRunner struct{}

func (fileDiskRunner) Run(_ context.Context, _ string, args ...string) error {
	path := args[len(args)-2]
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte("disk"), 0o644)
}

func fakeVM(signal string) VMRunner {
	return fakeVMWithExecutor(vmExec{write: signal})
}

func preseedInstallerEvidence() string {
	return strings.Join([]string{
		"katl input: mounted seed device /dev/disk/by-label/KATLSEED at /run/katl/preseed",
		"katl input: copied /run/katl/preseed/install-input.json to /run/katl/install-input.json",
		"katlos-install mode: action=run installMode=auto manifestPath=/run/katl/preseed/install-manifest.json manifestURL= inputMode=offline-media",
		"",
	}, "\n")
}

func preseedConfigBundleEvidence() string {
	return strings.Join([]string{
		"katl input: mounted seed device /dev/disk/by-label/KATLSEED at /run/katl/preseed",
		"katl input: copied /run/katl/preseed/install-input.json to /run/katl/install-input.json",
		"katlos-install mode: action=run installMode=auto bundlePath=/run/katl/preseed/config.katlcfg nodeName=cp-1 inputMode=offline-media",
		"",
	}, "\n")
}

func fakeVMWithExecutor(executor VMExecutor) VMRunner {
	return VMRunner{
		Executor: executor,
		probe: probe{
			lookPath: func(string) (string, error) { return "/usr/bin/virsh", nil },
			stat:     os.Stat,
			access:   func(string) error { return nil },
		},
	}
}

type fakePreseedRunner struct{}

func (fakePreseedRunner) Run(_ context.Context, name string, args ...string) error {
	switch name {
	case "truncate":
		path := args[len(args)-1]
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		return os.WriteFile(path, []byte("seed image"), 0o644)
	case "mformat", "mcopy":
		return nil
	default:
		return fmt.Errorf("unexpected preseed command %s", name)
	}
}

type stagedVMExec struct {
	first string
	wait  <-chan struct{}
	then  string
}

func (e stagedVMExec) Run(ctx context.Context, _ string, _ []string, serial io.Writer) error {
	if e.first != "" {
		_, _ = io.WriteString(serial, e.first)
	}
	if syncer, ok := serial.(interface{ Sync() error }); ok {
		_ = syncer.Sync()
	}
	select {
	case <-e.wait:
	case <-ctx.Done():
		return ctx.Err()
	}
	if e.then != "" {
		_, _ = io.WriteString(serial, e.then)
	}
	return nil
}

func readLog(t *testing.T, path string) handoffLog {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read handoff log: %v", err)
	}
	var log handoffLog
	if err := json.Unmarshal(data, &log); err != nil {
		t.Fatalf("decode handoff log: %v", err)
	}
	return log
}

func firstManifest() string {
	return `{
		"apiVersion": "install.katl.dev/v1alpha1",
		"kind": "InstallManifest",
		"node": {
			"identity": {
				"hostname": "lab-node-01",
				"ssh": {
					"authorizedKeys": [
						"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDAxMjM0NTY3ODlhYmNkZWYwMTIzNDU2Nzg5YWJjZGVm katl@example"
					]
				}
			},
			"systemRole": "control-plane"
		},
		"install": {
    "wipeTarget": true,
			"targetDisk": {"byID": "/dev/disk/by-id/virtio-katl-root", "minSizeMiB": 32}
		},
		"katlosImage": {
			"url": "https://example.invalid/katlos-install.squashfs",
			"sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"sizeBytes": 1073741824,
			"version": "2026.06.04",
			"architecture": "x86_64",
			"runtimeInterface": "katl-runtime-1",
			"role": "install"
		}
	}`
}
