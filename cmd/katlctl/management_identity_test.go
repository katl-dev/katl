package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/installer/configbundle"
)

func TestConfigBundleCreatesStableAutomaticManagementAccess(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KATLCTL_CONFIG", "")
	t.Setenv("KATLCTL_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	sourcePath := filepath.Join(dir, "cluster.yaml")
	if err := os.WriteFile(sourcePath, []byte(configBundleSource()), 0o644); err != nil {
		t.Fatal(err)
	}
	firstPath := filepath.Join(dir, "first.katlcfg")
	secondPath := filepath.Join(dir, "second.katlcfg")
	var firstErr bytes.Buffer
	if err := run(context.Background(), []string{"config", "bundle", sourcePath, "--output", firstPath}, &bytes.Buffer{}, &firstErr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(firstErr.String(), "Created management identity") || !strings.Contains(firstErr.String(), "Back up this file") {
		t.Fatalf("first stderr = %q", firstErr.String())
	}
	var secondErr bytes.Buffer
	if err := run(context.Background(), []string{"config", "bundle", sourcePath, "--output", secondPath}, &bytes.Buffer{}, &secondErr); err != nil {
		t.Fatal(err)
	}
	if secondErr.Len() != 0 {
		t.Fatalf("repeated bundle stderr = %q, want no repeated ceremony", secondErr.String())
	}

	identityPath, err := managementIdentityPath("lab")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(identityPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("management identity mode = %04o", info.Mode().Perm())
	}
	identity, err := readManagementIdentity(identityPath)
	if err != nil {
		t.Fatal(err)
	}
	firstArchive, err := os.ReadFile(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	archiveInfo, err := os.Stat(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	if archiveInfo.Mode().Perm() != 0o600 {
		t.Fatalf("compiled install bundle mode = %04o", archiveInfo.Mode().Perm())
	}
	secondArchive, err := os.ReadFile(secondPath)
	if err != nil {
		t.Fatal(err)
	}
	first, err := configbundle.ReadSelectedNode(bytes.NewReader(firstArchive), configbundle.ReadOptions{NodeName: "cp-1", AllowMissingKatlosImage: true})
	if err != nil {
		t.Fatal(err)
	}
	second, err := configbundle.ReadSelectedNode(bytes.NewReader(secondArchive), configbundle.ReadOptions{NodeName: "cp-1", AllowMissingKatlosImage: true})
	if err != nil {
		t.Fatal(err)
	}
	if first.InstallManifest.Node.Identity.Management.Empty() {
		t.Fatal("compiled install manifest has no node management identity")
	}
	if first.InstallManifest.Node.Identity.Management.ServerCertificate != second.InstallManifest.Node.Identity.Management.ServerCertificate {
		t.Fatal("repeated compilation changed the node's management identity")
	}
	for name, secret := range map[string]string{
		"management CA private key": identity.CertificateAuthority.PrivateKey,
		"operator private key":      identity.Operator.PrivateKey,
	} {
		if bytes.Contains(firstArchive, []byte(secret)) {
			t.Fatalf("compiled install bundle contains %s", name)
		}
	}
}
