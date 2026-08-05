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
	for _, internal := range []string{`"targetDisk"`, `"sets"`, `"notify"`, `"name":"kernel.`} {
		if strings.Contains(stdout.String(), internal) {
			t.Fatalf("resolve output exposes internal field %q:\n%s", internal, stdout.String())
		}
	}
	for _, public := range []string{`"systemDisk"`, `"storage"`, `"volumes"`, `"classification"`} {
		if !strings.Contains(stdout.String(), public) {
			t.Fatalf("resolve output missing %q:\n%s", public, stdout.String())
		}
	}
	foundDefaultFilesystem := false
	foundDefaultSize := false
	foundNodeIdentity := false
	for _, entry := range report.Provenance {
		if entry.Source == `spec.defaults.storage.volumes[name="data"].filesystem` {
			foundDefaultFilesystem = true
		}
		if entry.Source == `spec.defaults.storage.volumes[name="data"].selector.disk.minSizeMiB` {
			foundDefaultSize = true
		}
		if entry.Source == `spec.nodes["cp-1"].storage.volumes[name="data"].selector.disk.byID` {
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

func TestConfigDiffRefusesDifferentInferredNodes(t *testing.T) {
	dir := t.TempDir()
	beforePath := filepath.Join(dir, "before.yaml")
	afterPath := filepath.Join(dir, "after.yaml")
	before := configInspectionSource()
	after := strings.Replace(before, "name: cp-1", "name: cp-2", 1)
	if err := os.WriteFile(beforePath, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(afterPath, []byte(after), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{"config", "diff", beforePath, afterPath}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), `config diff resolved different nodes "cp-1" and "cp-2"`) || !strings.Contains(err.Error(), "node renames require an explicit lifecycle operation") {
		t.Fatalf("run() error = %v, want cross-node lifecycle refusal", err)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("stdout=%q stderr=%q, want empty", stdout.String(), stderr.String())
	}
}

func TestConfigInspectionReportsPerNodeKubeletConfiguration(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"kubelet-a.yaml", "kubelet-b.yaml"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("apiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\nsystemReserved:\n  cpu: 500m\ntopologyManagerPolicy: restricted\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	before := strings.Replace(configInspectionSource(), "      management:\n", "      kubernetes:\n        kubelet:\n          configFile: ./kubelet-a.yaml\n      management:\n", 1)
	after := strings.Replace(before, "./kubelet-a.yaml", "./kubelet-b.yaml", 1)
	beforePath := filepath.Join(dir, "before.yaml")
	afterPath := filepath.Join(dir, "after.yaml")
	if err := os.WriteFile(beforePath, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(afterPath, []byte(after), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"config", "resolve", beforePath, "--output", "json"}, &stdout, &stderr); err != nil {
		t.Fatalf("resolve error = %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	for _, want := range []string{`"kubelet"`, `"configFile": "./kubelet-a.yaml"`, `/etc/katl/kubeadm/node-cp-1/patches/kubeletconfiguration999+merge.yaml`} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("resolve output missing %q:\n%s", want, stdout.String())
		}
	}
	stdout.Reset()
	if err := run(context.Background(), []string{"config", "diff", beforePath, afterPath, "--output", "json"}, &stdout, &stderr); err != nil {
		t.Fatalf("diff error = %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), `.kubernetes.kubelet`) || !strings.Contains(stdout.String(), `"requiredOperation": "kubeadm-aware operation"`) {
		t.Fatalf("diff output does not classify kubelet change:\n%s", stdout.String())
	}
}

func configInspectionSource() string {
	source := strings.Replace(configBundleSource(), "  nodes:\n", `    storage:
      volumes:
        - name: data
          selector:
            disk:
              minSizeMiB: 1024
          filesystem: xfs
  nodes:
`, 1)
	return strings.Replace(source, "      install:\n", `      storage:
        volumes:
          - name: data
            selector:
              disk:
                byID: /dev/disk/by-id/ata-cp-data
      install:
`, 1)
}
