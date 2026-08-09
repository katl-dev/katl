package configapply

import (
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/installer/generation"
	"github.com/katl-dev/katl/internal/installer/manifest"
)

func TestVolumeMountNativeEtcFilesUseConventionPathsAndStableSources(t *testing.T) {
	files, err := volumeMountNativeEtcFiles([]manifest.Volume{
		{Name: "data", Selector: manifest.VolumeSelector{Disk: &manifest.DiskSelector{ByID: "/dev/disk/by-id/data"}}, Filesystem: "xfs"},
		{Name: "local-hostpath", Selector: manifest.VolumeSelector{Partition: &manifest.PartitionSelector{}}, Filesystem: "xfs"},
	}, []generation.VolumeBinding{
		{Name: "data", PartitionUUID: "data-partuuid", FilesystemUUID: "data-fsuuid"},
		{Name: "local-hostpath", FilesystemUUID: "hostpath-fsuuid"},
		{Name: "removed", PartitionUUID: "removed-partuuid"},
	})
	if err != nil {
		t.Fatalf("volumeMountNativeEtcFiles() error = %v", err)
	}
	content := make(map[string]string, len(files))
	for _, file := range files {
		content[file.Path] = file.Content
	}
	for path, want := range map[string]string{
		"/etc/systemd/system/var-mnt-data.mount":               "What=PARTUUID=data-partuuid\nWhere=/var/mnt/data",
		"/etc/systemd/system/var-mnt-local\\x2dhostpath.mount": "What=UUID=hostpath-fsuuid\nWhere=/var/mnt/local-hostpath",
	} {
		if !strings.Contains(content[path], want) {
			t.Fatalf("%s missing %q:\n%s", path, want, content[path])
		}
	}
	if !strings.Contains(content["/etc/systemd/system/katl-volumes.target.d/50-mounts.conf"], "Requires=var-mnt-data.mount var-mnt-local\\x2dhostpath.mount") {
		t.Fatalf("volume target drop-in = %q", content["/etc/systemd/system/katl-volumes.target.d/50-mounts.conf"])
	}
}

func TestVolumeMountNativeEtcFilesRequiresExactBindingSet(t *testing.T) {
	volumes := []manifest.Volume{{Name: "data", Filesystem: "xfs"}}
	for _, test := range []struct {
		name     string
		bindings []generation.VolumeBinding
		want     string
	}{
		{name: "missing", want: `volume "data" has no generation-owned device binding`},
		{name: "empty identity", bindings: []generation.VolumeBinding{{Name: "data"}}, want: `has no partition or filesystem UUID`},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := volumeMountNativeEtcFiles(volumes, test.bindings)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("volumeMountNativeEtcFiles() error = %v, want %q", err, test.want)
			}
		})
	}
}
