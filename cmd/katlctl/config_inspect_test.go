package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/installer/configbundle"
)

func TestConfigResolveShowsPublicEffectiveNode(t *testing.T) {
	source := configInspectionSource()
	path := filepath.Join(t.TempDir(), "cluster.yaml")
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"config", "resolve", path, "--output", "json"}, &stdout, &stderr); err != nil {
		t.Fatalf("run() error = %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	var report struct {
		Kind    string `json:"kind"`
		Node    string `json:"node"`
		Derived struct {
			StorageVolumes []configbundle.DerivedVolume `json:"storageVolumes"`
		} `json:"derived"`
		Provenance []configbundle.FieldProvenance   `json:"provenance"`
		OwnedFiles []configbundle.OwnedFile         `json:"ownedFiles"`
		Warnings   []configbundle.ResolutionWarning `json:"warnings"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode output: %v\n%s", err, stdout.String())
	}
	if report.Kind != configbundle.NodeResolutionKind || report.Node != "cp-1" {
		t.Fatalf("report identity = %#v", report)
	}
	if len(report.Derived.StorageVolumes) != 1 || report.Derived.StorageVolumes[0].MountPath != "/var/mnt/data" || report.Derived.StorageVolumes[0].PartitionLabel != "u-data" {
		t.Fatalf("derived volumes = %#v", report.Derived.StorageVolumes)
	}
	if len(report.Provenance) == 0 || len(report.OwnedFiles) == 0 || len(report.Warnings) == 0 {
		t.Fatalf("inspection detail missing: provenance=%d files=%d warnings=%d", len(report.Provenance), len(report.OwnedFiles), len(report.Warnings))
	}
	for _, internal := range []string{`"targetDisk"`, `"volumes"`, `"sets"`, `"notify"`, `"name":"kernel.`} {
		if strings.Contains(stdout.String(), internal) {
			t.Fatalf("resolve output exposes internal field %q:\n%s", internal, stdout.String())
		}
	}
	for _, public := range []string{`"systemDisk"`, `"storage"`, `"disks"`, `"classification"`} {
		if !strings.Contains(stdout.String(), public) {
			t.Fatalf("resolve output missing %q:\n%s", public, stdout.String())
		}
	}
	foundDefaultFilesystem := false
	foundDefaultSize := false
	foundNodeIdentity := false
	for _, entry := range report.Provenance {
		if entry.Source == `spec.defaults.storage.disks[name="data"].filesystem` {
			foundDefaultFilesystem = true
		}
		if entry.Source == `spec.defaults.storage.disks[name="data"].selector.disk.minSizeMiB` {
			foundDefaultSize = true
		}
		if entry.Source == `spec.nodes["cp-1"].storage.disks[name="data"].selector.disk.byID` {
			foundNodeIdentity = true
		}
	}
	if !foundDefaultFilesystem || !foundDefaultSize || !foundNodeIdentity {
		t.Fatalf("provenance does not attribute layered storage fields: %#v", report.Provenance)
	}
}

func TestConfigDiffClassifiesLifecycleAndTargetChanges(t *testing.T) {
	dir := t.TempDir()
	beforePath := filepath.Join(dir, "before.yaml")
	afterPath := filepath.Join(dir, "after.yaml")
	before := configInspectionSource()
	after := strings.Replace(before, "/dev/disk/by-id/ata-cp-root", "/dev/disk/by-id/ata-new-root", 1)
	after = strings.Replace(after, "address: 10.0.0.11", "address: 10.0.0.12", 1)
	if err := os.WriteFile(beforePath, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(afterPath, []byte(after), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"config", "diff", beforePath, afterPath, "--node", "cp-1", "--output", "json"}, &stdout, &stderr); err != nil {
		t.Fatalf("run() error = %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	var report configbundle.ConfigDiff
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode output: %v\n%s", err, stdout.String())
	}
	if report.Classification.Overall != "operation-only" || len(report.Changes) != 2 {
		t.Fatalf("diff = %#v", report)
	}
	if report.Changes[0].RequiredOperation != "wipe-reinstall" || report.Changes[1].Classification != "target-only" {
		t.Fatalf("changes = %#v", report.Changes)
	}
}

func TestConfigResolveRequiresSelectionForMultipleNodes(t *testing.T) {
	path := writeMultiControlPlaneClusterConfig(t)
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{"config", "resolve", path}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "selected node is required") {
		t.Fatalf("run() error = %v, want explicit multi-node selection", err)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("stdout=%q stderr=%q, want empty", stdout.String(), stderr.String())
	}
}

func configInspectionSource() string {
	source := strings.Replace(configBundleSource(), "  nodes:\n", `    storage:
      disks:
        - name: data
          selector:
            disk:
              minSizeMiB: 1024
          filesystem: xfs
  nodes:
`, 1)
	return strings.Replace(source, "      install:\n", `      storage:
        disks:
          - name: data
            selector:
              disk:
                byID: /dev/disk/by-id/ata-cp-data
      install:
`, 1)
}
