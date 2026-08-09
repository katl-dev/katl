package kubernetesidentity

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGenerateRoundTripHasStablePublicFingerprint(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	bundle, err := Generate(GenerateOptions{ClusterName: "homelab", Now: now})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	before, err := Validate(bundle, now)
	if err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	data, err := Marshal(bundle)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	parsed, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	after, err := Validate(parsed, now)
	if err != nil {
		t.Fatalf("Validate(parsed) error = %v", err)
	}
	if before.Fingerprint != after.Fingerprint || !strings.HasPrefix(before.Fingerprint, "sha256:") {
		t.Fatalf("fingerprints before=%q after=%q", before.Fingerprint, after.Fingerprint)
	}
	for _, secret := range []string{"kubernetesCA:", "frontProxyCA:", "etcdCA:", "serviceAccount:", "privateKey:"} {
		if !bytes.Contains(data, []byte(secret)) {
			t.Errorf("encoded identity is missing %q", secret)
		}
	}
}

func TestWriteUsesPrivatePermissionsAndRefusesOverwrite(t *testing.T) {
	bundle := generatedBundle(t)
	path := filepath.Join(t.TempDir(), "nested", "identity.katlkey")
	if err := Write(path, bundle); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("identity mode = %04o, want 0600", got)
	}
	if err := Write(path, bundle); err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("repeat Write() error = %v, want overwrite refusal", err)
	}
}

func TestImportReadsOnlySharedKubeadmIdentity(t *testing.T) {
	bundle := generatedBundle(t)
	files, err := Files(bundle)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for path, data := range files {
		fullPath := filepath.Join(dir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fullPath, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "apiserver.key"), []byte("node-specific"), 0o600); err != nil {
		t.Fatal(err)
	}
	imported, err := Import(ImportOptions{ClusterName: "replacement", CreatedAt: time.Now().UTC(), PKIDir: dir})
	if err != nil {
		t.Fatalf("Import() error = %v", err)
	}
	if imported.ClusterName != "replacement" {
		t.Fatalf("clusterName = %q", imported.ClusterName)
	}
	if imported.KubernetesCA != bundle.KubernetesCA || imported.ServiceAccount != bundle.ServiceAccount {
		t.Fatal("import changed shared identity material")
	}
}

func TestInstallIsIdempotentAndRefusesConflictingStateBeforeWrites(t *testing.T) {
	bundle := generatedBundle(t)
	root := t.TempDir()
	if err := Install(root, bundle); err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	if err := Install(root, bundle); err != nil {
		t.Fatalf("repeat Install() error = %v", err)
	}
	for _, file := range identityFiles {
		path := filepath.Join(root, "etc/kubernetes/pki", filepath.FromSlash(file.path))
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", file.path, err)
		}
		if info.Mode().Perm() != file.mode {
			t.Errorf("%s mode = %04o, want %04o", file.path, info.Mode().Perm(), file.mode)
		}
	}

	other := generatedBundle(t)
	conflictRoot := t.TempDir()
	pkiDir := filepath.Join(conflictRoot, "etc/kubernetes/pki")
	if err := os.MkdirAll(pkiDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkiDir, "ca.crt"), []byte("different"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := Install(conflictRoot, other)
	if err == nil || !strings.Contains(err.Error(), "different Kubernetes identity") {
		t.Fatalf("conflicting Install() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(pkiDir, "sa.key")); !os.IsNotExist(err) {
		t.Fatalf("conflict wrote sa.key before refusing: %v", err)
	}
}

func TestInstallRefusesIncompleteSharedIdentityBesideLeafCertificates(t *testing.T) {
	bundle := generatedBundle(t)
	root := t.TempDir()
	pkiDir := filepath.Join(root, "etc/kubernetes/pki")
	if err := os.MkdirAll(pkiDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkiDir, "apiserver.crt"), []byte("existing leaf"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := Install(root, bundle)
	if err == nil || !strings.Contains(err.Error(), "node-specific kubeadm PKI") {
		t.Fatalf("Install() error = %v", err)
	}
}

func generatedBundle(t *testing.T) Bundle {
	t.Helper()
	bundle, err := Generate(GenerateOptions{ClusterName: "homelab", Now: time.Now().UTC()})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	return bundle
}
