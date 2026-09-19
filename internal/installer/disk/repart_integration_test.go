package disk

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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
	if err := os.WriteFile(filepath.Join(definitions, "50-katl-volume.conf"), []byte(volumeDefinition(plan)), 0o600); err != nil {
		t.Fatalf("write repart definition: %v", err)
	}

	output, err := exec.Command(
		"systemd-repart",
		"--dry-run=no",
		"--discard=no",
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

func TestSystemRepartLayout(t *testing.T) {
	if os.Getenv("KATL_VERIFY_REPART") != "1" {
		t.Skip("set KATL_VERIFY_REPART=1 to run systemd-repart")
	}
	dir := t.TempDir()
	image := filepath.Join(dir, "system.raw")
	file, err := os.Create(image)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(4 << 30); err != nil {
		t.Fatal(err)
	}
	file.Close()
	plan := executorPlan()
	plan.TargetDiskPath = image
	operation := findOp(BuildDiskOperations(plan, ""), "create-gpt")
	operation.Args = append([]string{"--offline=yes"}, operation.Args...)
	if err := runRepartOperation(context.Background(), repartRunner{}, operation); err != nil {
		t.Fatal(err)
	}

	output, err := exec.Command("sfdisk", "--json", image).Output()
	if err != nil {
		t.Fatal(err)
	}
	var table struct {
		PartitionTable struct {
			SectorSize uint64 `json:"sectorsize"`
			Partitions []struct {
				Name  string `json:"name"`
				Start uint64 `json:"start"`
				Size  uint64 `json:"size"`
			} `json:"partitions"`
		} `json:"partitiontable"`
	}
	if err := json.Unmarshal(output, &table); err != nil {
		t.Fatal(err)
	}
	parts := table.PartitionTable.Partitions
	if len(parts) != 4 {
		t.Fatalf("partitions = %+v", parts)
	}
	for i, want := range []struct {
		name, filesystem string
		size             uint64
	}{
		{"KATL_ESP", "vfat", 512 << 20},
		{"KATL_ROOT_A", "", 1 << 30},
		{"KATL_ROOT_B", "", 1 << 30},
		{"KATL_STATE", "ext4", 0},
	} {
		part := parts[i]
		if part.Name != want.name || (want.size != 0 && part.Size*table.PartitionTable.SectorSize != want.size) {
			t.Fatalf("partition %d = %+v", i, part)
		}
		output, err := exec.Command("blkid", "-p", "-O", strconv.FormatUint(part.Start*table.PartitionTable.SectorSize, 10), "-S", strconv.FormatUint(part.Size*table.PartitionTable.SectorSize, 10), "-s", "TYPE", "-o", "value", image).CombinedOutput()
		if want.filesystem == "" {
			if err == nil || len(output) != 0 {
				t.Fatalf("root slot was formatted: output=%q err=%v", output, err)
			}
		} else if err != nil || strings.TrimSpace(string(output)) != want.filesystem {
			t.Fatalf("filesystem = %q, error=%v", output, err)
		}
	}
}

type repartRunner struct{}

func (repartRunner) Run(ctx context.Context, name string, args ...string) error {
	output, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, output)
	}
	return nil
}
