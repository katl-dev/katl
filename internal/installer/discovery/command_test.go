package discovery

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCommandDiscoverySourceCollectsReadOnlyFacts(t *testing.T) {
	runner := &fixtureOutputRunner{
		outputs: map[string][]byte{
			"lsblk": []byte(`{
  "blockdevices": [
    {
      "name": "nvme0n1",
      "path": "/dev/nvme0n1",
      "type": "disk",
      "size": 68719476736,
      "ro": false,
      "model": "KATL_TEST_DISK",
      "serial": "root-serial",
      "wwn": "0x5000katlroot",
      "fstype": null,
      "pttype": "gpt",
      "parttype": null,
      "mountpoints": [],
      "children": [
        {
          "name": "nvme0n1p1",
          "path": "/dev/nvme0n1p1",
          "type": "part",
          "size": 1073741824,
          "ro": false,
          "model": null,
          "serial": null,
          "wwn": null,
          "fstype": "vfat",
          "uuid": "esp-fs-uuid",
          "pttype": null,
          "parttype": "esp",
          "partuuid": "esp-part-uuid",
          "partlabel": "KATL_ESP",
          "mountpoints": ["/boot"]
        }
      ]
    }
  ]
}`),
			"findmnt": []byte(`{
  "filesystems": [
    {
      "source": "/dev/nvme0n1p1",
      "target": "/boot",
      "fstype": "vfat",
      "options": "rw,nosuid,nodev",
      "children": [
        {
          "source": "/dev/sdb",
          "target": "/boot/data",
          "fstype": "ext4",
          "options": "rw"
        }
      ]
    }
  ]
}`),
			"ip": []byte(`[
  {
    "ifname": "lo",
    "address": "00:00:00:00:00:00",
    "link_type": "loopback",
    "operstate": "UNKNOWN"
  },
  {
    "ifname": "eno1",
    "address": "52:54:00:12:34:56",
    "link_type": "ether",
    "operstate": "UP"
  }
]`),
		},
	}

	facts, err := (CommandDiscoverySource{
		Commands:    runner,
		ByIDDir:     filepath.Join(t.TempDir(), "missing-by-id"),
		ByPathDir:   filepath.Join(t.TempDir(), "missing-by-path"),
		SysBlockDir: filepath.Join(t.TempDir(), "missing-sys"),
	}).Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}

	if len(facts.BlockDevices) != 1 {
		t.Fatalf("block device count = %d, want 1", len(facts.BlockDevices))
	}
	disk := facts.BlockDevices[0]
	if disk.Path != "/dev/nvme0n1" || disk.PartitionSignature != "gpt" {
		t.Fatalf("disk = %#v, want /dev/nvme0n1 with gpt signature", disk)
	}
	if got := disk.Partitions[0].FilesystemSignature; got != "vfat" {
		t.Fatalf("partition filesystem signature = %q, want vfat", got)
	}
	if got := disk.Partitions[0].GPTLabel; got != "KATL_ESP" {
		t.Fatalf("partition GPT label = %q, want KATL_ESP", got)
	}
	if got := disk.Partitions[0].PartitionUUID; got != "esp-part-uuid" {
		t.Fatalf("partition UUID = %q, want esp-part-uuid", got)
	}
	if got := disk.Partitions[0].FilesystemUUID; got != "esp-fs-uuid" {
		t.Fatalf("filesystem UUID = %q, want esp-fs-uuid", got)
	}

	wantNICs := []NICFact{
		{Name: "eno1", MACAddress: "52:54:00:12:34:56", OperState: "up"},
	}
	if !reflect.DeepEqual(facts.NICs, wantNICs) {
		t.Fatalf("NICs = %#v, want %#v", facts.NICs, wantNICs)
	}

	wantMounts := []MountFact{
		{Source: "/dev/nvme0n1p1", Target: "/boot", Filesystem: "vfat", Options: []string{"rw", "nosuid", "nodev"}},
		{Source: "/dev/sdb", Target: "/boot/data", Filesystem: "ext4", Options: []string{"rw"}},
	}
	if !reflect.DeepEqual(facts.Mounts, wantMounts) {
		t.Fatalf("mounts = %#v, want %#v", facts.Mounts, wantMounts)
	}

	wantCommands := []string{"lsblk", "findmnt", "ip"}
	if !reflect.DeepEqual(runner.calls, wantCommands) {
		t.Fatalf("commands = %#v, want %#v", runner.calls, wantCommands)
	}
	if got := strings.Join(runner.args["lsblk"], " "); !strings.Contains(got, "PARTLABEL") || !strings.Contains(got, "PARTUUID") || !strings.Contains(got, "UUID") {
		t.Fatalf("lsblk args = %q, want stable partition identity columns", got)
	}
}

type fixtureOutputRunner struct {
	outputs map[string][]byte
	calls   []string
	args    map[string][]string
}

func (r *fixtureOutputRunner) Output(_ context.Context, name string, args ...string) ([]byte, error) {
	switch name {
	case "lsblk", "findmnt", "ip":
	default:
		return nil, fmt.Errorf("unexpected command %q", name)
	}

	r.calls = append(r.calls, name)
	if r.args == nil {
		r.args = make(map[string][]string)
	}
	r.args[name] = append([]string(nil), args...)
	output, ok := r.outputs[name]
	if !ok {
		return nil, fmt.Errorf("missing fixture for %q", name)
	}
	return output, nil
}

func TestCommandDiscoverySourceCollectsDiskIdentities(t *testing.T) {
	root := t.TempDir()
	devDir := filepath.Join(root, "dev")
	byIDDir := filepath.Join(devDir, "disk", "by-id")
	byPathDir := filepath.Join(devDir, "disk", "by-path")
	sysBlockDir := filepath.Join(root, "sys", "class", "block")
	for _, dir := range []string{byIDDir, byPathDir, filepath.Join(sysBlockDir, "vda"), filepath.Join(sysBlockDir, "vdb")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll() error = %v", err)
		}
	}
	links := map[string]string{
		filepath.Join(byIDDir, "virtio-katl-data"):               "../../vdb",
		filepath.Join(byPathDir, "pci-0000:00:04.0"):             "../../vda",
		filepath.Join(byPathDir, "virtio-pci-0000:00:04.0"):      "../../vda",
		filepath.Join(byPathDir, "pci-0000:00:04.0-part1"):       "../../vda1",
		filepath.Join(byPathDir, "pci-0000:00:05.0"):             "../../vdb",
		filepath.Join(byPathDir, "not-a-symlink-is-ignored.tmp"): "",
	}
	for link, target := range links {
		if target == "" {
			if err := os.WriteFile(link, nil, 0o644); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
			continue
		}
		if err := os.Symlink(target, link); err != nil {
			t.Fatalf("Symlink() error = %v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(sysBlockDir, "vda", "diskseq"), []byte("7\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	runner := &fixtureOutputRunner{
		outputs: map[string][]byte{
			"lsblk": []byte(`{
  "blockdevices": [
    {"name":"vda","path":"` + filepath.Join(devDir, "vda") + `","type":"disk","size":68719476736,"ro":false,"rota":true,"tran":null,"serial":null,"wwn":null,"pttype":"gpt","ptuuid":"6f1e3c2a-8d4b-4f5e-9a7c-1b2d3e4f5a6b","mountpoints":[],
     "children":[{"name":"vda1","path":"` + filepath.Join(devDir, "vda1") + `","type":"part","size":536870912,"ro":false,"rota":true,"pttype":"gpt","ptuuid":"6f1e3c2a-8d4b-4f5e-9a7c-1b2d3e4f5a6b","parttype":"c12a7328-f81f-11d2-ba4b-00a0c93ec93b","mountpoints":[]}]},
    {"name":"vdb","path":"` + filepath.Join(devDir, "vdb") + `","type":"disk","size":1073741824,"ro":false,"rota":false,"tran":"nvme","mountpoints":[]}
  ]
}`),
			"findmnt": []byte(`{"filesystems":[]}`),
			"ip":      []byte(`[]`),
		},
	}

	facts, err := (CommandDiscoverySource{
		Commands:    runner,
		ByIDDir:     byIDDir,
		ByPathDir:   byPathDir,
		SysBlockDir: sysBlockDir,
	}).Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}

	if len(facts.BlockDevices) != 2 {
		t.Fatalf("block devices = %#v, want vda and vdb", facts.BlockDevices)
	}
	vda, vdb := facts.BlockDevices[0], facts.BlockDevices[1]
	wantVDAPaths := []string{filepath.Join(byPathDir, "pci-0000:00:04.0"), filepath.Join(byPathDir, "virtio-pci-0000:00:04.0")}
	if !reflect.DeepEqual(vda.ByPath, wantVDAPaths) || len(vda.ByID) != 0 {
		t.Errorf("vda aliases byPath=%q byID=%q, want byPath %q and no byID", vda.ByPath, vda.ByID, wantVDAPaths)
	}
	if vda.PartitionTableUUID != "6f1e3c2a-8d4b-4f5e-9a7c-1b2d3e4f5a6b" || vda.DiskSeq != 7 || !vda.Rotational {
		t.Errorf("vda = %#v, want GPT GUID, diskseq 7, rotational", vda)
	}
	if want := []string{filepath.Join(byPathDir, "pci-0000:00:04.0-part1")}; !reflect.DeepEqual(vda.Partitions[0].ByPath, want) {
		t.Errorf("vda1 byPath = %q, want %q", vda.Partitions[0].ByPath, want)
	}
	if vdb.Transport != "nvme" || vdb.DiskSeq != 0 || !reflect.DeepEqual(vdb.ByID, []string{filepath.Join(byIDDir, "virtio-katl-data")}) {
		t.Errorf("vdb = %#v, want nvme transport, no diskseq, by-id alias", vdb)
	}
}
