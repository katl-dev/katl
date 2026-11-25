package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/managementidentity"
)

// writeTrustedNetworkConfig writes a two-node trusted-network config, or one
// stating no mode, and returns its path.
func writeTrustedNetworkConfig(t *testing.T, dir string, statesMode bool) string {
	t.Helper()
	path := filepath.Join(dir, "cluster.yaml")
	if err := run(context.Background(), append(initArgs(t, dir, "--management-authentication", "trusted-network"), path), new(bytes.Buffer), new(bytes.Buffer)); err != nil {
		t.Fatal(err)
	}
	if statesMode {
		return path
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	stripped := bytes.Replace(data, []byte("    managementAuthentication: trusted-network\n"), nil, 1)
	if bytes.Equal(stripped, data) {
		t.Fatal("config has no managementAuthentication line to remove")
	}
	if err := os.WriteFile(path, stripped, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// The secrets commands rewrite a config to mTLS. Doing that to a config whose
// nodes serve trusted-network management leaves every node unreachable, so
// they refuse and name the migration instead, however often they are rerun.
func TestManagementSecretsRefuseTrustedNetworkNodes(t *testing.T) {
	tests := map[string]struct {
		command      string
		statesMode   bool
		legacySecret bool
		want         []string
	}{
		"create, trusted-network": {command: "create", statesMode: true, want: []string{"says managementAuthentication: trusted-network", "katlctl management enable-mtls --config "}},
		"create, no mode":         {command: "create", want: []string{managementAuthenticationRequiredLine, "katlctl management enable-mtls --config CONFIG", "add `managementAuthentication: mtls`"}},
		// A workstation authority would otherwise be exported and referenced.
		"export, trusted-network": {command: "export", statesMode: true, legacySecret: true, want: []string{"says managementAuthentication: trusted-network", "katlctl management enable-mtls --config "}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("KATLCTL_CONFIG", filepath.Join(dir, "context.yaml"))
			path := writeTrustedNetworkConfig(t, dir, tt.statesMode)
			if tt.legacySecret {
				identity, err := managementidentity.Generate(managementidentity.GenerateOptions{ClusterName: "lab"})
				if err != nil {
					t.Fatal(err)
				}
				legacyPath, err := managementIdentityPath("lab")
				if err != nil {
					t.Fatal(err)
				}
				if err := managementidentity.Write(legacyPath, identity); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			for attempt := 1; attempt <= 2; attempt++ {
				err := run(context.Background(), []string{"management", "identity", tt.command, "--config", path}, new(bytes.Buffer), new(bytes.Buffer))
				if err == nil {
					t.Fatalf("attempt %d: %s succeeded", attempt, tt.command)
				}
				for _, want := range tt.want {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("attempt %d: error = %v, missing %q", attempt, err, want)
					}
				}
				if strings.Contains(err.Error(), "reprovision") {
					t.Fatalf("attempt %d: error = %v, still requires reprovisioning", attempt, err)
				}
			}

			if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, before) {
				t.Fatalf("config changed: %v\n%s", err, after)
			}
			if _, err := os.Stat(filepath.Join(dir, defaultManagementSecrets)); !os.IsNotExist(err) {
				t.Fatalf("secrets file exists: %v", err)
			}
		})
	}
}

// Stating mTLS is the explicit path for a config whose nodes were never
// installed: create then succeeds and keeps the stated mode.
func TestManagementSecretsCreateAfterStatingMutualTLS(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KATLCTL_CONFIG", filepath.Join(dir, "context.yaml"))
	path := writeTrustedNetworkConfig(t, dir, true)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.Replace(data, []byte("managementAuthentication: trusted-network"), []byte("managementAuthentication: mtls"), 1)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := run(context.Background(), []string{"management", "identity", "create", "--config", path}, new(bytes.Buffer), new(bytes.Buffer)); err != nil {
		t.Fatal(err)
	}

	source := readInitSource(t, path)
	if source.Spec.ManagementAuthentication != managementidentity.MutualTLS || source.Spec.ManagementIdentity != defaultManagementSecrets {
		t.Fatalf("spec = authentication %q, identity %q", source.Spec.ManagementAuthentication, source.Spec.ManagementIdentity)
	}
	if _, err := readManagementIdentity(filepath.Join(dir, defaultManagementSecrets)); err != nil {
		t.Fatal(err)
	}
}

// Replacing a trusted-network config with an mTLS one gives its installed
// nodes an authority they do not trust, so config init refuses it; keeping or
// restating the installed mode still works.
func TestConfigInitForceRefusesTrustedNetworkToMutualTLS(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KATLCTL_CONFIG", filepath.Join(dir, "context.yaml"))
	path := writeTrustedNetworkConfig(t, dir, true)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	for attempt := 1; attempt <= 2; attempt++ {
		err := run(context.Background(), append(initArgs(t, dir, "--force", "--management-authentication", "mtls"), path), new(bytes.Buffer), new(bytes.Buffer))
		if err == nil || !strings.Contains(err.Error(), "katlctl management enable-mtls --config "+path) {
			t.Fatalf("attempt %d: error = %v", attempt, err)
		}
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, before) {
		t.Fatalf("config changed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, defaultManagementSecrets)); !os.IsNotExist(err) {
		t.Fatalf("secrets file exists: %v", err)
	}

	if err := run(context.Background(), append(initArgs(t, dir, "--force", "--management-authentication", "trusted-network"), path), new(bytes.Buffer), new(bytes.Buffer)); err != nil {
		t.Fatalf("restating trusted-network: %v", err)
	}
	if source := readInitSource(t, path); source.Spec.ManagementAuthentication != managementidentity.TrustedNetwork {
		t.Fatalf("authentication = %q", source.Spec.ManagementAuthentication)
	}
}

func TestRotationNamesMigrationForTrustedNetwork(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KATLCTL_CONFIG", filepath.Join(dir, "context.yaml"))
	path := writeTrustedNetworkConfig(t, dir, true)

	err := run(context.Background(), []string{"management", "identity", "rotate", "--config", path, "--output", filepath.Join(dir, "next.yaml")}, new(bytes.Buffer), new(bytes.Buffer))

	if err == nil || !strings.Contains(err.Error(), "requires an mTLS cluster") || !strings.Contains(err.Error(), "katlctl management enable-mtls --config "+path) {
		t.Fatalf("error = %v", err)
	}
}
