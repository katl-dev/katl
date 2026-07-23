package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/katl-dev/katl/internal/installer/disk"
	"github.com/katl-dev/katl/internal/installer/generation"
	"github.com/katl-dev/katl/internal/installer/katlosimage"
	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/installer/operation"
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
		e.cleanupManagedHostUpgradeArtifact(*record.HostUpgradeRequest)
		return e.failHostUpgrade(record, "verify-katlos-image", err)
	}
	defer e.cleanupManagedHostUpgradeArtifact(*record.HostUpgradeRequest)
	defer e.cleanupHostUpgradeMount(payload)
	if strings.TrimSpace(payload.ImageSHA256) == "" || payload.ImageSizeBytes == 0 {
		return e.failHostUpgrade(record, "verify-katlos-image", fmt.Errorf("resolved KatlOS image identity is incomplete"))
	}
	record, err = e.Store.Update(record.OperationID, "host-upgrade-image-resolved", "verify-katlos-image", func(current operation.OperationRecord) (operation.OperationRecord, error) {
		current.HostUpgradeRequest.ImageSHA256 = payload.ImageSHA256
		current.HostUpgradeRequest.ImageSizeBytes = payload.ImageSizeBytes
		current.Phase = "verify-katlos-image"
		current.CompletedPhases = appendMissing(current.CompletedPhases, "verify-katlos-image")
		current.UpdatedAt = e.clock()
		current.NextAction = "stage the internally identified root and UKI components through systemd-sysupdate"
		return current, nil
	})
	if err != nil {
		return err
	}
	currentID, err := currentGenerationID(e.Root)
	if err != nil {
		return e.failHostUpgrade(record, "verify-katlos-image", err)
	}
	previousSpec, previousStatus, err := generation.ReadGeneration(e.Root, currentID)
	if err != nil {
		return e.failHostUpgrade(record, "verify-katlos-image", fmt.Errorf("read current generation: %w", err))
	}
	inactiveSlot, err := inactiveRoot(previousSpec.Root.Slot)
	if err != nil {
		return e.failHostUpgrade(record, "verify-katlos-image", err)
	}
	slots, err := e.inspectRootSlots(ctx, previousSpec.Root.PartitionUUID)
	if err != nil {
		return e.failHostUpgrade(record, "verify-katlos-image", err)
	}
	kubernetesState, err := inspectKubernetesNodeState(e.Root, e.Store)
	if err != nil {
		return e.failHostUpgrade(record, "verify-katlos-image", fmt.Errorf("inspect Kubernetes node state: %w", err))
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
		Bootstrapped:      kubernetesState.bootstrapped,
		CreatedAt:         e.clock(),
	})
	if err != nil {
		return e.failHostUpgrade(record, "verify-katlos-image", err)
	}
	bootRoot := filepath.Join(runtimeRoot(e.Root), "efi")
	if e.MountBootRoot != nil {
		if err := e.MountBootRoot(ctx, bootRoot); err != nil {
			return e.failHostUpgrade(record, "verify-katlos-image", fmt.Errorf("mount boot root: %w", err))
		}
	}
	record, err = e.Store.Update(record.OperationID, "host-upgrade-mutation-start", "stage-sysupdate-components", func(current operation.OperationRecord) (operation.OperationRecord, error) {
		current.Phase = "stage-sysupdate-components"
		current.ExternalMutationStarted = true
		current.MutationScopes = appendMissing(current.MutationScopes, "root-slot-labels", "runtime-root", "runtime-uki")
		current.UpdatedAt = e.clock()
		current.NextAction = "stage root and UKI components through systemd-sysupdate"
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

func (e *Executor) cleanupManagedHostUpgradeArtifact(request operation.HostUpgrade) {
	localRef := filepath.ToSlash(filepath.Clean(strings.TrimSpace(request.ImageLocalRef)))
	if path.Dir(localRef) != hostUpgradeUploadDirectory {
		return
	}
	name := path.Base(localRef)
	digest := strings.TrimSuffix(name, ".squashfs")
	if name != digest+".squashfs" || validateArtifactSHA256(digest) != nil {
		return
	}
	artifactPath := filepath.Join(runtimeRoot(e.Root), "var/lib/katl/artifacts", filepath.FromSlash(localRef))
	_ = os.Remove(artifactPath)
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
	argv := []string{"/usr/lib/systemd/systemd-sysupdate", "--no-pager", "--verify=no", "--definitions=" + definitions}
	if root != "/" {
		argv = append(argv, "--root="+root)
	}
	argv = append(argv, "update", payload.Index.Version)
	result := e.toolRunner()(ctx, argv, nil)
	if result.Err != nil || result.ExitStatus != 0 {
		return fmt.Errorf("systemd-sysupdate: %s", toolFailure(result))
	}
	if err := flushUpgradeDevice(ctx, e.toolRunner(), inactiveDevice); err != nil {
		return fmt.Errorf("flush staged runtime root: %w", err)
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

func (e *Executor) inspectRootSlots(ctx context.Context, activePartUUID string) (rootSlots, error) {
	active, err := e.toolOutput(ctx, "blkid", "-t", "PARTUUID="+activePartUUID, "-o", "device")
	if err != nil {
		return rootSlots{}, fmt.Errorf("find active root slot: %w", err)
	}
	activeDisk, activePart, err := e.partitionIdentity(ctx, active)
	if err != nil {
		return rootSlots{}, err
	}
	activeType, err := e.toolOutput(ctx, "lsblk", "-no", "PARTTYPE", active)
	if err != nil {
		return rootSlots{}, fmt.Errorf("inspect active root partition type: %w", err)
	}
	partitionTable, err := e.toolOutput(ctx, "lsblk", "-rno", "PATH,PARTTYPE", activeDisk)
	if err != nil {
		return rootSlots{}, fmt.Errorf("inspect root partitions on %s: %w", activeDisk, err)
	}
	rootDevices := make([]string, 0, 2)
	for _, line := range strings.Split(partitionTable, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.EqualFold(fields[1], activeType) {
			rootDevices = append(rootDevices, fields[0])
		}
	}
	if len(rootDevices) != 2 {
		return rootSlots{}, fmt.Errorf("find inactive root slot: found %d root partitions with type %q on %s, want 2", len(rootDevices), activeType, activeDisk)
	}
	inactive := ""
	activeFound := false
	for _, device := range rootDevices {
		if device == active {
			activeFound = true
			continue
		}
		inactive = device
	}
	if !activeFound || inactive == "" {
		return rootSlots{}, fmt.Errorf("find inactive root slot: active device %s is not one of %v", active, rootDevices)
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

func inactiveRoot(current string) (string, error) {
	switch current {
	case string(disk.RootSlotA):
		return string(disk.RootSlotB), nil
	case string(disk.RootSlotB):
		return string(disk.RootSlotA), nil
	default:
		return "", fmt.Errorf("current generation root slot %q is unsupported", current)
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

func flushUpgradeDevice(ctx context.Context, run ToolRunner, device string) error {
	result := run(ctx, []string{"blockdev", "--flushbufs", device}, nil)
	if result.Err != nil || result.ExitStatus != 0 {
		return fmt.Errorf("blockdev: %s", toolFailure(result))
	}
	return nil
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
Path=/efi/EFI/Linux
MatchPattern=katl_@v.efi
Mode=0644
InstancesMax=2
`, source)
}
