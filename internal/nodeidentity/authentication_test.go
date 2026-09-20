package nodeidentity

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/katl-dev/katl/internal/managementidentity"
)

func TestTrustedManagementProvisioning(t *testing.T) {
	root := t.TempDir()
	identity := managementidentity.NodeCredentials{Authentication: managementidentity.TrustedNetwork}
	for range 2 {
		if err := WriteManagementIdentity(root, "cp-1", identity); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(filepath.Join(root, ManagementAuthenticationPath))
	if err != nil || string(data) != "trusted-network\n" {
		t.Fatalf("mode = %q, %v", data, err)
	}
	for _, path := range []string{ManagementCACertificatePath, ManagementServerCertPath, ManagementServerPrivateKeyPath} {
		if _, err := os.Stat(filepath.Join(root, path)); !os.IsNotExist(err) {
			t.Fatalf("trusted mode created TLS material %s: %v", path, err)
		}
	}
	if err := WriteManagementIdentity(root, "cp-1", testManagementIdentity(t, "cp-1")); err == nil {
		t.Fatal("changed installed authentication implicitly")
	}
}

func TestLegacyManagementRetainsTLS(t *testing.T) {
	root := t.TempDir()
	identity := testManagementIdentity(t, "cp-1")
	if err := WriteManagementIdentity(root, "cp-1", identity); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, ManagementAuthenticationPath)); err != nil {
		t.Fatal(err)
	}
	mode, err := ManagementAuthentication(root)
	if err != nil || mode != managementidentity.MutualTLS {
		t.Fatalf("legacy mode = %q, %v", mode, err)
	}
	if err := WriteManagementIdentity(root, "cp-1", managementidentity.NodeCredentials{Authentication: managementidentity.TrustedNetwork}); err == nil {
		t.Fatal("silently downgraded legacy TLS")
	}
	if err := WriteManagementIdentity(root, "cp-1", identity); err != nil {
		t.Fatal(err)
	}
}
