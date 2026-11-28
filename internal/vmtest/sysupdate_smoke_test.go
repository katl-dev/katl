package vmtest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/flavour"
	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/configbundle"
	"github.com/katl-dev/katl/internal/installer/katlosimage"
	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/installer/operation"
	"github.com/katl-dev/katl/internal/katlc/agent"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/persistedrecord"
)

func TestInstalledRuntimeSysupdateRootUKITransfer(t *testing.T) {
	options := DefaultOptions()
	if !options.Enabled {
		t.Skip("set -katl.vmtest.run or KATL_VMTEST_RUN=1 to run installed runtime sysupdate root+UKI smoke")
	}
	runner := NewRunner(options)
	runtime := InstalledRuntimeConfig{}
	var worldScenario *WorldScenario
	spec := NodeSpec{Name: "sysupdate-partx-1", Role: ControlPlane}
	if worldRun, ok := installedRuntimeWorldRunFor(t, "installed-runtime-sysupdate-root-uki", spec); ok {
		runner = worldRun.Runner
		runtime = worldRun.Config
		worldScenario = worldRun.Scenario
	} else {
		_ = RequireWorld(t)
	}
	// The disk of an earlier Katl install stays attached through the upgrade
	// and rollbacks; every boot must keep using the installed disk.
	oldDisk := filepath.Join(t.TempDir(), "old-katl.raw")
	if err := WriteOldKatlDisk(context.Background(), filepath.Dir(oldDisk), oldDisk); err != nil {
		t.Fatalf("WriteOldKatlDisk() error = %v", err)
	}
	scenario := Scenario{Name: "installed-runtime-sysupdate-root-uki", Disks: []DiskFixture{SnapshotDisk("old", oldDisk, DiskRaw)}}
	result, err := runner.Plan(scenario)
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	result = requirePlannedVMHost(t, runner, scenario, result, HostRequirements{
		Libvirt: true,
		OVMF:    true,
		KVM:     runner.options().KVM,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	if err := CreateDisks(ctx, diskExec(nil), result.Disks); err != nil {
		t.Fatalf("CreateDisks() error = %v", err)
	}
	vm := runtime.VM
	vm.KVM = runner.options().KVM
	vm.RAMMiB = 2048
	vm.CPUs = 2
	vm.Timeout = 8 * time.Minute
	vm.VSock.Enabled = true
	vm.Agent.RequireHealth = true
	vm.Agent.Timeout = 30 * time.Second
	vm.PreserveNVRAM = true
	node, err := StartInstalledRuntimeNode(ctx, result, InstalledRuntimeNodeConfig{
		Name: spec.Name,
		Runtime: InstalledRuntimeConfig{
			Disk:            runtime.Disk,
			DiskFormat:      runtime.DiskFormat,
			ESPArtifacts:    runtime.ESPArtifacts,
			FixtureManifest: runtime.FixtureManifest,
			NodeMetadata:    runtime.NodeMetadata,
			VM:              vm,
		},
	}, VMRunner{})
	if err != nil {
		t.Fatalf("StartInstalledRuntimeNode() error = %v", err)
	}
	defer func() {
		if err := node.Stop(); err != nil && err != context.Canceled {
			t.Logf("Stop() error = %v", err)
		}
	}()
	client, err := DialAgent(ctx, node.VSock.GuestCID, node.VSock.Port, node.Result.Artifacts.VSockTranscript)
	if err != nil {
		t.Fatalf("DialAgent() error = %v", err)
	}
	defer func() {
		if client != nil {
			_ = client.Close()
		}
	}()
	guest := NewGuestControl(node.Result, client)
	defer func() {
		if t.Failed() {
			collectSysupdateFailureEvidence(ctx, guest)
		}
	}()

	previousGeneration := currentGenerationFromGuest(t, ctx, guest)
	previousSpec, _ := generationRecordsFromGuest(t, ctx, guest, previousGeneration)
	assertBootedGenerationIdentity(t, ctx, guest, previousSpec)
	previousUKIDigest := guestFileSHA256(t, ctx, guest, "previous-uki", previousSpec.Boot.UKIPath)
	sharedStateDigests := guestFileDigests(t, ctx, guest, "source", []string{
		"/var/lib/katl/identity/machine-id",
		"/var/lib/katl/cluster/intent.json",
	})
	sourceAuthorizedKeyDigest := guestFileSHA256(t, ctx, guest, "source-authorized-key", "/etc/ssh/authorized_keys/root")
	stateMarker := "/var/lib/katl/test-artifacts/host-upgrade-state-marker"
	writeGuestFile(t, ctx, guest, stateMarker, []byte("state-survives-host-upgrade-and-rollback\n"), 0o600)
	hostKeyPath := "/var/lib/katl/ssh/host-keys/ssh_host_ed25519_key"
	originalHostKeyDigest := guestFileSHA256(t, ctx, guest, "original-ssh-host-key", hostKeyPath)
	guestCommand(t, ctx, guest, "corrupt-ssh-host-key", "dd", "if=/etc/os-release", "of="+hostKeyPath, "bs=4096", "count=1", "conv=fsync")

	upgrade := pinnedUpgradeImage(t, RequireWorld(t), previousSpec.RuntimeVersion)
	endpoint := katlcEndpoint(t, node, "")
	candidateGeneration := "host-upgrade-" + strings.ReplaceAll(upgrade.Version, ".", "-")
	conn, katlc := dialKatlcAgentForVMTest(t, ctx, endpoint, spec.Name)
	nodeStatus, err := katlc.GetNodeStatus(ctx, &agentapi.GetNodeStatusRequest{})
	if err != nil {
		conn.Close()
		t.Fatalf("read node status before host upgrade: %v", err)
	}
	conn.Close()
	if !slices.Contains(nodeStatus.GetSupportedOperationKinds(), "host-upgrade-handoff") {
		t.Fatal("installed runtime does not advertise target-prepared host upgrade")
	}
	localRef := stageHostUpgradeArtifactForVMTest(t, ctx, endpoint, spec.Name, nodeStatus.GetMachineId(), upgrade)
	selectionBeforeRejectedPlan := readGuestFile(t, ctx, guest, "/var/lib/katl/boot/selection.json")
	rejectedCandidate := candidateGeneration + "-preboot-rejected"
	rejectedConfig := hostUpgradeConfiguration("2", "    kernel:\n      commandLine:\n        - console=ttyS0\n")
	assertHostUpgradePlanRejected(t, ctx, endpoint, spec.Name, nodeStatus.GetMachineId(), previousGeneration, rejectedCandidate, localRef, upgrade, rejectedConfig, "upgrade without --apply-config first")
	if resources := hostUpgradeImageResources(t, ctx, guest, "rejected-preview"); resources != "" {
		t.Fatalf("rejected preview retained native image resources: %s", resources)
	}
	if got := readGuestFile(t, ctx, guest, "/var/lib/katl/boot/selection.json"); got != selectionBeforeRejectedPlan {
		t.Fatal("rejected combined plan changed boot selection")
	}
	assertGuestMissing(t, ctx, guest, "/var/lib/katl/generations/"+rejectedCandidate)

	// A staging that fails after slot preparation must leave the node able to
	// plan and stage again in the same boot. A read-only ESP fails the staging
	// after prepareSysupdateSlots has run, so the retry depends on preparation
	// leaving the kernel's view of the inactive partition intact: a re-added
	// partition loses its PARTUUID in sysfs until reboot.
	failedCandidate := candidateGeneration + "-failed"
	guestCommand(t, ctx, guest, "efi-read-only", "mount", "-o", "remount,ro", "/efi")
	_, failedStatus := submitHostUpgradeAndWait(t, ctx, endpoint, spec.Name, nodeStatus.GetMachineId(), previousGeneration, failedCandidate, localRef, upgrade, "host-upgrade-handoff", "")
	guestCommand(t, ctx, guest, "efi-read-write", "mount", "-o", "remount,rw", "/efi")
	t.Logf("read-only ESP failure_reason: %s", failedStatus.GetFailureReason())
	if failedStatus.GetResult() != operation.ResultFailedNeedsRepair {
		t.Fatalf("host upgrade with a read-only ESP status = %+v, want result %s", failedStatus, operation.ResultFailedNeedsRepair)
	}
	if got := readGuestFile(t, ctx, guest, "/var/lib/katl/boot/selection.json"); got != selectionBeforeRejectedPlan {
		t.Fatal("failed staging changed boot selection")
	}
	waitHostUpgradeWorkspaceClean(t, ctx, guest, "failed")
	localRef = stageHostUpgradeArtifactForVMTest(t, ctx, endpoint, spec.Name, nodeStatus.GetMachineId(), upgrade)
	assertHostUpgradePlanAccepted(t, ctx, endpoint, spec.Name, nodeStatus.GetMachineId(), previousGeneration, candidateGeneration, localRef, upgrade)

	// A full ESP must be refused before anything is written. The refusal names
	// the space to free, and leaves the partition table and the ESP as they were.
	disk, inactivePart := inactiveRootSlot(t, ctx, guest)
	tableBeforeFull := guestCommandOutput(t, ctx, guest, "partition-table-before-full", "sfdisk", "--dump", disk)
	efiState := []string{"find", "/efi", "-type", "f", "-printf", "%p %s\n", "-exec", "sha256sum", "{}", ";"}
	efiBeforeFull := guestCommandOutput(t, ctx, guest, "efi-files-before-full", efiState...)
	fill, err := guest.RunCommand(ctx, GuestCommandRequest{Name: "efi-fill", Argv: []string{"dd", "if=/dev/zero", "of=/efi/katl-vmtest-fill", "bs=1M"}, Timeout: 5 * time.Minute, StdoutLimit: 1 << 16, StderrLimit: 1 << 16, AllowFailure: true})
	if err != nil || fill.ExitStatus == 0 {
		t.Fatalf("fill ESP: exit %d, err %v; want dd to stop at ENOSPC", fill.ExitStatus, err)
	}
	_, fullStatus := submitHostUpgradeAndWait(t, ctx, endpoint, spec.Name, nodeStatus.GetMachineId(), previousGeneration, candidateGeneration+"-full", localRef, upgrade, "host-upgrade-handoff", "")
	t.Logf("full ESP failure_reason: %s", fullStatus.GetFailureReason())
	guestCommand(t, ctx, guest, "efi-unfill", "find", "/efi", "-maxdepth", "1", "-name", "katl-vmtest-fill", "-delete")
	if fullStatus.GetResult() != "failed" || fullStatus.GetExternalMutationStarted() ||
		!strings.Contains(fullStatus.GetFailureReason(), "insufficient space on the EFI system partition") || !strings.Contains(fullStatus.GetFailureReason(), "Free at least") {
		t.Fatalf("host upgrade with a full ESP status = %+v, want a refusal before mutation that says how much to free", fullStatus)
	}
	if got := guestCommandOutput(t, ctx, guest, "partition-table-after-full", "sfdisk", "--dump", disk); got != tableBeforeFull {
		t.Fatalf("full-ESP refusal changed the partition table:\n%s\nwant\n%s", got, tableBeforeFull)
	}
	if got := guestCommandOutput(t, ctx, guest, "efi-files-after-full", efiState...); got != efiBeforeFull {
		t.Fatalf("full-ESP refusal changed the ESP:\n%s\nwant\n%s", got, efiBeforeFull)
	}
	if got := readGuestFile(t, ctx, guest, "/var/lib/katl/boot/selection.json"); got != selectionBeforeRejectedPlan {
		t.Fatal("full-ESP refusal changed boot selection")
	}
	waitHostUpgradeWorkspaceClean(t, ctx, guest, "full")
	localRef = stageHostUpgradeArtifactForVMTest(t, ctx, endpoint, spec.Name, nodeStatus.GetMachineId(), upgrade)

	// A staging that dies after sysupdate wrote the root slot, such as on a
	// full ESP before this check existed, leaves the slot in the pending type
	// and a partial UKI. Sysupdate refuses to update over that until it
	// vacuums, so the upgrade has to clear it. The type below is the one
	// systemd 262 wrote in that failure (derived from the root type).
	const pendingRootType = "64362E66-88A7-463B-B325-7AF60C81BE06"
	guestCommand(t, ctx, guest, "plant-partial-uki", "dd", "if=/dev/zero", "of=/efi/EFI/katl/.sysupdate.partial.katl-root-b-1.efi", "bs=1M", "count=4")
	guestCommand(t, ctx, guest, "plant-pending-type", "sfdisk", "--no-reread", "--no-tell-kernel", "--part-type", disk, inactivePart, pendingRootType)
	guestCommand(t, ctx, guest, "plant-pending-label", "sfdisk", "--no-reread", "--no-tell-kernel", "--part-label", disk, inactivePart, "katl_1")
	if dump := guestCommandOutput(t, ctx, guest, "partition-table-planted", "sfdisk", "--dump", disk); !strings.Contains(strings.ToUpper(dump), "TYPE="+pendingRootType) {
		t.Fatalf("planting the interrupted-transfer slot type failed:\n%s", dump)
	}

	combinedAuthorizedKey := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDAxMjM0NTY3ODlhYmNkZWYwMTIzNDU2Nzg5YWJjZGVm combined-upgrade@katl"
	combinedConfig := hostUpgradeConfiguration("3", "    identity:\n      authorizedKeys:\n        - "+combinedAuthorizedKey+"\n")
	operationID, status := submitHostUpgradeAndWait(t, ctx, endpoint, spec.Name, nodeStatus.GetMachineId(), previousGeneration, candidateGeneration, localRef, upgrade, "host-upgrade-handoff", combinedConfig)
	if status.GetResult() != operation.ResultSucceeded || !status.GetBootHealthPending() || status.GetCandidateGenerationId() != candidateGeneration {
		t.Fatalf("host upgrade operation status = %+v", status)
	}
	waitHostUpgradeWorkspaceClean(t, ctx, guest, "initial")
	recordData := readGuestFile(t, ctx, guest, "/var/lib/katl/operations/"+operationID+"/record.json")
	envelope, err := persistedrecord.DecodeEnvelope([]byte(recordData))
	if err != nil {
		t.Fatalf("decode host upgrade operation envelope: %v", err)
	}
	persisted, err := persistedrecord.DecodePayload[operation.Snapshot](envelope)
	if err != nil {
		t.Fatalf("decode host upgrade operation snapshot: %v", err)
	}
	request := persisted.Record.HostUpgradeRequest
	if request == nil || request.ImageSHA256 != upgrade.SHA256 || request.ImageSizeBytes != upgrade.SizeBytes {
		t.Fatalf("host upgrade did not persist derived image identity: %#v", request)
	}
	trial := bootSelectionFromGuest(t, ctx, guest)
	if trial.TrialGenerationID != candidateGeneration || trial.PreviousKnownGoodGenerationID != previousGeneration || !trial.PendingHealthValidation {
		t.Fatalf("host upgrade trial selection = %#v", trial)
	}
	candidateSpec := generationFromGuest(t, ctx, guest, candidateGeneration)
	if candidateSpec.RuntimeVersion != upgrade.Version || candidateSpec.Root.Slot == previousSpec.Root.Slot {
		t.Fatalf("host upgrade candidate spec = %#v; previous = %#v", candidateSpec, previousSpec)
	}
	// Staging cleared the interrupted transfer: sysupdate's leftovers are gone and
	// the slot is back to a plain root slot on the disk.
	if leftovers := strings.TrimSpace(guestCommandOutput(t, ctx, guest, "sysupdate-leftovers", "find", "/efi/EFI/katl", "-name", ".sysupdate.*")); leftovers != "" {
		t.Fatalf("staging left sysupdate files on the ESP:\n%s", leftovers)
	}
	if dump := guestCommandOutput(t, ctx, guest, "partition-table-staged", "sfdisk", "--dump", disk); strings.Contains(strings.ToUpper(dump), "TYPE="+pendingRootType) {
		t.Fatalf("staging left the pending slot type:\n%s", dump)
	}

	guest, client = restartGuestAndReconnect(t, ctx, &node, guest, client)
	waitGenerationPromotion(t, ctx, guest, candidateGeneration)
	assertBootedGenerationIdentity(t, ctx, guest, candidateSpec)
	if got := guestFileSHA256(t, ctx, guest, "retained-rollback-uki", previousSpec.Boot.UKIPath); got != previousUKIDigest {
		t.Fatalf("upgrade changed the previous generation UKI: %s, want %s", got, previousUKIDigest)
	}
	assertInstalledSSHReady(t, ctx, guest)
	assertHostUpgradeRuntimeReady(t, ctx, guest, "target")
	assertGuestFileDigests(t, ctx, guest, "target", sharedStateDigests)
	wantAuthorizedKeyDigest := sha256.Sum256([]byte(combinedAuthorizedKey + "\n"))
	if got := guestFileSHA256(t, ctx, guest, "combined-authorized-key", "/etc/ssh/authorized_keys/root"); got != hex.EncodeToString(wantAuthorizedKeyDigest[:]) {
		t.Fatalf("combined upgrade authorized key digest = %s, want %x", got, wantAuthorizedKeyDigest)
	}
	repairedHostKeyDigest := guestFileSHA256(t, ctx, guest, "repaired-ssh-host-key", hostKeyPath)
	if repairedHostKeyDigest == originalHostKeyDigest {
		t.Fatalf("repaired SSH host key was not replaced: %s", repairedHostKeyDigest)
	}
	assertGuestFileContains(t, ctx, guest, stateMarker, "state-survives-host-upgrade-and-rollback")
	promoted := bootSelectionFromGuest(t, ctx, guest)
	if promoted.DefaultGenerationID != candidateGeneration || promoted.PreviousKnownGoodGenerationID != previousGeneration || promoted.PendingHealthValidation {
		t.Fatalf("promoted host upgrade selection = %#v", promoted)
	}

	selectBootGeneration(t, ctx, endpoint, spec.Name, previousGeneration, true)
	guest, client = restartGuestAndReconnect(t, ctx, &node, guest, client)
	waitActiveGeneration(t, ctx, guest, previousGeneration)
	assertBootedGenerationIdentity(t, ctx, guest, previousSpec)
	assertHostUpgradeRuntimeReady(t, ctx, guest, "one-shot-rollback")
	assertGuestFileDigests(t, ctx, guest, "one-shot-rollback", sharedStateDigests)
	if got := guestFileSHA256(t, ctx, guest, "one-shot-rollback-host-key", hostKeyPath); got != repairedHostKeyDigest {
		t.Fatalf("one-shot rollback host key digest = %s, want %s", got, repairedHostKeyDigest)
	}
	if got := guestFileSHA256(t, ctx, guest, "one-shot-rollback-authorized-key", "/etc/ssh/authorized_keys/root"); got != sourceAuthorizedKeyDigest {
		t.Fatalf("one-shot rollback authorized key digest = %s, want source %s", got, sourceAuthorizedKeyDigest)
	}
	temporary := bootSelectionFromGuest(t, ctx, guest)
	if temporary.DefaultGenerationID != candidateGeneration {
		t.Fatalf("one-shot boot changed default: %+v", temporary)
	}

	guest, client = restartGuestAndReconnect(t, ctx, &node, guest, client)
	waitActiveGeneration(t, ctx, guest, candidateGeneration)
	assertBootedGenerationIdentity(t, ctx, guest, candidateSpec)
	assertHostUpgradeRuntimeReady(t, ctx, guest, "repeat-target")
	assertGuestFileDigests(t, ctx, guest, "repeat-target", sharedStateDigests)
	if got := guestFileSHA256(t, ctx, guest, "repeat-target-host-key", hostKeyPath); got != repairedHostKeyDigest {
		t.Fatalf("repeat target host key digest = %s, want %s", got, repairedHostKeyDigest)
	}
	if got := guestFileSHA256(t, ctx, guest, "repeat-target-authorized-key", "/etc/ssh/authorized_keys/root"); got != hex.EncodeToString(wantAuthorizedKeyDigest[:]) {
		t.Fatalf("repeat target authorized key digest = %s, want %x", got, wantAuthorizedKeyDigest)
	}
	selectBootGeneration(t, ctx, endpoint, spec.Name, previousGeneration, false)

	guest, client = restartGuestAndReconnect(t, ctx, &node, guest, client)
	waitGenerationPromotion(t, ctx, guest, previousGeneration)
	assertBootedGenerationIdentity(t, ctx, guest, previousSpec)
	assertHostUpgradeRuntimeReady(t, ctx, guest, "persistent-rollback")
	assertGuestFileDigests(t, ctx, guest, "persistent-rollback", sharedStateDigests)
	if got := guestFileSHA256(t, ctx, guest, "persistent-rollback-host-key", hostKeyPath); got != repairedHostKeyDigest {
		t.Fatalf("persistent rollback host key digest = %s, want %s", got, repairedHostKeyDigest)
	}
	if got := guestFileSHA256(t, ctx, guest, "persistent-rollback-authorized-key", "/etc/ssh/authorized_keys/root"); got != sourceAuthorizedKeyDigest {
		t.Fatalf("persistent rollback authorized key digest = %s, want source %s", got, sourceAuthorizedKeyDigest)
	}
	assertGuestFileContains(t, ctx, guest, stateMarker, "state-survives-host-upgrade-and-rollback")
	rolledBack := bootSelectionFromGuest(t, ctx, guest)
	if rolledBack.DefaultGenerationID != previousGeneration || rolledBack.PreviousKnownGoodGenerationID != candidateGeneration || rolledBack.PendingHealthValidation {
		t.Fatalf("rolled-back boot selection = %#v", rolledBack)
	}
	if previousSpec.RuntimeVersion == candidateSpec.RuntimeVersion || previousSpec.Boot.UKIPath == candidateSpec.Boot.UKIPath || previousSpec.Root.Slot == candidateSpec.Root.Slot {
		t.Fatalf("upgrade did not produce distinct version, root slot, and UKI identities: previous=%#v candidate=%#v", previousSpec, candidateSpec)
	}
	guest, client = upgradeAfterBootHealthRepair(t, ctx, &node, guest, client, endpoint, previousSpec, candidateSpec, upgrade)
	waitHostUpgradeWorkspaceClean(t, ctx, guest, "repeat")
	repeatedGeneration := currentGenerationFromGuest(t, ctx, guest)
	repeatedSpec := generationFromGuest(t, ctx, guest, repeatedGeneration)
	if repeatedGeneration == candidateGeneration || repeatedSpec.RuntimeVersion != upgrade.Version ||
		repeatedSpec.Root.Slot != candidateSpec.Root.Slot || repeatedSpec.Root.PartitionUUID != candidateSpec.Root.PartitionUUID {
		t.Fatalf("repeated host upgrade = %#v, want a new generation on the previously upgraded peer %#v", repeatedSpec, candidateSpec)
	}
	assertBootedGenerationIdentity(t, ctx, guest, repeatedSpec)
	assertGuestMissing(t, ctx, guest, "/efi/"+candidateSpec.Boot.LoaderEntryPath)
	guestCommand(t, ctx, guest, "boot-health-evidence", "systemctl", "show", "katl-boot-health.service", "--property=Result,ExecMainStatus")
	guestCommand(t, ctx, guest, "boot-complete-evidence", "systemctl", "is-active", "katl-boot-complete.target")
	waitForGenerationPruning(t, ctx, guest, candidateGeneration)
	t.Log("host upgrade, rollback, and repeated upgrade staging are serialized per node in v0.1; multi-node rollout orchestration remains operator-controlled")
	powerOffGuestForCleanSuccess(t, ctx, &node, guest, client)
	client = nil

	node.Result.finish(StatusPassed, "", runner.time())
	if err := runner.Write(scenario, node.Result); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if worldScenario != nil {
		if err := worldScenario.WriteResult(WorldStatusPassed, ""); err != nil {
			t.Fatalf("write world scenario result: %v", err)
		}
	}
}

func upgradeAfterBootHealthRepair(t *testing.T, ctx context.Context, node *RunningInstalledRuntimeNode, guest *GuestControl, client *AgentClient, endpoint string, source, fallback generation.GenerationSpec, upgrade builtUpgradeImage) (*GuestControl, *AgentClient) {
	t.Helper()
	result := node.Result
	katlctl := configApplyKatlctl(BuildKatlctl(t, ctx, repoRoot(t)), result)
	configPath := sysupdateRepairConfig(t, ctx, guest, filepath.Dir(katlctl.config), endpoint, node.Name, source)
	saveVMTestManagementContext(t, ctx, result, katlctl, configPath)
	statusArgs := []string{"node", "status", node.Name, "--config", configPath, "--output", "json"}
	listArgs := []string{"node", "generations", "list", node.Name, "--config", configPath, "--output", "json"}

	// Hide one of the queried units for this boot. The runtime mask disappears
	// on reboot and leaves the agent's repair commands usable while masked.
	guestCommand(t, ctx, guest, "mask-boot-health-evidence", "systemctl", "mask", "--runtime", "katl-boot-health-settled.target")
	const eligible = "vmtest-replaced-generation"
	writePrunableGeneration(t, ctx, guest, eligible, fallback)
	// A fresh agent evaluates pruning immediately, even if its previous pass
	// was healthy and scheduled the next pass an hour later.
	guestCommand(t, ctx, guest, "restart-pruning", "systemctl", "restart", "katlc-agent.service")
	if status := string(runKatlctl(t, ctx, result, katlctl, "unknown-boot-health", statusArgs...)); !strings.Contains(status, "cannot read boot health for this boot from systemd") {
		t.Fatalf("node status lacks the unreadable boot health diagnostic:\n%s", status)
	}
	slot := "/dev/disk/by-partuuid/" + fallback.Root.PartitionUUID
	slotBefore := guestFileSHA256(t, ctx, guest, "fallback-root-before", slot)
	upgradeArgs := []string{"node", "upgrade", node.Name, "--config", configPath, "--artifact", upgrade.Path}
	_, err := runKatlctlOutcome(t, ctx, result, katlctl, "refuse-unconfirmed-upgrade", upgradeArgs...)
	for _, want := range []string{"cannot read boot health for this boot from systemd", "katlctl node status", "katlctl cluster apply", "katlctl node reboot"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("upgrade error = %v, want %q", err, want)
		}
	}
	if after := guestFileSHA256(t, ctx, guest, "fallback-root-after", slot); after != slotBefore {
		t.Fatalf("refused upgrade changed the inactive root slot: %s, want %s", after, slotBefore)
	}
	var listed agentapi.ListGenerationsResponse
	mustUnmarshalProtoJSON(t, runKatlctl(t, ctx, result, katlctl, "retained-fallback", listArgs...), &listed)
	for _, id := range []string{fallback.GenerationID, eligible} {
		index := slices.IndexFunc(listed.GetGenerations(), func(gen *agentapi.Generation) bool { return gen.GetGenerationId() == id })
		if index < 0 {
			t.Fatalf("generation %s disappeared while boot health was unknown: %v", id, &listed)
		}
		gen := listed.GetGenerations()[index]
		if id == fallback.GenerationID && gen.GetUnavailableReason() != "" || id == eligible && len(gen.GetProtectedBy()) != 0 {
			t.Fatalf("unexpected fallback availability or prune protection: %v", gen)
		}
	}

	runKatlctl(t, ctx, result, katlctl, "apply-while-health-unknown", "cluster", "apply", "--node", node.Name, "--config", configPath)
	beforeReboot := guestBootID(t, ctx, client)
	runKatlctl(t, ctx, result, katlctl, "repair-reboot", "node", "reboot", node.Name, "--config", configPath)
	_ = client.Close()
	guest, client = reconnectGuestAfterBoot(t, ctx, node, beforeReboot)
	waitForGenerationPruning(t, ctx, guest, eligible)

	beforeUpgrade := guestBootID(t, ctx, client)
	runKatlctl(t, ctx, result, katlctl, "upgrade-after-repair", upgradeArgs...)
	_ = client.Close()
	return reconnectGuestAfterBoot(t, ctx, node, beforeUpgrade)
}

func sysupdateRepairConfig(t *testing.T, ctx context.Context, guest *GuestControl, dir, endpoint, nodeName string, source generation.GenerationSpec) string {
	t.Helper()
	installed, err := manifest.Decode(strings.NewReader(guestCommandOutput(t, ctx, guest, "installed-intent",
		"dd", "if=/var/lib/katl/install/manifest.json", "status=none")))
	if err != nil || installed.Node.Bootstrap == nil {
		t.Fatalf("read installed bootstrap intent: %v", err)
	}
	host, _, err := net.SplitHostPort(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	node := firstInstallWorldSourceNode(nodeName, ControlPlane, "/dev/disk/by-id/virtio-katl-root")
	node["management"] = jsonObject{"address": host}
	data, err := json.Marshal(jsonObject{
		"apiVersion": configbundle.APIVersion,
		"kind":       configbundle.Kind,
		"metadata":   jsonObject{"name": VMTestManagementClusterName},
		"spec": jsonObject{
			"managementAuthentication": "mtls",
			"controlPlaneEndpoint":     jsonObject{"host": "api.katl.test", "port": 6443},
			"kubernetes":               jsonObject{"version": installed.Node.Bootstrap.KubernetesVersion},
			"defaults": jsonObject{
				"access":            jsonObject{"ssh": jsonObject{"authorizedKeys": installed.Node.Identity.SSH.AuthorizedKeys}},
				"kernel":            jsonObject{"commandLine": source.ConfiguredKernelCommandLine},
				"hostConfiguration": vmtestSourceHostConfiguration(),
			},
			"nodes": []jsonObject{node},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "cluster.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writePrunableGeneration(t *testing.T, ctx context.Context, guest *GuestControl, id string, spec generation.GenerationSpec) {
	t.Helper()
	spec.GenerationID = id
	spec.Boot.LoaderEntryPath = "loader/entries/katl-" + id + ".conf"
	spec.Sysexts = nil
	spec.BundledConfexts = nil
	spec.Confexts = nil
	status, err := generation.NewGenerationStatus(spec, generation.CommitStateCommitted, generation.BootStateGood, generation.HealthStateHealthy, spec.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	status.UnavailableReason = "runtime root slot overwritten by an earlier upgrade"
	root := t.TempDir()
	if err := generation.WriteGeneration(root, spec, status); err != nil {
		t.Fatal(err)
	}
	path := "/var/lib/katl/generations/" + id
	for _, name := range []string{"spec.json", "status.json"} {
		writeGuestFile(t, ctx, guest, path+"/"+name, []byte(readFile(t, filepath.Join(root, path, name))), 0o600)
	}
}

func waitForGenerationPruning(t *testing.T, ctx context.Context, guest *GuestControl, id string) {
	t.Helper()
	obsoletePath := "/var/lib/katl/generations/" + id
	deadline := time.Now().Add(30 * time.Second)
	for {
		check, err := guest.RunCommand(ctx, GuestCommandRequest{
			Name: "obsolete-generation-cleanup", Argv: []string{"test", "!", "-e", obsoletePath}, AllowFailure: true,
		})
		if err == nil && check.ExitStatus == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("replaced OS generation %s remained after healthy boot: %v %#v", id, err, check)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func guestFileSHA256(t *testing.T, ctx context.Context, guest *GuestControl, name, path string) string {
	t.Helper()
	fields := strings.Fields(guestCommandOutput(t, ctx, guest, name, "sha256sum", path))
	if len(fields) != 2 || len(fields[0]) != sha256.Size*2 {
		t.Fatalf("%s returned invalid sha256sum output: %q", name, strings.Join(fields, " "))
	}
	return fields[0]
}

func guestFileDigests(t *testing.T, ctx context.Context, guest *GuestControl, phase string, paths []string) map[string]string {
	t.Helper()
	digests := make(map[string]string, len(paths))
	for _, path := range paths {
		digests[path] = guestFileSHA256(t, ctx, guest, phase+"-"+filepath.Base(path), path)
	}
	return digests
}

func assertGuestFileDigests(t *testing.T, ctx context.Context, guest *GuestControl, phase string, want map[string]string) {
	t.Helper()
	for path, digest := range want {
		if got := guestFileSHA256(t, ctx, guest, phase+"-"+filepath.Base(path), path); got != digest {
			t.Fatalf("%s digest after %s = %s, want %s", path, phase, got, digest)
		}
	}
}

func assertHostUpgradeRuntimeReady(t *testing.T, ctx context.Context, guest *GuestControl, phase string) {
	t.Helper()
	readyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := guest.WaitBootComplete(readyCtx); err != nil {
		t.Fatalf("runtime did not become ready after %s: %v", phase, err)
	}
	guestCommand(t, ctx, guest, phase+"-sshd-config", "sshd", "-t")
	assertSystemDiskMounts(t, ctx, guest, phase)
}

// assertSystemDiskMounts checks through the kernel mount table that /, /efi and
// /var come from one disk and that the old Katl disk is neither mounted nor
// repartitioned.
func assertSystemDiskMounts(t *testing.T, ctx context.Context, guest *GuestControl, phase string) {
	t.Helper()
	disks := map[string]string{}
	for _, target := range []string{"/", "/efi", "/var"} {
		source := strings.TrimSpace(guestCommandOutput(t, ctx, guest, phase+"-mount-source", "findmnt", "--noheadings", "--output", "SOURCE", "--mountpoint", target))
		sysfs := strings.TrimSpace(guestCommandOutput(t, ctx, guest, phase+"-mount-sysfs", "readlink", "-f", "/sys/class/block/"+filepath.Base(source)))
		disks[target] = "/dev/" + filepath.Base(filepath.Dir(sysfs))
	}
	if disks["/efi"] != disks["/"] || disks["/var"] != disks["/"] {
		t.Fatalf("%s: system mounts span disks %v, want /efi and /var on the root disk", phase, disks)
	}
	old := strings.TrimSpace(guestCommandOutput(t, ctx, guest, phase+"-old-disk", "readlink", "-f", "/dev/disk/by-id/virtio-katl-old"))
	if disks["/"] == old {
		t.Fatalf("%s: running root is on the old Katl disk %s", phase, old)
	}
	if mounts := guestCommandOutput(t, ctx, guest, phase+"-mounts", "findmnt", "--noheadings", "--list", "--output", "SOURCE"); strings.Contains(mounts, old) {
		t.Fatalf("%s: old Katl disk %s is mounted:\n%s", phase, old, mounts)
	}
	table := guestCommandOutput(t, ctx, guest, phase+"-old-disk-table", "sfdisk", "--dump", old)
	for _, label := range strings.Fields(OldKatlDiskLabels) {
		if !strings.Contains(table, `name="`+label+`"`) {
			t.Fatalf("%s: old Katl disk lost partition %s:\n%s", phase, label, table)
		}
	}
}

func submitHostUpgradeAndWait(t *testing.T, ctx context.Context, endpoint, nodeName, machineID, currentGeneration, candidateGeneration, localRef string, upgrade builtUpgradeImage, kind, configYAML string) (string, *agentapi.OperationStatus) {
	t.Helper()
	conn, katlc := dialKatlcAgentForVMTest(t, ctx, endpoint, nodeName)
	nodeStatus, err := katlc.GetNodeStatus(ctx, &agentapi.GetNodeStatusRequest{})
	if err != nil {
		conn.Close()
		t.Fatalf("read enrolled node identity before host upgrade: %v", err)
	}
	if nodeStatus.GetMachineId() != machineID {
		conn.Close()
		t.Fatalf("host upgrade machine identity = %q, want %q", nodeStatus.GetMachineId(), machineID)
	}
	hostUpgrade := &agentapi.HostUpgradeOperationRequest{
		ImageLocalRef:         localRef,
		ImageSha256:           upgrade.SHA256,
		ImageSizeBytes:        upgrade.SizeBytes,
		CandidateGenerationId: candidateGeneration,
	}
	hostUpgrade.ConfigYaml = configYAML
	accepted, err := katlc.SubmitOperation(ctx, &agentapi.SubmitOperationRequest{
		ApiVersion:                  operation.APIVersion,
		Kind:                        agent.RequestKind,
		ClientRequestId:             "vmtest-host-upgrade-" + candidateGeneration,
		OperationKind:               kind,
		Actor:                       "installed runtime host upgrade vmtest",
		ExpectedEnrollmentId:        nodeStatus.GetEnrollmentId(),
		ExpectedInventoryNodeName:   nodeStatus.GetInventoryNodeName(),
		ExpectedMachineId:           machineID,
		ExpectedCurrentGenerationId: currentGeneration,
		HostUpgrade:                 hostUpgrade,
	})
	conn.Close()
	if err != nil {
		t.Fatalf("submit host upgrade operation: %v", err)
	}
	return accepted.GetOperationId(), waitKatlcOperationTerminal(t, ctx, endpoint, accepted.GetOperationId(), nodeName)
}

func assertHostUpgradePlanRejected(t *testing.T, ctx context.Context, endpoint, nodeName, machineID, currentGeneration, candidateGeneration, localRef string, upgrade builtUpgradeImage, configYAML, want string) {
	t.Helper()
	conn, katlc := dialKatlcAgentForVMTest(t, ctx, endpoint, nodeName)
	defer conn.Close()
	nodeStatus, err := katlc.GetNodeStatus(ctx, &agentapi.GetNodeStatusRequest{})
	if err != nil {
		t.Fatalf("read enrolled node identity before rejected host upgrade plan: %v", err)
	}
	_, err = katlc.SubmitOperation(ctx, &agentapi.SubmitOperationRequest{
		ApiVersion: operation.APIVersion, Kind: agent.RequestKind,
		ClientRequestId: "vmtest-host-upgrade-" + candidateGeneration,
		OperationKind:   "host-upgrade-handoff", Actor: "installed runtime host upgrade vmtest",
		ExpectedEnrollmentId: nodeStatus.GetEnrollmentId(), ExpectedInventoryNodeName: nodeStatus.GetInventoryNodeName(),
		ExpectedMachineId: machineID, ExpectedCurrentGenerationId: currentGeneration, DryRun: true,
		HostUpgrade: &agentapi.HostUpgradeOperationRequest{
			ImageLocalRef: localRef, ImageSha256: upgrade.SHA256, ImageSizeBytes: upgrade.SizeBytes,
			CandidateGenerationId: candidateGeneration, ConfigYaml: configYAML,
		},
	})
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("combined host upgrade plan error = %v, want %q", err, want)
	}
}

// assertHostUpgradePlanAccepted dry-runs a host upgrade, which plans the root
// slots from the booted disk without changing it.
func assertHostUpgradePlanAccepted(t *testing.T, ctx context.Context, endpoint, nodeName, machineID, currentGeneration, candidateGeneration, localRef string, upgrade builtUpgradeImage) {
	t.Helper()
	conn, katlc := dialKatlcAgentForVMTest(t, ctx, endpoint, nodeName)
	defer conn.Close()
	nodeStatus, err := katlc.GetNodeStatus(ctx, &agentapi.GetNodeStatusRequest{})
	if err != nil {
		t.Fatalf("read enrolled node identity before host upgrade plan: %v", err)
	}
	_, err = katlc.SubmitOperation(ctx, &agentapi.SubmitOperationRequest{
		ApiVersion: operation.APIVersion, Kind: agent.RequestKind,
		ClientRequestId: "vmtest-host-upgrade-plan-" + candidateGeneration,
		OperationKind:   "host-upgrade-handoff", Actor: "installed runtime host upgrade vmtest",
		ExpectedEnrollmentId: nodeStatus.GetEnrollmentId(), ExpectedInventoryNodeName: nodeStatus.GetInventoryNodeName(),
		ExpectedMachineId: machineID, ExpectedCurrentGenerationId: currentGeneration, DryRun: true,
		HostUpgrade: &agentapi.HostUpgradeOperationRequest{
			ImageLocalRef: localRef, ImageSha256: upgrade.SHA256, ImageSizeBytes: upgrade.SizeBytes,
			CandidateGenerationId: candidateGeneration,
		},
	})
	if err != nil {
		t.Fatalf("host upgrade plan after failed staging: %v", err)
	}
}

func hostUpgradeConfiguration(version, overlay string) string {
	return "apiVersion: katl.dev/v1alpha1\n" +
		"kind: NodeConfigurationChange\n" +
		"metadata:\n" +
		"  sourceID: vmtest\n" +
		"  desiredVersion: \"" + version + "\"\n" +
		"apply:\n" +
		"  mode: next-boot\n" +
		"spec:\n" +
		"  clusterDefaults:\n" + overlay
}

func waitHostUpgradeWorkspaceClean(t *testing.T, ctx context.Context, guest *GuestControl, phase string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		remaining := strings.TrimSpace(guestCommandOutput(t, ctx, guest, "host-upgrade-workspace-clean-"+phase, "find", "/var/lib/katl/artifacts/host-upgrade", "-type", "f", "-print", "-quit"))
		resources := hostUpgradeImageResources(t, ctx, guest, phase)
		if remaining == "" && resources == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s host upgrade retained transient artifact %s or native image resources %s", phase, remaining, resources)
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func hostUpgradeImageResources(t *testing.T, ctx context.Context, guest *GuestControl, phase string) string {
	t.Helper()
	const base = "/var/lib/katl/artifacts/host-upgrade"
	mounts := guestCommandOutput(t, ctx, guest, "host-upgrade-mounts-"+phase, "findmnt", "--raw", "--noheadings", "--output", "TARGET,SOURCE")
	loops := guestCommandOutput(t, ctx, guest, "host-upgrade-loops-"+phase, "losetup", "--list", "--noheadings", "--output", "BACK-FILE")
	for _, line := range strings.Split(mounts+"\n"+loops, "\n") {
		if strings.Contains(line, base+"/") {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

func stageHostUpgradeArtifactForVMTest(t *testing.T, ctx context.Context, endpoint, nodeName, machineID string, upgrade builtUpgradeImage) string {
	t.Helper()
	conn, katlc := dialKatlcAgentForVMTest(t, ctx, endpoint, nodeName)
	defer conn.Close()
	nodeStatus, err := katlc.GetNodeStatus(ctx, &agentapi.GetNodeStatusRequest{})
	if err != nil {
		t.Fatalf("read enrolled node identity before host upgrade upload: %v", err)
	}
	if nodeStatus.GetMachineId() != machineID {
		t.Fatalf("host upgrade upload machine identity = %q, want %q", nodeStatus.GetMachineId(), machineID)
	}
	stream, err := katlc.StageHostUpgradeArtifact(ctx)
	if err != nil {
		t.Fatalf("start host upgrade artifact staging: %v", err)
	}
	file, err := os.Open(upgrade.Path)
	if err != nil {
		t.Fatalf("open host upgrade artifact: %v", err)
	}
	defer file.Close()
	buffer := make([]byte, 1<<20)
	first := true
	for {
		n, readErr := file.Read(buffer)
		if n > 0 {
			request := &agentapi.StageHostUpgradeArtifactRequest{Chunk: append([]byte(nil), buffer[:n]...)}
			if first {
				request.ApiVersion = operation.APIVersion
				request.Kind = agent.StageHostUpgradeArtifactRequestKind
				request.Actor = "installed runtime host upgrade vmtest"
				request.ExpectedEnrollmentId = nodeStatus.GetEnrollmentId()
				request.ExpectedInventoryNodeName = nodeStatus.GetInventoryNodeName()
				request.ExpectedMachineId = machineID
				request.ExpectedCurrentGenerationId = nodeStatus.GetCurrentGenerationId()
				request.Sha256 = upgrade.SHA256
				request.SizeBytes = upgrade.SizeBytes
				first = false
			}
			if err := stream.Send(request); err != nil {
				if err == io.EOF {
					if _, receiveErr := stream.CloseAndRecv(); receiveErr != nil {
						err = receiveErr
					}
				}
				t.Fatalf("send host upgrade artifact: %v", err)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			t.Fatalf("read host upgrade artifact: %v", readErr)
		}
	}
	staged, err := stream.CloseAndRecv()
	if err != nil {
		t.Fatalf("finish host upgrade artifact staging: %v", err)
	}
	wantRef := "host-upgrade/uploads/" + upgrade.SHA256 + ".squashfs"
	if staged.GetLocalRef() != wantRef || staged.GetSha256() != upgrade.SHA256 || staged.GetSizeBytes() != upgrade.SizeBytes {
		t.Fatalf("staged host upgrade artifact = %#v", staged)
	}
	return staged.GetLocalRef()
}

type builtUpgradeImage struct {
	Path      string
	Version   string
	SHA256    string
	SizeBytes uint64
}

func pinnedUpgradeImage(t *testing.T, world World, baseVersion string) builtUpgradeImage {
	t.Helper()
	upgrade := requirePinnedArtifact(t, world, "katlos-upgrade-image")
	root := requirePinnedArtifact(t, world, "runtime-root")
	metadata, err := katlosimage.ReadArtifactMetadata(upgrade.Path+".json", katlosimage.RoleUpgrade)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Version == baseVersion {
		t.Fatalf("pinned KatlOS upgrade image has the installed version %s", baseVersion)
	}
	indexData, err := exec.Command("unsquashfs", "-cat", upgrade.Path, metadata.EmbeddedIndexPath).Output()
	if err != nil {
		t.Fatalf("read KatlOS upgrade image index: %v", err)
	}
	var index katlosimage.Index
	if err := json.Unmarshal(indexData, &index); err != nil {
		t.Fatalf("decode KatlOS upgrade image index: %v", err)
	}
	rootMatches := false
	for _, component := range index.Components {
		if component.Role == katlosimage.ComponentRuntimeRoot && component.SHA256 == root.Digest {
			rootMatches = true
		}
	}
	if !rootMatches {
		t.Fatal("KatlOS upgrade image does not contain the current runtime root; rebuild the upgrade image")
	}
	return builtUpgradeImage{Path: upgrade.Path, Version: metadata.Version, SHA256: upgrade.Digest, SizeBytes: uint64(upgrade.SizeBytes)}
}

func requirePinnedArtifact(t *testing.T, world World, name string) WorldArtifact {
	t.Helper()
	for _, artifact := range world.Artifacts {
		if artifact.Name == name && artifact.Pinned {
			return artifact
		}
	}
	t.Fatalf("VM test world did not pin %s; build the required artifacts before using --no-rebuild", name)
	return WorldArtifact{}
}

func assertBootedGenerationIdentity(t *testing.T, ctx context.Context, guest *GuestControl, spec generation.GenerationSpec) {
	t.Helper()
	kernelFlavour, err := flavour.Normalize(spec.Root.Flavour)
	if err != nil {
		t.Fatal(err)
	}
	osRelease := readGuestFile(t, ctx, guest, "/usr/lib/os-release")
	if !strings.Contains(osRelease, "\nKATL_FLAVOUR="+kernelFlavour+"\n") {
		t.Fatalf("generation flavour %q disagrees with booted OS: %s", kernelFlavour, osRelease)
	}
	if got := currentGenerationFromGuest(t, ctx, guest); got != spec.GenerationID {
		t.Fatalf("booted generation = %q, want %q", got, spec.GenerationID)
	}
	cmdline := readGuestFile(t, ctx, guest, "/proc/cmdline")
	if !strings.Contains(cmdline, "katl.generation="+spec.GenerationID) || !strings.Contains(cmdline, "root=PARTUUID="+spec.Root.PartitionUUID) {
		t.Fatalf("booted kernel command line does not identify generation root: %s", cmdline)
	}
	for _, argument := range spec.KernelCommandLine {
		if !strings.Contains(cmdline, argument) {
			t.Fatalf("booted kernel command line is missing generation argument %q: %s", argument, cmdline)
		}
	}
	guestCommand(t, ctx, guest, "verify-automatic-efi-mount", "findmnt", "--mountpoint", "/efi")
	assertGuestExists(t, ctx, guest, spec.Boot.UKIPath)
}

type sysupdateGuestFixture struct {
	RootPath         string
	UKIPath          string
	RootSourcePath   string
	UKISourcePath    string
	RootTransferPath string
	UKITransferPath  string
}

func writeSysupdateGuestFixtureFromImage(t *testing.T, ctx context.Context, guest *GuestControl, runDir, version string, payload katlosimage.Payload) sysupdateGuestFixture {
	t.Helper()
	rootBytes, err := os.ReadFile(payload.ComponentPath(payload.Runtime))
	if err != nil {
		t.Fatalf("read runtime-root image component: %v", err)
	}
	ukiBytes, err := os.ReadFile(payload.ComponentPath(payload.Boot))
	if err != nil {
		t.Fatalf("read runtime-uki image component: %v", err)
	}
	if got := sha256Hex(rootBytes); got != payload.Runtime.SHA256 {
		t.Fatalf("runtime-root source digest = %s, want image component %s", got, payload.Runtime.SHA256)
	}
	if got := sha256Hex(ukiBytes); got != payload.Boot.SHA256 {
		t.Fatalf("runtime-uki source digest = %s, want image component %s", got, payload.Boot.SHA256)
	}
	guestSource := "/var/lib/katl/test-artifacts/sysupdate/source"
	guestDefinitions := "/var/lib/katl/test-artifacts/sysupdate/sysupdate.d"
	hostSource := filepath.Join(runDir, "sysupdate-source")
	hostDefinitions := filepath.Join(runDir, "sysupdate.d")
	rootName := "katl_" + version + ".root.squashfs"
	ukiName := "katl_" + version + ".efi"
	rootPath := guestSource + "/" + rootName
	ukiPath := guestSource + "/" + ukiName
	hostRootPath := filepath.Join(hostSource, rootName)
	hostUKIPath := filepath.Join(hostSource, ukiName)
	if err := os.MkdirAll(hostSource, 0o755); err != nil {
		t.Fatalf("create host sysupdate source: %v", err)
	}
	if err := os.MkdirAll(hostDefinitions, 0o755); err != nil {
		t.Fatalf("create host sysupdate definitions: %v", err)
	}
	if err := os.WriteFile(hostRootPath, rootBytes, 0o644); err != nil {
		t.Fatalf("write host sysupdate root source: %v", err)
	}
	if err := os.WriteFile(hostUKIPath, ukiBytes, 0o644); err != nil {
		t.Fatalf("write host sysupdate UKI source: %v", err)
	}
	shaSums := fmt.Sprintf("%s  %s\n%s  %s\n", sha256Hex(rootBytes), rootName, sha256Hex(ukiBytes), ukiName)
	if err := os.WriteFile(filepath.Join(hostSource, "SHA256SUMS"), []byte(shaSums), 0o644); err != nil {
		t.Fatalf("write host sysupdate SHA256SUMS: %v", err)
	}
	hostRootTransfer := filepath.Join(hostDefinitions, "50-katl-root.transfer")
	hostUKITransfer := filepath.Join(hostDefinitions, "70-katl-uki.transfer")
	if err := os.WriteFile(hostRootTransfer, []byte(rootTransfer(guestSource)), 0o644); err != nil {
		t.Fatalf("write host root transfer: %v", err)
	}
	if err := os.WriteFile(hostUKITransfer, []byte(ukiTransfer(guestSource)), 0o644); err != nil {
		t.Fatalf("write host UKI transfer: %v", err)
	}
	writeGuestFile(t, ctx, guest, rootPath, rootBytes, 0o644)
	writeGuestFile(t, ctx, guest, ukiPath, ukiBytes, 0o644)
	writeGuestFile(t, ctx, guest, guestSource+"/SHA256SUMS", []byte(shaSums), 0o644)
	writeGuestFile(t, ctx, guest, guestDefinitions+"/50-katl-root.transfer", []byte(rootTransfer(guestSource)), 0o644)
	writeGuestFile(t, ctx, guest, guestDefinitions+"/70-katl-uki.transfer", []byte(ukiTransfer(guestSource)), 0o644)
	return sysupdateGuestFixture{
		RootPath:         rootPath,
		UKIPath:          ukiPath,
		RootSourcePath:   hostRootPath,
		UKISourcePath:    hostUKIPath,
		RootTransferPath: hostRootTransfer,
		UKITransferPath:  hostUKITransfer,
	}
}

func writeSysupdateUpgradeImagePayload(t *testing.T, runDir string, version string, rootBytes, ukiBytes []byte) (katlosimage.Payload, string) {
	t.Helper()
	imageRoot := filepath.Join(runDir, "katlos-upgrade-image")
	components := map[string][]byte{
		"components/runtime/root.squashfs": rootBytes,
		"components/boot/katl.efi":         ukiBytes,
	}
	digests := make(map[string]string, len(components))
	sizes := make(map[string]int64, len(components))
	for rel, data := range components {
		path := filepath.Join(imageRoot, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create upgrade image component dir: %v", err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatalf("write upgrade image component: %v", err)
		}
		digests[rel] = sha256Hex(data)
		sizes[rel] = int64(len(data))
	}
	index := katlosimage.Index{
		APIVersion:       katlosimage.APIVersion,
		Kind:             katlosimage.Kind,
		ImageRole:        katlosimage.RoleUpgrade,
		Format:           katlosimage.FormatSquashFS,
		Version:          version,
		BuildID:          "vmtest-sysupdate-" + version,
		Architecture:     "x86_64",
		RuntimeInterface: "katl-runtime-1",
		CreatedAt:        "2026-06-17T12:00:00Z",
		Components: []katlosimage.Component{
			{
				Name:         "runtime-root",
				Role:         katlosimage.ComponentRuntimeRoot,
				Path:         "components/runtime/root.squashfs",
				Format:       "squashfs",
				SizeBytes:    sizes["components/runtime/root.squashfs"],
				SHA256:       digests["components/runtime/root.squashfs"],
				Version:      version,
				Architecture: "x86_64",
				Compatibility: katlosimage.Compatibility{
					RuntimeInterface: "katl-runtime-1",
				},
				InstallTarget: katlosimage.InstallTarget{
					Kind:       "root-slot",
					Filesystem: "squashfs",
				},
			},
			{
				Name:         "runtime-uki",
				Role:         katlosimage.ComponentRuntimeUKI,
				Path:         "components/boot/katl.efi",
				Format:       "uki",
				SizeBytes:    sizes["components/boot/katl.efi"],
				SHA256:       digests["components/boot/katl.efi"],
				Version:      version,
				Architecture: "x86_64",
				Compatibility: katlosimage.Compatibility{
					RuntimeInterface: "katl-runtime-1",
					RuntimeRoot: katlosimage.RuntimeRoot{
						Interface:      "katl-runtime-1",
						ArtifactPath:   "components/runtime/root.squashfs",
						ArtifactSHA256: digests["components/runtime/root.squashfs"],
					},
					KernelCommandLine: []string{"quiet"},
				},
				InstallTarget: katlosimage.InstallTarget{
					Kind:     "esp-or-xbootldr",
					Filename: "katl.efi",
				},
			},
		},
	}
	if err := os.MkdirAll(filepath.Join(imageRoot, "katlos"), 0o755); err != nil {
		t.Fatalf("create upgrade image index dir: %v", err)
	}
	indexData, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		t.Fatalf("marshal upgrade image index: %v", err)
	}
	if err := os.WriteFile(filepath.Join(imageRoot, "katlos", "image.json"), append(indexData, '\n'), 0o644); err != nil {
		t.Fatalf("write upgrade image index: %v", err)
	}
	imagePath := filepath.Join(runDir, "katlos-upgrade-"+version+"-x86_64.squashfs")
	if output, err := exec.Command("mksquashfs", imageRoot, imagePath, "-noappend", "-quiet").CombinedOutput(); err != nil {
		t.Fatalf("build upgrade image squashfs: %v\n%s", err, output)
	}
	imageInfo, err := os.Stat(imagePath)
	if err != nil {
		t.Fatalf("stat upgrade image squashfs: %v", err)
	}
	imageSHA, err := fileSHA256(imagePath)
	if err != nil {
		t.Fatalf("hash upgrade image squashfs: %v", err)
	}
	expected := manifest.KatlosImage{
		LocalRef:         filepath.Base(imagePath),
		SHA256:           imageSHA,
		SizeBytes:        uint64(imageInfo.Size()),
		Version:          version,
		Architecture:     "x86_64",
		RuntimeInterface: "katl-runtime-1",
		Role:             katlosimage.RoleUpgrade,
	}
	payload, err := katlosimage.ResolveDirectory(context.Background(), imageRoot, expected)
	if err != nil {
		t.Fatalf("resolve upgrade image payload: %v", err)
	}
	return payload, imagePath
}

func rootTransfer(source string) string {
	return fmt.Sprintf(`[Transfer]
ProtectVersion=0

[Source]
Type=regular-file
Path=%s
MatchPattern=katl_@v.root.squashfs

[Target]
Type=partition
Path=auto
MatchPattern=katl_@v
MatchPartitionType=root
ReadOnly=1
InstancesMax=2
`, source)
}

func ukiTransfer(source string) string {
	return fmt.Sprintf(`[Transfer]
ProtectVersion=0

[Source]
Type=regular-file
Path=%s
MatchPattern=katl_@v.efi

[Target]
Type=regular-file
Path=/EFI/Linux
PathRelativeTo=boot
MatchPattern=katl_@v+@l-@d.efi katl_@v+@l.efi katl_@v.efi
Mode=0644
TriesLeft=1
TriesDone=0
InstancesMax=2
`, source)
}

func rootSlotsFromSpec(t *testing.T, spec string) (string, string, string, string) {
	t.Helper()
	switch {
	case strings.Contains(spec, `"slot": "root-a"`):
		return "root-a", "root-b", "KATL_ROOT_A", "KATL_ROOT_B"
	case strings.Contains(spec, `"slot": "root-b"`):
		return "root-b", "root-a", "KATL_ROOT_B", "KATL_ROOT_A"
	default:
		t.Fatalf("current generation spec does not identify root-a/root-b slot:\n%s", spec)
		return "", "", "", ""
	}
}

type candidateGenerationSpecInput struct {
	GenerationID   string
	RuntimeVersion string
	Slot           string
	PartitionUUID  string
	RootSHA256     string
	UKIPath        string
	LoaderEntry    string
	CreatedAt      time.Time
}

func candidateGenerationSpec(t *testing.T, currentSpec string, input candidateGenerationSpecInput) generation.GenerationSpec {
	t.Helper()
	var current generation.GenerationSpec
	if err := json.Unmarshal([]byte(currentSpec), &current); err != nil {
		t.Fatalf("decode current generation spec: %v\n%s", err, currentSpec)
	}
	spec := current
	spec.GenerationID = input.GenerationID
	spec.RuntimeVersion = input.RuntimeVersion
	spec.PreviousGenerationID = current.GenerationID
	spec.Root.Slot = input.Slot
	spec.Root.PartitionUUID = input.PartitionUUID
	spec.Root.RuntimeVersion = input.RuntimeVersion
	spec.Root.RuntimeArtifactSHA256 = input.RootSHA256
	spec.Boot.UKIPath = input.UKIPath
	spec.Boot.LoaderEntryPath = input.LoaderEntry
	spec.CreatedAt = input.CreatedAt
	if err := generation.ValidateGenerationSpec(spec); err != nil {
		t.Fatalf("candidate generation spec invalid: %v\n%#v", err, spec)
	}
	return spec
}

func partitionDiskAndNumber(t *testing.T, device string) (string, string) {
	t.Helper()
	if device == "" {
		t.Fatal("partition device is empty")
	}
	lastDigit := len(device) - 1
	for lastDigit >= 0 && device[lastDigit] >= '0' && device[lastDigit] <= '9' {
		lastDigit--
	}
	if lastDigit == len(device)-1 {
		t.Fatalf("partition device %q has no numeric suffix", device)
	}
	disk := device[:lastDigit+1]
	part := device[lastDigit+1:]
	if strings.HasSuffix(disk, "p") {
		disk = strings.TrimSuffix(disk, "p")
	}
	if disk == "" || part == "" || disk == device {
		t.Fatalf("could not split partition device %q", device)
	}
	return disk, part
}

func generationFromGuest(t *testing.T, ctx context.Context, guest *GuestControl, generationID string) generation.GenerationSpec {
	t.Helper()
	spec, _ := generationRecordsFromGuest(t, ctx, guest, generationID)
	return spec
}

func writeGuestJSON(t *testing.T, ctx context.Context, guest *GuestControl, path string, value any) {
	t.Helper()
	data, err := generation.MarshalCanonicalJSON(value)
	if err != nil {
		t.Fatalf("marshal guest JSON %s: %v", path, err)
	}
	recordType := ""
	switch value.(type) {
	case generation.BootSelectionRecord:
		recordType = generation.BootSelectionRecordType
	case generation.GenerationSpec:
		recordType = generation.GenerationSpecRecordType
	case generation.GenerationStatus:
		recordType = generation.GenerationStatusRecordType
	}
	if recordType != "" {
		data, err = persistedrecord.MarshalEnvelope(persistedrecord.Envelope{
			RecordType:    recordType,
			RecordVersion: 1,
			Payload:       data,
		})
		if err != nil {
			t.Fatalf("marshal guest boot selection envelope %s: %v", path, err)
		}
	}
	writeGuestFile(t, ctx, guest, path, data, 0o644)
}

func collectSysupdateFailureEvidence(ctx context.Context, guest *GuestControl) {
	guest.CollectDiagnostics(ctx, GuestDiagnostics{
		Timeout: 2 * time.Minute,
		// The journal of the failing boot, and of the boot before it when the
		// test had already rebooted.
		Journals: []GuestJournalRequest{
			{Name: "upgrade-journal", MaxBytes: 4 << 20},
			{Name: "upgrade-previous-boot-journal", BootID: "-1", MaxBytes: 4 << 20},
			{Name: "upgrade-agent-journal", Units: []string{"katlc-agent.service"}},
		},
		Commands: []GuestCommandRequest{
			{Name: "upgrade-state-storage", Argv: []string{"findmnt", "--df", "--bytes", "--target", "/var/lib/katl"}},
			{Name: "upgrade-efi-storage", Argv: []string{"findmnt", "--df", "--bytes", "--target", "/efi"}},
			{Name: "sysupdate-status", Argv: []string{"/usr/lib/systemd/systemd-sysupdate", "--no-pager", "--verify=no", "--definitions=/var/lib/katl/test-artifacts/sysupdate/sysupdate.d", "list"}},
			{Name: "root-labels", Argv: []string{"blkid", "-o", "export"}, StdoutLimit: 1 << 20},
			{Name: "sysupdate-files", Argv: []string{"find", "/var/lib/katl/test-artifacts/sysupdate", "-maxdepth", "4", "-type", "f", "-print"}},
			{Name: "efi-linux-files", Argv: []string{"find", "/efi/EFI/Linux", "-maxdepth", "1", "-type", "f", "-print"}},
		},
	})
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func selectBootGeneration(t *testing.T, ctx context.Context, endpoint, node, id string, oneShot bool) {
	t.Helper()
	conn, client := dialKatlcAgentForVMTest(t, ctx, endpoint, node)
	defer conn.Close()
	state, err := client.GetNodeStatus(ctx, &agentapi.GetNodeStatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.SelectGeneration(ctx, &agentapi.GenerationMutationRequest{GenerationId: id, OneShot: oneShot, ExpectedMachineId: state.MachineId, ExpectedCurrentGenerationId: state.CurrentGenerationId, ExpectedEnrollmentId: state.EnrollmentId, ExpectedInventoryNodeName: state.InventoryNodeName})
	if err != nil {
		t.Fatalf("select generation: %v", err)
	}
}

func waitActiveGeneration(t *testing.T, ctx context.Context, guest *GuestControl, id string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		selection := bootSelectionFromGuest(t, ctx, guest)
		if selection.ActiveGenerationID == id && selection.BootedGenerationID == id && selection.TargetBootGenerationID == "" && !selection.PendingHealthValidation {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("generation %s did not become active", id)
}

// inactiveRootSlot returns the system disk device and the partition number of
// the root slot that is not mounted at /, read from the on-disk partition
// table.
func inactiveRootSlot(t *testing.T, ctx context.Context, guest *GuestControl) (string, string) {
	t.Helper()
	const rootType = "4F68BCE3-E8CD-4DB1-96E7-FBCAF984B709"
	source := strings.TrimSpace(guestCommandOutput(t, ctx, guest, "root-source", "findmnt", "-no", "SOURCE", "/"))
	parent := strings.TrimSpace(guestCommandOutput(t, ctx, guest, "root-disk", "lsblk", "-no", "PKNAME", source))
	disk := "/dev/" + parent
	for _, line := range strings.Split(guestCommandOutput(t, ctx, guest, "root-slots", "sfdisk", "--dump", disk), "\n") {
		device, rest, ok := strings.Cut(line, " : ")
		if !ok || device == source || !strings.Contains(strings.ToUpper(rest), "TYPE="+rootType) {
			continue
		}
		// nvme and mmc names put a "p" between the disk and the number.
		return disk, strings.TrimPrefix(strings.TrimPrefix(device, disk), "p")
	}
	t.Fatalf("no root slot other than %s on %s", source, disk)
	return "", ""
}
