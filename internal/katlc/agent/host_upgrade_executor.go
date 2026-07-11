package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/zariel/katl/internal/installer/disk"
	"github.com/zariel/katl/internal/installer/generation"
	"github.com/zariel/katl/internal/installer/katlosimage"
	"github.com/zariel/katl/internal/installer/manifest"
	"github.com/zariel/katl/internal/installer/operation"
)

func (e *Executor) executeHostUpgrade(ctx context.Context, record operation.OperationRecord) error {
	if record.HostUpgradeRequest == nil {
		return fmt.Errorf("host upgrade request is required")
	}
	resolve := e.ResolveHostUpgrade
	if resolve == nil {
		resolve = e.resolveHostUpgrade
	}
	payload, err := resolve(ctx, *record.HostUpgradeRequest)
	if err != nil {
		return e.failHostUpgrade(record, "verify-katlos-image", err)
	}
	defer e.cleanupHostUpgradeMount(payload)
	currentID, err := currentGenerationID(e.Root)
	if err != nil {
		return e.failHostUpgrade(record, "verify-katlos-image", err)
	}
	previousSpec, previousStatus, err := generation.ReadGeneration(e.Root, currentID)
	if err != nil {
		return e.failHostUpgrade(record, "verify-katlos-image", fmt.Errorf("read current generation: %w", err))
	}
	inactiveSlot, inactiveLabel, activeLabel, err := inactiveRoot(previousSpec.Root.Slot)
	if err != nil {
		return e.failHostUpgrade(record, "verify-katlos-image", err)
	}
	slots, err := e.inspectRootSlots(ctx, activeLabel, inactiveLabel)
	if err != nil {
		return e.failHostUpgrade(record, "verify-katlos-image", err)
	}
	candidate := record.HostUpgradeRequest.CandidateGenerationID
	ukiPath := "/efi/EFI/Linux/katl_" + payload.Index.Version + ".efi"
	entry := "loader/entries/katl-" + candidate + ".conf"
	plan, err := payload.HostUpgradePlan(katlosimage.HostUpgradeRequest{
		GenerationID:      candidate,
		PreviousSpec:      previousSpec,
		PreviousStatus:    previousStatus,
		RootSlot:          inactiveSlot,
		RootPartitionUUID: slots.InactivePartUUID,
		UKIPath:           ukiPath,
		LoaderEntryPath:   entry,
		OperationID:       record.OperationID,
		Bootstrapped:      len(previousSpec.Sysexts) > 0,
		CreatedAt:         e.clock(),
	})
	if err != nil {
		return e.failHostUpgrade(record, "verify-katlos-image", err)
	}
	record, err = e.Store.Update(record.OperationID, "host-upgrade-mutation-start", "stage-sysupdate-components", func(current operation.OperationRecord) (operation.OperationRecord, error) {
		current.Phase = "stage-sysupdate-components"
		current.ExternalMutationStarted = true
		current.MutationScopes = appendMissing(current.MutationScopes, "root-slot-labels", "runtime-root", "runtime-uki")
		current.UpdatedAt = e.clock()
		current.NextAction = "stage verified root and UKI components through systemd-sysupdate"
		return current, nil
	})
	if err != nil {
		return err
	}
	if err := e.prepareSysupdateSlots(ctx, slots, previousSpec.RuntimeVersion); err != nil {
		return e.failHostUpgrade(record, "stage-sysupdate-components", err)
	}
	if err := e.stageHostUpgrade(ctx, record, payload, slots.InactiveDevice, ukiPath); err != nil {
		return e.failHostUpgrade(record, "stage-sysupdate-components", err)
	}
	if err := katlosimage.StagePreservedAssets(runtimeRoot(e.Root), plan); err != nil {
		return e.failHostUpgrade(record, "write-candidate-generation", err)
	}
	if err := generation.WriteGeneration(e.Root, plan.Spec, plan.Status); err != nil {
		return e.failHostUpgrade(record, "write-candidate-generation", err)
	}
	bootRoot := filepath.Join(runtimeRoot(e.Root), "efi")
	if e.MountBootRoot != nil {
		if err := e.MountBootRoot(ctx, runtimeRoot(e.Root)); err != nil {
			return e.failHostUpgrade(record, "write-candidate-generation", err)
		}
	}
	machineID, err := os.ReadFile(filepath.Join(runtimeRoot(e.Root), "etc/machine-id"))
	if err != nil {
		return e.failHostUpgrade(record, "write-candidate-generation", err)
	}
	written, err := generation.WriteEntry(bootRoot, generation.LoaderRequest{
		Record:    generation.RecordFromSplit(plan.Spec, plan.Status),
		MachineID: strings.TrimSpace(string(machineID)),
	})
	if err != nil {
		return e.failHostUpgrade(record, "write-candidate-generation", err)
	}
	rel, err := bootRelativePath(bootRoot, written)
	if err != nil || rel != entry {
		return e.failHostUpgrade(record, "write-candidate-generation", fmt.Errorf("loader entry %q does not match plan %q: %w", rel, entry, err))
	}
	previousSelection, err := generation.ReadBootSelection(e.Root)
	if err != nil {
		return e.failHostUpgrade(record, "arm-trial-boot", err)
	}
	if err := generation.WriteBootSelection(e.Root, plan.BootSelection); err != nil {
		return e.failHostUpgrade(record, "arm-trial-boot", err)
	}
	if e.SetBootOneshot != nil {
		if err := e.SetBootOneshot(ctx, e.Root, entry); err != nil {
			if restoreErr := generation.WriteBootSelection(e.Root, previousSelection); restoreErr != nil {
				err = errors.Join(err, fmt.Errorf("restore boot selection: %w", restoreErr))
			}
			return e.failHostUpgrade(record, "arm-trial-boot", err)
		}
	}
	now := e.clock()
	plan.Status.CommitState = generation.CommitStateCommitted
	plan.Status.BootState = generation.BootStateTrying
	plan.Status.CommittedAt = &now
	plan.Status.CommittedByOperation = record.OperationID
	plan.Status.UpdatedAt = now
	if err := generation.WriteGenerationStatus(e.Root, plan.Spec, plan.Status); err != nil {
		return e.failHostUpgrade(record, "arm-trial-boot", err)
	}
	_, err = e.Store.Update(record.OperationID, "host-upgrade-staged", "arm-trial-boot", func(current operation.OperationRecord) (operation.OperationRecord, error) {
		current.PreviousGenerationID = previousSpec.GenerationID
		current.CompletedPhases = appendMissing(current.CompletedPhases, "accepted", "verify-katlos-image", "stage-sysupdate-components", "write-candidate-generation", "arm-trial-boot")
		current.Phase = "arm-trial-boot"
		current.PhaseIndex = len(current.CompletedPhases)
		current.ExternalMutationStarted = true
		current.MutationScopes = appendMissing(current.MutationScopes, "runtime-root", "runtime-uki", "boot-selection", "generation-state")
		current.ActivationState = operation.ActivationStatePending
		current.GenerationCommitState = operation.GenerationCommitCommitted
		current.BootHealthPending = true
		current.HostRollback = previousSpec.GenerationID
		current.PostMutationRollbackAllowed = true
		current.Terminal = true
		current.Result = operation.ResultSucceeded
		current.CompletedAt = &now
		current.UpdatedAt = now
		current.NextAction = "reboot into the bounded candidate trial; promote only after boot health passes"
		return current, nil
	})
	return err
}

func (e *Executor) cleanupHostUpgradeMount(payload katlosimage.Payload) {
	root := runtimeRoot(e.Root)
	mounts := filepath.Join(root, "var/lib/katl/artifacts/host-upgrade/mounts")
	rel, err := filepath.Rel(mounts, payload.Root)
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), bootRootMountTimeout)
	defer cancel()
	_ = hostUpgradeCommands{run: e.toolRunner()}.Run(ctx, "umount", payload.Root)
}

func (e *Executor) resolveHostUpgrade(ctx context.Context, request operation.HostUpgrade) (katlosimage.Payload, error) {
	root := runtimeRoot(e.Root)
	work := filepath.Join(root, "var/lib/katl/artifacts/host-upgrade")
	return (katlosimage.Resolver{
		MediaRoot: filepath.Join(root, "var/lib/katl/artifacts"),
		WorkDir:   work,
		Commands:  hostUpgradeCommands{run: e.toolRunner()},
		Client:    e.BundleClient,
	}).ResolveKatlosImage(ctx, manifest.KatlosImage{
		URL:       request.ImageURL,
		LocalRef:  request.ImageLocalRef,
		SHA256:    request.ImageSHA256,
		SizeBytes: request.ImageSizeBytes,
		Role:      katlosimage.RoleUpgrade,
	})
}

type hostUpgradeCommands struct{ run ToolRunner }

func (c hostUpgradeCommands) Run(ctx context.Context, name string, args ...string) error {
	result := c.run(ctx, append([]string{name}, args...), nil)
	if result.Err != nil || result.ExitStatus != 0 {
		return fmt.Errorf("%s: %s", name, toolFailure(result))
	}
	return nil
}

func (e *Executor) stageHostUpgrade(ctx context.Context, record operation.OperationRecord, payload katlosimage.Payload, inactiveDevice, ukiPath string) error {
	root := runtimeRoot(e.Root)
	work := filepath.Join(root, "var/lib/katl/artifacts/host-upgrade", record.OperationID)
	source := filepath.Join(work, "source")
	definitions := filepath.Join(work, "sysupdate.d")
	if err := os.MkdirAll(source, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(definitions, 0o700); err != nil {
		return err
	}
	rootName := "katl_" + payload.Index.Version + ".root.squashfs"
	ukiName := "katl_" + payload.Index.Version + ".efi"
	if err := copyUpgradeComponent(payload.ComponentPath(payload.Runtime), filepath.Join(source, rootName)); err != nil {
		return err
	}
	if err := copyUpgradeComponent(payload.ComponentPath(payload.Boot), filepath.Join(source, ukiName)); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(definitions, "50-katl-root.transfer"), []byte(rootTransferDefinition(source)), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(definitions, "70-katl-uki.transfer"), []byte(ukiTransferDefinition(source)), 0o600); err != nil {
		return err
	}
	argv := []string{"/usr/lib/systemd/systemd-sysupdate", "--no-pager", "--verify=no", "--sync=no", "--definitions=" + definitions}
	if root != "/" {
		argv = append(argv, "--root="+root)
	}
	argv = append(argv, "update", payload.Index.Version)
	result := e.toolRunner()(ctx, argv, nil)
	if result.Err != nil || result.ExitStatus != 0 {
		return fmt.Errorf("systemd-sysupdate: %s", toolFailure(result))
	}
	if err := verifyUpgradePrefix(inactiveDevice, payload.Runtime.SHA256, payload.Runtime.SizeBytes); err != nil {
		return fmt.Errorf("verify staged runtime root: %w", err)
	}
	if err := verifyUpgradeFile(filepath.Join(root, strings.TrimPrefix(ukiPath, "/")), payload.Boot.SHA256); err != nil {
		return fmt.Errorf("verify staged runtime UKI: %w", err)
	}
	return nil
}

type rootSlots struct {
	ActiveDevice     string
	ActiveDisk       string
	ActivePart       string
	InactiveDevice   string
	InactiveDisk     string
	InactivePart     string
	InactivePartUUID string
}

func (e *Executor) inspectRootSlots(ctx context.Context, activeLabel, inactiveLabel string) (rootSlots, error) {
	active, err := e.toolOutput(ctx, "blkid", "-t", "PARTLABEL="+activeLabel, "-o", "device")
	if err != nil {
		return rootSlots{}, fmt.Errorf("find active root slot: %w", err)
	}
	inactive, err := e.toolOutput(ctx, "blkid", "-t", "PARTLABEL="+inactiveLabel, "-o", "device")
	if err != nil {
		return rootSlots{}, fmt.Errorf("find inactive root slot: %w", err)
	}
	activeDisk, activePart, err := e.partitionIdentity(ctx, active)
	if err != nil {
		return rootSlots{}, err
	}
	inactiveDisk, inactivePart, err := e.partitionIdentity(ctx, inactive)
	if err != nil {
		return rootSlots{}, err
	}
	if activeDisk != inactiveDisk {
		return rootSlots{}, fmt.Errorf("root slots are on different disks %q and %q", activeDisk, inactiveDisk)
	}
	partUUID, err := e.toolOutput(ctx, "blkid", "-s", "PARTUUID", "-o", "value", inactive)
	if err != nil {
		return rootSlots{}, fmt.Errorf("read inactive root PARTUUID: %w", err)
	}
	if strings.ContainsAny(partUUID, " /\t\n") {
		return rootSlots{}, fmt.Errorf("read inactive root PARTUUID: invalid value %q", partUUID)
	}
	return rootSlots{active, activeDisk, activePart, inactive, inactiveDisk, inactivePart, partUUID}, nil
}

func (e *Executor) partitionIdentity(ctx context.Context, device string) (string, string, error) {
	value, err := e.toolOutput(ctx, "lsblk", "-no", "PKNAME,PARTN", device)
	if err != nil {
		return "", "", fmt.Errorf("inspect root partition %s: %w", device, err)
	}
	fields := strings.Fields(value)
	if len(fields) != 2 || fields[0] == "" || fields[1] == "" {
		return "", "", fmt.Errorf("inspect root partition %s: invalid lsblk output %q", device, value)
	}
	return filepath.Join("/dev", fields[0]), fields[1], nil
}

func (e *Executor) prepareSysupdateSlots(ctx context.Context, slots rootSlots, currentVersion string) error {
	installedLabel := "katl_" + strings.TrimSpace(currentVersion)
	for _, request := range []struct {
		disk, part, label string
	}{
		{slots.ActiveDisk, slots.ActivePart, installedLabel},
		{slots.InactiveDisk, slots.InactivePart, "_empty"},
	} {
		if _, err := e.toolOutput(ctx, "sfdisk", "--part-label", request.disk, request.part, request.label); err != nil {
			return fmt.Errorf("set root slot label %s: %w", request.label, err)
		}
		if _, err := e.toolOutput(ctx, "partx", "--update", "--nr", request.part, request.disk); err != nil {
			return fmt.Errorf("refresh root slot label %s: %w", request.label, err)
		}
	}
	return nil
}

func (e *Executor) toolOutput(ctx context.Context, name string, args ...string) (string, error) {
	result := e.toolRunner()(ctx, append([]string{name}, args...), nil)
	if result.Err != nil || result.ExitStatus != 0 {
		return "", fmt.Errorf("%s: %s", name, toolFailure(result))
	}
	value := strings.TrimSpace(string(result.Stdout))
	if value == "" && name != "sfdisk" && name != "partx" {
		return "", fmt.Errorf("%s returned empty output", name)
	}
	return value, nil
}

func (e *Executor) failHostUpgrade(record operation.OperationRecord, phase string, cause error) error {
	_, err := e.failRecord(record.OperationID, "host-upgrade-"+phase+"-failed", phase, "host upgrade failed before candidate activation", cause)
	return errors.Join(cause, err)
}

func inactiveRoot(current string) (string, string, string, error) {
	switch current {
	case string(disk.RootSlotA):
		return string(disk.RootSlotB), disk.GPTLabelRootB, disk.GPTLabelRootA, nil
	case string(disk.RootSlotB):
		return string(disk.RootSlotA), disk.GPTLabelRootA, disk.GPTLabelRootB, nil
	default:
		return "", "", "", fmt.Errorf("current generation root slot %q is unsupported", current)
	}
}

func copyUpgradeComponent(source, target string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func verifyUpgradePrefix(path, want string, size int64) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.CopyN(hash, file, size); err != nil {
		return err
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != want {
		return fmt.Errorf("SHA-256 %s does not match %s", got, want)
	}
	return nil
}

func verifyUpgradeFile(path, want string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	return verifyUpgradePrefix(path, want, info.Size())
}

func rootTransferDefinition(source string) string {
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

func ukiTransferDefinition(source string) string {
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
MatchPattern=katl_@v.efi
Mode=0644
InstancesMax=2
`, source)
}
