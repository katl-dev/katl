package configbundle

import (
	"strings"
	"testing"
)

func TestDiffNodeResolutionsRefusesDifferentNodeNames(t *testing.T) {
	_, err := DiffNodeResolutions(
		NodeResolution{Node: "cp-1"},
		NodeResolution{Node: "cp-2"},
	)
	if err == nil || !strings.Contains(err.Error(), `different nodes "cp-1" and "cp-2"`) || !strings.Contains(err.Error(), "node renames require an explicit lifecycle operation") {
		t.Fatalf("DiffNodeResolutions() error = %v, want lifecycle refusal", err)
	}
}

func TestDiffNodeResolutionsExplainsNonDestructiveStorageRemoval(t *testing.T) {
	volume := SourceStorageVolume{Name: "data"}
	before := NodeResolution{Node: "cp-1", Effective: SourceNode{Storage: SourceStorageLayer{Volumes: supplied([]SourceStorageVolume{volume})}}}
	after := NodeResolution{Node: "cp-1", Effective: SourceNode{Storage: SourceStorageLayer{Volumes: supplied([]SourceStorageVolume{})}}}
	diff, err := DiffNodeResolutions(before, after)
	if err != nil {
		t.Fatal(err)
	}
	if len(diff.Changes) != 1 || diff.Changes[0].Path != `spec.nodes["cp-1"].storage.volumes["data"]` ||
		diff.Changes[0].Classification != "online-applicable" ||
		!strings.Contains(diff.Changes[0].Message, "partition, filesystem, and data are preserved") {
		t.Fatalf("storage removal diff = %#v", diff)
	}
}

func TestDerivedVolumesExposeVolumeNamePartitionConvention(t *testing.T) {
	storage := SourceStorageLayer{Volumes: supplied([]SourceStorageVolume{{
		Name: "local-hostpath",
		Selector: &SourceVolumeSelector{Partition: &SourcePartitionSelector{
			ByVolumeName: supplied(true),
		}},
	}})}
	volumes, warnings := derivedVolumesAndWarnings(storage, `spec.nodes["cp-1"]`)
	if len(volumes) != 1 || volumes[0].PartitionLabel != "u-local-hostpath" || volumes[0].MountSource != "/dev/disk/by-partlabel/u-local-hostpath" {
		t.Fatalf("derived volumes = %#v", volumes)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %#v, want explicit convention without warning", warnings)
	}
}
