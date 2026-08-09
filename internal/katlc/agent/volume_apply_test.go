package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/installer/discovery"
	"github.com/katl-dev/katl/internal/installer/disk"
	"github.com/katl-dev/katl/internal/installer/generation"
	"github.com/katl-dev/katl/internal/installer/manifest"
)

func TestPrepareLiveVolumeUsesBoundedSystemdRepartDefinition(t *testing.T) {
	root := t.TempDir()
	var calls [][]string
	runner := func(_ context.Context, argv []string, _ func(int)) ToolResult {
		calls = append(calls, append([]string(nil), argv...))
		if argv[0] == "systemd-repart" {
			var definitions string
			for _, arg := range argv {
				if strings.HasPrefix(arg, "--definitions=") {
					definitions = strings.TrimPrefix(arg, "--definitions=")
				}
			}
			data, err := os.ReadFile(filepath.Join(definitions, "50-katl-volume.conf"))
			if err != nil {
				t.Fatalf("read repart definition: %v", err)
			}
			content := string(data)
			for _, want := range []string{"Type=11111111-2222-5333-8444-555555555555", "Label=u-data", "Format=xfs"} {
				if !strings.Contains(content, want) {
					t.Fatalf("repart definition missing %q:\n%s", want, content)
				}
			}
		}
		return ToolResult{}
	}
	err := prepareLiveVolume(context.Background(), runner, root, disk.VolumePlan{
		Name: "data", DevicePath: "/dev/vdb", Filesystem: "xfs", MountPath: "/var/mnt/data",
		Wipe: true, Repartition: true, TypeUUID: "11111111-2222-5333-8444-555555555555",
	})
	if err != nil {
		t.Fatalf("prepareLiveVolume() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "var/mnt/data")); err != nil {
		t.Fatalf("volume mount point: %v", err)
	}
	if got := strings.Join(calls[0], " "); !strings.Contains(got, "systemd-repart --dry-run=no --empty=force") || !strings.HasSuffix(got, " /dev/vdb") {
		t.Fatalf("repart argv = %q", got)
	}
	if got := strings.Join(calls[1], " "); got != "udevadm settle" {
		t.Fatalf("settle argv = %q", got)
	}
}

func TestPrepareLiveVolumePreservesExistingPartition(t *testing.T) {
	called := false
	runner := func(context.Context, []string, func(int)) ToolResult {
		called = true
		return ToolResult{}
	}
	if err := prepareLiveVolume(context.Background(), runner, t.TempDir(), disk.VolumePlan{
		Name: "data", DevicePath: "/dev/vdb1", Filesystem: "xfs", MountPath: "/var/mnt/data",
	}); err != nil {
		t.Fatalf("prepareLiveVolume() error = %v", err)
	}
	if called {
		t.Fatal("preserved partition ran a destructive tool")
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
    {"name":"vda","path":"/dev/vda","type":"disk","serial":"root","size":68719476736,"ro":false,"mountpoints":[]},
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
	_, err := (&Executor{RunTool: runner}).applyVolumes(context.Background(), current, desired, []generation.VolumeBinding{{Name: "data", PartitionUUID: "current"}}, "node-a", nil, nil)
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
	bindings, err := (&Executor{RunTool: runner}).applyVolumes(context.Background(), current, desired, []generation.VolumeBinding{{Name: "data", PartitionUUID: "keep-me"}}, "cp-1", nil, nil)
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

func TestDestructiveStorageAuthorityUsesDiscoveredTargetState(t *testing.T) {
	current := manifest.Manifest{Install: manifest.InstallConfig{TargetDisk: manifest.DiskSelector{Serial: "root"}}}
	desired := current
	desired.Install.Volumes = []manifest.Volume{{
		Name: "data", Selector: manifest.VolumeSelector{Disk: &manifest.DiskSelector{Serial: "data"}}, Filesystem: "xfs", Wipe: true,
	}}

	for _, test := range []struct {
		name             string
		partitionTable   string
		acknowledgements []string
		wantRequired     []string
		wantError        bool
	}{
		{name: "blank target is automatic"},
		{name: "non-blank target is refused", partitionTable: "gpt", wantRequired: []string{"cp-1/data"}, wantError: true},
		{name: "named non-blank target is authorized", partitionTable: "gpt", acknowledgements: []string{"cp-1/data"}, wantRequired: []string{"cp-1/data"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := volumeAuthorityRunner(test.partitionTable, nil)
			server := newTestServer(t)
			server.RunVolumeDiscovery = runner
			plan, err := server.validateVolumeTransition(context.Background(), "cp-1", current, desired, nil, test.acknowledgements, nil)
			if !slices.Equal(plan.requiredWipeAcknowledgements, test.wantRequired) {
				t.Fatalf("required acknowledgements = %v, want %v", plan.requiredWipeAcknowledgements, test.wantRequired)
			}
			var authority *disk.DestructiveVolumeAuthorityError
			if errors.As(err, &authority) != test.wantError {
				t.Fatalf("authority error = %v, want error %t", err, test.wantError)
			}
		})
	}
}

func TestApplyVolumesRefusesNonBlankTargetBeforeMutation(t *testing.T) {
	current := manifest.Manifest{Install: manifest.InstallConfig{TargetDisk: manifest.DiskSelector{Serial: "root"}}}
	desired := current
	desired.Install.Volumes = []manifest.Volume{{
		Name: "data", Selector: manifest.VolumeSelector{Disk: &manifest.DiskSelector{Serial: "data"}}, Filesystem: "xfs", Wipe: true,
	}}
	var mutations [][]string
	runner := volumeAuthorityRunner("gpt", &mutations)
	_, err := (&Executor{RunTool: runner}).applyVolumes(context.Background(), current, desired, nil, "cp-1", nil, nil)
	var authority *disk.DestructiveVolumeAuthorityError
	if !errors.As(err, &authority) || !reflect.DeepEqual(authority.Required, []string{"cp-1/data"}) {
		t.Fatalf("applyVolumes() error = %#v", err)
	}
	if len(mutations) != 0 {
		t.Fatalf("destructive storage refusal ran mutating tools: %v", mutations)
	}
}

func TestApplyVolumesMutatesNonBlankTargetOnlyWithNamedAuthority(t *testing.T) {
	current := manifest.Manifest{Install: manifest.InstallConfig{TargetDisk: manifest.DiskSelector{Serial: "root"}}}
	desired := current
	desired.Install.Volumes = []manifest.Volume{{
		Name: "data", Selector: manifest.VolumeSelector{Disk: &manifest.DiskSelector{Serial: "data"}}, Filesystem: "xfs", Wipe: true,
	}}
	var mutations [][]string
	runner := volumeAuthorityRunner("gpt", &mutations)
	_, err := (&Executor{Root: t.TempDir(), RunTool: runner}).applyVolumes(context.Background(), current, desired, nil, "cp-1", []string{"cp-1/data"}, nil)
	if err != nil {
		t.Fatalf("applyVolumes() error = %v", err)
	}
	if len(mutations) != 2 || mutations[0][0] != "systemd-repart" || mutations[1][0] != "udevadm" {
		t.Fatalf("acknowledged volume mutations = %v", mutations)
	}
}

func TestVolumeTransitionPreservesBoundDeviceWhenLabelBecomesAmbiguous(t *testing.T) {
	current := volumeManifest(manifest.PartitionSelector{})
	plan, err := planVolumeTransition(context.Background(), duplicateLabelVolumeRunner(), current, current,
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
	_, err := planVolumeTransition(context.Background(), duplicateLabelVolumeRunner(), current, current, nil, "cp-1", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "partition selector matched 2 partitions") {
		t.Fatalf("planVolumeTransition() error = %v, want ambiguous selector", err)
	}
}

func TestVolumeTransitionRequiresExplicitRebindForReplacement(t *testing.T) {
	current := volumeManifest(manifest.PartitionSelector{PartUUID: "old-part"})
	desired := volumeManifest(manifest.PartitionSelector{PartUUID: "new-part"})
	currentBinding := []generation.VolumeBinding{{Name: "data", PartitionUUID: "old-part", FilesystemUUID: "old-fs"}}

	plan, err := planVolumeTransition(context.Background(), duplicateLabelVolumeRunner(), current, desired, currentBinding, "cp-1", nil, nil)
	var authority *VolumeRebindAuthorityError
	if !errors.As(err, &authority) || !reflect.DeepEqual(plan.requiredRebinds, []string{"cp-1/data"}) {
		t.Fatalf("unapproved replacement plan = %#v error = %v", plan, err)
	}
	plan, err = planVolumeTransition(context.Background(), duplicateLabelVolumeRunner(), current, desired, currentBinding, "cp-1", nil, []string{"cp-1/data"})
	if err != nil {
		t.Fatalf("approved plan error = %v", err)
	}
	if len(plan.prepare) != 1 || plan.prepare[0].MountSource != "PARTUUID=new-part" {
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
{"name":"vda","path":"/dev/vda","type":"disk","serial":"root","size":68719476736,"ro":false,"mountpoints":[]},
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
				`{"name":"vda","path":"/dev/vda","type":"disk","serial":"root","size":68719476736,"ro":false,"mountpoints":[]},` +
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
	plan, err := planVolumeTransition(context.Background(), func(context.Context, []string, func(int)) ToolResult {
		t.Fatal("zero-volume tombstone retention must not inspect hardware")
		return ToolResult{}
	}, manifest.Manifest{}, manifest.Manifest{}, bindings, "cp-1", nil, nil)
	if err != nil {
		t.Fatalf("planVolumeTransition() error = %v", err)
	}
	if !reflect.DeepEqual(plan.bindings, bindings) {
		t.Fatalf("bindings = %#v, want retained tombstone %#v", plan.bindings, bindings)
	}
}
