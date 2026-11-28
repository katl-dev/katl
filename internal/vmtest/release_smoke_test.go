package vmtest

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/katlosimage"
	"github.com/katl-dev/katl/internal/persistedrecord"
	"github.com/katl-dev/katl/internal/releaseversion"
)

// Release qualification uses public SSH and the shipped CLI. Neither the
// installer nor the runtime is rebuilt or augmented with test support.
func TestReleaseLifecycle(t *testing.T) {
	if !DefaultOptions().Enabled {
		t.Skip("run with scripts/vmtest-run")
	}
	candidate := os.Getenv("KATL_VMTEST_RELEASE_DIR")
	if candidate == "" {
		t.Skip("set KATL_VMTEST_RELEASE_DIR to a verified production bundle")
	}
	previous := os.Getenv("KATL_VMTEST_PREVIOUS_RELEASE_DIR")
	if previous == "" {
		t.Fatal("KATL_VMTEST_PREVIOUS_RELEASE_DIR is required for release qualification")
	}
	for _, journey := range []struct {
		name, source string
		upgrade      bool
	}{
		{"install", candidate, false}, {"upgrade", previous, true},
	} {
		t.Run(journey.name, func(t *testing.T) { runReleaseLifecycle(t, journey.source, candidate, journey.upgrade) })
	}
}

func releaseFile(t *testing.T, dir, pattern string) string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, pattern))
	if err != nil || len(paths) != 1 {
		t.Fatalf("expected one release file %s: %v, %v", pattern, paths, err)
	}
	return paths[0]
}

// releaseJourney is one installed release node driven over public SSH and
// the shipped CLI.
type releaseJourney struct {
	t       *testing.T
	ctx     context.Context
	result  Result
	address string
	key     string
	public  string
	// installed is the ClusterConfig the node was installed from.
	installed string
	// katlctl runs the journey's primary CLI; runner gives another binary the
	// same workstation config.
	katlctl katlctlRunner
}

func (j *releaseJourney) observe(command string) (string, error) {
	return releaseSSH(j.ctx, j.key, j.address, command)
}

func (j *releaseJourney) runner(bin Katlctl) katlctlRunner {
	return katlctlRunner{bin: bin, config: j.katlctl.config}
}

func (j *releaseJourney) cli(label string, args ...string) {
	runKatlctl(j.t, j.ctx, j.result, j.katlctl, label, args...)
}

// healthy waits until the node runs the expected release with healthy boot
// health, SSH and management, and no VM test agent.
func (j *releaseJourney) healthy(expected katlosimage.ArtifactMetadata) error {
	if expected.ExtensionRelease == nil {
		return fmt.Errorf("release %s has no kernel identity", expected.Version)
	}
	want := expected.Version + "\n" + expected.ExtensionRelease.Target.Kernel.Release
	deadline, stop := context.WithTimeout(j.ctx, 3*time.Minute)
	defer stop()
	var last error
	for {
		text, err := releaseSSH(deadline, j.key, j.address, "systemctl is-active --quiet katl-boot-complete.target && systemctl is-active --quiet sshd.service && systemctl is-active --quiet katlc-agent.service && test \"$(systemctl show katl-boot-health.service -p Result --value)\" = success && test ! -e /usr/lib/katl/vmtest/katl-vmtest-agent && . /usr/lib/os-release && printf '%s\\n' \"$VERSION_ID\" && uname -r")
		if err == nil && text == want {
			return nil
		}
		last = fmt.Errorf("version/health: %q, expected %q: %v", text, want, err)
		select {
		case <-deadline.Done():
			return last
		case <-time.After(time.Second):
		}
	}
}

// rawSelection is the published selection file as the node holds it.
func (j *releaseJourney) rawSelection() (string, error) {
	return j.observe("cat /var/lib/katl/boot/selection.json")
}

// selection decodes the published selection and requires a settled boot.
func (j *releaseJourney) selection() (generation.BootSelectionRecord, error) {
	data, err := j.rawSelection()
	if err != nil {
		return generation.BootSelectionRecord{}, err
	}
	var envelope persistedrecord.Envelope
	if err := json.Unmarshal([]byte(data), &envelope); err != nil {
		return generation.BootSelectionRecord{}, err
	}
	value, err := persistedrecord.DecodePayload[generation.BootSelectionRecord](envelope)
	if err != nil {
		return value, err
	}
	if err := generation.ValidateBootSelection(value); err != nil {
		return value, err
	}
	if value.PendingHealthValidation || value.RecoveryRequired || value.DefaultGenerationID != value.BootedGenerationID {
		return value, fmt.Errorf("unhealthy boot selection: %+v", value)
	}
	return value, nil
}

func (j *releaseJourney) bootID() (string, error) {
	return j.observe("cat /proc/sys/kernel/random/boot_id")
}

// releaseFlavour returns the flavour's file suffix and image prefix.
func releaseFlavour() (suffix, imagePrefix string) {
	if os.Getenv("KATL_FLAVOUR") == "lts" {
		return "-lts", "katlos-lts-"
	}
	return "", "katlos-"
}

func releaseImage(t *testing.T, dir string, role, kind string) (string, katlosimage.ArtifactMetadata) {
	t.Helper()
	_, imagePrefix := releaseFlavour()
	image := releaseFile(t, dir, imagePrefix+kind+"-*.squashfs")
	metadata, err := katlosimage.ReadArtifactMetadata(image+".json", role)
	if err != nil {
		t.Fatal(err)
	}
	return image, metadata
}

func releaseKatlctl(t *testing.T, dir string) Katlctl {
	t.Helper()
	path := releaseFile(t, dir, "katlctl-*-linux-amd64")
	// A gate may mount the bundle read-only, so only a download that lost
	// its mode is changed.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 != 0o111 {
		if err := os.Chmod(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return NewKatlctl(path)
}

// runReleaseJourney installs the source release with its own installer,
// reboots into it, enrolls the node through bin, and runs journey once the
// installed runtime is up. Journeys must never skip: katl-vmtest-ran checks
// only the outcome of the top-level test, so a skipped subtest would pass the
// gate.
func runReleaseJourney(t *testing.T, name, source string, bin Katlctl, journey func(*releaseJourney) error) {
	t.Helper()
	world := RequireWorld(t)
	suffix, _ := releaseFlavour()
	image, metadata := releaseImage(t, source, katlosimage.RoleInstall, "install")
	key := filepath.Join(t.TempDir(), "id_ed25519")
	if output, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("SSH key: %v: %s", err, output)
	}
	public, err := os.ReadFile(key + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(t.TempDir(), "artifacts.json")
	if err := writeJSON(indexPath, mkosiArtifactIndex{Artifacts: []mkosiArtifact{{Kind: "katlos-install-image", Path: image, MetadataPath: image + ".json"}}}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KATL_MKOSI_ARTIFACT_INDEX", indexPath)
	run, err := PlanFirstInstallWorldRun(world, name, repoRoot(t), NodeSpec{Name: "cp-1", Role: ControlPlane}, FirstInstallWorldInput{
		Installer: InstallerBootConfig{InstallerISO: releaseFile(t, source, "katl-installer"+suffix+".iso")},
		Mode:      FirstInstallWorldPreseed, SSHAuthorizedKey: strings.TrimSpace(string(public)), UseInstalledESP: true, TargetDiskSize: "32G",
		DataVolumeDiskID: "/dev/disk/by-id/virtio-katl-data",
	}, KVMOn)
	if run.Scenario != nil {
		t.Cleanup(run.Scenario.Release)
	}
	if err != nil {
		t.Fatal(err)
	}
	installed := filepath.Join(run.Scenario.Dir, "inputs", "config-bundle-source", "cp-1", "cluster.yaml")
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Minute)
	defer cancel()
	vm := VMConfig{KVM: KVMOn, RAMMiB: 2048, CPUs: 2, Timeout: 35 * time.Minute, SerialIdleTimeout: -1, PreserveNVRAM: true, Expect: runtimeKernelBootSignal}
	vm.Network = run.Node.VMNetwork()
	vm.SerialHooks = []SerialHook{{Name: name, Signal: runtimeKernelBootSignal, Run: func(ctx context.Context, event SerialHookEvent) error {
		lease, err := WaitLibvirtLease(ctx, event.Plan.VirshPath, event.Plan.LibvirtURI, event.Plan.LibvirtNetwork, event.Plan.MACAddress, 2*time.Minute)
		if err != nil {
			return err
		}
		result := event.Result
		result.IPAddress = lease.IPAddress
		j := &releaseJourney{t: t, ctx: ctx, result: result, address: lease.IPAddress, key: key, public: strings.TrimSpace(string(public)), installed: installed, katlctl: configApplyKatlctl(bin, result)}
		if err := j.healthy(metadata); err != nil {
			return err
		}
		// Reuse the installation's management identity through the public CLI.
		enrollConfigApplyNode(t, ctx, result, j.katlctl, lease.IPAddress+":9443", j.public)
		return journey(j)
	}}}
	run.Config.Installer.VM = vm
	run.Config.Runtime.VM = vm
	run.Config.RebootIntoInstalled = true
	result, err := RunFirstInstall(ctx, run.Runner, Scenario{Name: name, Disks: []DiskFixture{ExtraDisk("data", string(DiskRaw), "4G")}}, run.Config)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusPassed {
		t.Fatalf("release journey: %s (%s)", result.FailureSummary, result.RunDir)
	}
	if err := run.Scenario.WriteResult(WorldStatusPassed, ""); err != nil {
		t.Fatal(err)
	}
}

func runReleaseLifecycle(t *testing.T, source, candidate string, upgrade bool) {
	t.Helper()
	_, metadata := releaseImage(t, source, katlosimage.RoleInstall, "install")
	target, targetMetadata := releaseImage(t, candidate, katlosimage.RoleUpgrade, "upgrade")
	name := "release-install"
	if upgrade {
		name = "release-upgrade"
	}
	runReleaseJourney(t, name, source, releaseKatlctl(t, candidate), func(j *releaseJourney) error {
		ctx, result, katlctl, observe, cli, healthy, selection := j.ctx, j.result, j.katlctl, j.observe, j.cli, j.healthy, j.selection
		// The data volume mounts, through the runtime's own activation unit
		// rather than a copy installed into /etc that would shadow later
		// runtimes' units.
		volumeMounted := func(stage string) error {
			text, err := observe("findmnt --noheadings --output SOURCE --mountpoint /var/mnt/data >/dev/null && cat /var/mnt/data/marker && test ! -e /etc/systemd/system/katl-volumes-activate.service && systemctl show katl-volumes-activate.service -p FragmentPath --value")
			if err != nil || text != "release\n/usr/lib/systemd/system/katl-volumes-activate.service" {
				return fmt.Errorf("%s: data volume and activation unit = %q: %v", stage, text, err)
			}
			return nil
		}
		if _, err := observe("echo release > /var/mnt/data/marker && sync"); err != nil {
			return fmt.Errorf("write data volume marker: %w", err)
		}
		if err := volumeMounted("installed"); err != nil {
			return err
		}
		cli("initial-status", "node", "status", "cp-1", "--output", "json")
		original, err := selection()
		if err != nil {
			return err
		}
		machine, err := observe("cat /etc/machine-id")
		if err != nil {
			return err
		}
		boot, err := j.bootID()
		if err != nil {
			return err
		}
		cli("reboot", "node", "reboot", "cp-1", "--timeout", "5m")
		if err := healthy(metadata); err != nil {
			return err
		}
		rebooted, err := j.bootID()
		if err != nil || rebooted == boot {
			return fmt.Errorf("public reboot did not change boot identity: %v", err)
		}
		if upgrade {
			config := filepath.Join(result.RunDir, "katlctl", "enrollment", "cluster.yaml")
			cli("upgrade-plan", "node", "upgrade", "cp-1", "--config", config, "--artifact", target, "--flavour", first(targetMetadata.Flavour, "standard"), "--plan")
			planned, err := selection()
			if err != nil || planned.BootedGenerationID != original.BootedGenerationID || planned.DefaultGenerationID != original.DefaultGenerationID || planned.TargetBootGenerationID != "" || planned.TrialGenerationID != "" {
				return fmt.Errorf("plan changed current generation: %v", err)
			}
			plannedBoot, err := j.bootID()
			if err != nil || plannedBoot != rebooted {
				return fmt.Errorf("plan rebooted the node: %v", err)
			}
			cli("upgrade", "node", "upgrade", "cp-1", "--config", config, "--artifact", target, "--flavour", first(targetMetadata.Flavour, "standard"), "--timeout", "10m")
			if err := healthy(targetMetadata); err != nil {
				return err
			}
			active, err := selection()
			if err != nil || active.BootedGenerationID == original.BootedGenerationID {
				return fmt.Errorf("upgrade did not promote a new generation: %v", err)
			}
			// Verify the booted kernel independently of management success.
			kernel, err := observe("uname -r")
			if err != nil {
				return err
			}
			if targetMetadata.ExtensionRelease == nil || kernel != targetMetadata.ExtensionRelease.Target.Kernel.Release {
				return fmt.Errorf("booted kernel %q does not match release metadata", kernel)
			}
			if err := volumeMounted("upgraded"); err != nil {
				return err
			}
			cli("rollback-select", "node", "generations", "select", original.BootedGenerationID, "cp-1")
			cli("rollback-reboot", "node", "reboot", "cp-1", "--timeout", "5m")
			if err := healthy(metadata); err != nil {
				return err
			}
			rolledBack, err := selection()
			if err != nil || rolledBack.BootedGenerationID != original.BootedGenerationID {
				return fmt.Errorf("rollback did not restore source generation: %v", err)
			}
			if err := volumeMounted("rolled back"); err != nil {
				return err
			}
			// The runtime that owns boot-move intents completes any intent it
			// finds, so this assertion needs a runtime that can read them but
			// cannot complete them: a release between the first reader and
			// this build. Delete the block when the upgrade baseline reaches
			// the first release with the owner.
			readerOnly, err := readerOnlyBaseline(metadata.Version, targetMetadata.Version, observe)
			if err != nil {
				return err
			}
			if readerOnly {
				if err := assertReaderOnlyKeepsIntent(t, ctx, result, katlctl, observe, rolledBack); err != nil {
					return err
				}
				if err := healthy(metadata); err != nil {
					return err
				}
			}
		}
		finalSelection, err := selection()
		if err != nil || finalSelection.BootedGenerationID != original.BootedGenerationID {
			return fmt.Errorf("final boot selection differs from the source: %v", err)
		}
		persisted, err := observe("cat /etc/machine-id")
		if err != nil || persisted != machine {
			return fmt.Errorf("machine identity changed: %v", err)
		}
		cli("final-status", "node", "status", "cp-1", "--output", "json")
		return nil
	})
}

// TestReleaseNonReaderUpgrade upgrades a node from a published release that
// predates boot-move records. The candidate katlctl refuses the direct
// upgrade, and the old release's katlctl, which no check stops, upgrades it:
// a failed upgrade returns to a manageable old release, and a promoted one
// can roll back to it.
func TestReleaseNonReaderUpgrade(t *testing.T) {
	if !DefaultOptions().Enabled {
		t.Skip("run with scripts/vmtest-run")
	}
	candidate := os.Getenv("KATL_VMTEST_RELEASE_DIR")
	if candidate == "" {
		t.Skip("set KATL_VMTEST_RELEASE_DIR to a verified production bundle")
	}
	source := os.Getenv("KATL_VMTEST_NONREADER_RELEASE_DIR")
	if source == "" {
		t.Fatal("KATL_VMTEST_NONREADER_RELEASE_DIR is required: a verified bundle of a release before " + generation.FirstBootMoveReader.String())
	}
	_, metadata := releaseImage(t, source, katlosimage.RoleInstall, "install")
	if generation.ReadsBootMoves(metadata.Version) {
		t.Fatalf("KATL_VMTEST_NONREADER_RELEASE_DIR holds %s, which reads boot-move records", metadata.Version)
	}
	target, targetMetadata := releaseImage(t, candidate, katlosimage.RoleUpgrade, "upgrade")
	candidateKatlctl := releaseKatlctl(t, candidate)

	runReleaseJourney(t, "release-nonreader-upgrade", source, releaseKatlctl(t, source), func(j *releaseJourney) error {
		config := filepath.Join(j.result.RunDir, "katlctl", "enrollment", "cluster.yaml")
		flavour := first(targetMetadata.Flavour, "standard")
		original, err := j.selection()
		if err != nil {
			return err
		}
		gen0 := original.BootedGenerationID
		unchanged := func(stage, selection, generations string) error {
			gotSelection, err := j.rawSelection()
			if err != nil || gotSelection != selection {
				return fmt.Errorf("%s changed the boot selection: %v", stage, err)
			}
			gotGenerations := runKatlctl(t, j.ctx, j.result, j.katlctl, stage+"-generations", "node", "generations", "list", "cp-1")
			if string(gotGenerations) != generations {
				return fmt.Errorf("%s changed the generations list:\n%s\nwant:\n%s", stage, gotGenerations, generations)
			}
			return nil
		}
		selectionBefore, err := j.rawSelection()
		if err != nil {
			return err
		}
		generationsBefore := string(runKatlctl(t, j.ctx, j.result, j.katlctl, "initial-generations", "node", "generations", "list", "cp-1"))

		// The candidate katlctl refuses the direct upgrade and names the
		// intermediate release.
		refused, err := runKatlctlOutcome(t, j.ctx, j.result, j.runner(candidateKatlctl), "upgrade-refused",
			"node", "upgrade", "cp-1", "--config", config, "--artifact", target, "--flavour", flavour, "--timeout", "10m")
		if err == nil {
			return fmt.Errorf("candidate katlctl upgraded a %s node directly:\n%s", metadata.Version, refused)
		}
		// The command keeps the node's flavour, which a non-standard node
		// must name.
		sourceFlags := ""
		if flavour != "standard" {
			sourceFlags = " --flavour " + flavour
		}
		want := "First run 'katlctl node upgrade cp-1 --config " + config + sourceFlags + " --version " + generation.FirstBootMoveReader.String() + "', then retry this upgrade"
		if refusal := err.Error(); !strings.Contains(refusal, "which predates boot-move records") || !strings.Contains(refusal, want) {
			return fmt.Errorf("candidate katlctl refusal = %q, want %q", refusal, want)
		}
		if err := unchanged("refused-upgrade", selectionBefore, generationsBefore); err != nil {
			return err
		}

		// The old katlctl is not stopped. A failed upgrade returns to the old
		// release with management intact.
		fault := filepath.Join(filepath.Dir(config), "cluster.fault.yaml")
		if err := writeNonReaderFaultConfig(j.installed, j.address, fault); err != nil {
			return err
		}
		failed, err := runKatlctlOutcome(t, j.ctx, j.result, j.katlctl, "upgrade-failed",
			"node", "upgrade", "cp-1", "--config", fault, "--apply-config", "--artifact", target, "--flavour", flavour, "--timeout", "15m")
		if err == nil || !strings.Contains(string(failed)+err.Error(), "upgrade result: failed; health failed") {
			return fmt.Errorf("faulted upgrade = %v:\n%s\nwant 'upgrade result: failed; health failed'", err, failed)
		}
		if err := j.healthy(metadata); err != nil {
			return fmt.Errorf("after the failed upgrade: %w", err)
		}
		if back, err := j.selection(); err != nil || back.BootedGenerationID != gen0 {
			return fmt.Errorf("failed upgrade did not return to generation %s: %v", gen0, err)
		}
		if err := assertNonReaderRecords(j); err != nil {
			return err
		}
		j.cli("failed-status", "node", "status", "cp-1")
		j.cli("failed-generations", "node", "generations", "list", "cp-1")
		failedBoot, err := j.bootID()
		if err != nil {
			return err
		}
		j.cli("failed-reboot", "node", "reboot", "cp-1", "--timeout", "5m")
		if err := j.healthy(metadata); err != nil {
			return err
		}
		if next, err := j.bootID(); err != nil || next == failedBoot {
			return fmt.Errorf("reboot after the failed upgrade did not change boot identity: %v", err)
		}
		if err := assertNonReaderRecords(j); err != nil {
			return err
		}

		// A healthy upgrade promotes, and the old release stays a rollback
		// target.
		j.cli("upgrade", "node", "upgrade", "cp-1", "--config", config, "--artifact", target, "--flavour", flavour, "--timeout", "15m")
		if err := j.healthy(targetMetadata); err != nil {
			return err
		}
		promoted, err := j.selection()
		if err != nil || promoted.BootedGenerationID == gen0 {
			return fmt.Errorf("upgrade did not promote a new generation: %v", err)
		}
		candidateCLI := j.runner(candidateKatlctl)
		runKatlctl(t, j.ctx, j.result, candidateCLI, "rollback-select", "node", "generations", "select", gen0, "cp-1")
		runKatlctl(t, j.ctx, j.result, candidateCLI, "rollback-reboot", "node", "reboot", "cp-1", "--timeout", "5m")
		if err := j.healthy(metadata); err != nil {
			return err
		}
		if back, err := j.selection(); err != nil || back.BootedGenerationID != gen0 {
			return fmt.Errorf("rollback did not restore generation %s: %v", gen0, err)
		}
		if err := assertNonReaderRecords(j); err != nil {
			return err
		}
		j.cli("final-status", "node", "status", "cp-1")
		return nil
	})
}

// assertNonReaderRecords checks, on a node running a release before the first
// boot-move reader, that the selection carries no boot-move field and that
// the release never failed to read its boot records.
func assertNonReaderRecords(j *releaseJourney) error {
	selection, err := j.rawSelection()
	if err != nil {
		return err
	}
	if strings.Contains(selection, "revision") || strings.Contains(selection, "spentRecoveryTrialID") {
		return fmt.Errorf("selection carries a boot-move field:\n%s", selection)
	}
	journal, err := j.observe("journalctl --no-pager -o cat | grep -E 'unknown field|cannot read boot-move records' || true")
	if err != nil {
		return err
	}
	if journal != "" {
		return fmt.Errorf("journal reports unreadable boot records:\n%s", journal)
	}
	return nil
}

// writeNonReaderFaultConfig writes the installed config with the node's
// management address and a host configuration that cannot apply, so the
// upgrade passes preflight and the upgraded candidate fails boot health. The
// enrollment config cannot serve as the base: --apply-config would also change
// the endpoint, system disk, kernel and volumes the node was installed with,
// and preflight rejects those changes.
func writeNonReaderFaultConfig(installed, address, fault string) error {
	data, err := os.ReadFile(installed)
	if err != nil {
		return err
	}
	var config map[string]any
	if err := yaml.Unmarshal(data, &config); err != nil {
		return fmt.Errorf("installed config: %w", err)
	}
	spec, _ := config["spec"].(map[string]any)
	nodes, _ := spec["nodes"].([]any)
	var node map[string]any
	for _, item := range nodes {
		if candidate, ok := item.(map[string]any); ok && candidate["name"] == "cp-1" {
			node = candidate
		}
	}
	if node == nil {
		return fmt.Errorf("installed config %s has no node cp-1", installed)
	}
	node["management"] = map[string]any{"address": address}
	node["hostConfiguration"] = map[string]any{"sysfs": []any{
		map[string]any{"path": "/sys/module/katl_nonreader_missing/parameters/never", "value": "1"},
	}}
	faulted, err := yaml.Marshal(config)
	if err != nil {
		return err
	}
	return os.WriteFile(fault, faulted, 0o600)
}

func releaseSSH(ctx context.Context, key, address, command string) (string, error) {
	probe, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	output, err := exec.CommandContext(probe, "ssh", "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes", "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "ConnectTimeout=5", "-i", key, "root@"+address, command).Output()
	return strings.TrimSpace(string(output)), err
}

// readerOnlyBaseline reports whether the runtime of the baseline release reads
// boot-move records without owning their completion. It must be a release
// before the candidate that still ships the legacy boot-deadline unit, which
// only the intent owner removes.
func readerOnlyBaseline(baseline, candidate string, observe func(string) (string, error)) (bool, error) {
	if !generation.ReadsBootMoves(baseline) {
		return false, nil
	}
	baselineVersion, err := releaseversion.Parse(baseline)
	if err != nil {
		return false, nil
	}
	candidateVersion, err := releaseversion.Parse(candidate)
	if err != nil || baselineVersion.Base().Compare(candidateVersion.Base()) >= 0 {
		return false, nil
	}
	unit, err := observe("systemctl cat katl-boot-deadman.service")
	if err != nil {
		return false, fmt.Errorf("read the baseline boot deadline unit: %w", err)
	}
	return strings.Contains(unit, "ConditionPathExists=/run/katl/boot-health/pending"), nil
}

// assertReaderOnlyKeepsIntent plants a boot-move intent on a reader-only
// runtime and requires it to leave the intent and the selection alone, fail
// boot health and refuse managed reboots. It removes the intent and reboots
// before returning.
func assertReaderOnlyKeepsIntent(t *testing.T, ctx context.Context, result Result, katlctl katlctlRunner, observe func(string) (string, error), current generation.BootSelectionRecord) error {
	t.Helper()
	const intentPath = "/var/lib/katl/boot/move-intent.json"
	bootID, err := observe("cat /proc/sys/kernel/random/boot_id")
	if err != nil {
		return err
	}
	intent := string(plantedBootMoveIntent(t, "vmtest-reader-only-intent", bootID, current, current))
	if _, err := observe("printf '%s' '" + intent + "' > " + intentPath + " && sync"); err != nil {
		return fmt.Errorf("plant boot-move intent: %w", err)
	}
	selectionBefore, err := observe("cat /var/lib/katl/boot/selection.json")
	if err != nil {
		return err
	}
	if _, err := observe("systemd-run --quiet --collect --on-active=1s systemctl reboot --no-block"); err != nil {
		return fmt.Errorf("reboot with a planted intent: %w", err)
	}
	deadline, stop := context.WithTimeout(ctx, 5*time.Minute)
	defer stop()
	for {
		if next, err := observe("cat /proc/sys/kernel/random/boot_id"); err == nil && next != bootID {
			break
		}
		select {
		case <-deadline.Done():
			return fmt.Errorf("node did not reboot with the planted intent: %w", deadline.Err())
		case <-time.After(2 * time.Second):
		}
	}

	// The reader-only runtime still activates the generation, but it cannot
	// complete the intent, so boot health fails and nothing changes.
	for {
		if _, err := observe("systemctl is-failed --quiet katl-boot-health.service"); err == nil {
			break
		}
		select {
		case <-deadline.Done():
			return fmt.Errorf("katl-boot-health.service did not fail with the planted intent: %w", deadline.Err())
		case <-time.After(2 * time.Second):
		}
	}
	activation, err := observe("systemctl show katl-generation-activate.service --property=Result --value")
	if err != nil || activation != "success" {
		return fmt.Errorf("generation activation result = %q with the planted intent: %v", activation, err)
	}
	if got, err := observe("cat /var/lib/katl/boot/selection.json"); err != nil || got != selectionBefore {
		return fmt.Errorf("reader-only runtime changed the boot selection: %v", err)
	}
	if got, err := observe("cat " + intentPath); err != nil || got != intent {
		return fmt.Errorf("reader-only runtime changed the boot-move intent: %v", err)
	}
	if _, err := observe("systemctl start katlc-agent.service"); err != nil {
		return fmt.Errorf("start the management agent: %w", err)
	}
	if _, err := runKatlctlOutcome(t, ctx, result, katlctl, "reboot-refused-by-intent", "node", "reboot", "cp-1", "--timeout", "1m"); err == nil {
		return fmt.Errorf("managed reboot was accepted while an unreadable boot-move intent exists")
	}

	if _, err := observe("rm -- " + intentPath); err != nil {
		return fmt.Errorf("remove the planted intent: %w", err)
	}
	runKatlctl(t, ctx, result, katlctl, "reboot-after-intent-removed", "node", "reboot", "cp-1", "--timeout", "5m")
	return nil
}
