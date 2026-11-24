package scenarios

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/katl-dev/katl/internal/installer/handoff"
	"github.com/katl-dev/katl/internal/vmtest"
)

const (
	// Fixed GPT identities let the test tell the operator-prepared layout from
	// the layout Katl writes.
	installerESPDiskGUID      = "6b6b7f0e-2c4e-4b55-9c3a-6b61746c6573"
	installerESPPartitionUUID = "6b6b7f0e-2c4e-4b55-9c3a-657370000001"
)

// An operator copies the release installer UKI to the removable-media path of
// the only disk's ESP. The disk reports no serial, WWN, or by-id alias, as many
// virtual disks do, so Katl selects it by its attachment path. The installer
// then repartitions that same disk from a remote image, and the machine boots
// the installed system without anyone changing firmware settings or disks.
func TestInstallerESPSameDiskInstall(t *testing.T) {
	options := vmtest.DefaultOptions()
	if !options.Enabled {
		t.Skip("set -katl.vmtest.run or KATL_VMTEST_RUN=1 to run the same-disk installer ESP journey")
	}
	world := vmtest.RequireWorld(t)
	worldScenario := world.NewScenario(t, "installer-esp-same-disk")
	options.StateRoot = filepath.Join(worldScenario.Dir, "vm-runs")
	options.Keep = vmtest.KeepFailed
	fail := func(err error) {
		t.Helper()
		_ = worldScenario.WriteResult(vmtest.WorldStatusFailed, err.Error())
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	repo := katlRepoRoot(t)
	katlctl := buildKatlctlCommand(t, ctx, repo, filepath.Join(worldScenario.Dir, "katlctl.yaml"))
	privateKey, _, err := ensureWorldSSHKey(world)
	if err != nil {
		fail(err)
	}
	installerUKI, err := worldArtifactPath(world, "installer-uki")
	if err != nil {
		fail(err)
	}
	image, err := worldArtifactPath(world, "katlos-install-image")
	if err != nil {
		fail(err)
	}
	node, err := worldScenario.AddNode(vmtest.NodeSpec{Name: "cp-1", Role: vmtest.ControlPlane})
	if err != nil {
		fail(err)
	}

	disk := filepath.Join(worldScenario.Dir, "root.raw")
	if err := writeInstallerESPDisk(ctx, worldScenario.Dir, disk, installerUKI); err != nil {
		fail(err)
	}
	imageURL, imageRequests, stopImageServer, err := serveInstallImage(world.Network.Gateway, image)
	if err != nil {
		fail(err)
	}
	defer stopImageServer()

	runner := vmtest.NewRunner(options)
	scenario := vmtest.Scenario{Name: "installer-esp-same-disk"}
	result, err := runner.Plan(scenario)
	if err != nil {
		fail(err)
	}
	// One disk, one NIC, and one NVRAM store for the whole journey: the
	// power cycle below reuses all three unchanged. 2 GiB is the documented
	// minimum installer RAM; the downloaded image is staged in memory.
	vm := vmtest.VMConfig{
		Boot:          vmtest.VMBoot{Image: disk, ImageFormat: vmtest.DiskRaw, ImageWithoutSerial: true},
		Network:       vmtest.VMNetworkConfig{MAC: node.MACAddress},
		PreserveNVRAM: true,
		KVM:           options.KVM,
		RAMMiB:        2048,
		CPUs:          2,
		Timeout:       30 * time.Minute,
	}
	journey := installerESPJourney{
		katlctl:    katlctl,
		privateKey: privateKey,
		publicKey:  privateKey + ".pub",
		dir:        worldScenario.Dir,
		imageURL:   imageURL,
		imageJSON:  image + ".json",
	}
	result = vmtest.VMRunner{}.RunWithVM(ctx, result, vm, func(handle *vmtest.VMHandle) error {
		return journey.installAndPowerOff(ctx, handle)
	})
	if result.Status != vmtest.StatusPassed {
		_ = runner.Write(scenario, result)
		fail(fmt.Errorf("install journey: %s (%s)", result.FailureSummary, result.RunDir))
	}
	if imageRequests.Load() == 0 {
		fail(errors.New("installer never fetched the remote KatlOS image"))
	}

	vm.OVMFVars = filepath.Join(result.VMDir, "OVMF_VARS.fd")
	vm.Timeout = 10 * time.Minute
	result = vmtest.VMRunner{}.RunWithVM(ctx, result, vm, func(handle *vmtest.VMHandle) error {
		return journey.verifyColdBoot(ctx, handle)
	})
	if err := runner.Write(scenario, result); err != nil {
		fail(err)
	}
	if result.Status != vmtest.StatusPassed {
		fail(fmt.Errorf("cold boot: %s (%s)", result.FailureSummary, result.RunDir))
	}
	if err := worldScenario.WriteResult(vmtest.WorldStatusPassed, ""); err != nil {
		t.Fatal(err)
	}
}

type installerESPJourney struct {
	katlctl    katlctlRunner
	privateKey string
	publicKey  string
	dir        string
	imageURL   string
	imageJSON  string

	address string
	// byPath is the attachment path the installer reports for the disk.
	byPath string
}

func (j *installerESPJourney) installAndPowerOff(ctx context.Context, handle *vmtest.VMHandle) error {
	announcement, err := handle.WaitForHandoffAnnouncement(time.Second)
	if err != nil {
		return err
	}
	endpoint, err := url.Parse(announcement.Base)
	if err != nil {
		return err
	}
	j.address = endpoint.Hostname()

	if j.byPath, err = requireSelectableTargetDisk(ctx, endpoint.String()); err != nil {
		return err
	}
	config := filepath.Join(j.dir, "cluster.yaml")
	// Katl does not choose a disk without a hardware identity implicitly, so
	// the operator names it by the attachment path the installer reports.
	if _, err := j.cli(ctx, "config", "init", config, "--installer", j.address, "--ssh-authorized-key", j.publicKey); err == nil || !strings.Contains(err.Error(), "will not choose it implicitly") {
		return fmt.Errorf("config init chose an identity-less disk implicitly: %v", err)
	}
	if _, err := j.cli(ctx, "config", "init", config, "--node", "cp-1=control-plane,"+j.address+","+j.byPath, "--ssh-authorized-key", j.publicKey); err != nil {
		return err
	}
	if err := requireConfiguredSystemDisk(config, j.byPath); err != nil {
		return err
	}
	if _, err := j.cli(ctx, "install", "ssh", "--config", config, "--node", "cp-1"); err != nil {
		return err
	}
	installerBoot, err := j.ssh(ctx, "cat /proc/sys/kernel/random/boot_id")
	if err != nil {
		return err
	}
	if err := j.requireTargetDiskUnused(ctx); err != nil {
		return err
	}

	bundle := filepath.Join(j.dir, "cluster.katlcfg")
	if _, err := j.cli(ctx, "config", "bundle", config, "--output", bundle, "--katlos-image-url", j.imageURL, "--katlos-image-metadata", j.imageJSON); err != nil {
		return err
	}
	if _, err := j.cli(ctx, "install", "apply", "--config", bundle, "--node", "cp-1", "--timeout", "20m"); err != nil {
		return err
	}

	if _, err := j.waitInstalledBoot(ctx, installerBoot); err != nil {
		return fmt.Errorf("automatic reboot: %w", err)
	}
	if err := j.requireGeneration0(ctx, config); err != nil {
		return fmt.Errorf("after automatic reboot: %w", err)
	}
	if err := j.requireRootOnNewLayout(ctx); err != nil {
		return fmt.Errorf("after automatic reboot: %w", err)
	}
	// Firmware that ignores the NVRAM entry written during install still
	// finds a boot loader at the removable-media path the installer used.
	loaders, err := j.ssh(ctx, "sha256sum /efi/EFI/BOOT/BOOTX64.EFI /efi/EFI/systemd/systemd-bootx64.efi")
	if err != nil {
		return fmt.Errorf("installed ESP has no fallback boot loader: %w", err)
	}
	if digests := strings.Fields(loaders); len(digests) != 4 || digests[0] != digests[2] {
		return fmt.Errorf("installed ESP fallback is not systemd-boot:\n%s", loaders)
	}

	command := exec.CommandContext(ctx, handle.Plan.VirshPath, "-c", handle.Plan.LibvirtURI, "shutdown", handle.Plan.DomainName, "--mode", "acpi")
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("power off installed node: %w: %s", err, output)
	}
	if err := handle.Wait(); err != nil {
		return fmt.Errorf("wait for power off: %w", err)
	}
	return nil
}

func (j *installerESPJourney) verifyColdBoot(ctx context.Context, _ *vmtest.VMHandle) error {
	if _, err := j.waitInstalledBoot(ctx, ""); err != nil {
		return err
	}
	if err := j.requireGeneration0(ctx, filepath.Join(j.dir, "cluster.yaml")); err != nil {
		return err
	}
	return j.requireRootOnNewLayout(ctx)
}

// requireSelectableTargetDisk checks that the installer offers the boot disk
// despite it reporting no hardware identity, and returns its attachment path.
func requireSelectableTargetDisk(ctx context.Context, endpoint string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/v1/status", nil)
	if err != nil {
		return "", err
	}
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	var status handoff.HandoffStatus
	if err := json.NewDecoder(response.Body).Decode(&status); err != nil {
		return "", fmt.Errorf("decode installer status: %w", err)
	}
	if len(status.Disks) != 1 {
		return "", fmt.Errorf("installer reports %d disks, want the single boot disk: %+v", len(status.Disks), status.Disks)
	}
	disk := status.Disks[0]
	if len(disk.ByID) != 0 || disk.WWN != "" || disk.Serial != "" {
		return "", fmt.Errorf("boot disk reports a hardware identity, want none: %+v", disk)
	}
	if !disk.Selectable || disk.Mounted || len(disk.ByPath) == 0 || !strings.EqualFold(disk.GPTGUID, installerESPDiskGUID) {
		return "", fmt.Errorf("boot disk is not selectable by attachment path and GPT GUID %s: %+v", installerESPDiskGUID, disk)
	}
	return slices.Min(disk.ByPath), nil
}

func requireConfiguredSystemDisk(path, byPath string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var config struct {
		Spec struct {
			Nodes []struct {
				Install struct {
					SystemDisk struct {
						ByPath string `yaml:"byPath"`
					} `yaml:"systemDisk"`
				} `yaml:"install"`
			} `yaml:"nodes"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(data, &config); err != nil {
		return err
	}
	if len(config.Spec.Nodes) != 1 || config.Spec.Nodes[0].Install.SystemDisk.ByPath != byPath {
		return fmt.Errorf("generated configuration does not select %s:\n%s", byPath, data)
	}
	return nil
}

// The installer must not hold the disk it is about to erase, so neither the
// ESP it booted from nor the bundle and image staging may live on that disk.
func (j *installerESPJourney) requireTargetDiskUnused(ctx context.Context) error {
	disk, err := j.ssh(ctx, "readlink -f "+j.byPath)
	if err != nil {
		return err
	}
	mounts, err := j.ssh(ctx, "cat /proc/self/mountinfo")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(mounts, "\n") {
		fields := strings.Fields(line)
		separator := -1
		for i, field := range fields {
			if field == "-" {
				separator = i
				break
			}
		}
		if separator < 0 || separator+2 >= len(fields) {
			continue
		}
		if source := fields[separator+2]; strings.HasPrefix(source, disk) {
			return fmt.Errorf("installer mounts %s from the target disk: %s", source, line)
		}
	}
	staging, err := j.ssh(ctx, "findmnt --noheadings --output SOURCE,FSTYPE --target /run/katl")
	if err != nil {
		return err
	}
	if fields := strings.Fields(staging); len(fields) != 2 || fields[1] != "tmpfs" {
		return fmt.Errorf("installer staging /run/katl is not RAM-backed: %q", staging)
	}
	return nil
}

func (j *installerESPJourney) waitInstalledBoot(ctx context.Context, previousBoot string) (string, error) {
	deadline, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	var last error
	for {
		boot, err := j.ssh(deadline, "systemctl is-active --quiet katl-boot-complete.target && systemctl is-active --quiet katlc-agent.service && cat /proc/sys/kernel/random/boot_id")
		if err == nil && boot != previousBoot {
			return boot, nil
		}
		last = err
		select {
		case <-deadline.Done():
			return "", fmt.Errorf("installed system did not become ready: %v", last)
		case <-time.After(5 * time.Second):
		}
	}
}

// Boot health reports pending until its checks settle, so only a settled
// health is a verdict.
func (j *installerESPJourney) requireGeneration0(ctx context.Context, config string) error {
	deadline, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	for {
		output, err := j.cli(deadline, "node", "status", "cp-1", "--config", config, "--output", "json")
		if err != nil {
			return err
		}
		var status struct {
			Health     string `json:"health"`
			Generation string `json:"generation"`
		}
		if err := json.Unmarshal(output, &status); err != nil {
			return fmt.Errorf("decode node status: %w: %s", err, output)
		}
		if status.Health != "pending" {
			if !strings.EqualFold(status.Health, "ok") || status.Generation != "0" {
				return fmt.Errorf("node status = health %q generation %q, want OK generation 0", status.Health, status.Generation)
			}
			return nil
		}
		select {
		case <-deadline.Done():
			return fmt.Errorf("node boot health stayed pending: %s", output)
		case <-time.After(5 * time.Second):
		}
	}
}

func (j *installerESPJourney) requireRootOnNewLayout(ctx context.Context) error {
	disk, err := j.ssh(ctx, "readlink -f "+j.byPath)
	if err != nil {
		return err
	}
	chain, err := j.ssh(ctx, `lsblk --noheadings --raw --paths --inverse --output NAME,TYPE,PTUUID,PARTUUID "$(findmnt --noheadings --output SOURCE --target /)"`)
	if err != nil {
		return err
	}
	for _, line := range strings.Split(chain, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[1] != "disk" {
			continue
		}
		if len(fields) < 3 || fields[0] != disk {
			return fmt.Errorf("running root is not on the boot disk %s:\n%s", disk, chain)
		}
		if strings.EqualFold(fields[2], installerESPDiskGUID) {
			return fmt.Errorf("boot disk still carries the operator-prepared partition table:\n%s", chain)
		}
		return nil
	}
	return fmt.Errorf("running root has no backing disk:\n%s", chain)
}

func (j *installerESPJourney) cli(ctx context.Context, args ...string) ([]byte, error) {
	command, err := j.katlctl.command(ctx, args...)
	if err != nil {
		return nil, err
	}
	var stderr strings.Builder
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return output, fmt.Errorf("katlctl %s: %w\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), err, output, stderr.String())
	}
	return output, nil
}

func (j *installerESPJourney) ssh(ctx context.Context, command string) (string, error) {
	probe, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	output, err := exec.CommandContext(probe, "ssh", "-F", "/dev/null", "-i", j.privateKey,
		"-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes", "-o", "ConnectTimeout=5",
		"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "LogLevel=ERROR",
		"root@"+j.address, command).Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return "", fmt.Errorf("ssh %q: %w: %s", command, err, exit.Stderr)
		}
		return "", fmt.Errorf("ssh %q: %w", command, err)
	}
	return strings.TrimSpace(string(output)), nil
}

func worldArtifactPath(world vmtest.World, name string) (string, error) {
	for _, artifact := range world.Artifacts {
		if artifact.Name == name {
			return artifact.Path, nil
		}
	}
	return "", fmt.Errorf("vmtest world has no %s artifact", name)
}

// writeInstallerESPDisk lays out the disk the way an operator would by hand:
// a GPT with one FAT ESP holding the installer at the removable-media path.
func writeInstallerESPDisk(ctx context.Context, workDir, disk, installerUKI string) error {
	const espOffsetMiB, espSizeMiB = 1, 512
	esp := filepath.Join(workDir, "esp.img")
	commands := [][]string{
		{"truncate", "-s", "32G", disk},
		{"truncate", "-s", strconv.Itoa(espSizeMiB) + "M", esp},
		{"mformat", "-i", esp, "-F", "-v", "ESP", "::"},
		{"mmd", "-i", esp, "::/EFI", "::/EFI/BOOT"},
		{"mcopy", "-i", esp, installerUKI, "::/EFI/BOOT/BOOTX64.EFI"},
		{"dd", "if=" + esp, "of=" + disk, "bs=1M", "seek=" + strconv.Itoa(espOffsetMiB), "conv=notrunc,sparse", "status=none"},
	}
	for _, argv := range commands {
		if output, err := exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput(); err != nil {
			return fmt.Errorf("%s: %w: %s", strings.Join(argv, " "), err, output)
		}
	}
	table := fmt.Sprintf("label: gpt\nlabel-id: %s\nstart=%dMiB, size=%dMiB, type=C12A7328-F81F-11D2-BA4B-00A0C93EC93B, uuid=%s, name=\"EFI System\"\n",
		installerESPDiskGUID, espOffsetMiB, espSizeMiB, installerESPPartitionUUID)
	command := exec.CommandContext(ctx, "sfdisk", "--no-reread", "--no-tell-kernel", disk)
	command.Stdin = strings.NewReader(table)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("partition installer disk: %w: %s", err, output)
	}
	return os.Remove(esp)
}

func serveInstallImage(gateway, image string) (string, *atomic.Int64, func(), error) {
	if net.ParseIP(gateway) == nil {
		return "", nil, nil, fmt.Errorf("world network gateway %q is not an IP address", gateway)
	}
	listener, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		return "", nil, nil, err
	}
	name := filepath.Base(image)
	requests := &atomic.Int64{}
	mux := http.NewServeMux()
	mux.HandleFunc("/"+name, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.ServeFile(w, r, image)
	})
	server := &http.Server{Handler: mux}
	go func() { _ = server.Serve(listener) }()
	port := listener.Addr().(*net.TCPAddr).Port
	return "http://" + net.JoinHostPort(gateway, strconv.Itoa(port)) + "/" + name, requests, func() { _ = server.Close() }, nil
}
