package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/bootstrap/cluster"
	"github.com/katl-dev/katl/internal/installer/configbundle"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/katlc/transport"
	"github.com/katl-dev/katl/internal/katlctl/workstation"
	"github.com/katl-dev/katl/internal/managementidentity"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"gopkg.in/yaml.v3"
)

func TestMissingProjectSecretsNeverCreateAuthority(t *testing.T) {
	configPath := writeClusterConfig(t)
	createTestManagementSecrets(t, configPath)
	secretsPath := filepath.Join(filepath.Dir(configPath), defaultManagementSecrets)
	if err := os.Remove(secretsPath); err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{
		{"config", "bundle", configPath, "--output", filepath.Join(t.TempDir(), "cluster.katlcfg")},
		{"context", "save", "--config", configPath},
		{"cluster", "wipe", "--config", configPath, "--all"},
	} {
		err := run(context.Background(), args, io.Discard, io.Discard)
		if err == nil || !strings.Contains(err.Error(), secretsPath) || !strings.Contains(err.Error(), "restore") {
			t.Fatalf("%v: %v", args, err)
		}
		if _, err := os.Stat(secretsPath); !os.IsNotExist(err) {
			t.Fatalf("missing authority was recreated: %v", err)
		}
	}
}

func TestProjectSecretsSurviveContextLoss(t *testing.T) {
	configPath := writeClusterConfig(t)
	createTestManagementSecrets(t, configPath)
	secretsPath := filepath.Join(filepath.Dir(configPath), defaultManagementSecrets)
	identity, err := readManagementIdentity(secretsPath)
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeKatlcAgentClient{nodeStatus: enrolledStatus("cp-1", "install-1", "machine-1")}
	startServer := func(identity managementidentity.Bundle) string {
		t.Helper()
		node, _, err := managementidentity.EnsureNode(&identity, "cp-1", time.Now(), nil)
		if err != nil {
			t.Fatal(err)
		}
		tlsConfig, err := transport.ServerTLSConfigForNode(node)
		if err != nil {
			t.Fatal(err)
		}
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		server := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsConfig)))
		agentapi.RegisterKatlcAgentServer(server, &clusterApplyIdentityServer{client: fake})
		go func() { _ = server.Serve(listener) }()
		t.Cleanup(func() { server.Stop(); listener.Close() })
		return listener.Addr().String()
	}
	endpoint := startServer(identity)
	oldDial := dialKatlcAgent
	t.Cleanup(func() { dialKatlcAgent = oldDial })
	dialKatlcAgent = func(ctx context.Context, _ string) (katlcAgentConnection, error) {
		return dialKatlcAgentTCP(ctx, endpoint)
	}
	savedPath, err := workstation.ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Dir(savedPath)); err != nil {
		t.Fatal(err)
	}
	args := []string{"context", "save", "--config", configPath}
	if err := run(context.Background(), args, io.Discard, io.Discard); err != nil {
		t.Fatalf("restore from project secrets: %v", err)
	}

	fake.nodeStatus = enrolledStatus("cp-1", "install-2", "machine-2")
	for range 2 {
		if err := run(context.Background(), args, io.Discard, io.Discard); err != nil {
			t.Fatalf("refresh trusted reinstall: %v", err)
		}
	}
	cfg, err := workstation.Load(savedPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Clusters[0].Nodes[0].EnrollmentID != "install-2" || cfg.Clusters[0].Nodes[0].MachineID != "machine-2" {
		t.Fatal("context retained the old installation")
	}
	before, err := os.ReadFile(savedPath)
	if err != nil {
		t.Fatal(err)
	}
	other, err := managementidentity.Generate(managementidentity.GenerateOptions{ClusterName: "lab"})
	if err != nil {
		t.Fatal(err)
	}
	endpoint = startServer(other)
	err = run(context.Background(), args, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "different management authority") {
		t.Fatalf("wrong-authority recovery error: %v", err)
	}
	after, err := os.ReadFile(savedPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("failed authentication changed the saved context")
	}
}

func TestEncryptedProjectSecrets(t *testing.T) {
	for _, command := range []string{"sops", "age-keygen"} {
		if _, err := exec.LookPath(command); err != nil {
			t.Skip("requires SOPS and age for the encrypted-file integration check")
		}
	}
	configPath := writeClusterConfig(t)
	createTestManagementSecrets(t, configPath)
	secretsPath := filepath.Join(filepath.Dir(configPath), defaultManagementSecrets)
	keyPath := filepath.Join(t.TempDir(), "age.key")
	if output, err := exec.Command("age-keygen", "-o", keyPath).CombinedOutput(); err != nil {
		t.Fatalf("create test age key: %v: %s", err, output)
	}
	recipient, err := exec.Command("age-keygen", "-y", keyPath).Output()
	if err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("sops", "encrypt", "--age", strings.TrimSpace(string(recipient)), "--in-place", secretsPath).CombinedOutput(); err != nil {
		t.Fatalf("encrypt test secrets: %v: %s", err, output)
	}
	t.Setenv("SOPS_AGE_KEY_FILE", keyPath)
	before, err := os.ReadFile(secretsPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(secretsPath, 0o644); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := run(context.Background(), []string{"config", "bundle", configPath, "--output", filepath.Join(t.TempDir(), "cluster.katlcfg")}, io.Discard, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	after, err := os.ReadFile(secretsPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("compilation rewrote encrypted project secrets")
	}
}

func TestConfigInitForcePreservesMissingAuthority(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KATLCTL_CONFIG", filepath.Join(dir, "context", "katlctl.yaml"))
	key := filepath.Join(dir, "ssh.pub")
	if err := os.WriteFile(key, []byte(uxTestSSHKey), 0o600); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "cluster.yaml")
	args := []string{"config", "init", config, "--management-authentication", "mtls", "--name", "lab", "--ssh-authorized-key", key, "--node", "cp-1=control-plane,192.0.2.1,/dev/disk/by-id/test"}
	if err := run(context.Background(), args, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(dir, defaultManagementSecrets)
	if err := os.Remove(secret); err != nil {
		t.Fatal(err)
	}
	args = append(args, "--force")
	if err := run(context.Background(), args, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "restore") {
		t.Fatalf("force must not replace lost trust: %v", err)
	}
	after, err := os.ReadFile(config)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("failed init changed existing config")
	}
	if _, err := os.Stat(secret); !os.IsNotExist(err) {
		t.Fatalf("lost authority was regenerated: %v", err)
	}
}

func TestPartialWipeDoesNotRequireOtherNodes(t *testing.T) {
	configPath := writeClusterConfig(t)
	createTestManagementSecrets(t, configPath)
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	source, err := configbundle.DecodeSource(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	offline := source.Spec.Nodes[0]
	offline.Name = "offline-worker"
	offline.ControlPlane = false
	offline.Management.Address = "192.0.2.222"
	source.Spec.Nodes = append(source.Spec.Nodes, offline)
	data, err = yaml.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	contextPath, err := workstation.ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(contextPath); err != nil {
		t.Fatal(err)
	}
	client := readyWipeClusterClient("new-machine")
	client.nodeStatus.InventoryNodeName = "cp-1"
	client.nodeStatus.EnrollmentId = "new-install"
	oldDial := dialKatlcAgent
	dialKatlcAgent = func(_ context.Context, endpoint string) (katlcAgentConnection, error) {
		if strings.Contains(endpoint, "192.0.2.222") {
			t.Fatal("contacted an unselected node")
		}
		return katlcAgentConnection{Client: client, Close: func() error { return nil }}, nil
	}
	oldConnector := newWipeClusterConnector
	newWipeClusterConnector = func() cluster.AgentConnector {
		return newFakeWipeClusterConnector(map[string]*fakeKatlcAgentClient{"cp-1": client})
	}
	t.Cleanup(func() { dialKatlcAgent = oldDial; newWipeClusterConnector = oldConnector })
	if err := run(context.Background(), []string{"cluster", "wipe", "--config", configPath, "--node", "cp-1", "--allow-partial-cluster", "--plan"}, io.Discard, io.Discard); err != nil {
		t.Fatalf("plan selected node without context or other nodes: %v", err)
	}
	if len(client.submitRequests) != 0 {
		t.Fatal("plan submitted a destructive operation")
	}
}

func TestConfigInitRetainsSecretsReference(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KATLCTL_CONFIG", filepath.Join(dir, "context.yaml"))
	key := filepath.Join(dir, "ssh.pub")
	if err := os.WriteFile(key, []byte(uxTestSSHKey), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "cluster.yaml")
	args := []string{"config", "init", path, "--management-authentication", "mtls", "--name", "lab", "--ssh-authorized-key", key, "--node", "cp-1=control-plane,192.0.2.1,/dev/disk/by-id/test"}
	if err := run(context.Background(), args, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	secrets := filepath.Join(dir, "custom.yaml")
	if err := os.Rename(filepath.Join(dir, defaultManagementSecrets), secrets); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := referenceManagementSecrets(path, secrets, data); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(secrets)
	if err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), append(args, "--force"), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	source, err := configbundle.DecodeSource(bytes.NewReader(data))
	if err != nil || source.Spec.ManagementIdentity != "custom.yaml" {
		t.Fatalf("reference changed: %s", data)
	}
	after, err := os.ReadFile(secrets)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("existing secrets changed")
	}
}
