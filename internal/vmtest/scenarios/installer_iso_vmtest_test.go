package scenarios

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/installer/handoff"
	installstatus "github.com/katl-dev/katl/internal/installer/status"
	"github.com/katl-dev/katl/internal/managementidentity"
	"github.com/katl-dev/katl/internal/vmtest"
)

const installerISOTestSSHKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDAxMjM0NTY3ODlhYmNkZWYwMTIzNDU2Nzg5YWJjZGVm katl@example"

func TestInstallerISOBootSmoke(t *testing.T) {
	options := vmtest.DefaultOptions()
	if !options.Enabled {
		t.Skip("set -katl.vmtest.run or KATL_VMTEST_RUN=1 to run installer ISO boot smoke")
	}
	world := vmtest.RequireWorld(t)
	worldScenario := world.NewScenario(t, "installer-iso-boot")
	options.StateRoot = filepath.Join(worldScenario.Dir, "vm-runs")
	options.Keep = vmtest.KeepFailed
	iso := os.Getenv("KATL_INSTALLER_ISO")
	if iso == "" {
		iso = filepath.Join(world.MkosiDir(katlRepoRoot(t)), "katl-installer.iso")
	}
	runner := vmtest.NewRunner(options)
	result, err := vmtest.RunInstallerBoot(
		context.Background(),
		runner,
		vmtest.Scenario{Name: "installer-iso-boot"},
		vmtest.InstallerBootConfig{
			InstallerISO: iso,
			Expect:       "reboot: Power down",
			VM: vmtest.VMConfig{
				Network: worldScenario.VMNetwork(),
				SerialHooks: []vmtest.SerialHook{{
					Name:   "power-button",
					Signal: "katlos-install progress: waiting for configuration at",
					Run: func(ctx context.Context, event vmtest.SerialHookEvent) error {
						command := exec.CommandContext(ctx, event.Plan.VirshPath, "-c", event.Plan.LibvirtURI,
							"shutdown", event.Plan.DomainName, "--mode", "acpi")
						if output, err := command.CombinedOutput(); err != nil {
							return fmt.Errorf("press installer power button: %w: %s", err, output)
						}
						return nil
					},
				}},
				KVM:     options.KVM,
				RAMMiB:  2048,
				CPUs:    2,
				Timeout: 3 * time.Minute,
			},
		},
		vmtest.VMRunner{},
	)
	if err != nil {
		_ = worldScenario.WriteSetupFailure(err)
		t.Fatalf("RunInstallerBoot() error = %v", err)
	}
	if result.Status != vmtest.StatusPassed {
		if err := worldScenario.WriteResult(vmtest.WorldStatusFailed, result.FailureSummary); err != nil {
			t.Fatalf("write failed world result: %v", err)
		}
		t.Fatalf("installer ISO boot status = %q: %s", result.Status, result.FailureSummary)
	}
	if err := worldScenario.WriteResult(vmtest.WorldStatusPassed, ""); err != nil {
		t.Fatalf("write passed world result: %v", err)
	}
}

func TestInstallerPXEBootSmoke(t *testing.T) {
	options := vmtest.DefaultOptions()
	if !options.Enabled {
		t.Skip("set -katl.vmtest.run or KATL_VMTEST_RUN=1 to run installer PXE boot smoke")
	}
	world := vmtest.RequireWorld(t)
	worldScenario := world.NewScenario(t, "installer-pxe-boot")
	options.StateRoot = filepath.Join(worldScenario.Dir, "vm-runs")
	options.Keep = vmtest.KeepFailed
	repo := katlRepoRoot(t)
	katlctl := buildKatlctlCommand(t, context.Background(), repo, filepath.Join(worldScenario.Dir, "katlctl.yaml"))
	privateKey, publicKey, err := vmtest.EnsureWorldSSHKey(world)
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(worldScenario.Dir, "cluster.yaml")
	if err := os.WriteFile(config, []byte(fmt.Sprintf(`apiVersion: config.katl.dev/v1alpha1
kind: ClusterConfig
metadata:
  name: pxe-network
spec:
  controlPlaneEndpoint:
    host: api.pxe.test
  kubernetes:
    version: v1.36.1
  defaults:
    access:
      ssh:
        authorizedKeys: [%q]
  nodes:
    - name: pxe-node
      controlPlane: true
      install:
        systemDisk:
          byID: /dev/disk/by-id/virtio-katl-root
`, publicKey)), 0o600); err != nil {
		t.Fatal(err)
	}
	node, err := worldScenario.AddNode(vmtest.NodeSpec{Name: "pxe-node", Role: vmtest.ControlPlane})
	if err != nil {
		t.Fatal(err)
	}
	network := node.VMNetwork()
	network.ExtraDisconnected = true
	kernel := os.Getenv("KATL_INSTALLER_KERNEL")
	if kernel == "" {
		kernel = filepath.Join(world.MkosiDir(repo), "katl-installer.vmlinuz")
	}
	initrd := os.Getenv("KATL_INSTALLER_INITRD")
	if initrd == "" {
		initrd = filepath.Join(world.MkosiDir(repo), "katl-installer.initrd")
	}
	result, err := vmtest.RunInstallerBoot(
		context.Background(),
		vmtest.NewRunner(options),
		vmtest.Scenario{Name: "installer-pxe-boot"},
		vmtest.InstallerBootConfig{
			InstallerKernel: kernel,
			InstallerInitrd: initrd,
			CommandLine: []string{
				"rd.systemd.unit=katl-installer.target",
				"console=ttyS0,115200n8",
				"systemd.log_target=console",
				"loglevel=6",
			},
			Expect: "katlos-install progress: waiting for configuration at",
			VM: vmtest.VMConfig{
				Network: network,
				HandoffHooks: []vmtest.HandoffHook{{
					Name: "handoff-ready",
					Run: func(ctx context.Context, event vmtest.HandoffEvent) error {
						// Nothing in the installer pulls network-online.target, so
						// katlos-install's own address wait is the network gate.
						// Reaching katlctl and sshd at the reserved address shows
						// the guest holds it despite the disconnected second NIC.
						endpoint, err := url.Parse(event.Base)
						if err != nil {
							return err
						}
						command, err := katlctl.command(ctx, "install", "ssh", "--config", config, "--node", "pxe-node", "--endpoint", endpoint.String())
						if err != nil {
							return err
						}
						if output, err := command.CombinedOutput(); err != nil {
							return fmt.Errorf("enable installer SSH: %w: %s", err, output)
						}
						command = exec.CommandContext(ctx, "ssh", "-F", "/dev/null", "-i", privateKey,
							"-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-o", "StrictHostKeyChecking=no",
							"-o", "UserKnownHostsFile=/dev/null", "root@"+endpoint.Hostname(),
							installerHostReadinessCheck)
						var stderr bytes.Buffer
						command.Stderr = &stderr
						if output, err := command.Output(); err != nil {
							details := strings.TrimSpace(stderr.String() + "\n" + installerHostServiceStatus(string(output)))
							return fmt.Errorf("installer host services were not ready: %w: %s", err, details)
						}
						return nil
					},
				}},
				KVM:     options.KVM,
				RAMMiB:  2048,
				CPUs:    2,
				Timeout: 3 * time.Minute,
			},
		},
		vmtest.VMRunner{},
	)
	if err != nil {
		_ = worldScenario.WriteSetupFailure(err)
		t.Fatalf("RunInstallerBoot() error = %v", err)
	}
	if result.Status != vmtest.StatusPassed {
		if err := worldScenario.WriteResult(vmtest.WorldStatusFailed, result.FailureSummary); err != nil {
			t.Fatalf("write failed world result: %v", err)
		}
		t.Fatalf("installer PXE boot status = %q: %s", result.Status, result.FailureSummary)
	}
	if err := worldScenario.WriteResult(vmtest.WorldStatusPassed, ""); err != nil {
		t.Fatalf("write passed world result: %v", err)
	}
}

// Collect the service state on the same SSH connection, and preserve failure
// even when the diagnostic command succeeds.
const installerHostReadinessCheck = `
unit=systemd-logind.service
if ! systemctl is-active --quiet "$unit"; then
    printf '%s is not active\n' "$unit" >&2
elif test -e /run/nologin; then
    echo '/run/nologin is present' >&2
elif ! test -S /run/dbus/system_bus_socket; then
    echo '/run/dbus/system_bus_socket is missing or is not a socket' >&2
elif ! busctl --system list >/dev/null; then
    echo 'cannot list the D-Bus system bus' >&2
else
    exit 0
fi
systemctl show --all --property=Id,ActiveState,SubState,Result,ExecMainStartTimestampMonotonic "$unit"
exit 1
`

func installerHostServiceStatus(output string) string {
	properties := make(map[string]string)
	for line := range strings.SplitSeq(strings.TrimSpace(output), "\n") {
		if key, value, ok := strings.Cut(line, "="); ok {
			properties[key] = value
		}
	}
	unit := properties["Id"]
	if unit == "" {
		return strings.TrimSpace(output)
	}
	state := properties["ActiveState"]
	result := properties["Result"]
	startedAt, hasStartTimestamp := properties["ExecMainStartTimestampMonotonic"]
	status := state
	if state == "inactive" && hasStartTimestamp && (startedAt == "0" || startedAt == "") {
		status = "never started"
	}
	return fmt.Sprintf("%s: %s (Id=%s ActiveState=%s SubState=%s Result=%s ExecMainStartTimestampMonotonic=%s)",
		unit, status, unit, state, properties["SubState"], result, startedAt)
}

func TestInstallerHostServiceStatus(t *testing.T) {
	for _, tt := range []struct {
		name   string
		output string
		want   string
	}{
		{
			name: "never started",
			output: `Result=success
ExecMainStartTimestampMonotonic=0
Id=systemd-logind.service
ActiveState=inactive
SubState=dead
`,
			want: "systemd-logind.service: never started (Id=systemd-logind.service ActiveState=inactive SubState=dead Result=success ExecMainStartTimestampMonotonic=0)",
		},
		{
			name: "failed before main process",
			output: `Result=exit-code
ExecMainStartTimestampMonotonic=0
Id=systemd-logind.service
ActiveState=failed
SubState=failed
`,
			want: "systemd-logind.service: failed (Id=systemd-logind.service ActiveState=failed SubState=failed Result=exit-code ExecMainStartTimestampMonotonic=0)",
		},
		{
			name: "inactive after starting",
			output: `Result=success
ExecMainStartTimestampMonotonic=2194614
Id=systemd-logind.service
ActiveState=inactive
SubState=dead
`,
			want: "systemd-logind.service: inactive (Id=systemd-logind.service ActiveState=inactive SubState=dead Result=success ExecMainStartTimestampMonotonic=2194614)",
		},
		{
			name: "active",
			output: `Result=success
ExecMainStartTimestampMonotonic=2194614
Id=systemd-logind.service
ActiveState=active
SubState=running
`,
			want: "systemd-logind.service: active (Id=systemd-logind.service ActiveState=active SubState=running Result=success ExecMainStartTimestampMonotonic=2194614)",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := installerHostServiceStatus(tt.output); got != tt.want {
				t.Errorf("service status = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestInstallerISOFirstInstallStorageAuthority(t *testing.T) {
	options := vmtest.DefaultOptions()
	if !options.Enabled {
		t.Skip("set -katl.vmtest.run or KATL_VMTEST_RUN=1 to run installer ISO first-install smoke")
	}
	world := vmtest.RequireWorld(t)
	worldScenario := world.NewScenario(t, "installer-iso-first-install")
	options.StateRoot = filepath.Join(worldScenario.Dir, "vm-runs")
	options.Keep = vmtest.KeepFailed
	iso := os.Getenv("KATL_INSTALLER_ISO")
	if iso == "" {
		iso = filepath.Join(world.MkosiDir(katlRepoRoot(t)), "katl-installer.iso")
	}
	identity, err := managementidentity.Generate(managementidentity.GenerateOptions{ClusterName: "installer-storage"})
	if err != nil {
		t.Fatal(err)
	}
	credentials, _, err := managementidentity.EnsureNode(&identity, "iso-node", time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	management, err := json.Marshal(credentials)
	if err != nil {
		t.Fatal(err)
	}
	manifest := []byte(fmt.Sprintf(`apiVersion: install.katl.dev/v1alpha1
kind: InstallManifest
node:
  identity:
    hostname: iso-node
    management: %s
    ssh:
      authorizedKeys:
        - %s
  systemRole: control-plane
install:
  wipeTarget: true
  targetDisk:
    byID: /dev/disk/by-id/virtio-katl-root
  volumes:
    - name: data
      selector:
        disk:
          byID: /dev/disk/by-id/virtio-katl-data
      filesystem: xfs
      wipe: false
`, management, installerISOTestSSHKey))
	node, err := worldScenario.AddNode(vmtest.NodeSpec{Name: "iso-node", Role: vmtest.ControlPlane})
	if err != nil {
		t.Fatal(err)
	}
	vm := vmtest.VMConfig{
		KVM:     options.KVM,
		RAMMiB:  2048,
		CPUs:    2,
		Timeout: 12 * time.Minute,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	var dataDisk string
	diskRunner := nonBlankDataDiskRunner{dataPath: &dataDisk}
	// This domain reboots from installer media into the installed runtime, so
	// its runtime observer and vsock device must survive that same-domain reboot.
	runtimeVM := vmtest.FirstInstallRuntimeVMConfig(vm)
	result, err := vmtest.RunFirstInstall(ctx, vmtest.NewRunner(options), vmtest.Scenario{
		Name: "installer-iso-first-install-storage-authority",
		Disks: []vmtest.DiskFixture{
			vmtest.TargetDisk("root", string(vmtest.DiskRaw), "32G"),
			vmtest.ExtraDisk("data", string(vmtest.DiskRaw), "4G"),
		},
	}, vmtest.FirstInstallConfig{
		Network:   node.VMNetwork(),
		Installer: vmtest.InstallerBootConfig{InstallerISO: iso, VM: runtimeVM},
		Runtime: vmtest.InstalledRuntimeConfig{
			VM: runtimeVM,
		},
		Manifest:            manifest,
		PreseedManifest:     true,
		GuestHandoff:        true,
		RebootIntoInstalled: true,
		TargetDisk:          vmtest.TargetDisk("root", string(vmtest.DiskRaw), "32G"),
		DiskRunner:          diskRunner,
		HandoffPoster: func(ctx context.Context, endpoint string, payload []byte) (int, string, error) {
			statusURL, err := url.Parse(endpoint)
			if err != nil {
				return 0, "", err
			}
			statusURL.Path = "/v1/status"
			statusURL.RawQuery = ""
			response, err := (&http.Client{Timeout: 10 * time.Second}).Get(statusURL.String())
			if err != nil {
				return 0, "", err
			}
			var observed handoff.HandoffStatus
			err = json.NewDecoder(response.Body).Decode(&observed)
			response.Body.Close()
			if err != nil {
				return 0, "", err
			}
			if observed.State != handoff.HandoffWaiting || observed.InstallStatus.State != installstatus.StateFailedBeforeMutation || observed.InstallStatus.DestructiveMutation || !strings.Contains(observed.InstallStatus.RetryHint, "katlctl install apply") {
				return 0, "", fmt.Errorf("automatic refusal did not remain available for corrected configuration: %+v", observed)
			}
			status, body, err := postInstallerManifest(ctx, endpoint, payload)
			if err != nil {
				return 0, "", err
			}
			if status != http.StatusBadRequest || !strings.Contains(body, "set wipe to true") {
				return 0, "", fmt.Errorf("preserve handoff status=%d body=%s", status, body)
			}
			if dataDisk == "" {
				return 0, "", fmt.Errorf("non-blank data disk path was not recorded")
			}
			filesystem, err := exec.CommandContext(ctx, "blkid", "-p", "-s", "TYPE", "-o", "value", dataDisk).CombinedOutput()
			if err != nil || strings.TrimSpace(string(filesystem)) != "ext4" {
				return 0, "", fmt.Errorf("refused handoff changed existing data disk signature: type=%q err=%v", filesystem, err)
			}
			corrected := bytes.Replace(payload, []byte("wipe: false"), []byte("wipe: true"), 1)
			return postInstallerManifest(ctx, endpoint, corrected)
		},
	})
	if err != nil {
		_ = worldScenario.WriteSetupFailure(err)
		t.Fatalf("RunFirstInstall() error = %v", err)
	}
	if result.Status != vmtest.StatusPassed {
		if err := worldScenario.WriteResult(vmtest.WorldStatusFailed, result.FailureSummary); err != nil {
			t.Fatalf("write failed world result: %v", err)
		}
		t.Fatalf("installer ISO first-install status = %q: %s", result.Status, result.FailureSummary)
	}
	if err := worldScenario.WriteResult(vmtest.WorldStatusPassed, ""); err != nil {
		t.Fatalf("write passed world result: %v", err)
	}
}

type nonBlankDataDiskRunner struct {
	dataPath *string
}

func (r nonBlankDataDiskRunner) Run(ctx context.Context, name string, args ...string) error {
	if err := (vmtest.ExecDiskRunner{}).Run(ctx, name, args...); err != nil {
		return err
	}
	if name != "qemu-img" || len(args) < 2 {
		return nil
	}
	path := args[len(args)-2]
	if !strings.Contains(filepath.Base(path), "-data.") {
		return nil
	}
	if err := exec.CommandContext(ctx, "mkfs.ext4", "-F", path).Run(); err != nil {
		return fmt.Errorf("create existing ext4 data signature: %w", err)
	}
	*r.dataPath = path
	return nil
}

func postInstallerManifest(ctx context.Context, endpoint string, payload []byte) (int, string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return 0, "", err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		return 0, "", err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	return response.StatusCode, string(body), err
}
