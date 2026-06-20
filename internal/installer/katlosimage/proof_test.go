package katlosimage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSingleImageProofReportsInstallComponents(t *testing.T) {
	root, _ := writeImagePayload(t, nil)
	payload, err := ResolveDirectory(context.Background(), root, expectedImage())
	if err != nil {
		t.Fatalf("ResolveDirectory() error = %v", err)
	}
	imagePath := writeProofImage(t, []byte("install image"))

	report, err := payload.SingleImageProof(SingleImageProofRequest{
		ImagePath:      imagePath,
		ImageSHA256:    strings.Repeat("b", 64),
		ImageSizeBytes: 2048,
	})
	if err != nil {
		t.Fatalf("SingleImageProof() error = %v", err)
	}
	if report.Kind != "KatlOSSingleImageProof" || report.EmbeddedIndex.ImageRole != RoleInstall {
		t.Fatalf("report = %#v", report)
	}
	if !hasComponentProof(report, ComponentRuntimeRoot) || !hasComponentProof(report, ComponentRuntimeUKI) || hasComponentProof(report, ComponentKubernetes) {
		t.Fatalf("component proofs = %#v", report.Components)
	}
	if report.ImageSHA256 != sha256Bytes([]byte("install image")) || report.ImageSizeBytes != uint64(len("install image")) {
		t.Fatalf("image proof identity = %#v", report)
	}
	if len(report.Verification) < 2 || report.Verification[0].Field != "image" {
		t.Fatalf("verification = %#v", report.Verification)
	}

	path := filepath.Join(t.TempDir(), "proof", "single-image.json")
	if err := WriteSingleImageProof(path, report); err != nil {
		t.Fatalf("WriteSingleImageProof() error = %v", err)
	}
	var roundTrip SingleImageProofReport
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, err)
	}
	if err := json.Unmarshal(data, &roundTrip); err != nil {
		t.Fatalf("decode report: %v\n%s", err, data)
	}
	if roundTrip.ImagePath != report.ImagePath || len(roundTrip.Components) != 2 {
		t.Fatalf("roundTrip = %#v", roundTrip)
	}
}

func TestSingleImageProofReportsUpgradeSysupdateMetadata(t *testing.T) {
	payload := upgradePayload(t, nil)
	imagePath := writeProofImage(t, []byte("upgrade image"))
	rootSource := filepath.Join(t.TempDir(), "source", "katl_2026.06.17.root.squashfs")
	ukiSource := filepath.Join(filepath.Dir(rootSource), "katl_2026.06.17.efi")
	rootTransfer := filepath.Join(t.TempDir(), "sysupdate.d", "50-katl-root.transfer")
	ukiTransfer := filepath.Join(filepath.Dir(rootTransfer), "70-katl-uki.transfer")
	writeProofFile(t, rootSource, []byte("runtime root"))
	writeProofFile(t, ukiSource, []byte("runtime uki"))
	writeProofFile(t, rootTransfer, []byte("[Source]\nPath=/var/lib/katl/sysupdate/source\nMatchPattern=katl_@v.root.squashfs\n"))
	writeProofFile(t, ukiTransfer, []byte("[Source]\nPath=/var/lib/katl/sysupdate/source\nMatchPattern=katl_@v.efi\n"))

	report, err := payload.SingleImageProof(SingleImageProofRequest{
		ImagePath: imagePath,
		Sysupdate: &SysupdateProof{
			SourcePath:       "/var/lib/katl/sysupdate/source",
			RootTransferPath: rootTransfer,
			UKITransferPath:  ukiTransfer,
			RootSourcePath:   rootSource,
			UKISourcePath:    ukiSource,
		},
	})
	if err != nil {
		t.Fatalf("SingleImageProof(upgrade) error = %v", err)
	}
	if report.EmbeddedIndex.ImageRole != RoleUpgrade || report.Sysupdate == nil || report.Sysupdate.SourcePath == "" {
		t.Fatalf("report = %#v", report)
	}
	if !strings.Contains(report.Verification[len(report.Verification)-1].Message, "runtime-uki") {
		t.Fatalf("verification = %#v", report.Verification)
	}
	if report.Sysupdate.RootSourceSHA256 != payload.Runtime.SHA256 || report.Sysupdate.UKISourceSHA256 != payload.Boot.SHA256 {
		t.Fatalf("sysupdate proof = %#v", report.Sysupdate)
	}
}

func TestSingleImageProofFailsWhenRuntimeRoleOnlyLooseExternal(t *testing.T) {
	root, _ := writeImagePayload(t, func(index *Index) {
		index.Components = index.Components[1:]
	})
	index, err := readIndex(filepath.Join(root, "katlos", "image.json"))
	if err != nil {
		t.Fatalf("readIndex() error = %v", err)
	}
	payload := Payload{
		Root:  root,
		Index: index,
		Boot:  index.Components[0],
	}
	_, err = payload.SingleImageProof(SingleImageProofRequest{
		ImagePath: writeProofImage(t, []byte("missing runtime image")),
	})
	if err == nil || !strings.Contains(err.Error(), `missing component role "runtime-root"`) || !strings.Contains(err.Error(), "image index") {
		t.Fatalf("SingleImageProof() error = %v, want missing image-index component role", err)
	}
}

func TestVerifyInstallManifestSingleImage(t *testing.T) {
	image, err := VerifyInstallManifestSingleImage([]byte(validInstallManifest()))
	if err != nil {
		t.Fatalf("VerifyInstallManifestSingleImage() error = %v", err)
	}
	if image.URL == "" || image.Role != RoleInstall {
		t.Fatalf("katlosImage = %#v", image)
	}

	legacy := strings.Replace(validInstallManifest(),
		`"katlosImage": {
    "url": "https://example.invalid/katlos-install.squashfs",
    "sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "sizeBytes": 1024,
    "version": "2026.06.06",
    "architecture": "x86_64",
    "runtimeInterface": "katl-runtime-1",
    "role": "install"
  }`,
		`"artifacts": {
    "runtimeRoot": {"url": "https://example.invalid/root.squashfs"},
    "uki": {"url": "https://example.invalid/katl.efi"},
    "sysexts": [{"url": "https://example.invalid/kubernetes.raw"}]
  }`, 1)
	_, err = VerifyInstallManifestSingleImage([]byte(legacy))
	if err == nil || !strings.Contains(err.Error(), `loose component field "artifacts"`) {
		t.Fatalf("VerifyInstallManifestSingleImage() error = %v, want loose artifacts rejection", err)
	}

	missing := strings.Replace(validInstallManifest(), `"katlosImage"`, `"runtimeRoot"`, 1)
	_, err = VerifyInstallManifestSingleImage([]byte(missing))
	if err == nil || !strings.Contains(err.Error(), `loose component field "runtimeRoot"`) {
		t.Fatalf("VerifyInstallManifestSingleImage() error = %v, want loose runtimeRoot rejection", err)
	}
}

func hasComponentProof(report SingleImageProofReport, role string) bool {
	for _, component := range report.Components {
		if component.Role == role && component.Verified {
			return true
		}
	}
	return false
}

func writeProofFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
}

func writeProofImage(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "katlos-image.squashfs")
	writeProofFile(t, path, data)
	return path
}

func sha256Bytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func validInstallManifest() string {
	return `{
  "apiVersion": "install.katl.dev/v1alpha1",
  "kind": "InstallManifest",
  "node": {
    "identity": {
      "hostname": "cp-1",
      "ssh": {
        "authorizedKeys": ["ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDAxMjM0NTY3ODlhYmNkZWYwMTIzNDU2Nzg5YWJjZGVm katl@example"]
      }
    },
    "systemRole": "control-plane"
  },
  "install": {
    "wipeTarget": true,
    "targetDisk": {"byID": "/dev/disk/by-id/virtio-katl-root"}
  },
  "katlosImage": {
    "url": "https://example.invalid/katlos-install.squashfs",
    "sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "sizeBytes": 1024,
    "version": "2026.06.06",
    "architecture": "x86_64",
    "runtimeInterface": "katl-runtime-1",
    "role": "install"
  }
}`
}
