package disk

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestVolumeRepartDefinitionCreatesPartition(t *testing.T) {
	if os.Getenv("KATL_VERIFY_REPART") != "1" {
		t.Skip("set KATL_VERIFY_REPART=1 to run systemd-repart")
	}
	if _, err := exec.LookPath("systemd-repart"); err != nil {
		t.Skip("systemd-repart not available")
	}
	if _, err := exec.LookPath("sfdisk"); err != nil {
		t.Skip("sfdisk not available")
	}
	if _, err := exec.LookPath("mkfs.ext4"); err != nil {
		t.Skip("mkfs.ext4 not available")
	}

	dir := t.TempDir()
	image := filepath.Join(dir, "volume.raw")
	file, err := os.OpenFile(image, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("create image: %v", err)
	}
	if err := file.Truncate(64 * 1024 * 1024); err != nil {
		_ = file.Close()
		t.Fatalf("create image: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close image: %v", err)
	}
	definitions := filepath.Join(dir, "repart.d")
	if err := os.Mkdir(definitions, 0o755); err != nil {
		t.Fatalf("create definitions directory: %v", err)
	}
	plan := VolumePlan{
		Name:       "data",
		Filesystem: "ext4",
		TypeUUID:   volumePartitionTypeUUID("data"),
	}
	if err := os.WriteFile(filepath.Join(definitions, "50-katl-volume.conf"), []byte(RepartDefinition(plan)), 0o600); err != nil {
		t.Fatalf("write repart definition: %v", err)
	}

	output, err := exec.Command(
		"systemd-repart",
		"--dry-run=no",
		"--empty=force",
		"--offline=yes",
		"--definitions="+definitions,
		image,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("systemd-repart failed: %v\n%s", err, output)
	}

	output, err = exec.Command("sfdisk", "--json", image).Output()
	if err != nil {
		t.Fatalf("inspect partition table: %v", err)
	}
	var table struct {
		PartitionTable struct {
			Partitions []struct {
				Name string `json:"name"`
			} `json:"partitions"`
		} `json:"partitiontable"`
	}
	if err := json.Unmarshal(output, &table); err != nil {
		t.Fatalf("decode partition table: %v", err)
	}
	if len(table.PartitionTable.Partitions) != 1 || table.PartitionTable.Partitions[0].Name != "u-data" {
		t.Fatalf("partitions = %#v, want one u-data partition", table.PartitionTable.Partitions)
	}
}
