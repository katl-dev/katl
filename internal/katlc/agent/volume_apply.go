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

type volumeTransitionPlan struct {
	stopNames                    []string
	prepare                      []disk.VolumePlan
	bindings                     []generation.VolumeBinding
	requiredWipeAcknowledgements []string
	requiredRebinds              []string
}

func (p volumeTransitionPlan) validateApplyMode(mode string) error {
	if mode == generation.ApplyModeLive || len(p.stopNames) == 0 && len(p.prepare) == 0 {
		return nil
	}
	return fmt.Errorf("volume changes that stop or prepare devices require live apply and cannot be combined with next-boot-only changes; apply the volume change separately")
}

type VolumeRebindAuthorityError struct {
	Required    []string
	Transitions []string
}

func (e *VolumeRebindAuthorityError) Error() string {
	flags := make([]string, 0, len(e.Required))
	for _, required := range e.Required {
		flags = append(flags, "--rebind-volume "+required)
	}
	message := "volume replacement requires explicit rebind authority: " + strings.Join(flags, ", ")
	if len(e.Transitions) > 0 {
		message += " (" + strings.Join(e.Transitions, "; ") + ")"
	}
	return message
}

func (e *Executor) applyVolumes(ctx context.Context, current, desired manifest.Manifest, currentBindings []generation.VolumeBinding, nodeName string, acknowledgements, rebinds []string) ([]generation.VolumeBinding, error) {
	run := e.RunTool
	if run == nil {
		run = runChildProcess
	}
	plan, err := planVolumeTransition(ctx, run, current, desired, currentBindings, nodeName, acknowledgements, rebinds)
	if err != nil {
		return nil, err
	}

	for _, name := range plan.stopNames {
		unit, err := generation.MountUnitName("/var/mnt/" + name)
		if err != nil {
			return nil, err
		}
		result := run(ctx, []string{"systemctl", "stop", unit}, nil)
		if result.Err != nil || result.ExitStatus != 0 && result.ExitStatus != 5 {
			err := fmt.Errorf("stop volume %s: %s", name, toolFailure(result))
			return nil, fmt.Errorf("%w; stop workloads using /var/mnt/%s and retry", err, name)
		}
	}
	if len(plan.prepare) == 0 {
		return plan.bindings, nil
	}

	for _, volume := range plan.prepare {
		if err := prepareLiveVolume(ctx, run, e.Root, volume); err != nil {
			return nil, err
		}
	}
	facts, err := discoverVolumeFacts(ctx, run)
	if err != nil {
		return nil, err
	}
	_, diskBindings, err := disk.BindVolumePlans(facts, plan.prepare)
	if err != nil {
		return nil, fmt.Errorf("bind prepared volumes: %w", err)
	}
	byName := volumeBindingsByName(plan.bindings)
	for _, binding := range diskBindings {
		byName[binding.Name] = generation.VolumeBinding{
			Name: binding.Name, PartitionUUID: binding.PartitionUUID, FilesystemUUID: binding.FilesystemUUID,
		}
	}
	return sortedVolumeBindings(byName), nil
}

func changedVolumes(current, desired manifest.Manifest) ([]string, []manifest.Volume) {
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
	return stopNames, changed
}

func planVolumeTransition(ctx context.Context, run ToolRunner, current, desired manifest.Manifest, currentBindings []generation.VolumeBinding, nodeName string, acknowledgements, rebinds []string) (volumeTransitionPlan, error) {
	if err := disk.ValidateDestructiveVolumeAcknowledgementKeys(acknowledgements); err != nil {
		return volumeTransitionPlan{}, err
	}
	if err := disk.ValidateVolumeRebindKeys(rebinds); err != nil {
		return volumeTransitionPlan{}, err
	}
	stopNames, _ := changedVolumes(current, desired)
	if len(current.Install.Volumes) == 0 && len(desired.Install.Volumes) == 0 {
		return volumeTransitionPlan{bindings: sortedVolumeBindings(volumeBindingsByName(currentBindings))}, nil
	}
	if len(desired.Install.Volumes) == 0 {
		return volumeTransitionPlan{stopNames: stopNames, bindings: sortedVolumeBindings(volumeBindingsByName(currentBindings))}, nil
	}
	facts, err := discoverVolumeFacts(ctx, run)
	if err != nil {
		return volumeTransitionPlan{}, err
	}
	currentNames := make([]string, 0, len(current.Install.Volumes))
	for _, volume := range current.Install.Volumes {
		currentNames = append(currentNames, volume.Name)
	}
	facts = factsWithoutManagedVolumeMounts(facts, currentNames)

	currentVolumes := volumesByName(current.Install.Volumes)
	currentByName := volumeBindingsByName(currentBindings)
	// Retain bindings for removed volumes as generation-owned tombstones. They
	// render no mount, but prevent remove/re-add from bypassing replacement
	// authority for the same logical name.
	desiredBindings := volumeBindingsByName(currentBindings)
	var prepare []disk.VolumePlan
	var requiredRebinds []string
	var rebindTransitions []string
	nodeName = storageAuthorityNodeName(nodeName, current)

	for _, desiredVolume := range desired.Install.Volumes {
		before, existed := currentVolumes[desiredVolume.Name]
		configChanged := !existed || !reflect.DeepEqual(before, desiredVolume)
		binding, wasBound := currentByName[desiredVolume.Name]
		if wasBound && !configChanged {
			if _, matchErr := disk.MatchVolumeBinding(facts, diskVolumeBinding(binding)); matchErr == nil {
				desiredBindings[binding.Name] = binding
				continue
			}
		}

		resolve := desiredVolume
		if !configChanged {
			// Wipe is desired provisioning state, not permission to repeat a
			// destructive action merely to migrate a legacy generation binding.
			resolve.Wipe = false
		}
		plans, planErr := planLiveVolumes(facts, desired, []manifest.Volume{resolve})
		if planErr != nil {
			return volumeTransitionPlan{}, planErr
		}
		candidate := plans[0]
		var candidateBinding generation.VolumeBinding
		if !candidate.Repartition {
			_, bound, bindErr := disk.BindVolumePlans(facts, plans)
			if bindErr != nil {
				return volumeTransitionPlan{}, bindErr
			}
			candidateBinding = generationVolumeBinding(bound[0])
		}
		if wasBound && (candidate.Repartition || !sameVolumeDevice(binding, candidateBinding)) {
			key := nodeName + "/" + desiredVolume.Name
			requiredRebinds = append(requiredRebinds, key)
			rebindTransitions = append(rebindTransitions, fmt.Sprintf("%s: %s -> %s", key, volumeBindingDescription(binding), candidateVolumeDescription(candidate, candidateBinding)))
		}
		if configChanged {
			prepare = append(prepare, candidate)
		} else {
			desiredBindings[desiredVolume.Name] = candidateBinding
		}
	}

	requiredWipes := disk.RequiredDestructiveVolumeAcknowledgements(nodeName, prepare)
	result := volumeTransitionPlan{
		stopNames: stopNames, prepare: prepare, bindings: sortedVolumeBindings(desiredBindings),
		requiredWipeAcknowledgements: requiredWipes, requiredRebinds: uniqueSorted(requiredRebinds),
	}
	var authorityErrors []error
	if err := disk.ValidateDestructiveVolumeAcknowledgements(nodeName, prepare, acknowledgements); err != nil {
		authorityErrors = append(authorityErrors, err)
	}
	if missing := missingAuthorities(result.requiredRebinds, rebinds); len(missing) > 0 {
		authorityErrors = append(authorityErrors, &VolumeRebindAuthorityError{Required: missing, Transitions: rebindTransitions})
	}
	if len(authorityErrors) > 0 {
		return result, errorsJoin(authorityErrors...)
	}
	return result, nil
}

func (s *Server) validateVolumeTransition(ctx context.Context, nodeName string, current, desired manifest.Manifest, currentBindings []generation.VolumeBinding, acknowledgements, rebinds []string) (volumeTransitionPlan, error) {
	run := s.RunVolumeDiscovery
	if run == nil {
		run = runChildProcess
	}
	return planVolumeTransition(ctx, run, current, desired, currentBindings, nodeName, acknowledgements, rebinds)
}

func diskVolumeBinding(binding generation.VolumeBinding) disk.VolumeBinding {
	return disk.VolumeBinding{Name: binding.Name, PartitionUUID: binding.PartitionUUID, FilesystemUUID: binding.FilesystemUUID}
}

func generationVolumeBinding(binding disk.VolumeBinding) generation.VolumeBinding {
	return generation.VolumeBinding{Name: binding.Name, PartitionUUID: binding.PartitionUUID, FilesystemUUID: binding.FilesystemUUID}
}

func volumeBindingsByName(bindings []generation.VolumeBinding) map[string]generation.VolumeBinding {
	out := make(map[string]generation.VolumeBinding, len(bindings))
	for _, binding := range bindings {
		out[binding.Name] = binding
	}
	return out
}

func sortedVolumeBindings(bindings map[string]generation.VolumeBinding) []generation.VolumeBinding {
	out := make([]generation.VolumeBinding, 0, len(bindings))
	for _, binding := range bindings {
		out = append(out, binding)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func sameVolumeDevice(left, right generation.VolumeBinding) bool {
	if strings.TrimSpace(left.PartitionUUID) != "" && strings.TrimSpace(right.PartitionUUID) != "" {
		return strings.TrimSpace(left.PartitionUUID) == strings.TrimSpace(right.PartitionUUID)
	}
	return strings.TrimSpace(left.FilesystemUUID) != "" && strings.TrimSpace(left.FilesystemUUID) == strings.TrimSpace(right.FilesystemUUID)
}

func volumeBindingDescription(binding generation.VolumeBinding) string {
	source, err := disk.VolumeBindingMountSource(diskVolumeBinding(binding))
	if err != nil {
		return "unbound"
	}
	return source
}

func candidateVolumeDescription(plan disk.VolumePlan, binding generation.VolumeBinding) string {
	if source := volumeBindingDescription(binding); source != "unbound" {
		return source
	}
	return plan.DevicePath + " (identity assigned during provisioning)"
}

func missingAuthorities(required, provided []string) []string {
	have := make(map[string]struct{}, len(provided))
	for _, value := range provided {
		have[strings.TrimSpace(value)] = struct{}{}
	}
	var missing []string
	for _, value := range required {
		if _, ok := have[value]; !ok {
			missing = append(missing, value)
		}
	}
	return missing
}

func uniqueSorted(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		seen[value] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for value := range seen {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func storageAuthorityNodeName(requested string, current manifest.Manifest) string {
	if requested = strings.TrimSpace(requested); requested != "" {
		return requested
	}
	if current.Node.Bootstrap != nil {
		if name := strings.TrimSpace(current.Node.Bootstrap.InventoryNodeName); name != "" {
			return name
		}
	}
	return strings.TrimSpace(current.Node.Identity.Hostname)
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
