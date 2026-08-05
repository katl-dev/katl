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
	err := (&Executor{RunTool: runner}).applyVolumes(context.Background(), current, desired, "node-a", nil)
	if err == nil || !strings.Contains(err.Error(), "partition selector matched no partitions") {
		t.Fatalf("applyVolumes() error = %v, want missing partition", err)
	}
	if systemctlCalled {
		t.Fatal("applyVolumes() stopped the existing mount before preflight completed")
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
			required, err := server.validateDestructiveStorageAuthority(context.Background(), "cp-1", current, desired, test.acknowledgements)
			if !slices.Equal(required, test.wantRequired) {
				t.Fatalf("required acknowledgements = %v, want %v", required, test.wantRequired)
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
	err := (&Executor{RunTool: runner}).applyVolumes(context.Background(), current, desired, "cp-1", nil)
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
	err := (&Executor{Root: t.TempDir(), RunTool: runner}).applyVolumes(context.Background(), current, desired, "cp-1", []string{"cp-1/data"})
	if err != nil {
		t.Fatalf("applyVolumes() error = %v", err)
	}
	if len(mutations) != 2 || mutations[0][0] != "systemd-repart" || mutations[1][0] != "udevadm" {
		t.Fatalf("acknowledged volume mutations = %v", mutations)
	}
}

func volumeAuthorityRunner(partitionTable string, mutations *[][]string) ToolRunner {
	return func(_ context.Context, argv []string, _ func(int)) ToolResult {
		switch argv[0] {
		case "lsblk":
			pttype := ""
			if partitionTable != "" {
				pttype = `,"pttype":"` + partitionTable + `"`
			}
			return ToolResult{Stdout: []byte(`{"blockdevices":[` +
				`{"name":"vda","path":"/dev/vda","type":"disk","serial":"root","size":68719476736,"ro":false,"mountpoints":[]},` +
				`{"name":"vdb","path":"/dev/vdb","type":"disk","serial":"data","size":68719476736,"ro":false,"mountpoints":[]` + pttype + `}` +
				`]}`)}
		case "findmnt":
			return ToolResult{Stdout: []byte(`{"filesystems":[]}`)}
		case "ip":
			return ToolResult{Stdout: []byte(`[]`)}
		default:
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
