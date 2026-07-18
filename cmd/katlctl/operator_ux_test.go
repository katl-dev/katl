package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/installer/configapply"
	"github.com/katl-dev/katl/internal/installer/configbundle"
	"github.com/katl-dev/katl/internal/installer/operation"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/katlctl/workstation"
)

const uxTestSSHKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDAxMjM0NTY3ODlhYmNkZWYwMTIzNDU2Nzg5YWJjZGVm katl@example"

func TestConfigInitEmitsStarterClusterConfig(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id_ed25519.pub")
	if err := os.WriteFile(keyPath, []byte(uxTestSSHKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KATLCTL_CONFIG", filepath.Join(dir, "katlctl.yaml"))

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{
		"config", "init",
		"--name", "homelab",
		"--ssh-authorized-key", keyPath,
		"--node", "cp-1=control-plane,192.0.2.11,/dev/disk/by-id/ata-cp-root",
		"--node", "worker-1=worker,192.0.2.21,/dev/disk/by-id/ata-worker-root",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run() error = %v\nstderr=%s", err, stderr.String())
	}
	source, err := configbundle.DecodeSource(bytes.NewReader(stdout.Bytes()))
	if err != nil {
		t.Fatalf("DecodeSource() error = %v\n%s", err, stdout.String())
	}
	if source.Metadata.Name != "homelab" || source.Spec.ControlPlaneEndpoint != "" || source.Spec.Kubernetes.Version != configbundle.DefaultKubernetesVersion || len(source.Spec.Nodes) != 2 {
		t.Fatalf("generated source = %#v", source)
	}
	if got := source.Spec.Nodes[0].Bootstrap.Address; got != "192.0.2.11" {
		t.Fatalf("generated bootstrap address = %q", got)
	}
	if !source.Spec.Nodes[0].ControlPlane || source.Spec.Nodes[1].ControlPlane {
		t.Fatalf("generated control-plane choices = %#v", source.Spec.Nodes)
	}
	rendered := stdout.String()
	for _, internalDefault := range []string{"katlosImage:", "wipeTarget:", "systemRoleDefaults:", "kubeadmConfigs:", "nodeClasses:", "overrides:", "bundle:", "catalogRef:", "hostname:", "access:"} {
		if strings.Contains(rendered, internalDefault) {
			t.Fatalf("generated config contains internal default %q:\n%s", internalDefault, rendered)
		}
	}
	for _, guidance := range []string{"# controlPlaneEndpoint:", "# Set controlPlane: true", "# Nodes use DHCP by default"} {
		if !strings.Contains(rendered, guidance) {
			t.Fatalf("generated config is missing guidance %q:\n%s", guidance, rendered)
		}
	}
}

func TestConfigInitRendersExplicitIntent(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id_ed25519.pub")
	if err := os.WriteFile(keyPath, []byte(uxTestSSHKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{
		"config", "init",
		"--ssh-authorized-key", keyPath,
		"--control-plane-endpoint", "api.home.arpa:6443",
		"--kubernetes-version", "v1.36.2",
		"--node", "cp-1=control-plane,192.0.2.11,/dev/disk/by-id/ata-cp-root",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run() error = %v\nstderr=%s", err, stderr.String())
	}
	source, err := configbundle.DecodeSource(bytes.NewReader(stdout.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if source.Spec.ControlPlaneEndpoint != "api.home.arpa:6443" || source.Spec.Kubernetes.Version != "v1.36.2" {
		t.Fatalf("explicit intent = %#v", source.Spec)
	}
}

func TestConfigInitUsesSSHAgentKeys(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("KATLCTL_CONFIG", filepath.Join(dir, "katlctl.yaml"))
	oldAgent := sshAgentPublicKeys
	sshAgentPublicKeys = func() ([]byte, error) {
		return []byte(uxTestSSHKey + "\n" + uxTestSSHKey + "\n"), nil
	}
	t.Cleanup(func() { sshAgentPublicKeys = oldAgent })

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{
		"config", "init",
		"--node", "cp-1=control-plane,192.0.2.11,/dev/disk/by-id/ata-cp-root",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run() error = %v\nstderr=%s", err, stderr.String())
	}
	source, err := configbundle.DecodeSource(bytes.NewReader(stdout.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	keys := source.Spec.Defaults.Identity.SSH.AuthorizedKeys
	if len(keys) != 1 || keys[0] != uxTestSSHKey {
		t.Fatalf("authorized keys = %#v", keys)
	}
	if !strings.Contains(stderr.String(), "using 1 SSH public key(s) from the active SSH agent") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestConfigInitWithoutSSHKeysWritesEditableConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("KATLCTL_CONFIG", filepath.Join(dir, "katlctl.yaml"))
	oldAgent := sshAgentPublicKeys
	sshAgentPublicKeys = func() ([]byte, error) { return nil, errors.New("no agent") }
	t.Cleanup(func() { sshAgentPublicKeys = oldAgent })

	outputPath := filepath.Join(dir, "cluster.yaml")
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{
		"config", "init", outputPath,
		"--node", "cp-1=control-plane,192.0.2.11,/dev/disk/by-id/ata-cp-root",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run() error = %v\nstderr=%s", err, stderr.String())
	}
	data, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	source, err := configbundle.DecodeSource(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if keys := source.Spec.Defaults.Identity.SSH.AuthorizedKeys; len(keys) != 0 {
		t.Fatalf("authorized keys = %#v", keys)
	}
	if !strings.Contains(stderr.String(), "generated ClusterConfig has no SSH authorized keys") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if !strings.Contains(string(data), "# defaults:") || !strings.Contains(string(data), "#             authorizedKeys:") {
		t.Fatalf("generated config has no commented SSH key guidance:\n%s", data)
	}
}

func TestConfigInitExplicitSSHKeyDoesNotFallBack(t *testing.T) {
	oldAgent := sshAgentPublicKeys
	sshAgentPublicKeys = func() ([]byte, error) {
		t.Fatal("SSH agent was queried for an explicit key path")
		return nil, nil
	}
	t.Cleanup(func() { sshAgentPublicKeys = oldAgent })

	_, _, err := configSSHKeys(filepath.Join(t.TempDir(), "missing.pub"))
	if err == nil || !strings.Contains(err.Error(), "read SSH public key") {
		t.Fatalf("configSSHKeys() error = %v", err)
	}
}

func TestContextSaveCreatesReachableContext(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "katlctl.yaml")
	sourcePath := writeClusterConfig(t)
	fake := &fakeKatlcAgentClient{nodeStatus: &agentapi.NodeStatus{MachineId: "machine-cp-1"}}
	oldDial := dialKatlcAgent
	dialKatlcAgent = func(_ context.Context, endpoint string) (katlcAgentConnection, error) {
		if endpoint != "10.0.0.11:9443" {
			t.Fatalf("dial endpoint=%q", endpoint)
		}
		return katlcAgentConnection{Client: fake, Close: func() error { return nil }}, nil
	}
	t.Cleanup(func() { dialKatlcAgent = oldDial })

	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"context", "save", "--config", sourcePath, "--context-file", configPath}, &stdout, &stderr); err != nil {
		t.Fatalf("run() error = %v\nstderr=%s", err, stderr.String())
	}
	cfg, err := workstation.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	topology, err := cfg.SelectedTopology("lab")
	if err != nil {
		t.Fatal(err)
	}
	if len(topology.Nodes) != 1 || topology.Nodes[0].ManagementEndpoint != "10.0.0.11:9443" {
		t.Fatalf("topology = %#v", topology)
	}
}

func TestContextListCurrentAndUse(t *testing.T) {
	path := writeKatlctlConfig(t, `currentContext: lab
contexts:
- name: lab
  cluster: lab
- name: stage
  cluster: stage
clusters:
- name: lab
  nodes:
  - name: cp-1
    managementEndpoint: 192.0.2.11:9443
    systemRole: control-plane
- name: stage
  nodes:
  - name: cp-1
    managementEndpoint: 192.0.2.21:9443
    systemRole: control-plane
`)

	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"context", "list", "--context-file", path}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(stdout.String())
	if !slices.Contains(fields, "*") || !slices.Contains(fields, "lab") || !slices.Contains(fields, "stage") {
		t.Fatalf("context list = %q", stdout.String())
	}
	stdout.Reset()
	if err := run(context.Background(), []string{"context", "use", "stage", "--context-file", path}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if err := run(context.Background(), []string{"context", "current", "--context-file", path}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(stdout.String()); got != "stage" {
		t.Fatalf("current context = %q", got)
	}
}

func TestContextMissingFileExplainsHowToCreateOne(t *testing.T) {
	t.Setenv("KATLCTL_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{"context", "list"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "katlctl context save --config cluster.yaml") {
		t.Fatalf("error = %v", err)
	}
}

func TestConfigApplyUsesClusterConfigAndDerivesBookkeeping(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "katlctl.yaml")
	cfg := workstation.Config{CurrentContext: "lab", Contexts: []workstation.Context{{Name: "lab", Cluster: "lab"}}, Clusters: []workstation.Cluster{{
		Name: "lab", Nodes: []workstation.Node{{Name: "cp-1", ManagementEndpoint: "10.0.0.11:9443", SystemRole: inventory.RoleControlPlane}},
	}}}
	if err := workstation.Save(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	fake := &fakeKatlcAgentClient{
		validateResult: &agentapi.ConfigValidationResult{Accepted: true, AcceptedApplyMode: "live"},
		stageAccepted:  &agentapi.OperationAccepted{OperationId: "apply-1", OperationKind: "generation-apply", InitialStatus: &agentapi.OperationStatus{Terminal: true, Result: operation.ResultSucceeded}},
	}
	oldDial := dialKatlcAgent
	dialKatlcAgent = func(_ context.Context, endpoint string) (katlcAgentConnection, error) {
		if endpoint != "10.0.0.11:9443" {
			t.Fatalf("dial endpoint=%q", endpoint)
		}
		return katlcAgentConnection{Client: fake, Close: func() error { return nil }}, nil
	}
	t.Cleanup(func() { dialKatlcAgent = oldDial })
	oldNow := configApplyNow
	configApplyNow = func() time.Time { return time.Unix(0, 42).UTC() }
	t.Cleanup(func() { configApplyNow = oldNow })

	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"node", "apply", "cp-1", "--config", writeClusterConfig(t), "--output", "json"}, &stdout, &stderr); err != nil {
		t.Fatalf("run() error = %v\nstderr=%s", err, stderr.String())
	}
	if fake.validateRequest == nil || fake.validateRequest.CandidateGenerationId != "config-42" {
		t.Fatalf("validate request = %#v", fake.validateRequest)
	}
	change, err := configapply.DecodeNodeConfigurationChange(strings.NewReader(fake.validateRequest.ConfigYaml), configapply.TrustedBundleRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if change.DesiredVersion != "42" {
		t.Fatalf("desired version = %q", change.DesiredVersion)
	}
}
