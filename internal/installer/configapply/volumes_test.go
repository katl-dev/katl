package configapply

import (
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/installer/manifest"
)

func TestVolumeMountNativeEtcFilesUseConventionPathsAndStableSources(t *testing.T) {
	files, err := volumeMountNativeEtcFiles([]manifest.Volume{
		{Name: "data", Selector: manifest.VolumeSelector{Disk: &manifest.DiskSelector{ByID: "/dev/disk/by-id/data"}}, Filesystem: "xfs"},
		{Name: "local-hostpath", Selector: manifest.VolumeSelector{Partition: &manifest.PartitionSelector{}}, Filesystem: "xfs"},
	})
	if err != nil {
		t.Fatalf("volumeMountNativeEtcFiles() error = %v", err)
	}
	content := make(map[string]string, len(files))
	for _, file := range files {
		content[file.Path] = file.Content
	}
	for path, want := range map[string]string{
		"/etc/systemd/system/var-mnt-data.mount":               "What=/dev/disk/by-partlabel/u-data\nWhere=/var/mnt/data",
		"/etc/systemd/system/var-mnt-local\\x2dhostpath.mount": "What=/dev/disk/by-partlabel/u-local-hostpath\nWhere=/var/mnt/local-hostpath",
	} {
		if !strings.Contains(content[path], want) {
			t.Fatalf("%s missing %q:\n%s", path, want, content[path])
		}
	}
	if !strings.Contains(content["/etc/systemd/system/katl-volumes.target.d/50-mounts.conf"], "Requires=var-mnt-data.mount var-mnt-local\\x2dhostpath.mount") {
		t.Fatalf("volume target drop-in = %q", content["/etc/systemd/system/katl-volumes.target.d/50-mounts.conf"])
	}
}
