package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/configapply"
	"github.com/katl-dev/katl/internal/installer/manifest"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// volumeDisksRunner reports a system disk whose state partition is mounted at
// /var and one data partition per given filesystem UUID, all carrying the
// PARTUUID data-part, as a dd clone of the data disk does.
func volumeDisksRunner(filesystemUUIDs ...string) ToolRunner {
	return volumeDisksRunnerWith(nil, filesystemUUIDs...)
}

// volumeDisksRunnerWith also reports the given lsblk device objects.
func volumeDisksRunnerWith(extra []string, filesystemUUIDs ...string) ToolRunner {
	return func(_ context.Context, argv []string, _ func(int)) ToolResult {
		switch argv[0] {
		case "lsblk":
			devices := []string{`{"name":"vda","path":"/dev/vda","type":"disk","serial":"root","size":68719476736,"ro":false,"mountpoints":[],"children":[{"name":"vda5","path":"/dev/vda5","type":"part","size":1073741824,"ro":false,"fstype":"ext4","uuid":"state-fs","partuuid":"state-part","partlabel":"KATL_STATE","mountpoints":["/var"]}]}`}
			for i, fsUUID := range filesystemUUIDs {
				name := string(rune('b' + i))
				devices = append(devices, fmt.Sprintf(`{"name":"vd%[1]s","path":"/dev/vd%[1]s","type":"disk","serial":"data-%[1]s","size":4294967296,"ro":false,"mountpoints":[],"children":[{"name":"vd%[1]s1","path":"/dev/vd%[1]s1","type":"part","size":4293918720,"ro":false,"fstype":"xfs","uuid":%[2]q,"partuuid":"data-part","partlabel":"u-data","mountpoints":[]}]}`, name, fsUUID))
			}
			devices = append(devices, extra...)
			return ToolResult{Stdout: []byte(`{"blockdevices":[` + strings.Join(devices, ",") + `]}`)}
		case "findmnt":
			return ToolResult{Stdout: []byte(`{"filesystems":[{"source":"/dev/vda5","target":"/var","fstype":"ext4","options":"rw"}]}`)}
		case "ip":
			return ToolResult{Stdout: []byte(`[]`)}
		default:
			return ToolResult{Err: fmt.Errorf("unexpected command %q", argv[0]), ExitStatus: 1}
		}
	}
}

// writeVolumeGeneration records a generation that mounts volume data, bound
// to PARTUUID data-part and filesystem data-fs, and keeps a binding for a
// removed volume whose disk is gone.
func writeVolumeGeneration(t *testing.T, root, generationID string, others ...string) {
	t.Helper()
	spec, _, err := generation.ReadGeneration(root, "generation-0")
	if err != nil {
		t.Fatal(err)
	}
	spec.GenerationID = generationID
	spec.Confexts = nil
	spec.Boot.LoaderEntryPath = "loader/entries/katl-" + generationID + ".conf"
	spec.VolumeBindings = []generation.VolumeBinding{
		{Name: "data", PartitionUUID: "data-part", FilesystemUUID: "data-fs"},
		{Name: "removed", PartitionUUID: "removed-part", FilesystemUUID: "removed-fs"},
	}
	for _, name := range others {
		spec.VolumeBindings = append(spec.VolumeBindings, generation.VolumeBinding{Name: name, PartitionUUID: name + "-part", FilesystemUUID: name + "-fs"})
	}
	state, err := generation.NewGenerationStatus(spec, generation.CommitStateCommitted, generation.BootStateGood, generation.HealthStateHealthy, spec.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := generation.WriteGeneration(root, spec, state); err != nil {
		t.Fatal(err)
	}
	mounted := validVolumeStatusManifest()
	mounted.Install.Volumes = []manifest.Volume{{
		Name: "data", Selector: manifest.VolumeSelector{Disk: &manifest.DiskSelector{ByID: "/dev/disk/by-id/virtio-katl-data"}}, Filesystem: "xfs",
	}}
	for _, name := range others {
		mounted.Install.Volumes = append(mounted.Install.Volumes, manifest.Volume{
			Name: name, Selector: manifest.VolumeSelector{Disk: &manifest.DiskSelector{ByID: "/dev/disk/by-id/virtio-katl-" + name}}, Filesystem: "xfs",
		})
	}
	if err := configapply.WriteGenerationManifest(root, generationID, mounted); err != nil {
		t.Fatal(err)
	}
}

// shortVolumeAttachWait keeps the check's wait for absent disks short.
func shortVolumeAttachWait(t *testing.T, wait time.Duration) {
	t.Helper()
	restoreWait, restoreInterval := volumeAttachWait, volumeAttachInterval
	t.Cleanup(func() { volumeAttachWait, volumeAttachInterval = restoreWait, restoreInterval })
	volumeAttachWait, volumeAttachInterval = wait, 10*time.Millisecond
}

// linkActiveGeneration links the generated confext to a generation, as
// activation does.
func linkActiveGeneration(t *testing.T, root, generationID string) {
	t.Helper()
	link := filepath.Join(root, "run/confexts", generation.GeneratedConfextName)
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/var/lib/katl/generations/"+generationID+"/confext", link); err != nil {
		t.Fatal(err)
	}
}

func TestCheckActiveVolumesWaitsForLateDisk(t *testing.T) {
	shortVolumeAttachWait(t, 5*time.Second)
	server := newTestServer(t)
	writeCleanGenerationZeroState(t, server.Root)
	writeVolumeGeneration(t, server.Root, "generation-1")
	linkActiveGeneration(t, server.Root, "generation-1")
	var discoveries atomic.Int32
	attached, detached := volumeDisksRunner("data-fs"), volumeDisksRunner()
	runner := func(ctx context.Context, argv []string, started func(int)) ToolResult {
		if argv[0] == "lsblk" && discoveries.Add(1) < 3 {
			return detached(ctx, argv, started)
		}
		return attached(ctx, argv, started)
	}

	err := checkActiveVolumes(context.Background(), hermeticVolumeDevices(t), runner, server.Root)

	if err != nil || discoveries.Load() < 3 {
		t.Fatalf("checkActiveVolumes() = %v after %d discoveries, want the disk found once it attached", err, discoveries.Load())
	}
}

// A refusal that waiting cannot change, such as a clone, ends the wait for an
// absent disk at once.
func TestCheckActiveVolumesRefusesCloneWithoutWaiting(t *testing.T) {
	shortVolumeAttachWait(t, time.Hour)
	server := newTestServer(t)
	writeCleanGenerationZeroState(t, server.Root)
	writeVolumeGeneration(t, server.Root, "generation-1", "logs")
	linkActiveGeneration(t, server.Root, "generation-1")
	done := make(chan error, 1)

	go func() {
		done <- checkActiveVolumes(context.Background(), hermeticVolumeDevices(t), volumeDisksRunner("data-fs", "data-fs"), server.Root)
	}()

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "Detach the copy") || !strings.Contains(err.Error(), `volume "logs"`) {
			t.Fatalf("checkActiveVolumes() error = %v, want both the clone and the absent volume named", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("checkActiveVolumes() kept waiting for an absent disk beside a clone")
	}
}

func TestCheckActiveVolumes(t *testing.T) {
	shortVolumeAttachWait(t, 0)
	for _, test := range []struct {
		name    string
		disks   []string
		mention []string
	}{
		{name: "bound disk", disks: []string{"data-fs"}},
		{name: "clone beside the original", disks: []string{"data-fs", "data-fs"}, mention: []string{"generation-1", `volume "data"`, "/dev/vdb1, /dev/vdc1", "Do not wipe either disk", "sgdisk --randomize-guids"}},
		{name: "filesystem recreated", disks: []string{"new-fs"}, mention: []string{`volume "data"`, "data-fs", "new-fs", "--rebind-volume node-1/data"}},
		{name: "disk detached", mention: []string{`volume "data"`, "data-part", "Attach the disk"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := newTestServer(t)
			writeCleanGenerationZeroState(t, server.Root)
			writeVolumeGeneration(t, server.Root, "generation-1")
			link := filepath.Join(server.Root, "run/confexts", generation.GeneratedConfextName)
			if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("/var/lib/katl/generations/generation-1/confext", link); err != nil {
				t.Fatal(err)
			}

			err := checkActiveVolumes(context.Background(), hermeticVolumeDevices(t), volumeDisksRunner(test.disks...), server.Root)

			if test.mention == nil {
				if err != nil {
					t.Fatalf("checkActiveVolumes() error = %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("checkActiveVolumes() error = nil, want the unresolved volume")
			}
			for _, part := range test.mention {
				if !strings.Contains(err.Error(), part) {
					t.Fatalf("checkActiveVolumes() error = %v, want it to name %q", err, part)
				}
			}
			if strings.Contains(err.Error(), "removed") {
				t.Fatalf("checkActiveVolumes() error = %v, want the removed volume's retained binding ignored", err)
			}
		})
	}
}

func TestSelectGenerationRefusesUnresolvedVolumes(t *testing.T) {
	server := newTestServer(t)
	writeCleanGenerationZeroState(t, server.Root)
	writeVolumeGeneration(t, server.Root, "before-replacement")
	before, err := generation.ReadBootSelection(server.Root)
	if err != nil {
		t.Fatal(err)
	}
	server.volumeDevices = hermeticVolumeDevices(t)
	// The volume was reformatted after the target generation was recorded.
	server.RunVolumeDiscovery = volumeDisksRunner("replacement-fs")
	server.MountBootRoot = func(context.Context, string) error { t.Fatal("refused selection reached the boot entries"); return nil }
	request := &agentapi.GenerationMutationRequest{GenerationId: "before-replacement", ExpectedEnrollmentId: testEnrollmentID, ExpectedInventoryNodeName: testInventoryNodeName, ExpectedMachineId: testMachineID, ExpectedCurrentGenerationId: "generation-0"}

	_, err = server.SelectGeneration(context.Background(), request)

	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), `volume "data"`) || !strings.Contains(err.Error(), "replacement-fs") || !strings.Contains(err.Error(), "select a generation recorded after that change") {
		t.Fatalf("SelectGeneration() error = %v, want a refusal naming the reformatted volume", err)
	}
	after, err := generation.ReadBootSelection(server.Root)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("boot selection = %+v, want unchanged %+v", after, before)
	}

	// With the disk detached, the remedy is to attach it, not to pick a newer
	// generation, which does not resolve either.
	server.RunVolumeDiscovery = volumeDisksRunner()
	_, err = server.SelectGeneration(context.Background(), request)
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "Attach the disk") {
		t.Fatalf("SelectGeneration() with the disk detached = %v, want a refusal saying to attach it", err)
	}
}

func TestNodeVolumeStatusExplainsUnresolvedBinding(t *testing.T) {
	root := t.TempDir()
	writeCleanGenerationZeroState(t, root)
	writeVolumeGeneration(t, root, "generation-1")
	runner := func(_ context.Context, argv []string, _ func(int)) ToolResult {
		switch argv[0] {
		case "systemctl":
			return ToolResult{Stdout: []byte("LoadState=loaded\nActiveState=inactive\nSubState=dead\nResult=success\n")}
		default:
			return ToolResult{}
		}
	}

	status, err := nodeVolumeStatus(context.Background(), root, "generation-1", runner, hermeticVolumeDevices(t), volumeDisksRunner("data-fs", "data-fs"))

	if err != nil {
		t.Fatalf("nodeVolumeStatus() error = %v", err)
	}
	if len(status) != 1 || !strings.Contains(status[0].GetFailureDiagnostic(), "/dev/vdb1, /dev/vdc1") || !strings.Contains(status[0].GetFailureDiagnostic(), "Detach the copy") {
		t.Fatalf("volume status = %#v, want the clone explained", status)
	}
}

func TestVolumeTransitionRequiresRebindForRecreatedFilesystem(t *testing.T) {
	config := volumeManifest(manifest.PartitionSelector{PartUUID: "data-part"})
	bound := []generation.VolumeBinding{{Name: "data", PartitionUUID: "data-part", FilesystemUUID: "data-fs"}}

	plan, err := planVolumeTransition(context.Background(), hermeticVolumeDevices(t), volumeDisksRunner("new-fs"), config, config, bound, "cp-1", nil, nil)

	var authority *VolumeRebindAuthorityError
	if !errors.As(err, &authority) || !slices.Equal(plan.requiredRebinds, []string{"cp-1/data"}) {
		t.Fatalf("plan = %#v error = %v, want rebind authority for the recreated filesystem", plan, err)
	}
}

func TestNodeVolumeStatusExplainsHeldBackVolume(t *testing.T) {
	root := t.TempDir()
	writeCleanGenerationZeroState(t, root)
	writeVolumeGeneration(t, root, "generation-1", "logs")
	logs := `{"name":"vdd","path":"/dev/vdd","type":"disk","serial":"logs","size":4294967296,"ro":false,"mountpoints":[],"children":[{"name":"vdd1","path":"/dev/vdd1","type":"part","size":4293918720,"ro":false,"fstype":"xfs","uuid":"logs-fs","partuuid":"logs-part","mountpoints":[]}]}`
	devices := volumeDisksRunnerWith([]string{logs}, "data-fs", "data-fs")
	units := func(context.Context, []string, func(int)) ToolResult {
		return ToolResult{Stdout: []byte("LoadState=loaded\nActiveState=inactive\nSubState=dead\nResult=success\n")}
	}

	status, err := nodeVolumeStatus(context.Background(), root, "generation-1", units, hermeticVolumeDevices(t), devices)

	if err != nil {
		t.Fatalf("nodeVolumeStatus() error = %v", err)
	}
	diagnostics := map[string]string{}
	for _, volume := range status {
		diagnostics[volume.GetName()] = volume.GetFailureDiagnostic()
	}
	if !strings.Contains(diagnostics["data"], "Detach the copy") {
		t.Fatalf("data diagnostic = %q, want the clone explained", diagnostics["data"])
	}
	if !strings.Contains(diagnostics["logs"], "Held back") || !strings.Contains(diagnostics["logs"], "data") {
		t.Fatalf("logs diagnostic = %q, want it held back by data", diagnostics["logs"])
	}
}
