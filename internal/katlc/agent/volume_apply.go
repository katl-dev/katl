package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/discovery"
	"github.com/katl-dev/katl/internal/installer/disk"
	"github.com/katl-dev/katl/internal/installer/manifest"
)

type volumeTransitionPlan struct {
	stopNames       []string
	prepare         []disk.VolumePlan
	bindings        []generation.VolumeBinding
	requiredRebinds []string
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
	source := e.volumeDevices.source(run)
	plan, err := planVolumeTransition(ctx, source, current, desired, currentBindings, nodeName, acknowledgements, rebinds)
	if err != nil {
		return nil, err
	}

	recovery := volumeRecovery{run: run, source: source, bindings: volumeBindingsByName(currentBindings), prepared: plan.prepare}
	for _, name := range plan.stopNames {
		unit, err := generation.MountUnitName("/var/mnt/" + name)
		if err != nil {
			return nil, recovery.fail(ctx, err)
		}
		result := run(ctx, []string{"systemctl", "stop", unit}, nil)
		// Exit status 5 means the unit is not loaded, so nothing was mounted
		// and there is nothing to start again.
		if result.Err == nil && result.ExitStatus == 5 {
			continue
		}
		if result.Err != nil || result.ExitStatus != 0 {
			err := fmt.Errorf("stop volume %s: %s; stop workloads using /var/mnt/%s and retry", name, toolFailure(result), name)
			return nil, recovery.fail(ctx, err)
		}
		recovery.stopped = append(recovery.stopped, name)
	}
	if len(plan.prepare) == 0 {
		return plan.bindings, nil
	}

	recovery.rewritten, err = prepareLiveVolumes(ctx, run, source, e.Root, plan.prepare)
	if err != nil {
		return nil, recovery.fail(ctx, fmt.Errorf("%w; check the disks attached to the node, then retry the apply", err))
	}
	// udev settles so the partitions just created or formatted are visible
	// before they are bound.
	if err := runVolumeTool(ctx, run, "settle udev", "udevadm", "settle"); err != nil {
		return nil, recovery.fail(ctx, err)
	}
	facts, err := discoverVolumeFacts(ctx, source)
	if err != nil {
		return nil, recovery.fail(ctx, err)
	}
	_, diskBindings, err := disk.BindVolumePlans(facts, plan.prepare)
	if err != nil {
		return nil, recovery.fail(ctx, fmt.Errorf("bind prepared volumes: %w; check the disks attached to the node, then retry the apply", err))
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

func planVolumeTransition(ctx context.Context, source discovery.DiscoverySource, current, desired manifest.Manifest, currentBindings []generation.VolumeBinding, nodeName string, acknowledgements, rebinds []string) (volumeTransitionPlan, error) {
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
	facts, err := discoverVolumeFacts(ctx, source)
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

	// Unchanged volumes still found by their bindings are kept as they are;
	// every other desired volume is resolved again. All of them are planned
	// together, so no volume can claim storage another one holds.
	var kept []disk.VolumeBinding
	var resolve []manifest.Volume
	var changed []bool
	for _, desiredVolume := range desired.Install.Volumes {
		before, existed := currentVolumes[desiredVolume.Name]
		configChanged := !existed || !reflect.DeepEqual(before, desiredVolume)
		binding, wasBound := currentByName[desiredVolume.Name]
		if wasBound && !configChanged {
			if _, matchErr := disk.MatchVolumeBinding(facts, diskVolumeBinding(binding)); matchErr == nil {
				desiredBindings[binding.Name] = binding
				kept = append(kept, diskVolumeBinding(binding))
				continue
			}
		}
		if !configChanged {
			// Wipe is desired provisioning state, not permission to repeat a
			// destructive action merely to migrate a legacy generation binding.
			desiredVolume.Wipe = false
		}
		resolve = append(resolve, desiredVolume)
		changed = append(changed, configChanged)
	}
	rootDisk, err := runningSystemDisk(facts)
	if err != nil {
		return volumeTransitionPlan{}, err
	}
	// Every volume the apply stops is changed or removed, and its current
	// storage is held until the apply succeeds.
	var released []disk.VolumeBinding
	for _, name := range stopNames {
		if binding, ok := currentByName[name]; ok {
			released = append(released, diskVolumeBinding(binding))
		}
	}
	plans, err := disk.PlanVolumeChanges(facts, rootDisk, manifest.BuildVolumeRequests(resolve), kept, released)
	if err != nil {
		return volumeTransitionPlan{}, err
	}

	for i, candidate := range plans {
		binding, wasBound := currentByName[candidate.Name]
		var candidateBinding generation.VolumeBinding
		if !candidate.Repartition {
			_, bound, bindErr := disk.BindVolumePlans(facts, []disk.VolumePlan{candidate})
			if bindErr != nil {
				return volumeTransitionPlan{}, bindErr
			}
			candidateBinding = generationVolumeBinding(bound[0])
		}
		if wasBound && (candidate.Repartition || !sameVolumeDevice(binding, candidateBinding)) {
			key := nodeName + "/" + candidate.Name
			requiredRebinds = append(requiredRebinds, key)
			rebindTransitions = append(rebindTransitions, fmt.Sprintf("%s: %s -> %s", key, volumeBindingDescription(binding), candidateVolumeDescription(candidate, candidateBinding)))
		}
		if changed[i] {
			prepare = append(prepare, candidate)
		} else {
			desiredBindings[candidate.Name] = candidateBinding
		}
	}

	result := volumeTransitionPlan{
		stopNames: stopNames, prepare: prepare, bindings: sortedVolumeBindings(desiredBindings),
		requiredRebinds: uniqueSorted(requiredRebinds),
	}
	if missing := missingAuthorities(result.requiredRebinds, rebinds); len(missing) > 0 {
		return result, &VolumeRebindAuthorityError{Required: missing, Transitions: rebindTransitions}
	}
	return result, nil
}

func (s *Server) validateVolumeTransition(ctx context.Context, nodeName string, current, desired manifest.Manifest, currentBindings []generation.VolumeBinding, acknowledgements, rebinds []string) (volumeTransitionPlan, error) {
	run := s.RunVolumeDiscovery
	if run == nil {
		run = runChildProcess
	}
	return planVolumeTransition(ctx, s.volumeDevices.source(run), current, desired, currentBindings, nodeName, acknowledgements, rebinds)
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
		return strings.EqualFold(strings.TrimSpace(left.PartitionUUID), strings.TrimSpace(right.PartitionUUID))
	}
	return strings.TrimSpace(left.FilesystemUUID) != "" && strings.EqualFold(strings.TrimSpace(left.FilesystemUUID), strings.TrimSpace(right.FilesystemUUID))
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
	return plan.Disk.Path + " (identity assigned during provisioning)"
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

func discoverVolumeFacts(ctx context.Context, source discovery.DiscoverySource) (discovery.HardwareFacts, error) {
	facts, err := source.Discover(ctx)
	if err != nil {
		return discovery.HardwareFacts{}, fmt.Errorf("rediscover volume targets: %w", err)
	}
	return facts, nil
}

// runningSystemDisk finds the disk whose state partition is mounted at /var.
// An installed node is identified by its own partitions rather than the
// install selector, whose GPT GUID or kernel name need not match after install.
func runningSystemDisk(facts discovery.HardwareFacts) (discovery.BlockDevice, error) {
	sources := map[string]bool{}
	for _, mount := range facts.Mounts {
		if mount.Target == "/var" {
			sources[mount.Source] = true
		}
	}
	var disks []discovery.BlockDevice
	for _, device := range facts.BlockDevices {
		if slices.ContainsFunc(device.Partitions, func(partition discovery.BlockDevice) bool {
			return sources[partition.Path] || slices.Contains(partition.Mountpoints, "/var")
		}) {
			disks = append(disks, device)
		}
	}
	if len(disks) != 1 {
		return discovery.BlockDevice{}, fmt.Errorf("identify the running system disk: %d disks hold a partition mounted at /var, want 1", len(disks))
	}
	return disks[0], nil
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

// prepareLiveVolumes creates the volumes' mount points, then provisions them
// through disk.PrepareVolumes and returns the volumes it rewrote.
func prepareLiveVolumes(ctx context.Context, run ToolRunner, source discovery.DiscoverySource, root string, plans []disk.VolumePlan) ([]string, error) {
	for _, plan := range plans {
		mountPath := filepath.Join(filepath.Clean(root), plan.MountPath[1:])
		if err := os.MkdirAll(mountPath, 0o755); err != nil {
			return nil, fmt.Errorf("create volume %q mount point: %w", plan.Name, err)
		}
	}
	return disk.PrepareVolumes(ctx, volumeDiscoveryRunner{run: run}, source, plans)
}

func (r volumeDiscoveryRunner) Run(ctx context.Context, name string, args ...string) error {
	return runVolumeTool(ctx, r.run, name, append([]string{name}, args...)...)
}

func runVolumeTool(ctx context.Context, run ToolRunner, action string, argv ...string) error {
	result := run(ctx, argv, nil)
	if result.Err != nil || result.ExitStatus != 0 {
		return fmt.Errorf("%s: %s", action, toolFailure(result))
	}
	return nil
}

// volumeDevices names the host directories volume discovery reads. The zero
// value reads the running host.
type volumeDevices struct {
	sysBlockDir  string
	byIDDir      string
	byPathDir    string
	byDiskSeqDir string
}

func (d volumeDevices) source(run ToolRunner) discovery.CommandDiscoverySource {
	return discovery.CommandDiscoverySource{
		Commands:     volumeDiscoveryRunner{run: run},
		SysBlockDir:  d.sysBlockDir,
		ByIDDir:      d.byIDDir,
		ByPathDir:    d.byPathDir,
		ByDiskSeqDir: d.byDiskSeqDir,
	}
}

// volumeRecovery records what a live volume apply has done to the node's
// storage so that, when it fails, it can restore what is still intact.
type volumeRecovery struct {
	run    ToolRunner
	source discovery.DiscoverySource
	// bindings are the current generation's volume bindings, which the
	// stopped volumes' mount units mount.
	bindings map[string]generation.VolumeBinding
	// prepared are the plans of the volumes the apply prepares.
	prepared []disk.VolumePlan
	// stopped are the volumes whose mount units the apply stopped.
	stopped []string
	// rewritten are the volumes a destructive command was started on.
	rewritten []string
}

// volumeStorage is a disk, by kernel attachment sequence, or one partition on
// it; partition 0 is the whole disk.
type volumeStorage struct {
	diskSeq   uint64
	partition uint64
}

// touched lists the storage the rewritten volumes' commands write.
func (r volumeRecovery) touched() map[volumeStorage]string {
	out := map[volumeStorage]string{}
	for _, plan := range r.prepared {
		if !slices.Contains(r.rewritten, plan.Name) {
			continue
		}
		storage := volumeStorage{diskSeq: plan.Disk.DiskSeq}
		if !plan.Repartition {
			storage.partition = plan.Partition.Number
		}
		out[storage] = plan.Name
	}
	return out
}

// rewrittenBy names the rewritten volume whose commands wrote the storage the
// named volume's current binding resolves to, or returns "" when its storage
// was not written. A binding that cannot be matched to one partition is an
// error: its storage cannot be shown to be intact.
func (r volumeRecovery) rewrittenBy(facts discovery.HardwareFacts, touched map[volumeStorage]string, name string) (string, error) {
	binding, ok := r.bindings[name]
	if !ok {
		return "", nil
	}
	partition, err := disk.MatchVolumeBinding(facts, diskVolumeBinding(binding))
	if err != nil {
		return "", err
	}
	for _, device := range facts.BlockDevices {
		if !slices.ContainsFunc(device.Partitions, func(candidate discovery.BlockDevice) bool { return candidate.Path == partition.Path }) {
			continue
		}
		for _, storage := range []volumeStorage{{diskSeq: device.DiskSeq}, {diskSeq: device.DiskSeq, partition: partition.PartitionNumber}} {
			if owner, ok := touched[storage]; ok {
				return owner, nil
			}
		}
	}
	return "", nil
}

// fail starts the mount units of the stopped volumes whose storage was not
// rewritten, so a refused change leaves their workloads' storage mounted. A
// volume stays unmounted when its own storage, or the storage its current
// binding resolves to, was rewritten, since its unit could mount the new empty
// filesystem as if it held the old data. The error names each volume left
// unmounted and what to do next.
func (r volumeRecovery) fail(ctx context.Context, cause error) error {
	touched := r.touched()
	var facts discovery.HardwareFacts
	var checkErr error
	if len(touched) > 0 {
		// udev settles so partitions a failed command left behind are
		// visible before the stopped volumes are resolved.
		checkErr = runVolumeTool(ctx, r.run, "settle udev", "udevadm", "settle")
		if checkErr == nil {
			facts, checkErr = discoverVolumeFacts(ctx, r.source)
		}
	}

	var restarted, failed, overwritten, unchecked, writers []string
	causes := map[string]error{}
	for _, name := range r.stopped {
		if slices.Contains(r.rewritten, name) {
			continue
		}
		if len(touched) > 0 {
			if checkErr != nil {
				unchecked = append(unchecked, name)
				causes[name] = checkErr
				continue
			}
			owner, err := r.rewrittenBy(facts, touched, name)
			if err != nil {
				// A binding that resolves to no single partition leaves no
				// device for its unit to mount, or an unknown one.
				unchecked = append(unchecked, name)
				causes[name] = err
				continue
			}
			if owner != "" {
				overwritten = append(overwritten, name)
				if !slices.Contains(writers, owner) {
					writers = append(writers, owner)
				}
				continue
			}
		}
		unit, err := generation.MountUnitName("/var/mnt/" + name)
		if err == nil {
			err = runVolumeTool(ctx, r.run, "start "+unit, "systemctl", "start", unit)
		}
		if err != nil {
			failed = append(failed, name)
		} else {
			restarted = append(restarted, name)
		}
	}

	err := cause
	if len(restarted) > 0 {
		err = fmt.Errorf("%w; volumes %s were stopped for this apply and have been mounted again", err, strings.Join(restarted, ", "))
	}
	if len(failed) > 0 {
		units := make([]string, 0, len(failed))
		for _, name := range failed {
			unit, _ := generation.MountUnitName("/var/mnt/" + name)
			units = append(units, unit)
		}
		err = fmt.Errorf("%w; volumes %s were stopped for this apply and could not be mounted again; once their disks are attached, run systemctl start %s on the node", err, strings.Join(failed, ", "), strings.Join(units, " "))
	}
	if len(overwritten) > 0 {
		err = fmt.Errorf("%w; volumes %s were stopped for this apply and left unmounted, because the storage they are bound to was repartitioned or formatted for volumes %s, so the data they held may be gone; check that storage before mounting them again", err, strings.Join(overwritten, ", "), strings.Join(writers, ", "))
	}
	if len(unchecked) > 0 {
		details := make([]string, 0, len(unchecked))
		for _, name := range unchecked {
			details = append(details, fmt.Sprintf("%s (%v)", name, causes[name]))
		}
		err = fmt.Errorf("%w; volumes %s were stopped for this apply and left unmounted, because their storage could not be checked against the volumes being rewritten: %s; check their disks, then retry the apply", err, strings.Join(unchecked, ", "), strings.Join(details, "; "))
	}
	if len(r.rewritten) > 0 {
		err = fmt.Errorf("%w; volumes %s were being repartitioned or formatted when the apply stopped, so they may hold incomplete filesystems; a rewritten volume stays unmounted until the apply is retried, which finishes preparing it once the cause is fixed", err, strings.Join(r.rewritten, ", "))
	}
	return err
}
