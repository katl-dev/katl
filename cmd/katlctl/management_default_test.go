package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/installer/configbundle"
	"github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/katlc/transport"
	"github.com/katl-dev/katl/internal/katlctl/workstation"
	"github.com/katl-dev/katl/internal/managementidentity"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"gopkg.in/yaml.v3"
)

const managementAuthenticationRequiredLine = "add `managementAuthentication: trusted-network` under spec"

// initArgs returns config init arguments for a two-node cluster.
func initArgs(t *testing.T, dir string, extra ...string) []string {
	t.Helper()
	key := filepath.Join(dir, "ssh.pub")
	if err := os.WriteFile(key, []byte(uxTestSSHKey), 0o600); err != nil {
		t.Fatal(err)
	}
	return append([]string{"config", "init", "--name", "lab", "--ssh-authorized-key", key,
		"--node", "cp-1=control-plane,192.0.2.11,/dev/disk/by-id/test-1",
		"--node", "worker-1=worker,192.0.2.21,/dev/disk/by-id/test-2"}, extra...)
}

func readInitSource(t *testing.T, path string) configbundle.SourceConfig {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	source, err := configbundle.DecodeSource(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return source
}

func TestConfigInitDefaultsToMutualTLS(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KATLCTL_CONFIG", filepath.Join(dir, "context.yaml"))
	path := filepath.Join(dir, "cluster.yaml")

	if err := run(context.Background(), append(initArgs(t, dir), path), new(bytes.Buffer), new(bytes.Buffer)); err != nil {
		t.Fatal(err)
	}

	source := readInitSource(t, path)
	if source.Spec.ManagementAuthentication != "mtls" || source.Spec.ManagementIdentity == "" {
		t.Fatalf("spec = authentication %q, identity %q", source.Spec.ManagementAuthentication, source.Spec.ManagementIdentity)
	}
	secrets := filepath.Join(dir, source.Spec.ManagementIdentity)
	info, err := os.Stat(secrets)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("secrets file = %v, %v", info, err)
	}
	identity, err := readManagementIdentity(secrets)
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range []string{"cp-1", "worker-1"} {
		if identity.Nodes[node].Certificate == "" {
			t.Fatalf("secrets hold no certificate for %s", node)
		}
	}
}

func TestConfigInitTrustedNetworkCreatesNoSecrets(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KATLCTL_CONFIG", filepath.Join(dir, "context.yaml"))
	path := filepath.Join(dir, "cluster.yaml")

	if err := run(context.Background(), append(initArgs(t, dir, "--management-authentication", "trusted-network"), path), new(bytes.Buffer), new(bytes.Buffer)); err != nil {
		t.Fatal(err)
	}

	source := readInitSource(t, path)
	if source.Spec.ManagementAuthentication != "trusted-network" || source.Spec.ManagementIdentity != "" {
		t.Fatalf("spec = authentication %q, identity %q", source.Spec.ManagementAuthentication, source.Spec.ManagementIdentity)
	}
	if _, err := os.Stat(filepath.Join(dir, defaultManagementSecrets)); !os.IsNotExist(err) {
		t.Fatalf("secrets file exists: %v", err)
	}
}

func TestConfigInitForceKeepsPreviousMode(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KATLCTL_CONFIG", filepath.Join(dir, "context.yaml"))
	path := filepath.Join(dir, "cluster.yaml")
	if err := run(context.Background(), append(initArgs(t, dir, "--management-authentication", "trusted-network"), path), new(bytes.Buffer), new(bytes.Buffer)); err != nil {
		t.Fatal(err)
	}

	if err := run(context.Background(), append(initArgs(t, dir, "--force"), path), new(bytes.Buffer), new(bytes.Buffer)); err != nil {
		t.Fatal(err)
	}

	if source := readInitSource(t, path); source.Spec.ManagementAuthentication != "trusted-network" || source.Spec.ManagementIdentity != "" {
		t.Fatalf("forced config changed mode: authentication %q, identity %q", source.Spec.ManagementAuthentication, source.Spec.ManagementIdentity)
	}
	if _, err := os.Stat(filepath.Join(dir, defaultManagementSecrets)); !os.IsNotExist(err) {
		t.Fatalf("secrets file exists: %v", err)
	}
}

func TestConfigInitForceRefusesPreviousConfigWithoutMode(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KATLCTL_CONFIG", filepath.Join(dir, "context.yaml"))
	path := filepath.Join(dir, "cluster.yaml")
	if err := run(context.Background(), append(initArgs(t, dir, "--management-authentication", "trusted-network"), path), new(bytes.Buffer), new(bytes.Buffer)); err != nil {
		t.Fatal(err)
	}
	previous, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	previous = bytes.Replace(previous, []byte("    managementAuthentication: trusted-network\n"), nil, 1)
	if err := os.WriteFile(path, previous, 0o644); err != nil {
		t.Fatal(err)
	}

	err = run(context.Background(), append(initArgs(t, dir, "--force"), path), new(bytes.Buffer), new(bytes.Buffer))

	if err == nil || !strings.Contains(err.Error(), managementAuthenticationRequiredLine) {
		t.Fatalf("error = %v", err)
	}
	if after, readErr := os.ReadFile(path); readErr != nil || !bytes.Equal(after, previous) {
		t.Fatalf("config changed: %v", readErr)
	}
	if _, err := os.Stat(filepath.Join(dir, defaultManagementSecrets)); !os.IsNotExist(err) {
		t.Fatalf("secrets file exists: %v", err)
	}
}

func TestConfigInitMutualTLSNeedsOutputPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KATLCTL_CONFIG", filepath.Join(dir, "context.yaml"))
	var stdout bytes.Buffer

	err := run(context.Background(), initArgs(t, dir), &stdout, new(bytes.Buffer))

	if err == nil || !strings.Contains(err.Error(), "pass PATH") || stdout.Len() != 0 {
		t.Fatalf("error = %v, stdout = %q", err, stdout.String())
	}
}

// A configuration that states no management authentication must be refused
// before any command contacts a node or changes workstation state.
func TestCommandsRefuseConfigWithoutManagementAuthentication(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	defer server.Close()
	workerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	defer workerServer.Close()

	dir := t.TempDir()
	contextPath := filepath.Join(dir, "context.yaml")
	t.Setenv("KATLCTL_CONFIG", contextPath)
	path := filepath.Join(dir, "cluster.yaml")
	if err := run(context.Background(), append(initArgs(t, dir, "--management-authentication", "trusted-network"), path), new(bytes.Buffer), new(bytes.Buffer)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.ReplaceAll(data, []byte("192.0.2.11"), []byte(server.Listener.Addr().String()))
	data = bytes.ReplaceAll(data, []byte("192.0.2.21"), []byte(workerServer.Listener.Addr().String()))
	data = bytes.Replace(data, []byte("    managementAuthentication: trusted-network\n"), nil, 1)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(contextPath, []byte("contexts: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var dials atomic.Int32
	oldDial := dialKatlcAgent
	t.Cleanup(func() { dialKatlcAgent = oldDial })
	dialKatlcAgent = func(context.Context, string) (katlcAgentConnection, error) {
		dials.Add(1)
		return katlcAgentConnection{}, errors.New("unexpected dial")
	}

	for _, command := range [][]string{
		{"install", "apply", "--config", path, "--no-wait"},
		{"cluster", "apply", "--config", path},
		{"cluster", "bootstrap", "--config", path},
		{"node", "upgrade", "cp-1", "--config", path, "--version", "2026.10.3"},
		{"node", "status", "cp-1", "--config", path},
		{"context", "save", "--config", path},
		{"config", "bundle", path, "--output", filepath.Join(dir, "cluster.katlcfg")},
		{"config", "add-node", path, "--installer", server.Listener.Addr().String()},
		{"cluster", "wipe", "--config", path, "--all"},
		{"node", "wipe", "worker-1", "--config", path},
		{"node", "join", "worker-1", "--config", path},
		{"management", "identity", "rotate", "--config", path, "--output", filepath.Join(dir, "next.yaml")},
		{"cluster", "etcd", "members", "--config", path},
		{"cluster", "etcd", "snapshot", "--config", path, "--output", filepath.Join(dir, "etcd.snapshot")},
		{"cluster", "etcd", "remove", "worker-1", "--config", path},
		{"kubernetes", "upgrade", "--config", path},
	} {
		t.Run(strings.Join(commandWords(command), " "), func(t *testing.T) {
			err := run(context.Background(), command, new(bytes.Buffer), new(bytes.Buffer))
			if err == nil || !strings.Contains(err.Error(), managementAuthenticationRequiredLine) {
				t.Fatalf("error = %v", err)
			}
		})
	}

	if dials.Load() != 0 || requests.Load() != 0 {
		t.Fatalf("contacted nodes: %d dials, %d requests", dials.Load(), requests.Load())
	}
	if saved, err := os.ReadFile(contextPath); err != nil || string(saved) != "contexts: []\n" {
		t.Fatalf("context file = %q, %v", saved, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "cluster.katlcfg")); !os.IsNotExist(err) {
		t.Fatalf("bundle written: %v", err)
	}
}

// commandWords returns the leading words of a command line, before any flag or
// argument that follows a flag.
func commandWords(command []string) []string {
	for index, word := range command {
		if strings.HasPrefix(word, "-") || strings.Contains(word, string(filepath.Separator)) {
			return command[:index]
		}
	}
	return command
}

// Naming the mode on the command line states it, so a previous configuration
// without the field cannot block the rewrite.
func TestConfigInitForceTrustedNetworkReplacesConfigWithoutMode(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KATLCTL_CONFIG", filepath.Join(dir, "context.yaml"))
	path := filepath.Join(dir, "cluster.yaml")
	if err := run(context.Background(), append(initArgs(t, dir, "--management-authentication", "trusted-network"), path), new(bytes.Buffer), new(bytes.Buffer)); err != nil {
		t.Fatal(err)
	}
	previous, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	previous = bytes.Replace(previous, []byte("    managementAuthentication: trusted-network\n"), nil, 1)
	if err := os.WriteFile(path, previous, 0o644); err != nil {
		t.Fatal(err)
	}

	err = run(context.Background(), append(initArgs(t, dir, "--force", "--management-authentication", "trusted-network"), path), new(bytes.Buffer), new(bytes.Buffer))
	if err != nil {
		t.Fatal(err)
	}

	if source := readInitSource(t, path); source.Spec.ManagementAuthentication != "trusted-network" || source.Spec.ManagementIdentity != "" {
		t.Fatalf("spec = authentication %q, identity %q", source.Spec.ManagementAuthentication, source.Spec.ManagementIdentity)
	}
}

func TestConfigInitForceTrustedNetworkOverMutualTLSWarns(t *testing.T) {
	const notice = "the replaced configuration selected mTLS management"
	dir := t.TempDir()
	t.Setenv("KATLCTL_CONFIG", filepath.Join(dir, "context.yaml"))
	path := filepath.Join(dir, "cluster.yaml")
	if err := run(context.Background(), append(initArgs(t, dir), path), new(bytes.Buffer), new(bytes.Buffer)); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer

	if err := run(context.Background(), append(initArgs(t, dir, "--force", "--management-authentication", "trusted-network"), path), new(bytes.Buffer), &stderr); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(stderr.String(), notice) || !strings.Contains(stderr.String(), "katlctl management enable-mtls") {
		t.Fatalf("stderr = %q, want mode-change notice", stderr.String())
	}
	if source := readInitSource(t, path); source.Spec.ManagementAuthentication != "trusted-network" {
		t.Fatalf("authentication = %q", source.Spec.ManagementAuthentication)
	}

	stderr.Reset()
	if err := run(context.Background(), append(initArgs(t, dir, "--force", "--management-authentication", "trusted-network"), path), new(bytes.Buffer), &stderr); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stderr.String(), notice) {
		t.Fatalf("stderr = %q, notice printed without a mode change", stderr.String())
	}
}

// A trusted-network client reaching an mTLS node over an address-only
// endpoint must be told to select the configuration, not see a bare gRPC
// connection error.
func TestTrustedNetworkClientExplainsMutualTLSNode(t *testing.T) {
	now := time.Now().UTC()
	identity, err := managementidentity.Generate(managementidentity.GenerateOptions{ClusterName: "lab", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	node, _, err := managementidentity.EnsureNode(&identity, "cp-1", now, nil)
	if err != nil {
		t.Fatal(err)
	}
	serverTLS, _, err := transport.ServerTLSConfigForNode(node)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS)))
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctx = withManagementDial(ctx, "cp-1", &managementidentity.ClientCredentials{Authentication: managementidentity.TrustedNetwork})
	conn, err := dialKatlcAgentTCP(ctx, listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	_, err = conn.Client.GetNodeStatus(ctx, &agentapi.GetNodeStatusRequest{})

	if err == nil || !strings.Contains(err.Error(), "likely serves mTLS") || !strings.Contains(err.Error(), "--config") || !strings.Contains(err.Error(), "rerun") || !strings.Contains(err.Error(), "katlctl management enable-mtls") {
		t.Fatalf("error = %v", err)
	}
}

// An unusable saved config must fail target resolution for an address-only
// call, not fall back to a plaintext trusted-network target.
func TestEndpointRefusesUnusableSavedConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "context.yaml")
	data, err := yaml.Marshal(workstation.Config{
		CurrentContext: "lab",
		Contexts:       []workstation.Context{{Name: "lab", Cluster: "lab"}},
		Clusters: []workstation.Cluster{{
			Name:       "lab",
			Nodes:      []workstation.Node{{Name: "cp-1", ManagementEndpoint: "10.0.0.1:9443", EnrollmentID: "enroll-1"}},
			Management: &managementidentity.ClientCredentials{Authentication: managementidentity.MutualTLS},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KATLCTL_CONFIG", path)

	_, err = resolveManagementTarget(context.Background(), managementTargetOptions{nodeName: "cp-1", endpoint: "127.0.0.1:9443"})

	if err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("error = %v, want the permission error with its remedy", err)
	}
}
