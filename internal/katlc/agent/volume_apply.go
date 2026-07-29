package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/katl-dev/katl/internal/installer/discovery"
	"github.com/katl-dev/katl/internal/installer/disk"
	"github.com/katl-dev/katl/internal/installer/generation"
	"github.com/katl-dev/katl/internal/installer/manifest"
)

func (e *Executor) applyVolumes(ctx context.Context, current, desired manifest.Manifest) error {
	run := e.RunTool
	if run == nil {
		run = runChildProcess
	}

	currentByName := volumesByName(current.Install.Volumes)
	desiredByName := volumesByName(desired.Install.Volumes)
	var stopNames []string
	for name, before := range currentByName {
		after, remains := desiredByName[name]
		if remains && reflect.DeepEqual(before, after) {
			continue
		}
		stopNames = append(stopNames, name)
	}
	sort.Strings(stopNames)

	var changed []manifest.Volume
	for name, after := range desiredByName {
		if before, exists := currentByName[name]; exists && reflect.DeepEqual(before, after) {
			continue
		}
		changed = append(changed, after)
	}
	sort.Slice(changed, func(i, j int) bool { return changed[i].Name < changed[j].Name })

	if len(changed) > 0 {
		facts, err := discoverVolumeFacts(ctx, run)
		if err != nil {
			return err
		}
		preflight := factsWithoutManagedVolumeMounts(facts, stopNames)
		if _, err := planLiveVolumes(preflight, desired, changed); err != nil {
			return err
		}
	}

	for _, name := range stopNames {
		unit, err := generation.MountUnitName("/var/mnt/" + name)
		if err != nil {
			return err
		}
		result := run(ctx, []string{"systemctl", "stop", unit}, nil)
		if result.Err != nil || result.ExitStatus != 0 && result.ExitStatus != 5 {
			err := fmt.Errorf("stop volume %s: %s", name, toolFailure(result))
			return fmt.Errorf("%w; stop workloads using /var/mnt/%s and retry", err, name)
		}
	}

	if len(changed) == 0 {
		return nil
	}

	facts, err := discoverVolumeFacts(ctx, run)
	if err != nil {
		return err
	}
	plans, err := planLiveVolumes(facts, desired, changed)
	if err != nil {
		return err
	}
	for _, plan := range plans {
		if err := prepareLiveVolume(ctx, run, e.Root, plan); err != nil {
			return err
		}
	}
	return nil
}

func discoverVolumeFacts(ctx context.Context, run ToolRunner) (discovery.HardwareFacts, error) {
	facts, err := (discovery.CommandDiscoverySource{Commands: volumeDiscoveryRunner{run: run}}).Discover(ctx)
	if err != nil {
		return discovery.HardwareFacts{}, fmt.Errorf("rediscover volume targets: %w", err)
	}
	return facts, nil
}

func planLiveVolumes(facts discovery.HardwareFacts, desired manifest.Manifest, changed []manifest.Volume) ([]disk.VolumePlan, error) {
	rootDisk, err := discovery.MatchDiskIdentity(facts, discovery.TargetDiskSelector{
		ByID:       desired.Install.TargetDisk.ByID,
		WWN:        desired.Install.TargetDisk.WWN,
		Serial:     desired.Install.TargetDisk.Serial,
		MinSizeMiB: desired.Install.TargetDisk.MinSizeMiB,
	})
	if err != nil {
		return nil, fmt.Errorf("resolve installed root disk: %w", err)
	}
	plans, err := disk.PlanVolumes(facts, rootDisk, manifest.BuildVolumeRequests(changed))
	if err != nil {
		return nil, err
	}
	return plans, nil
}

func factsWithoutManagedVolumeMounts(facts discovery.HardwareFacts, names []string) discovery.HardwareFacts {
	targets := make(map[string]struct{}, len(names))
	for _, name := range names {
		targets["/var/mnt/"+name] = struct{}{}
	}
	out := facts
	out.Mounts = out.Mounts[:0:0]
	for _, mount := range facts.Mounts {
		if _, managed := targets[mount.Target]; !managed {
			out.Mounts = append(out.Mounts, mount)
		}
	}
	out.BlockDevices = cloneDevicesWithoutMounts(facts.BlockDevices, targets)
	return out
}

func cloneDevicesWithoutMounts(devices []discovery.BlockDevice, targets map[string]struct{}) []discovery.BlockDevice {
	out := make([]discovery.BlockDevice, len(devices))
	for i, device := range devices {
		out[i] = device
		out[i].Mountpoints = nil
		for _, mountpoint := range device.Mountpoints {
			if _, managed := targets[strings.TrimSpace(mountpoint)]; !managed {
				out[i].Mountpoints = append(out[i].Mountpoints, mountpoint)
			}
		}
		out[i].Partitions = cloneDevicesWithoutMounts(device.Partitions, targets)
	}
	return out
}

func volumesByName(volumes []manifest.Volume) map[string]manifest.Volume {
	out := make(map[string]manifest.Volume, len(volumes))
	for _, volume := range volumes {
		out[volume.Name] = volume
	}
	return out
}

type volumeDiscoveryRunner struct {
	run ToolRunner
}

func (r volumeDiscoveryRunner) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	result := r.run(ctx, append([]string{name}, args...), nil)
	if result.Err != nil || result.ExitStatus != 0 {
		return nil, fmt.Errorf("%s: %s", name, toolFailure(result))
	}
	return result.Stdout, nil
}

func prepareLiveVolume(ctx context.Context, run ToolRunner, root string, plan disk.VolumePlan) error {
	mountPath := filepath.Join(filepath.Clean(root), plan.MountPath[1:])
	if err := os.MkdirAll(mountPath, 0o755); err != nil {
		return fmt.Errorf("create volume %q mount point: %w", plan.Name, err)
	}
	switch {
	case plan.Repartition:
		dir, err := os.MkdirTemp("", "katl-volume-repart-")
		if err != nil {
			return fmt.Errorf("create volume %q repart definition directory: %w", plan.Name, err)
		}
		defer os.RemoveAll(dir)
		if err := os.WriteFile(filepath.Join(dir, "50-katl-volume.conf"), []byte(disk.RepartDefinition(plan)), 0o600); err != nil {
			return fmt.Errorf("write volume %q repart definition: %w", plan.Name, err)
		}
		if err := runVolumeTool(ctx, run, "initialize volume "+plan.Name, "systemd-repart", "--dry-run=no", "--empty=force", "--definitions="+dir, plan.DevicePath); err != nil {
			return err
		}
		return runVolumeTool(ctx, run, "settle volume "+plan.Name, "udevadm", "settle")
	case plan.Wipe:
		if err := runVolumeTool(ctx, run, "wipe volume "+plan.Name, "wipefs", "--all", plan.DevicePath); err != nil {
			return err
		}
		return runVolumeTool(ctx, run, "format volume "+plan.Name, "mkfs."+plan.Filesystem, plan.DevicePath)
	default:
		return nil
	}
}

func runVolumeTool(ctx context.Context, run ToolRunner, action string, argv ...string) error {
	result := run(ctx, argv, nil)
	if result.Err != nil || result.ExitStatus != 0 {
		return fmt.Errorf("%s: %s", action, toolFailure(result))
	}
	return nil
}
