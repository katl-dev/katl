package installer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zariel/katl/internal/installer/manifest"
)

func TestMaterializeInstallRecordRejectsUncleanGenerationID(t *testing.T) {
	installManifest, err := manifest.Decode(strings.NewReader(validInstallManifestForRecord()))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	_, err = MaterializeInstallRecord(InstallRecordRequest{
		TargetRoot: t.TempDir(),
		Manifest:   installManifest,
		Record:     *minimalRecord(" 2026.06.04-001"),
		Chown:      func(string, int, int) error { return nil },
	})
	if err == nil || !strings.Contains(err.Error(), "must not contain leading or trailing whitespace") {
		t.Fatalf("MaterializeInstallRecord() error = %v, want generation id whitespace rejection", err)
	}
}

func TestMaterializeInstallRecordRejectsConfextSymlinkEscape(t *testing.T) {
	installManifest, err := manifest.Decode(strings.NewReader(validInstallManifestForRecord()))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	targetRoot := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	confextDir := filepath.Join(targetRoot, "var/lib/katl/generations/2026.06.04-001/confext")
	if err := os.MkdirAll(confextDir, 0o755); err != nil {
		t.Fatalf("mkdir confext: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(confextDir, "etc")); err != nil {
		t.Fatalf("symlink etc: %v", err)
	}

	_, err = MaterializeInstallRecord(InstallRecordRequest{
		TargetRoot: targetRoot,
		Manifest:   installManifest,
		Record:     *minimalRecord("2026.06.04-001"),
		Chown:      func(string, int, int) error { return nil },
	})
	if err == nil || !strings.Contains(err.Error(), "refusing to follow symlink") {
		t.Fatalf("MaterializeInstallRecord() error = %v, want symlink rejection", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "extension-release.d/extension-release.katl-node")); !os.IsNotExist(err) {
		t.Fatalf("outside write err = %v, want no escaped extension-release write", err)
	}
}

func validInstallManifestForRecord() string {
	return `{
		"apiVersion": "install.katl.dev/v1alpha1",
		"kind": "InstallManifest",
		"node": {
			"identity": {
				"hostname": "lab-node-01",
				"ssh": {
					"authorizedKeys": [
						"` + sshKey + `"
					]
				}
			},
			"systemRole": "control-plane"
		},
		"install": {
    "wipeTarget": true,
			"targetDisk": {"byID": "/dev/disk/by-id/ata-root", "minSizeMiB": 32768}
		},
		"katlosImage": {
			"url": "https://example.invalid/katlos-install.squashfs",
			"sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"sizeBytes": 1073741824,
			"version": "2026.06.04",
			"architecture": "x86_64",
			"runtimeInterface": "katl-runtime-1",
			"role": "install"
		}
	}`
}
