package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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

	_, err := prepareLiveVolumes(context.Background(), runner, devices.source(runner), root, []disk.VolumePlan{{
		Name: "data", Disk: discovery.DiskIdentity{Path: "/dev/vdb", DiskSeq: 2, SizeBytes: 68719476736, Serial: "data"}, Filesystem: "xfs", MountPath: "/var/mnt/data",
		Wipe: true, Repartition: true, TypeUUID: "11111111-2222-5333-8444-555555555555",
	}})

	if err != nil {
		t.Fatalf("prepareLiveVolumes() error = %v", err)
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

	if _, err := prepareLiveVolumes(context.Background(), runner, hermeticVolumeDevices(t).source(runner), t.TempDir(), []disk.VolumePlan{plan}); err != nil {
		t.Fatalf("prepareLiveVolumes() error = %v", err)
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
	// The disk is cleared first, so the kernel lists none of its old
	// partitions when repart adds the new one.
	if len(writes) != 2 || strings.Join(writes[0], " ") != "wipefs --all /dev/disk/by-diskseq/2" || writes[1][0] != "systemd-repart" || writes[1][len(writes[1])-1] != "/dev/disk/by-diskseq/2" {
		t.Fatalf("acknowledged volume mutations = %v, want a wipefs, then one repart, of /dev/disk/by-diskseq/2", mutations)
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
// the disks and partitions used by these tests fixed diskseqs, by-diskseq
// links, and partition numbers, so the host's block devices never leak into a
// test.
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
	byDiskSeq := t.TempDir()
	for seq, disk := range map[string]string{"1": "/dev/vda", "2": "/dev/vdb", "3": "/dev/vdc"} {
		if err := os.Symlink(disk, filepath.Join(byDiskSeq, seq)); err != nil {
			t.Fatal(err)
		}
	}
	missing := filepath.Join(t.TempDir(), "missing")
	return volumeDevices{sysBlockDir: sysBlock, byIDDir: missing, byPathDir: missing, byDiskSeqDir: byDiskSeq}
}

// storageRunner reports data disk vdb holding volume keep on vdb1, mounted
// when keepMounted, beside a free partition vdb2, and spare disk vdc with one
// free partition vdc1. Every other command is recorded and succeeds unless
// fail reports it failed. lsblk calls are counted so a fixture can change
// what later discoveries see.
type storageRunner struct {
	keepMounted bool
	// dataSerial, when set, replaces vdb's serial from the second discovery
	// on, as if another disk had taken its kernel name.
	dataSerial  string
	fail        func(argv []string) bool
	commands    []string
	discoveries int
}

func (r *storageRunner) run(_ context.Context, argv []string, _ func(int)) ToolResult {
	switch argv[0] {
	case "lsblk":
		r.discoveries++
		serial := "data"
		if r.dataSerial != "" && r.discoveries > 1 {
			serial = r.dataSerial
		}
		keepMounts := `[]`
		if r.keepMounted {
			keepMounts = `["/var/mnt/keep"]`
		}
		return ToolResult{Stdout: []byte(`{"blockdevices":[` +
			`{"name":"vda","path":"/dev/vda","type":"disk","serial":"root","size":68719476736,"ro":false,"mountpoints":[],"children":[{"name":"vda5","path":"/dev/vda5","type":"part","size":1073741824,"ro":false,"partlabel":"KATL_STATE","mountpoints":["/var"]}]},` +
			`{"name":"vdb","path":"/dev/vdb","type":"disk","serial":"` + serial + `","size":68719476736,"ro":false,"pttype":"gpt","mountpoints":[],"children":[` +
			`{"name":"vdb1","path":"/dev/vdb1","type":"part","size":1073741824,"ro":false,"fstype":"xfs","uuid":"keep-fs","partuuid":"keep-part","mountpoints":` + keepMounts + `},` +
			`{"name":"vdb2","path":"/dev/vdb2","type":"part","size":1073741824,"ro":false,"fstype":"ext4","uuid":"data-fs","partuuid":"data-part","mountpoints":[]}` +
			`]},` +
			`{"name":"vdc","path":"/dev/vdc","type":"disk","serial":"spare","size":68719476736,"ro":false,"pttype":"gpt","mountpoints":[],"children":[` +
			`{"name":"vdc1","path":"/dev/vdc1","type":"part","size":1073741824,"ro":false,"fstype":"xfs","uuid":"spare-fs","partuuid":"spare-part","mountpoints":[]}` +
			`]}]}`)}
	case "findmnt":
		if r.keepMounted {
			return ToolResult{Stdout: []byte(`{"filesystems":[{"source":"/dev/vdb1","target":"/var/mnt/keep","fstype":"xfs","options":"rw"}]}`)}
		}
		return ToolResult{Stdout: []byte(`{"filesystems":[]}`)}
	case "ip":
		return ToolResult{Stdout: []byte(`[]`)}
	default:
		r.commands = append(r.commands, strings.Join(argv, " "))
		if r.fail != nil && r.fail(argv) {
			return ToolResult{ExitStatus: 1, Stderr: []byte(argv[0] + " failed")}
		}
		return ToolResult{}
	}
}

// writes lists the recorded commands other than udev settling.
func (r *storageRunner) writes() []string {
	var writes []string
	for _, command := range r.commands {
		if command != "udevadm settle" {
			writes = append(writes, command)
		}
	}
	return writes
}

var (
	keepVolume  = manifest.Volume{Name: "keep", Selector: manifest.VolumeSelector{Partition: &manifest.PartitionSelector{PartUUID: "keep-part"}}, Filesystem: "xfs"}
	keepBinding = generation.VolumeBinding{Name: "keep", PartitionUUID: "keep-part", FilesystemUUID: "keep-fs"}
)

func volumesManifest(volumes ...manifest.Volume) manifest.Manifest {
	return manifest.Manifest{Install: manifest.InstallConfig{TargetDisk: manifest.DiskSelector{Serial: "root"}, Volumes: volumes}}
}

func TestApplyVolumesFormatsPartitionBesideMountedVolume(t *testing.T) {
	current := volumesManifest(keepVolume)
	desired := volumesManifest(keepVolume, manifest.Volume{
		Name: "data", Selector: manifest.VolumeSelector{Partition: &manifest.PartitionSelector{PartUUID: "data-part"}}, Filesystem: "xfs", Wipe: true,
	})
	runner := &storageRunner{keepMounted: true}

	got, err := (&Executor{Root: t.TempDir(), RunTool: runner.run, volumeDevices: hermeticVolumeDevices(t)}).applyVolumes(context.Background(), current, desired, []generation.VolumeBinding{keepBinding}, "cp-1", nil, nil)

	if err != nil {
		t.Fatalf("applyVolumes() error = %v", err)
	}
	want := []string{"wipefs --all /dev/disk/by-diskseq/2-part2", "mkfs.xfs -K /dev/disk/by-diskseq/2-part2"}
	if !reflect.DeepEqual(runner.writes(), want) {
		t.Fatalf("commands = %q, want only the sibling partition formatted and keep left mounted", runner.writes())
	}
	wantBindings := []generation.VolumeBinding{{Name: "data", PartitionUUID: "data-part", FilesystemUUID: "data-fs"}, keepBinding}
	if !reflect.DeepEqual(got, wantBindings) {
		t.Fatalf("bindings = %#v, want %#v", got, wantBindings)
	}
}

func TestApplyVolumesRefusesSharedStorage(t *testing.T) {
	wipeDisk := func(name, serial string) manifest.Volume {
		return manifest.Volume{Name: name, Selector: manifest.VolumeSelector{Disk: &manifest.DiskSelector{Serial: serial}}, Filesystem: "xfs", Wipe: true}
	}
	partition := func(name string, selector manifest.PartitionSelector, wipe bool) manifest.Volume {
		return manifest.Volume{Name: name, Selector: manifest.VolumeSelector{Partition: &selector}, Filesystem: "xfs", Wipe: wipe}
	}

	for _, test := range []struct {
		name        string
		current     manifest.Manifest
		bindings    []generation.VolumeBinding
		desired     manifest.Manifest
		keepMounted bool
		rebinds     []string
		names       []string
	}{
		{
			name:    "repartition the disk of a mounted kept volume",
			current: volumesManifest(keepVolume), bindings: []generation.VolumeBinding{keepBinding},
			desired: volumesManifest(keepVolume, wipeDisk("data", "data")), keepMounted: true,
			names: []string{"keep", "data"},
		},
		{
			name:    "repartition the disk of an unmounted kept volume",
			current: volumesManifest(keepVolume), bindings: []generation.VolumeBinding{keepBinding},
			desired: volumesManifest(keepVolume, wipeDisk("data", "data")),
			names:   []string{"keep", "data"},
		},
		{
			name:    "format the partition of an unmounted kept volume",
			current: volumesManifest(keepVolume), bindings: []generation.VolumeBinding{keepBinding},
			desired: volumesManifest(keepVolume, partition("data", manifest.PartitionSelector{PartUUID: "keep-part"}, true)),
			names:   []string{"keep", "data"},
		},
		{
			name:    "repartition the disk of a new preserved volume",
			current: volumesManifest(),
			desired: volumesManifest(wipeDisk("b", "spare"), partition("c", manifest.PartitionSelector{PartUUID: "spare-part"}, false)),
			names:   []string{"b", "c"},
		},
		{
			name:    "preserve one partition as two volumes",
			current: volumesManifest(),
			desired: volumesManifest(partition("a", manifest.PartitionSelector{PartUUID: "spare-part"}, false), partition("b", manifest.PartitionSelector{FilesystemUUID: "spare-fs"}, false)),
			names:   []string{"a", "b"},
		},
		{
			// keep moves to vdc1 with rebind authority; its data is still on
			// vdb1, which a failed apply would mount again.
			name:    "format the old partition of a moved volume",
			current: volumesManifest(keepVolume), bindings: []generation.VolumeBinding{keepBinding},
			desired: volumesManifest(partition("keep", manifest.PartitionSelector{PartUUID: "spare-part"}, false), partition("data", manifest.PartitionSelector{PartUUID: "keep-part"}, true)),
			rebinds: []string{"cp-1/keep"},
			names:   []string{"keep", "data"},
		},
		{
			name:    "format the partition of a removed volume",
			current: volumesManifest(keepVolume), bindings: []generation.VolumeBinding{keepBinding},
			desired: volumesManifest(partition("data", manifest.PartitionSelector{PartUUID: "keep-part"}, true)),
			names:   []string{"keep", "data"},
		},
		{
			name:    "repartition the disk of a removed volume",
			current: volumesManifest(keepVolume), bindings: []generation.VolumeBinding{keepBinding},
			desired: volumesManifest(wipeDisk("data", "data")),
			names:   []string{"keep", "data"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &storageRunner{keepMounted: test.keepMounted}

			_, err := (&Executor{Root: t.TempDir(), RunTool: runner.run, volumeDevices: hermeticVolumeDevices(t)}).applyVolumes(context.Background(), test.current, test.desired, test.bindings, "cp-1", nil, test.rebinds)

			if err == nil {
				t.Fatal("applyVolumes() error = nil, want refusal of volumes sharing storage")
			}
			for _, name := range test.names {
				if !strings.Contains(err.Error(), fmt.Sprintf("%q", name)) {
					t.Fatalf("applyVolumes() error = %v, want refusal naming volume %q", err, name)
				}
			}
			if len(runner.commands) != 0 {
				t.Fatalf("commands = %q, want none before the refusal", runner.commands)
			}
		})
	}
}

func TestApplyVolumesChecksEveryVolumeBeforeRewritingAny(t *testing.T) {
	desired := volumesManifest(
		manifest.Volume{Name: "a", Selector: manifest.VolumeSelector{Disk: &manifest.DiskSelector{Serial: "spare"}}, Filesystem: "xfs", Wipe: true},
		manifest.Volume{Name: "b", Selector: manifest.VolumeSelector{Partition: &manifest.PartitionSelector{PartUUID: "data-part"}}, Filesystem: "xfs", Wipe: true},
	)
	// After planning, another disk takes b's disk's kernel name, so b's
	// recheck refuses while a's disk is still the planned one.
	runner := &storageRunner{dataSerial: "replaced"}

	_, err := (&Executor{Root: t.TempDir(), RunTool: runner.run, volumeDevices: hermeticVolumeDevices(t)}).applyVolumes(context.Background(), volumesManifest(), desired, nil, "cp-1", nil, nil)

	if !errors.Is(err, discovery.ErrTargetDiskChanged) {
		t.Fatalf("applyVolumes() error = %v, want ErrTargetDiskChanged", err)
	}
	if writes := runner.writes(); len(writes) != 0 {
		t.Fatalf("commands = %q, want no volume rewritten before every volume is checked", writes)
	}
}

func TestApplyVolumesLeavesRewrittenVolumesUnmounted(t *testing.T) {
	// a is reformatted in place; b only changes how it selects its partition.
	current := volumesManifest(
		manifest.Volume{Name: "a", Selector: manifest.VolumeSelector{Partition: &manifest.PartitionSelector{PartUUID: "spare-part"}}, Filesystem: "xfs"},
		manifest.Volume{Name: "b", Selector: manifest.VolumeSelector{Partition: &manifest.PartitionSelector{PartUUID: "data-part"}}, Filesystem: "ext4"},
	)
	desired := volumesManifest(
		manifest.Volume{Name: "a", Selector: manifest.VolumeSelector{Partition: &manifest.PartitionSelector{PartUUID: "spare-part"}}, Filesystem: "xfs", Wipe: true},
		manifest.Volume{Name: "b", Selector: manifest.VolumeSelector{Partition: &manifest.PartitionSelector{FilesystemUUID: "data-fs"}}, Filesystem: "ext4"},
	)
	bindings := []generation.VolumeBinding{
		{Name: "a", PartitionUUID: "spare-part", FilesystemUUID: "spare-fs"},
		{Name: "b", PartitionUUID: "data-part", FilesystemUUID: "data-fs"},
	}
	// The first destructive command fails, so a counts as rewritten before
	// any of its commands has completed.
	runner := &storageRunner{fail: func(argv []string) bool { return argv[0] == "wipefs" }}

	_, err := (&Executor{Root: t.TempDir(), RunTool: runner.run, volumeDevices: hermeticVolumeDevices(t)}).applyVolumes(context.Background(), current, desired, bindings, "cp-1", nil, nil)

	if err == nil || !strings.Contains(err.Error(), "volumes a ") || !strings.Contains(err.Error(), "unmounted") {
		t.Fatalf("applyVolumes() error = %v, want failure naming a as left unmounted", err)
	}
	if slices.Contains(runner.commands, "systemctl start var-mnt-a.mount") {
		t.Fatalf("commands = %q, want a, whose storage was rewritten, left unmounted", runner.commands)
	}
	if !slices.Contains(runner.commands, "systemctl start var-mnt-b.mount") {
		t.Fatalf("commands = %q, want b, whose storage is intact, mounted again", runner.commands)
	}
}

func TestVolumeRecoveryLeavesVolumesOnRewrittenStorageUnmounted(t *testing.T) {
	// Planning refuses a volume that rewrites another's storage, so this
	// recovery path is reached only if storage moves between planning and
	// execution; it is driven directly. keep is bound to vdb1 (disk sequence
	// 2, partition 1) and spare to vdc1; data was being written.
	bindings := map[string]generation.VolumeBinding{
		"keep":  keepBinding,
		"spare": {Name: "spare", PartitionUUID: "spare-part", FilesystemUUID: "spare-fs"},
	}
	vdb := discovery.DiskIdentity{Path: "/dev/vdb", DiskSeq: 2, SizeBytes: 68719476736}

	for _, test := range []struct {
		name string
		data disk.VolumePlan
	}{
		{"partition formatted", disk.VolumePlan{Name: "data", TargetKind: "partition", Disk: vdb, Partition: disk.PartitionIdentity{Number: 1}, Wipe: true}},
		{"disk repartitioned", disk.VolumePlan{Name: "data", TargetKind: "disk", Disk: vdb, Wipe: true, Repartition: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &storageRunner{}
			recovery := volumeRecovery{
				run: runner.run, source: hermeticVolumeDevices(t).source(runner.run), bindings: bindings,
				prepared: []disk.VolumePlan{test.data}, stopped: []string{"keep", "spare"}, rewritten: []string{"data"},
			}

			err := recovery.fail(context.Background(), errors.New("mkfs.xfs failed"))

			if slices.Contains(runner.commands, "systemctl start var-mnt-keep.mount") {
				t.Fatalf("commands = %q, want keep, whose storage data rewrote, left unmounted", runner.commands)
			}
			if !slices.Contains(runner.commands, "systemctl start var-mnt-spare.mount") {
				t.Fatalf("commands = %q, want spare, whose storage is intact, mounted again", runner.commands)
			}
			if !mentionsAll(err.Error(), "volumes keep were stopped for this apply and left unmounted", "formatted for volumes data", "volumes spare were stopped for this apply and have been mounted again") {
				t.Fatalf("fail() error = %v, want keep named as left unmounted because of data, and spare as mounted again", err)
			}
		})
	}
}

func mentionsAll(message string, parts ...string) bool {
	for _, part := range parts {
		if !strings.Contains(message, part) {
			return false
		}
	}
	return true
}

func TestApplyVolumesRestartsStoppedVolumesAfterRefusal(t *testing.T) {
	current := volumeManifest(manifest.PartitionSelector{PartUUID: "old-part"})
	desired := current
	desired.Install.Volumes = []manifest.Volume{{
		Name: "data", Selector: manifest.VolumeSelector{Disk: &manifest.DiskSelector{Serial: "new"}}, Filesystem: "xfs", Wipe: true,
	}}
	bindings := []generation.VolumeBinding{{Name: "data", PartitionUUID: "old-part", FilesystemUUID: "old-fs"}}

	for _, test := range []struct {
		name string
		// stopStatus is the exit status of systemctl stop; 5 means the
		// mount unit is not loaded.
		stopStatus int
		want       []string
		mounted    bool
	}{
		{"loaded unit", 0, []string{"systemctl stop var-mnt-data.mount", "udevadm settle", "systemctl start var-mnt-data.mount"}, true},
		{"unloaded unit", 5, []string{"systemctl stop var-mnt-data.mount", "udevadm settle"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
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
					if argv[0] == "systemctl" && argv[1] == "stop" {
						return ToolResult{ExitStatus: test.stopStatus}
					}
					return ToolResult{}
				}
			}

			_, err := (&Executor{Root: t.TempDir(), RunTool: runner, volumeDevices: hermeticVolumeDevices(t)}).applyVolumes(context.Background(), current, desired, bindings, "cp-1", nil, []string{"cp-1/data"})

			if !errors.Is(err, discovery.ErrTargetDiskChanged) {
				t.Fatalf("applyVolumes() error = %v, want ErrTargetDiskChanged", err)
			}
			if mounted := strings.Contains(err.Error(), "mounted again"); mounted != test.mounted {
				t.Fatalf("applyVolumes() error = %v; reports data mounted again = %v, want %v", err, mounted, test.mounted)
			}
			if !reflect.DeepEqual(commands, test.want) {
				t.Fatalf("commands = %q, want %q", commands, test.want)
			}
		})
	}
}

func TestVolumeRecoveryLeavesVolumesUnverifiableUnmounted(t *testing.T) {
	// data was being formatted on vdc1 (disk sequence 3, partition 1). keep is
	// bound to vdb1 and other to vdb2, both outside the storage data wrote.
	bindings := map[string]generation.VolumeBinding{
		"keep":  keepBinding,
		"other": {Name: "other", PartitionUUID: "data-part", FilesystemUUID: "data-fs"},
	}
	data := disk.VolumePlan{Name: "data", TargetKind: "partition", Disk: discovery.DiskIdentity{Path: "/dev/vdc", DiskSeq: 3, SizeBytes: 68719476736}, Partition: disk.PartitionIdentity{Number: 1}, Wipe: true}

	for _, test := range []struct {
		name string
		// run wraps the storage fixture's runner.
		run       func(storage *storageRunner) ToolRunner
		stopped   []string
		unmounted []string
		started   []string
		want      []string
	}{
		{
			name: "rediscovery fails",
			run: func(storage *storageRunner) ToolRunner {
				return func(ctx context.Context, argv []string, progress func(int)) ToolResult {
					if argv[0] == "lsblk" {
						return ToolResult{ExitStatus: 1, Stderr: []byte("lsblk failed")}
					}
					return storage.run(ctx, argv, progress)
				}
			},
			stopped:   []string{"keep", "other"},
			unmounted: []string{"keep", "other"},
			want:      []string{"volumes keep, other were stopped for this apply and left unmounted", "could not be checked"},
		},
		{
			// A cloned disk carries keep's PARTUUID, so the binding resolves
			// to two partitions and cannot show that keep's storage is intact.
			name: "binding ambiguous",
			run: func(storage *storageRunner) ToolRunner {
				return func(ctx context.Context, argv []string, progress func(int)) ToolResult {
					result := storage.run(ctx, argv, progress)
					if argv[0] == "lsblk" {
						result.Stdout = bytes.ReplaceAll(result.Stdout, []byte(`"partuuid":"spare-part"`), []byte(`"partuuid":"keep-part"`))
					}
					return result
				}
			},
			stopped:   []string{"keep", "other"},
			unmounted: []string{"keep"},
			started:   []string{"other"},
			want:      []string{"volumes keep were stopped for this apply and left unmounted", "ambiguous", "matched 2 partitions"},
		},
		{
			// The binding resolves nowhere, so its unit has no device to mount.
			name: "binding resolves nowhere",
			run: func(storage *storageRunner) ToolRunner {
				return func(ctx context.Context, argv []string, progress func(int)) ToolResult {
					result := storage.run(ctx, argv, progress)
					if argv[0] == "lsblk" {
						result.Stdout = bytes.ReplaceAll(result.Stdout, []byte(`"partuuid":"keep-part"`), []byte(`"partuuid":"moved-part"`))
					}
					return result
				}
			},
			stopped:   []string{"keep", "other"},
			unmounted: []string{"keep"},
			started:   []string{"other"},
			want:      []string{"volumes keep were stopped for this apply and left unmounted", "matched no partitions"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			storage := &storageRunner{}
			run := test.run(storage)
			recovery := volumeRecovery{
				run: run, source: hermeticVolumeDevices(t).source(run), bindings: bindings,
				prepared: []disk.VolumePlan{data}, stopped: test.stopped, rewritten: []string{"data"},
			}

			err := recovery.fail(context.Background(), errors.New("mkfs.xfs failed"))

			for _, name := range test.unmounted {
				if slices.Contains(storage.commands, "systemctl start var-mnt-"+name+".mount") {
					t.Fatalf("commands = %q, want %s left unmounted", storage.commands, name)
				}
			}
			for _, name := range test.started {
				if !slices.Contains(storage.commands, "systemctl start var-mnt-"+name+".mount") {
					t.Fatalf("commands = %q, want %s, whose storage is verified intact, mounted again", storage.commands, name)
				}
			}
			if !mentionsAll(err.Error(), test.want...) {
				t.Fatalf("fail() error = %v, want it to contain %q", err, test.want)
			}
		})
	}
}
