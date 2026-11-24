package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/disk"
)

// volumeFault is one volume of a generation whose binding does not resolve
// by the binding rule.
type volumeFault struct {
	binding *disk.VolumeBindingError
	// rebindKey is the NODE/VOLUME authority a rebinding apply needs.
	rebindKey string
}

// generationVolumeFaults resolves the binding of every volume a generation
// mounts. Bindings retained for removed volumes render no mount and are not
// checked. Devices are discovered only when the generation mounts a volume,
// so a node without volumes does not depend on discovery.
func generationVolumeFaults(ctx context.Context, devices volumeDevices, run ToolRunner, root, generationID string) ([]volumeFault, error) {
	mounted, err := generationManifest(root, generationID)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(mounted.Install.Volumes) == 0 {
		return nil, nil
	}
	spec, _, err := generation.ReadGeneration(root, generationID)
	if err != nil {
		return nil, fmt.Errorf("read generation %s: %w", generationID, err)
	}
	facts, err := discoverVolumeFacts(ctx, devices.source(run))
	if err != nil {
		return nil, err
	}
	system, err := systemDiskDevice(facts, devices.systemDisk)
	if err != nil {
		return nil, err
	}
	bindings := volumeBindingsByName(spec.VolumeBindings)
	nodeName := storageAuthorityNodeName("", mounted)
	var faults []volumeFault
	for _, volume := range mounted.Install.Volumes {
		binding := diskVolumeBinding(bindings[volume.Name])
		binding.Name = volume.Name
		_, err := disk.MatchVolumeBinding(facts, system, binding)
		var bindingErr *disk.VolumeBindingError
		if errors.As(err, &bindingErr) {
			faults = append(faults, volumeFault{binding: bindingErr, rebindKey: nodeName + "/" + volume.Name})
		} else if err != nil {
			return nil, err
		}
	}
	return faults, nil
}

// remedy tells the operator how to bring the volume back.
func (f volumeFault) remedy() string {
	switch f.binding.Fault {
	case disk.BindingDuplicated:
		return "Another disk carries a copy of this partition, such as a dd clone, so Katl cannot tell which one holds the volume. Do not wipe either disk. Detach the copy, or give it new partition GUIDs with sgdisk --randomize-guids, then reboot the node."
	case disk.BindingStale:
		return fmt.Sprintf("The filesystem was recreated after the binding was recorded, for example by a later wipe or an interrupted format, so it may not hold the volume's data. If it is now the volume's storage, apply the node's configuration with --rebind-volume %s.", f.rebindKey)
	case disk.BindingOnSystemDisk:
		return fmt.Sprintf("That partition is on the disk the node boots from, so it is not volume storage. Check the disks attached to the node; if the volume was replaced, select the replacement in the node's configuration and apply it with --rebind-volume %s.", f.rebindKey)
	case disk.BindingUnrecorded:
		return fmt.Sprintf("Select the volume's partition in the node's configuration and apply it with --rebind-volume %s.", f.rebindKey)
	default:
		return fmt.Sprintf("Attach the disk that holds the volume, then reboot the node. If the volume was replaced, select the replacement in the node's configuration and apply it with --rebind-volume %s.", f.rebindKey)
	}
}

func (f volumeFault) String() string {
	return f.binding.Error() + ". " + f.remedy()
}

// Data disks can attach after the activation unit starts, as USB and some
// HBAs do. While a recorded PARTUUID is merely absent the check rediscovers
// for up to volumeAttachWait, as the mount units' device wait used to.
var (
	volumeAttachWait     = time.Minute
	volumeAttachInterval = 2 * time.Second
)

// CheckActiveVolumes resolves, by the binding rule, every volume binding of
// the generation whose mount units systemd has loaded, which is the one the
// generated confext links. katl-volumes-activate.service runs it before it
// starts katl-volumes.target, which requires every volume's mount, so no
// volume is mounted while any binding does not resolve.
func CheckActiveVolumes(ctx context.Context, root string) error {
	// Disks probed during coldplug must be in the udev database before the
	// first discovery, or a clone that enumerated first would be the only
	// partition seen with the volume's PARTUUID. A settle that fails or times
	// out leaves the check to the devices udev has processed.
	_ = runChildProcess(ctx, []string{"udevadm", "settle", "--timeout=30"}, nil)
	return checkActiveVolumes(ctx, volumeDevices{}, runChildProcess, root)
}

func checkActiveVolumes(ctx context.Context, devices volumeDevices, run ToolRunner, root string) error {
	generationID, err := activeConfextGeneration(root)
	if err != nil || generationID == "" {
		return err
	}

	deadline := time.Now().Add(volumeAttachWait)
	var faults []volumeFault
	for {
		faults, err = generationVolumeFaults(ctx, devices, run, root, generationID)
		if err != nil || len(faults) == 0 {
			return err
		}
		waiting := !slices.ContainsFunc(faults, func(fault volumeFault) bool { return fault.binding.Fault != disk.BindingAbsent })
		if !waiting || !time.Now().Before(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(volumeAttachInterval):
		}
	}
	lines := make([]string, 0, len(faults)+1)
	lines = append(lines, fmt.Sprintf("Generation %s mounts no volumes while a recorded binding does not match the attached disks:", generationID))
	for _, fault := range faults {
		lines = append(lines, fault.String())
	}
	return errors.New(strings.Join(lines, "\n"))
}

// activeConfextGeneration returns the generation the generated confext links,
// or "" when none is active and so no volume mount units exist.
func activeConfextGeneration(root string) (string, error) {
	for _, name := range []string{generation.GeneratedConfextName, "katl-node"} {
		link := filepath.Join(filepath.Clean(root), strings.TrimPrefix(generation.DefaultConfextsActivationDir, "/"), name)
		target, err := os.Readlink(link)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("read the active generation: %w", err)
		}
		if filepath.Dir(filepath.Dir(target)) != generation.GenerationRecordsDir {
			return "", fmt.Errorf("generated confext %s links %s, not a generation record", link, target)
		}
		return filepath.Base(filepath.Dir(target)), nil
	}
	return "", nil
}

// refuseUnresolvedVolumes refuses to select a generation whose volumes no
// longer resolve, since booting it would leave them unmounted.
func refuseUnresolvedVolumes(ctx context.Context, devices volumeDevices, run ToolRunner, root, generationID string) error {
	faults, err := generationVolumeFaults(ctx, devices, run, root, generationID)
	if err != nil || len(faults) == 0 {
		return err
	}
	causes := make([]string, 0, len(faults))
	for _, fault := range faults {
		causes = append(causes, fault.binding.Error()+". "+fault.selectionRemedy(generationID))
	}
	return fmt.Errorf("generation %s would boot with its volumes unmounted: %s", generationID, strings.Join(causes, " "))
}

// selectionRemedy tells the operator how to make a generation selectable.
func (f volumeFault) selectionRemedy(generationID string) string {
	switch f.binding.Fault {
	case disk.BindingDuplicated:
		return f.remedy() + " Then select the generation again."
	case disk.BindingAbsent:
		return "Attach the disk that holds the volume, then select the generation again. If the disk was replaced, generation " + generationID + " cannot mount it; select a generation recorded after the replacement."
	case disk.BindingStale:
		return "The volume was replaced or reformatted after generation " + generationID + " was recorded, so that generation cannot mount it; select a generation recorded after that change."
	default:
		return "Generation " + generationID + " cannot mount this volume as recorded; select a generation whose volumes match the attached disks."
	}
}

// volumeFaultDiagnostics explains, for each volume of a generation, why it
// is not mounted: its own binding does not resolve, or another volume's does
// and activation holds every volume back. It returns nil when every binding
// resolves or the check cannot run.
func volumeFaultDiagnostics(ctx context.Context, devices volumeDevices, run ToolRunner, root, generationID string, volumes []string) map[string]string {
	faults, err := generationVolumeFaults(ctx, devices, run, root, generationID)
	if err != nil || len(faults) == 0 {
		return nil
	}
	out := make(map[string]string, len(volumes))
	var faulty []string
	for _, fault := range faults {
		out[fault.binding.Volume] = fault.String()
		faulty = append(faulty, fault.binding.Volume)
	}
	for _, name := range volumes {
		if _, ok := out[name]; !ok {
			out[name] = fmt.Sprintf("Held back: Katl mounts no volumes while the binding of %s does not match the attached disks.", strings.Join(faulty, ", "))
		}
	}
	return out
}
