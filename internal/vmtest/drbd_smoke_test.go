package vmtest

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/installer/configbundle"
	"github.com/katl-dev/katl/internal/installer/katlosimage"
	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/managementidentity"
)

const drbdToolsRoot = "/var/lib/katl/test-artifacts/drbd-tools"

func TestDRBDConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cluster.yaml")
	if err := os.WriteFile(path, []byte(drbdConfiguration("192.0.2.1", "192.0.2.2", "v1.36.1")), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := configbundle.PlanConfiguration(configbundle.BuildRequest{SourcePath: path}); err != nil {
		t.Fatal(err)
	}
}

// This workload owns only the two disposable data disks. Katl owns installation
// and removal of the driver; drbdadm owns the user-managed replication resource.
func TestInstalledRuntimeDRBDReplication(t *testing.T) {
	if !DefaultOptions().Enabled {
		t.Skip("run with scripts/vmtest-run and release images advertising DRBD9")
	}
	testWorld := RequireWorld(t)
	network, err := netip.ParsePrefix(testWorld.Network.CIDR)
	if err != nil {
		t.Fatal(err)
	}
	toolsImage := first(os.Getenv("KATL_VMTEST_DRBD_TOOLS_IMAGE"), filepath.Join(repoRoot(t), "_build", "mkosi", "katl-vmtest-drbd-tools.raw"))
	if _, err := os.Stat(toolsImage); err != nil {
		t.Fatalf("build test tools with scripts/mkosi --profile vmtest-drbd-tools -f build: %v", err)
	}
	upgradeImage := strings.TrimSpace(os.Getenv("KATL_VMTEST_DRBD_UPGRADE_IMAGE"))
	if upgradeImage == "" {
		t.Fatal("KATL_VMTEST_DRBD_UPGRADE_IMAGE must name a production upgrade image with its exact DRBD release closure")
	}
	upgradeMetadata, err := katlosimage.ReadArtifactMetadata(upgradeImage+".json", katlosimage.RoleUpgrade)
	if err != nil {
		t.Fatal(err)
	}
	if upgradeMetadata.ExtensionRelease == nil || upgradeMetadata.ExtensionRelease.Extensions["ghcr.io/katl-dev/katl/extensions/drbd9"] == "" {
		t.Fatal("qualification target must advertise its release-owned DRBD9 bundle")
	}
	initialImage, initialMetadata := upgradeImage, upgradeMetadata
	previousImage := strings.TrimSpace(os.Getenv("KATL_VMTEST_DRBD_PREVIOUS_IMAGE"))
	if previousImage != "" {
		initialImage = previousImage
		initialMetadata, err = katlosimage.ReadArtifactMetadata(previousImage+".json", katlosimage.RoleUpgrade)
		if err != nil {
			t.Fatal(err)
		}
		if initialMetadata.ExtensionRelease == nil || initialMetadata.ExtensionRelease.Extensions["ghcr.io/katl-dev/katl/extensions/drbd9"] == "" {
			t.Fatal("previous release must advertise DRBD9")
		}
		if initialMetadata.ExtensionRelease.Target.Kernel.Release == upgradeMetadata.ExtensionRelease.Target.Kernel.Release {
			t.Fatal("kernel-upgrade qualification requires two different kernel releases")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	katlctl := buildKatlctlForConfigApplySmoke(t, ctx)
	agentPath := filepath.Join(t.TempDir(), "katl-vmtest-agent")
	build := exec.CommandContext(ctx, "go", "build", "-buildvcs=false", "-o", agentPath, "./cmd/katl-vmtest-agent")
	build.Dir = repoRoot(t)
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build qualification agent: %v\n%s", err, output)
	}
	agentData, err := os.ReadFile(agentPath)
	if err != nil {
		t.Fatal(err)
	}
	var nodes [2]RunningInstalledRuntimeNode
	var clients [2]*AgentClient
	var guests [2]*GuestControl
	var worlds [2]installedRuntimeWorldRun
	var previousGenerations [2]string
	var driverVersion string
	var kubernetesVersion string
	for i := range nodes {
		name := fmt.Sprintf("cp-%d", i+1)
		world, ok := installedRuntimeWorldRunFor(t, "drbd-"+name, NodeSpec{
			Name: name,
			Role: ControlPlane,
		})
		if !ok {
			t.Fatal("DRBD qualification requires a VM-test world")
		}
		worlds[i] = world
		// libvirt can relabel backing files. Keep that ownership change inside
		// the disposable run rather than touching a shared build artifact.
		toolsCopy := filepath.Join(world.Scenario.Dir, "drbd-tools.raw")
		if err := copyFile(toolsImage, toolsCopy, 0o644); err != nil {
			t.Fatal(err)
		}
		scenario := Scenario{
			Name: "drbd-" + name,
			Disks: []DiskFixture{
				ExtraDisk("drbd-data", "raw", "256M"),
				SnapshotDisk("drbd-tools", toolsCopy, DiskRaw),
			},
		}
		result, err := world.Runner.Plan(scenario)
		if err != nil {
			t.Fatal(err)
		}
		result = requirePlannedVMHost(t, world.Runner, scenario, result, HostRequirements{
			Libvirt: true,
			OVMF:    true,
			KVM:     world.Runner.options().KVM,
		})
		if err := CreateDisks(ctx, diskExec(nil), result.Disks); err != nil {
			t.Fatal(err)
		}
		runtime := world.Config
		runtime.VM.RAMMiB = 2048
		runtime.VM.CPUs = 2
		runtime.VM.Timeout = 25 * time.Minute
		runtime.VM.PreserveNVRAM = true
		runtime.VM.Network.MAC = world.Node.MACAddress
		nodes[i], err = StartInstalledRuntimeNode(ctx, result, InstalledRuntimeNodeConfig{
			Name:    name,
			Runtime: runtime,
		}, VMRunner{})
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if clients[i] != nil {
				_ = clients[i].Close()
			}
			if t.Failed() {
				_ = nodes[i].StopFailure("DRBD qualification failed")
			} else {
				// Stop cancels the VM executor after successful qualification.
				if err := nodes[i].Stop(); err != nil && err != context.Canceled {
					t.Errorf("stop DRBD test node: %v", err)
					return
				}
				result.Status = StatusPassed
				if err := CleanupDisks(result); err != nil {
					t.Errorf("clean disposable DRBD disks: %v", err)
				}
			}
		}()
		if nodes[i].Result.IPAddress == "" {
			t.Fatal("installed node has no observed management address")
		}
		clients[i], err = DialAgent(ctx, nodes[i].VSock.GuestCID, nodes[i].VSock.Port, nodes[i].Result.Artifacts.VSockTranscript)
		if err != nil {
			t.Fatal(err)
		}
		guests[i] = NewGuestControl(nodes[i].Result, clients[i])
		installed, err := manifest.Decode(strings.NewReader(guestCommandOutput(t, ctx, guests[i], "installed-intent", "dd", "if=/var/lib/katl/install/manifest.json", "status=none")))
		if err != nil {
			t.Fatal(err)
		}
		if installed.Node.Bootstrap == nil {
			t.Fatal("installed fixture has no bootstrap intent")
		}
		installedVersion := installed.Node.Bootstrap.KubernetesVersion
		if installedVersion == "" || (kubernetesVersion != "" && installedVersion != kubernetesVersion) {
			t.Fatalf("DRBD fixture needs matching Kubernetes bootstrap intent, got %q and %q", kubernetesVersion, installedVersion)
		}
		kubernetesVersion = installedVersion
	}

	directory := filepath.Join(nodes[0].Result.RunDir, "configuration")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	identity, err := VMTestManagementIdentity(VMTestManagementClusterName)
	if err != nil {
		t.Fatal(err)
	}
	if err := managementidentity.Write(filepath.Join(directory, "management", VMTestManagementClusterName+".katlkey"), identity); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KATLCTL_CONFIG", filepath.Join(directory, "katlctl.yaml"))
	t.Setenv("KATLCTL_CONFIG_DIR", "")
	configPath := filepath.Join(directory, "cluster.yaml")
	config := drbdConfiguration(nodes[0].Result.IPAddress, nodes[1].Result.IPAddress, kubernetesVersion)
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := configbundle.PlanConfiguration(configbundle.BuildRequest{SourcePath: configPath}); err != nil {
		t.Fatalf("qualification configuration: %v", err)
	}
	runKatlctl(t, ctx, nodes[0].Result, katlctl, "context", "context", "save", "--config", configPath)
	for i := range nodes {
		previousBoot := guestBootID(t, ctx, clients[i])
		// Test control survives in writable state and user configuration, so the
		// target runtime and kernel bundle are exactly the release artifacts.
		for offset := 0; offset < len(agentData); offset += 512 << 10 {
			end := min(offset+(512<<10), len(agentData))
			if _, err := guests[i].WriteFile(ctx, GuestFileRequest{
				Name:     "qualification-agent",
				Path:     "/var/lib/katl/test-artifacts/katl-vmtest-agent",
				Content:  agentData[offset:end],
				Offset:   uint64(offset),
				Mode:     0o755,
				Truncate: offset == 0,
			}); err != nil {
				t.Fatal(err)
			}
		}
		// The future image supplies the unpromoted extension closure. No registry
		// publication or retained node OCI cache is needed for qualification.
		runKatlctl(t, ctx, nodes[i].Result, katlctl, "add-driver", "node", "upgrade", nodes[i].Name, "--config", configPath, "--artifact", initialImage, "--flavour", first(initialMetadata.Flavour, "standard"), "--apply-config")
		_ = clients[i].Close()
		guests[i], clients[i] = reconnectGuestAfterBoot(t, ctx, &nodes[i], previousBoot)
		assertGuestAddress(t, ctx, guests[i], nodes[i].Result.IPAddress, network.Bits())
		assertGuestMissing(t, ctx, guests[i], "/usr/lib/katl/vmtest/katl-vmtest-agent")
		generation := currentGenerationFromGuest(t, ctx, guests[i])
		previousGenerations[i] = generation
		runKatlctl(t, ctx, nodes[i].Result, katlctl, "unchanged-apply", "cluster", "apply", "--node", nodes[i].Name, "--config", configPath)
		if got := currentGenerationFromGuest(t, ctx, guests[i]); got != generation {
			t.Fatalf("unchanged apply replaced generation %q with %q", generation, got)
		}
		if boot := bootSelectionFromGuest(t, ctx, guests[i]); boot.TrialGenerationID != "" || boot.TargetBootGenerationID != "" {
			t.Fatalf("unchanged apply left a pending boot selection: %+v", boot)
		}
		version := assertDRBDProvider(t, ctx, guests[i], initialMetadata)
		if driverVersion != "" && version != driverVersion {
			t.Fatalf("initial nodes have different DRBD versions: %q, %q", driverVersion, version)
		}
		driverVersion = version
		mountDRBDTools(t, ctx, guests[i])
		resource := fmt.Sprintf(`global { usage-count no; }
resource katl-test {
  protocol C;
  options { auto-promote no; }
  disk /dev/disk/by-id/virtio-katl-drbd-data;
  device /dev/drbd0;
  meta-disk internal;
  on cp-1 { node-id 0; address %s:7789; }
  on cp-2 { node-id 1; address %s:7789; }
  connection-mesh { hosts cp-1 cp-2; }
}
`, nodes[0].Result.IPAddress, nodes[1].Result.IPAddress)
		writeGuestFile(t, ctx, guests[i], drbdToolsRoot+"/etc/drbd.conf", []byte(resource), 0o600)
		drbdCommand(t, ctx, guests[i], "create-metadata", "create-md", "katl-test")
		drbdCommand(t, ctx, guests[i], "start-resource", "up", "katl-test")
	}
	drbdCommand(t, ctx, guests[0], "initial-primary", "primary", "--force", "katl-test")
	drbdCommand(t, ctx, guests[0], "initial-connect", "wait-connect", "katl-test")
	drbdCommand(t, ctx, guests[0], "initial-sync", "wait-sync", "katl-test")
	assertDRBDReplication(t, ctx, guests, "initial replication")

	// Keep one primary while the secondary is disconnected, then prove that
	// bytes written during the interruption reach its actual backing device.
	drbdCommand(t, ctx, guests[1], "interrupt-peer", "down", "katl-test")
	writeDRBDMarker(t, ctx, guests[0], "written while peer was interrupted")
	drbdCommand(t, ctx, guests[1], "recover-peer", "up", "katl-test")
	drbdCommand(t, ctx, guests[0], "recovery-connect", "wait-connect", "katl-test")
	drbdCommand(t, ctx, guests[0], "recovery-sync", "wait-sync", "katl-test")
	assertDRBDMarker(t, ctx, guests[1], "written while peer was interrupted")

	drbdCommand(t, ctx, guests[0], "stop-before-reboot", "down", "katl-test")
	guests[0], clients[0] = restartGuestAndReconnect(t, ctx, &nodes[0], guests[0], clients[0])
	assertGuestAddress(t, ctx, guests[0], nodes[0].Result.IPAddress, network.Bits())
	mountDRBDTools(t, ctx, guests[0])
	drbdCommand(t, ctx, guests[0], "start-after-reboot", "up", "katl-test")
	drbdCommand(t, ctx, guests[0], "reboot-connect", "wait-connect", "katl-test")
	drbdCommand(t, ctx, guests[0], "reboot-sync", "wait-sync", "katl-test")
	drbdCommand(t, ctx, guests[0], "resume-primary", "primary", "katl-test")
	assertDRBDReplication(t, ctx, guests, "replication after reboot")
	for i := range nodes {
		drbdCommand(t, ctx, guests[i], "stop-resource", "down", "katl-test")
	}
	if previousImage != "" {
		var upgradedGenerations [2]string
		for _, phase := range []string{"upgrade", "rollback", "return"} {
			metadata := upgradeMetadata
			if phase == "rollback" {
				metadata = initialMetadata
			}
			for i := range nodes {
				beforeBoot := guestBootID(t, ctx, clients[i])
				if phase == "upgrade" {
					runKatlctl(t, ctx, nodes[i].Result, katlctl, "kernel-upgrade", "node", "upgrade", nodes[i].Name, "--config", configPath, "--artifact", upgradeImage, "--flavour", first(upgradeMetadata.Flavour, "standard"))
				} else {
					target := previousGenerations[i]
					if phase == "return" {
						target = upgradedGenerations[i]
					}
					runKatlctl(t, ctx, nodes[i].Result, katlctl, phase+"-select", "node", "generations", "select", target, nodes[i].Name, "--config", configPath)
					runKatlctl(t, ctx, nodes[i].Result, katlctl, phase+"-reboot", "node", "reboot", nodes[i].Name, "--config", configPath)
				}
				_ = clients[i].Close()
				guests[i], clients[i] = reconnectGuestAfterBoot(t, ctx, &nodes[i], beforeBoot)
				assertGuestAddress(t, ctx, guests[i], nodes[i].Result.IPAddress, network.Bits())
				current := currentGenerationFromGuest(t, ctx, guests[i])
				waitGenerationPromotion(t, ctx, guests[i], current)
				if phase == "upgrade" {
					upgradedGenerations[i] = current
				} else if phase == "rollback" && current != previousGenerations[i] || phase == "return" && current != upgradedGenerations[i] {
					t.Fatalf("%s selected unexpected generation %q", phase, current)
				}
				if version := assertDRBDProvider(t, ctx, guests[i], metadata); version != driverVersion {
					t.Fatalf("kernel transition changed DRBD software version from %q to %q", driverVersion, version)
				}
				// Between node upgrades the same configuration must remain a no-op
				// on both releases, including after rolling back a generation.
				for j := range nodes {
					before := currentGenerationFromGuest(t, ctx, guests[j])
					runKatlctl(t, ctx, nodes[j].Result, katlctl, phase+"-reapply", "cluster", "apply", "--node", nodes[j].Name, "--config", configPath)
					boot := bootSelectionFromGuest(t, ctx, guests[j])
					if currentGenerationFromGuest(t, ctx, guests[j]) != before || boot.TrialGenerationID != "" || boot.TargetBootGenerationID != "" {
						t.Fatalf("%s reapply changed node %s generation", phase, nodes[j].Name)
					}
				}
			}
			for i := range nodes {
				mountDRBDTools(t, ctx, guests[i])
				drbdCommand(t, ctx, guests[i], phase+"-resource-up", "up", "katl-test")
			}
			drbdCommand(t, ctx, guests[0], phase+"-connect", "wait-connect", "katl-test")
			drbdCommand(t, ctx, guests[0], phase+"-sync", "wait-sync", "katl-test")
			drbdCommand(t, ctx, guests[0], phase+"-primary", "primary", "katl-test")
			assertDRBDReplication(t, ctx, guests, "replication after "+phase)
			for i := range nodes {
				drbdCommand(t, ctx, guests[i], phase+"-resource-down", "down", "katl-test")
			}
		}
	}
	withoutDriver := strings.Replace(config, "    systemExtensions:\n      - release: ghcr.io/katl-dev/katl/extensions/drbd9", "    systemExtensions: []", 1)
	if err := os.WriteFile(configPath, []byte(withoutDriver), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := range nodes {
		runKatlctl(t, ctx, nodes[i].Result, katlctl, "remove-driver", "cluster", "apply", "--node", nodes[i].Name, "--config", configPath)
		candidate := bootSelectionFromGuest(t, ctx, guests[i]).TrialGenerationID
		if candidate == "" {
			t.Fatal("driver removal did not stage a trial generation")
		}
		guests[i], clients[i] = restartGuestAndReconnect(t, ctx, &nodes[i], guests[i], clients[i])
		waitGenerationPromotion(t, ctx, guests[i], candidate)
		if got := currentGenerationFromGuest(t, ctx, guests[i]); got != candidate {
			t.Fatalf("removal booted generation %q, want %q", got, candidate)
		}
		provider := guestCommandOutput(t, ctx, guests[i], "base-provider", "modprobe", "--show-depends", "drbd")
		if strings.Contains(provider, "/extra/katl/") {
			t.Fatalf("removed extension remains in module dependency indexes: %s", provider)
		}
		assertGuestMissing(t, ctx, guests[i], "/sys/module/drbd")
		if err := worlds[i].Scenario.WriteResult(WorldStatusPassed, ""); err != nil {
			t.Fatal(err)
		}
	}
}

func assertDRBDProvider(t *testing.T, ctx context.Context, guest *GuestControl, metadata katlosimage.ArtifactMetadata) string {
	t.Helper()
	if kernel := strings.TrimSpace(guestCommandOutput(t, ctx, guest, "kernel", "uname", "-r")); kernel != metadata.ExtensionRelease.Target.Kernel.Release {
		t.Fatalf("running kernel %q differs from target %q", kernel, metadata.ExtensionRelease.Target.Kernel.Release)
	}
	provider := guestCommandOutput(t, ctx, guest, "provider", "modinfo", "-n", "drbd")
	if !strings.Contains(provider, "/extra/katl/drbd.ko") {
		t.Fatalf("unexpected DRBD provider: %s", provider)
	}
	selected := guestCommandOutput(t, ctx, guest, "selected-identity", "modinfo", "-F", "srcversion", "drbd")
	loaded := readGuestFile(t, ctx, guest, "/sys/module/drbd/srcversion")
	if strings.TrimSpace(selected) == "" || strings.TrimSpace(selected) != strings.TrimSpace(loaded) {
		t.Fatalf("loaded DRBD identity %q differs from selected %q", loaded, selected)
	}
	version := strings.TrimSpace(guestCommandOutput(t, ctx, guest, "driver-version", "modinfo", "-F", "version", "drbd"))
	if version == "" {
		t.Fatal("selected DRBD has no software version")
	}
	return version
}

func mountDRBDTools(t *testing.T, ctx context.Context, guest *GuestControl) {
	t.Helper()
	guestCommand(t, ctx, guest, "tools-directory", "install", "-d", drbdToolsRoot)
	guestCommand(t, ctx, guest, "tools-mount", "mount", "/dev/disk/by-id/virtio-katl-drbd-tools-part1", drbdToolsRoot)
	for _, path := range []string{"dev", "proc", "sys"} {
		guestCommand(t, ctx, guest, "tools-"+path, "mount", "--bind", "/"+path, drbdToolsRoot+"/"+path)
	}
}

func drbdCommand(t *testing.T, ctx context.Context, guest *GuestControl, name string, args ...string) {
	t.Helper()
	guestCommand(t, ctx, guest, name, append([]string{"chroot", drbdToolsRoot, "/usr/sbin/drbdadm"}, args...)...)
}

func writeDRBDMarker(t *testing.T, ctx context.Context, guest *GuestControl, marker string) {
	t.Helper()
	path := "/var/lib/katl/test-artifacts/drbd-marker"
	writeGuestFile(t, ctx, guest, path, []byte(marker), 0o600)
	guestCommand(t, ctx, guest, "write-replicated-bytes", "dd", "if="+path, "of=/dev/drbd0", "conv=fsync")
}

func assertDRBDMarker(t *testing.T, ctx context.Context, guest *GuestControl, marker string) {
	t.Helper()
	// Reading the backing disk does not trust the driver's reported sync state.
	got := guestCommandOutput(t, ctx, guest, "read-replicated-bytes", "dd", "if=/dev/disk/by-id/virtio-katl-drbd-data", "iflag=direct", "bs=4096", "count=1", "status=none")
	if !strings.HasPrefix(got, marker) {
		t.Fatalf("peer backing bytes = %q, want %q", got, marker)
	}
}

func assertDRBDReplication(t *testing.T, ctx context.Context, guests [2]*GuestControl, marker string) {
	t.Helper()
	writeDRBDMarker(t, ctx, guests[0], marker)
	assertDRBDMarker(t, ctx, guests[1], marker)
}

func drbdConfiguration(firstAddress, secondAddress, kubernetesVersion string) string {
	return fmt.Sprintf(`apiVersion: config.katl.dev/v1alpha1
kind: ClusterConfig
metadata:
  name: %s
spec:
  managementAuthentication: mtls
  controlPlaneEndpoint:
    host: %s
    port: 6443
  kubernetes:
    version: %s
  defaults:
    access:
      ssh:
        authorizedKeys:
          - ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDAxMjM0NTY3ODlhYmNkZWYwMTIzNDU2Nzg5YWJjZGVm katl@example
    systemExtensions:
      - release: ghcr.io/katl-dev/katl/extensions/drbd9
    hostConfiguration:
      enabledUnits: [qualification-agent.service]
      fileSets:
        qualification:
          files:
            - path: /etc/systemd/system/qualification-agent.service
              content: |
                [Unit]
                Description=Disposable qualification agent
                After=basic.target
                RequiresMountsFor=/var/lib/katl/test-artifacts
                [Service]
                ExecStart=/var/lib/katl/test-artifacts/katl-vmtest-agent
                Restart=on-failure
                [Install]
                WantedBy=multi-user.target
  nodes:
    - name: cp-1
      controlPlane: true
      install:
        systemDisk:
          byID: /dev/disk/by-id/virtio-katl-root
      management:
        address: %s
    - name: cp-2
      controlPlane: true
      install:
        systemDisk:
          byID: /dev/disk/by-id/virtio-katl-root
      management:
        address: %s
`, VMTestManagementClusterName, firstAddress, kubernetesVersion, firstAddress, secondAddress)
}
