package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/installer/configbundle"
	"github.com/katl-dev/katl/internal/managementidentity"
)

func TestConfigBundlePreservesProjectManagementAccess(t *testing.T) {
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
	createTestManagementSecrets(t, sourcePath)
	var firstErr bytes.Buffer
	if err := run(context.Background(), []string{"config", "bundle", sourcePath, "--output", firstPath}, &bytes.Buffer{}, &firstErr); err != nil {
		t.Fatal(err)
	}
	if firstErr.Len() != 0 {
		t.Fatalf("compilation should only read secrets: %q", firstErr.String())
	}
	var secondErr bytes.Buffer
	if err := run(context.Background(), []string{"config", "bundle", sourcePath, "--output", secondPath}, &bytes.Buffer{}, &secondErr); err != nil {
		t.Fatal(err)
	}
	if secondErr.Len() != 0 {
		t.Fatalf("repeated bundle stderr = %q, want no repeated ceremony", secondErr.String())
	}

	identityPath := filepath.Join(dir, defaultManagementSecrets)
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

func createTestManagementSecrets(t *testing.T, configPath string) {
	t.Helper()
	if err := run(context.Background(), []string{"management", "identity", "create", "--config", configPath}, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
}

func TestManagementDialMissingContextExplainsRecovery(t *testing.T) {
	t.Setenv("KATLCTL_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	_, err := managementDialForEndpoint("192.0.2.10:9443")
	if err == nil {
		t.Fatal("managementDialForEndpoint() error = nil")
	}
	for _, want := range []string{"katlctl management identity import IDENTITY", "katlctl context save --config CLUSTER.yaml"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, missing %q", err, want)
		}
	}
}

func ensureManagementIdentity(clusterName string, stderr io.Writer) (managementidentity.Bundle, string, error) {
	path, err := managementIdentityPath(clusterName)
	if err != nil {
		return managementidentity.Bundle{}, "", err
	}
	bundle, err := readManagementIdentity(path)
	if err == nil {
		if bundle.ClusterName != strings.TrimSpace(clusterName) {
			return managementidentity.Bundle{}, "", fmt.Errorf("management identity %s belongs to cluster %q, not %q", path, bundle.ClusterName, clusterName)
		}
		return bundle, path, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return managementidentity.Bundle{}, "", err
	}
	bundle, err = managementidentity.Generate(managementidentity.GenerateOptions{ClusterName: clusterName, Now: time.Now().UTC()})
	if err != nil {
		return managementidentity.Bundle{}, "", err
	}
	if err := managementidentity.Write(path, bundle); err != nil {
		return managementidentity.Bundle{}, "", err
	}
	if stderr != nil {
		fmt.Fprintf(stderr, "Created management identity for cluster %s at %s\n", clusterName, path)
		fmt.Fprintln(stderr, "Back up this file; Katl uses it automatically and it is required to reinstall nodes without changing management trust.")
	}
	return bundle, path, nil
}
