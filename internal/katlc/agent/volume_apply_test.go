package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/discovery"
	"github.com/katl-dev/katl/internal/installer/disk"
	"github.com/katl-dev/katl/internal/installer/manifest"
)

func TestPrepareLiveVolumeUsesBoundedSystemdRepartDefinition(t *testing.T) {
	root := t.TempDir()
	var repart []string
	discover := volumeAuthorityRunner("gpt", nil)
	runner := func(ctx context.Context, argv []string, progress func(int)) ToolResult {
		if argv[0] != "systemd-repart" {
			return discover(ctx, argv, progress)
		}
		repart = append([]string(nil), argv...)
		var definitions string
		for _, arg := range argv {
			if strings.HasPrefix(arg, "--definitions=") {
				definitions = strings.TrimPrefix(arg, "--definitions=")
			}
		}
		files, err := filepath.Glob(filepath.Join(definitions, "*.conf"))
		if err != nil || len(files) != 1 {
			t.Fatalf("definitions = %v, err = %v", files, err)
		}
		data, err := os.ReadFile(files[0])
		if err != nil {
			t.Fatalf("read repart definition: %v", err)
		}
		content := string(data)
		for _, want := range []string{"Type=11111111-2222-5333-8444-555555555555", "Label=u-data", "Format=xfs"} {
			if !strings.Contains(content, want) {
				t.Fatalf("repart definition missing %q:\n%s", want, content)
			}
		}
		return ToolResult{}
	}
	devices := hermeticVolumeDevices(t)

	err := prepareLiveVolume(context.Background(), runner, devices.source(runner), root, disk.VolumePlan{
		Name: "data", Disk: discovery.DiskIdentity{Path: "/dev/vdb", DiskSeq: 2, SizeBytes: 68719476736, Serial: "data"}, Filesystem: "xfs", MountPath: "/var/mnt/data",
		Wipe: true, Repartition: true, TypeUUID: "11111111-2222-5333-8444-555555555555",
	})

	if err != nil {
		t.Fatalf("prepareLiveVolume() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "var/mnt/data")); err != nil {
		t.Fatalf("volume mount point: %v", err)
	}
	if got := strings.Join(repart, " "); !strings.Contains(got, "systemd-repart --dry-run=no --empty=force") || !strings.HasSuffix(got, " /dev/disk/by-diskseq/2") || !strings.Contains(got, "--discard=no") {
		t.Fatalf("repart argv = %q, want the disk addressed by diskseq", got)
	}
}

func TestPrepareLiveVolumePreservesExistingPartition(t *testing.T) {
	called := false
	runner := func(context.Context, []string, func(int)) ToolResult {
		called = true
		return ToolResult{}
	}
	plan := disk.VolumePlan{
		Name: "data", Disk: discovery.DiskIdentity{Path: "/dev/vdb", DiskSeq: 2}, Partition: disk.PartitionIdentity{Number: 1, UUID: "data-part"},
		Filesystem: "xfs", MountPath: "/var/mnt/data",
	}

	if err := prepareLiveVolume(context.Background(), runner, hermeticVolumeDevices(t).source(runner), t.TempDir(), plan); err != nil {
		t.Fatalf("prepareLiveVolume() error = %v", err)
	}
	if called {
		t.Fatal("preserved partition ran a tool")
	}
}

func TestApplyVolumesPreflightsBeforeStoppingExistingMount(t *testing.T) {
	current := manifest.Manifest{
		Install: manifest.InstallConfig{
			TargetDisk: manifest.DiskSelector{Serial: "root"},
			Volumes: []manifest.Volume{{
				Name: "data", Selector: manifest.VolumeSelector{Partition: &manifest.PartitionSelector{PartUUID: "current"}}, Filesystem: "xfs",
			}},
		},
	}
	desired := current
	desired.Install.Volumes = []manifest.Volume{{
		Name: "data", Selector: manifest.VolumeSelector{Partition: &manifest.PartitionSelector{PartUUID: "missing"}}, Filesystem: "xfs",
	}}
	var systemctlCalled bool
	runner := func(_ context.Context, argv []string, _ func(int)) ToolResult {
		switch argv[0] {
		case "lsblk":
			return ToolResult{Stdout: []byte(`{
  "blockdevices": [
    {"name":"vda","path":"/dev/vda","type":"disk","serial":"root","size":68719476736,"ro":false,"mountpoints":[],"children":[{"name":"vda5","path":"/dev/vda5","type":"part","size":1073741824,"ro":false,"partlabel":"KATL_STATE","mountpoints":["/var"]}]},
    {"name":"vdb","path":"/dev/vdb","type":"disk","serial":"data","size":68719476736,"ro":false,"mountpoints":[],"children":[
      {"name":"vdb1","path":"/dev/vdb1","type":"part","size":68702699520,"ro":false,"fstype":"xfs","partuuid":"current","partlabel":"u-data","mountpoints":["/var/mnt/data"]}
    ]}
  ]
}`)}
		case "findmnt":
			return ToolResult{Stdout: []byte(`{"filesystems":[{"source":"/dev/vdb1","target":"/var/mnt/data","fstype":"xfs","options":"rw"}]}`)}
		case "ip":
			return ToolResult{Stdout: []byte(`[]`)}
		case "systemctl":
			systemctlCalled = true
			return ToolResult{}
		default:
			return ToolResult{Err: fmt.Errorf("unexpected command %q", argv[0]), ExitStatus: 1}
		}
	}
	_, err := (&Executor{RunTool: runner, volumeDevices: hermeticVolumeDevices(t)}).applyVolumes(context.Background(), current, desired, []generation.VolumeBinding{{Name: "data", PartitionUUID: "current"}}, "node-a", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "partition selector matched no partitions") {
		t.Fatalf("applyVolumes() error = %v, want missing partition", err)
	}
	if systemctlCalled {
		t.Fatal("applyVolumes() stopped the existing mount before preflight completed")
	}
}

func TestApplyVolumesRemovalOnlyUnmountsWithoutTouchingStorage(t *testing.T) {
	current := manifest.Manifest{Install: manifest.InstallConfig{
		TargetDisk: manifest.DiskSelector{Serial: "root"},
		Volumes: []manifest.Volume{{
			Name: "data", Selector: manifest.VolumeSelector{Partition: &manifest.PartitionSelector{PartUUID: "keep-me"}}, Filesystem: "xfs",
		}},
	}}
	desired := current
	desired.Install.Volumes = []manifest.Volume{}
	var calls [][]string
	runner := func(_ context.Context, argv []string, _ func(int)) ToolResult {
		calls = append(calls, append([]string(nil), argv...))
		if len(argv) != 3 || argv[0] != "systemctl" || argv[1] != "stop" || argv[2] != "var-mnt-data.mount" {
			return ToolResult{Err: fmt.Errorf("unexpected storage-removal command %q", argv), ExitStatus: 1}
		}
		return ToolResult{}
	}
	bindings, err := (&Executor{RunTool: runner, volumeDevices: hermeticVolumeDevices(t)}).applyVolumes(context.Background(), current, desired, []generation.VolumeBinding{{Name: "data", PartitionUUID: "keep-me"}}, "cp-1", nil, nil)
	if err != nil {
		t.Fatalf("applyVolumes() removal error = %v", err)
	}
	if !reflect.DeepEqual(bindings, []generation.VolumeBinding{{Name: "data", PartitionUUID: "keep-me"}}) {
		t.Fatalf("removal bindings = %#v, want retained tombstone", bindings)
	}
	if len(calls) != 1 {
		t.Fatalf("removal commands = %v, want only the managed mount stop", calls)
	}
}

func TestApplyVolumesPreservesWithoutWipe(t *testing.T) {
	current := manifest.Manifest{Install: manifest.InstallConfig{TargetDisk: manifest.DiskSelector{Serial: "root"}}}
	desired := current
	desired.Install.Volumes = []manifest.Volume{{
		Name: "data", Selector: manifest.VolumeSelector{Disk: &manifest.DiskSelector{Serial: "data"}}, Filesystem: "xfs", Wipe: false,
	}}
	var mutations [][]string
	runner := volumeAuthorityRunner("gpt", &mutations)
	_, err := (&Executor{RunTool: runner, volumeDevices: hermeticVolumeDevices(t)}).applyVolumes(context.Background(), current, desired, nil, "cp-1", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "set wipe to true") {
		t.Fatalf("applyVolumes() error = %#v", err)
	}
	if len(mutations) != 0 {
		t.Fatalf("destructive storage refusal ran mutating tools: %v", mutations)
	}
}

func TestApplyVolumesHonorsWipe(t *testing.T) {
	current := manifest.Manifest{Install: manifest.InstallConfig{TargetDisk: manifest.DiskSelector{Serial: "root"}}}
	desired := current
	desired.Install.Volumes = []manifest.Volume{{
		Name: "data", Selector: manifest.VolumeSelector{Disk: &manifest.DiskSelector{Serial: "data"}}, Filesystem: "xfs", Wipe: true,
	}}
	var mutations [][]string
	runner := volumeAuthorityRunner("gpt", &mutations)
	_, err := (&Executor{Root: t.TempDir(), RunTool: runner, volumeDevices: hermeticVolumeDevices(t)}).applyVolumes(context.Background(), current, desired, nil, "cp-1", nil, nil)
	if err != nil {
		t.Fatalf("applyVolumes() error = %v", err)
	}
	var writes [][]string
	for _, argv := range mutations {
		if argv[0] != "udevadm" {
			writes = append(writes, argv)
		}
	}
	if len(writes) != 1 || writes[0][0] != "systemd-repart" || writes[0][len(writes[0])-1] != "/dev/disk/by-diskseq/2" {
		t.Fatalf("acknowledged volume mutations = %v, want one repart of /dev/disk/by-diskseq/2", mutations)
	}
}

func TestApplyVolumesRefusesDiskReplacedBeforePreparation(t *testing.T) {
	current := manifest.Manifest{Install: manifest.InstallConfig{TargetDisk: manifest.DiskSelector{Serial: "root"}}}
	desired := current
	desired.Install.Volumes = []manifest.Volume{{
		Name: "data", Selector: manifest.VolumeSelector{Disk: &manifest.DiskSelector{Serial: "data"}}, Filesystem: "xfs", Wipe: true,
	}}
	// After planning, the selected disk is detached and another disk takes
	// its kernel name.
	discoveries := 0
	var mutations [][]string
	runner := func(_ context.Context, argv []string, _ func(int)) ToolResult {
		switch argv[0] {
		case "lsblk":
			discoveries++
			serial := "data"
			if discoveries > 1 {
				serial = "other"
			}
			return ToolResult{Stdout: []byte(`{"blockdevices":[` +
				`{"name":"vda","path":"/dev/vda","type":"disk","serial":"root","size":68719476736,"ro":false,"mountpoints":[],"children":[{"name":"vda5","path":"/dev/vda5","type":"part","size":1073741824,"ro":false,"partlabel":"KATL_STATE","mountpoints":["/var"]}]},` +
				`{"name":"vdb","path":"/dev/vdb","type":"disk","serial":"` + serial + `","size":68719476736,"ro":false,"mountpoints":[]}` +
				`]}`)}
		case "findmnt":
			return ToolResult{Stdout: []byte(`{"filesystems":[]}`)}
		case "ip":
			return ToolResult{Stdout: []byte(`[]`)}
		default:
			mutations = append(mutations, append([]string(nil), argv...))
			return ToolResult{}
		}
	}

	_, err := (&Executor{Root: t.TempDir(), RunTool: runner, volumeDevices: hermeticVolumeDevices(t)}).applyVolumes(context.Background(), current, desired, nil, "cp-1", nil, nil)

	if !errors.Is(err, discovery.ErrTargetDiskChanged) {
		t.Fatalf("applyVolumes() error = %v, want ErrTargetDiskChanged", err)
	}
	for _, argv := range mutations {
		if argv[0] != "udevadm" {
			t.Fatalf("replaced disk was mutated: %v", mutations)
		}
	}
}

func TestVolumeTransitionPreservesBoundDeviceWhenLabelBecomesAmbiguous(t *testing.T) {
	current := volumeManifest(manifest.PartitionSelector{})
	plan, err := planVolumeTransition(context.Background(), hermeticVolumeDevices(t).source(duplicateLabelVolumeRunner()), current, current,
		[]generation.VolumeBinding{{Name: "data", PartitionUUID: "old-part", FilesystemUUID: "old-fs"}}, "cp-1", nil, nil)
	if err != nil {
		t.Fatalf("planVolumeTransition() error = %v", err)
	}
	want := []generation.VolumeBinding{{Name: "data", PartitionUUID: "old-part", FilesystemUUID: "old-fs"}}
	if !reflect.DeepEqual(plan.bindings, want) || len(plan.prepare) != 0 || len(plan.stopNames) != 0 {
		t.Fatalf("transition = %#v, want preserved binding only", plan)
	}
}

func TestVolumeTransitionBlocksAmbiguousLabelWithoutPriorBinding(t *testing.T) {
	current := volumeManifest(manifest.PartitionSelector{})
	_, err := planVolumeTransition(context.Background(), hermeticVolumeDevices(t).source(duplicateLabelVolumeRunner()), current, current, nil, "cp-1", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "partition selector matched 2 partitions") {
		t.Fatalf("planVolumeTransition() error = %v, want ambiguous selector", err)
	}
}

func TestVolumeTransitionRequiresExplicitRebindForReplacement(t *testing.T) {
	current := volumeManifest(manifest.PartitionSelector{PartUUID: "old-part"})
	desired := volumeManifest(manifest.PartitionSelector{PartUUID: "new-part"})
	currentBinding := []generation.VolumeBinding{{Name: "data", PartitionUUID: "old-part", FilesystemUUID: "old-fs"}}

	plan, err := planVolumeTransition(context.Background(), hermeticVolumeDevices(t).source(duplicateLabelVolumeRunner()), current, desired, currentBinding, "cp-1", nil, nil)
	var authority *VolumeRebindAuthorityError
	if !errors.As(err, &authority) || !reflect.DeepEqual(plan.requiredRebinds, []string{"cp-1/data"}) {
		t.Fatalf("unapproved replacement plan = %#v error = %v", plan, err)
	}
	plan, err = planVolumeTransition(context.Background(), hermeticVolumeDevices(t).source(duplicateLabelVolumeRunner()), current, desired, currentBinding, "cp-1", nil, []string{"cp-1/data"})
	if err != nil {
		t.Fatalf("approved plan error = %v", err)
	}
	if len(plan.prepare) != 1 || plan.prepare[0].Partition.UUID != "new-part" {
		t.Fatalf("approved replacement plan = %#v", plan)
	}
}

func volumeManifest(selector manifest.PartitionSelector) manifest.Manifest {
	if selector.PartUUID == "" && selector.FilesystemUUID == "" && selector.ByID == "" {
		// Empty manifest selectors represent the compiled byVolumeName
		// convention through BuildVolumeRequests.
	}
	return manifest.Manifest{Install: manifest.InstallConfig{
		TargetDisk: manifest.DiskSelector{Serial: "root"},
		Volumes: []manifest.Volume{{
			Name: "data", Selector: manifest.VolumeSelector{Partition: &selector}, Filesystem: "xfs",
		}},
	}}
}

func duplicateLabelVolumeRunner() ToolRunner {
	return func(_ context.Context, argv []string, _ func(int)) ToolResult {
		switch argv[0] {
		case "lsblk":
			return ToolResult{Stdout: []byte(`{"blockdevices":[
{"name":"vda","path":"/dev/vda","type":"disk","serial":"root","size":68719476736,"ro":false,"mountpoints":[],"children":[{"name":"vda5","path":"/dev/vda5","type":"part","size":1073741824,"ro":false,"partlabel":"KATL_STATE","mountpoints":["/var"]}]},
{"name":"vdb","path":"/dev/vdb","type":"disk","serial":"old","size":68719476736,"ro":false,"mountpoints":[],"children":[{"name":"vdb1","path":"/dev/vdb1","type":"part","size":68702699520,"ro":false,"fstype":"xfs","uuid":"old-fs","partuuid":"old-part","partlabel":"u-data","mountpoints":["/var/mnt/data"]}]},
{"name":"vdc","path":"/dev/vdc","type":"disk","serial":"new","size":68719476736,"ro":false,"mountpoints":[],"children":[{"name":"vdc1","path":"/dev/vdc1","type":"part","size":68702699520,"ro":false,"fstype":"xfs","uuid":"new-fs","partuuid":"new-part","partlabel":"u-data","mountpoints":[]}]}
]}`)}
		case "findmnt":
			return ToolResult{Stdout: []byte(`{"filesystems":[{"source":"/dev/vdb1","target":"/var/mnt/data","fstype":"xfs","options":"rw"}]}`)}
		case "ip":
			return ToolResult{Stdout: []byte(`[]`)}
		default:
			return ToolResult{Err: fmt.Errorf("unexpected command %q", argv[0]), ExitStatus: 1}
		}
	}
}

func volumeAuthorityRunner(partitionTable string, mutations *[][]string) ToolRunner {
	prepared := false
	return func(_ context.Context, argv []string, _ func(int)) ToolResult {
		switch argv[0] {
		case "lsblk":
			pttype := ""
			if partitionTable != "" {
				pttype = `,"pttype":"` + partitionTable + `"`
			}
			children := ""
			if prepared {
				children = `,"children":[{"name":"vdb1","path":"/dev/vdb1","type":"part","size":68702699520,"ro":false,"fstype":"xfs","uuid":"fs-data","partuuid":"part-data","partlabel":"u-data","mountpoints":[]}]`
			}
			return ToolResult{Stdout: []byte(`{"blockdevices":[` +
				`{"name":"vda","path":"/dev/vda","type":"disk","serial":"root","size":68719476736,"ro":false,"mountpoints":[],"children":[{"name":"vda5","path":"/dev/vda5","type":"part","size":1073741824,"ro":false,"partlabel":"KATL_STATE","mountpoints":["/var"]}]},` +
				`{"name":"vdb","path":"/dev/vdb","type":"disk","serial":"data","size":68719476736,"ro":false,"mountpoints":[]` + pttype + children + `}` +
				`]}`)}
		case "findmnt":
			return ToolResult{Stdout: []byte(`{"filesystems":[]}`)}
		case "ip":
			return ToolResult{Stdout: []byte(`[]`)}
		default:
			if argv[0] == "systemd-repart" {
				prepared = true
			}
			if mutations != nil {
				*mutations = append(*mutations, append([]string(nil), argv...))
			}
			return ToolResult{}
		}
	}
}

func TestFactsWithoutManagedVolumeMountsOnlyClearsSelectedVolume(t *testing.T) {
	facts := discovery.HardwareFacts{
		BlockDevices: []discovery.BlockDevice{{
			Path: "/dev/vdb", Type: discovery.DeviceDisk, Partitions: []discovery.BlockDevice{
				{Path: "/dev/vdb1", Type: discovery.DevicePartition, Mountpoints: []string{"/var/mnt/data"}},
				{Path: "/dev/vdb2", Type: discovery.DevicePartition, Mountpoints: []string{"/var/mnt/other"}},
			},
		}},
		Mounts: []discovery.MountFact{
			{Source: "/dev/vdb1", Target: "/var/mnt/data"},
			{Source: "/dev/vdb2", Target: "/var/mnt/other"},
		},
	}
	filtered := factsWithoutManagedVolumeMounts(facts, []string{"data"})
	if len(filtered.Mounts) != 1 || filtered.Mounts[0].Target != "/var/mnt/other" {
		t.Fatalf("filtered mounts = %#v", filtered.Mounts)
	}
	partitions := filtered.BlockDevices[0].Partitions
	if len(partitions[0].Mountpoints) != 0 || len(partitions[1].Mountpoints) != 1 {
		t.Fatalf("filtered partitions = %#v", partitions)
	}
	if len(facts.Mounts) != 2 || len(facts.BlockDevices[0].Partitions[0].Mountpoints) != 1 {
		t.Fatal("factsWithoutManagedVolumeMounts mutated its input")
	}
}

func TestVolumeTransitionPlanRejectsDeferredDeviceMutation(t *testing.T) {
	plan := volumeTransitionPlan{prepare: []disk.VolumePlan{{Name: "data"}}}
	if err := plan.validateApplyMode(generation.ApplyModeNextBoot); err == nil || !strings.Contains(err.Error(), "apply the volume change separately") {
		t.Fatalf("validateApplyMode(next-boot) error = %v", err)
	}
	if err := plan.validateApplyMode(generation.ApplyModeLive); err != nil {
		t.Fatalf("validateApplyMode(live) error = %v", err)
	}
	if err := (volumeTransitionPlan{}).validateApplyMode(generation.ApplyModeNextBoot); err != nil {
		t.Fatalf("validateApplyMode(binding-only next-boot) error = %v", err)
	}
}

func TestVolumeTransitionPlanRetainsTombstonesWithoutDesiredVolumes(t *testing.T) {
	bindings := []generation.VolumeBinding{{Name: "data", PartitionUUID: "part-data", FilesystemUUID: "fs-data"}}
	plan, err := planVolumeTransition(context.Background(), hermeticVolumeDevices(t).source(func(context.Context, []string, func(int)) ToolResult {
		t.Fatal("zero-volume tombstone retention must not inspect hardware")
		return ToolResult{}
	}), manifest.Manifest{}, manifest.Manifest{}, bindings, "cp-1", nil, nil)
	if err != nil {
		t.Fatalf("planVolumeTransition() error = %v", err)
	}
	if !reflect.DeepEqual(plan.bindings, bindings) {
		t.Fatalf("bindings = %#v, want retained tombstone %#v", plan.bindings, bindings)
	}
}

func TestRunningSystemDiskUsesMountedState(t *testing.T) {
	facts := discovery.HardwareFacts{
		BlockDevices: []discovery.BlockDevice{
			{Path: "/dev/sda", Type: discovery.DeviceDisk, Partitions: []discovery.BlockDevice{{Path: "/dev/sda5", Type: discovery.DevicePartition, GPTLabel: "KATL_STATE"}}},
			{Path: "/dev/sdb", Type: discovery.DeviceDisk, Partitions: []discovery.BlockDevice{{Path: "/dev/sdb5", Type: discovery.DevicePartition, GPTLabel: "KATL_STATE"}}},
		},
		Mounts: []discovery.MountFact{{Source: "/dev/sdb5", Target: "/var"}},
	}

	got, err := runningSystemDisk(facts)
	if err != nil || got.Path != "/dev/sdb" {
		t.Fatalf("runningSystemDisk() = %s, %v; want /dev/sdb, the disk whose state is mounted", got.Path, err)
	}

	facts.Mounts = nil
	if _, err := runningSystemDisk(facts); err == nil {
		t.Fatal("runningSystemDisk() error = nil without a mounted /var, want failure")
	}
}

// hermeticVolumeDevices points volume discovery at a fixture sysfs that gives
// the disks and partitions used by these tests fixed diskseqs and partition
// numbers, so the host's block devices never leak into a test.
func hermeticVolumeDevices(t *testing.T) volumeDevices {
	t.Helper()
	sysBlock := filepath.Join(t.TempDir(), "class", "block")
	attributes := map[string]string{
		"vda/diskseq": "1", "vdb/diskseq": "2", "vdc/diskseq": "3",
		"vda5/partition": "5", "vdb1/partition": "1", "vdb2/partition": "2", "vdc1/partition": "1",
	}
	for name, value := range attributes {
		path := filepath.Join(sysBlock, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(value+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	missing := filepath.Join(t.TempDir(), "missing")
	return volumeDevices{sysBlockDir: sysBlock, byIDDir: missing, byPathDir: missing}
}

// sharedDiskRunner reports data disk vdb holding the mounted, unchanged
// volume keep on vdb1 and a free partition vdb2.
func sharedDiskRunner(mutations *[][]string) ToolRunner {
	return func(_ context.Context, argv []string, _ func(int)) ToolResult {
		switch argv[0] {
		case "lsblk":
			return ToolResult{Stdout: []byte(`{"blockdevices":[` +
				`{"name":"vda","path":"/dev/vda","type":"disk","serial":"root","size":68719476736,"ro":false,"mountpoints":[],"children":[{"name":"vda5","path":"/dev/vda5","type":"part","size":1073741824,"ro":false,"partlabel":"KATL_STATE","mountpoints":["/var"]}]},` +
				`{"name":"vdb","path":"/dev/vdb","type":"disk","serial":"data","size":68719476736,"ro":false,"pttype":"gpt","mountpoints":[],"children":[` +
				`{"name":"vdb1","path":"/dev/vdb1","type":"part","size":1073741824,"ro":false,"fstype":"xfs","uuid":"keep-fs","partuuid":"keep-part","mountpoints":["/var/mnt/keep"]},` +
				`{"name":"vdb2","path":"/dev/vdb2","type":"part","size":1073741824,"ro":false,"fstype":"ext4","uuid":"data-fs","partuuid":"data-part","mountpoints":[]}` +
				`]}]}`)}
		case "findmnt":
			return ToolResult{Stdout: []byte(`{"filesystems":[{"source":"/dev/vdb1","target":"/var/mnt/keep","fstype":"xfs","options":"rw"}]}`)}
		case "ip":
			return ToolResult{Stdout: []byte(`[]`)}
		default:
			*mutations = append(*mutations, append([]string(nil), argv...))
			return ToolResult{}
		}
	}
}

func TestApplyVolumesBesideMountedVolumeOnSameDisk(t *testing.T) {
	keep := manifest.Volume{Name: "keep", Selector: manifest.VolumeSelector{Partition: &manifest.PartitionSelector{PartUUID: "keep-part"}}, Filesystem: "xfs"}
	current := manifest.Manifest{Install: manifest.InstallConfig{TargetDisk: manifest.DiskSelector{Serial: "root"}, Volumes: []manifest.Volume{keep}}}
	bindings := []generation.VolumeBinding{{Name: "keep", PartitionUUID: "keep-part", FilesystemUUID: "keep-fs"}}

	t.Run("format a sibling partition", func(t *testing.T) {
		desired := current
		desired.Install.Volumes = []manifest.Volume{keep, {
			Name: "data", Selector: manifest.VolumeSelector{Partition: &manifest.PartitionSelector{PartUUID: "data-part"}}, Filesystem: "xfs", Wipe: true,
		}}
		var mutations [][]string

		got, err := (&Executor{Root: t.TempDir(), RunTool: sharedDiskRunner(&mutations), volumeDevices: hermeticVolumeDevices(t)}).applyVolumes(context.Background(), current, desired, bindings, "cp-1", nil, nil)

		if err != nil {
			t.Fatalf("applyVolumes() error = %v", err)
		}
		var writes []string
		for _, argv := range mutations {
			if argv[0] != "udevadm" {
				writes = append(writes, strings.Join(argv, " "))
			}
		}
		want := []string{"wipefs --all /dev/disk/by-diskseq/2-part2", "mkfs.xfs -K /dev/disk/by-diskseq/2-part2"}
		if !reflect.DeepEqual(writes, want) {
			t.Fatalf("commands = %q, want only the sibling partition formatted and keep left mounted", writes)
		}
		wantBindings := []generation.VolumeBinding{{Name: "data", PartitionUUID: "data-part", FilesystemUUID: "data-fs"}, bindings[0]}
		if !reflect.DeepEqual(got, wantBindings) {
			t.Fatalf("bindings = %#v, want %#v", got, wantBindings)
		}
	})

	t.Run("repartition the shared disk", func(t *testing.T) {
		desired := current
		desired.Install.Volumes = []manifest.Volume{keep, {
			Name: "data", Selector: manifest.VolumeSelector{Disk: &manifest.DiskSelector{Serial: "data"}}, Filesystem: "xfs", Wipe: true,
		}}
		var mutations [][]string

		_, err := (&Executor{Root: t.TempDir(), RunTool: sharedDiskRunner(&mutations), volumeDevices: hermeticVolumeDevices(t)}).applyVolumes(context.Background(), current, desired, bindings, "cp-1", nil, nil)

		if !errors.Is(err, disk.ErrVolumeRefused) || !strings.Contains(err.Error(), "is mounted") {
			t.Fatalf("applyVolumes() error = %v, want refusal of the disk holding mounted volume keep", err)
		}
		for _, argv := range mutations {
			if argv[0] != "udevadm" {
				t.Fatalf("mounted volume's disk was touched: %v", mutations)
			}
		}
	})
}

func TestApplyVolumesRestartsStoppedVolumesAfterRefusal(t *testing.T) {
	current := volumeManifest(manifest.PartitionSelector{PartUUID: "old-part"})
	desired := current
	desired.Install.Volumes = []manifest.Volume{{
		Name: "data", Selector: manifest.VolumeSelector{Disk: &manifest.DiskSelector{Serial: "new"}}, Filesystem: "xfs", Wipe: true,
	}}
	// Planning sees disk vdc; by preparation another disk has its name.
	discoveries := 0
	var commands []string
	runner := func(_ context.Context, argv []string, _ func(int)) ToolResult {
		switch argv[0] {
		case "lsblk":
			discoveries++
			serial := "new"
			if discoveries > 1 {
				serial = "other"
			}
			return ToolResult{Stdout: []byte(`{"blockdevices":[` +
				`{"name":"vda","path":"/dev/vda","type":"disk","serial":"root","size":68719476736,"ro":false,"mountpoints":[],"children":[{"name":"vda5","path":"/dev/vda5","type":"part","size":1073741824,"ro":false,"partlabel":"KATL_STATE","mountpoints":["/var"]}]},` +
				`{"name":"vdb","path":"/dev/vdb","type":"disk","serial":"old","size":68719476736,"ro":false,"mountpoints":[],"children":[{"name":"vdb1","path":"/dev/vdb1","type":"part","size":68702699520,"ro":false,"fstype":"xfs","uuid":"old-fs","partuuid":"old-part","mountpoints":["/var/mnt/data"]}]},` +
				`{"name":"vdc","path":"/dev/vdc","type":"disk","serial":"` + serial + `","size":68719476736,"ro":false,"mountpoints":[]}` +
				`]}`)}
		case "findmnt":
			return ToolResult{Stdout: []byte(`{"filesystems":[{"source":"/dev/vdb1","target":"/var/mnt/data","fstype":"xfs","options":"rw"}]}`)}
		case "ip":
			return ToolResult{Stdout: []byte(`[]`)}
		default:
			commands = append(commands, strings.Join(argv, " "))
			return ToolResult{}
		}
	}
	bindings := []generation.VolumeBinding{{Name: "data", PartitionUUID: "old-part", FilesystemUUID: "old-fs"}}

	_, err := (&Executor{Root: t.TempDir(), RunTool: runner, volumeDevices: hermeticVolumeDevices(t)}).applyVolumes(context.Background(), current, desired, bindings, "cp-1", nil, []string{"cp-1/data"})

	if !errors.Is(err, discovery.ErrTargetDiskChanged) || !strings.Contains(err.Error(), "volumes data were stopped for this apply and have been mounted again") {
		t.Fatalf("applyVolumes() error = %v, want refusal that reports data mounted again", err)
	}
	want := []string{"systemctl stop var-mnt-data.mount", "udevadm settle", "systemctl start var-mnt-data.mount"}
	if !reflect.DeepEqual(commands, want) {
		t.Fatalf("commands = %q, want %q", commands, want)
	}
}
