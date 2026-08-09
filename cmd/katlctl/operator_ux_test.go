package main

import (
	"bytes"
	"context"
	"errors"
	"io"
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
	if source.Metadata.Name != "homelab" || configbundle.SourceControlPlaneEndpoint(source) != "" || source.Spec.Kubernetes.Version != configbundle.DefaultKubernetesVersion || len(source.Spec.Nodes) != 2 {
		t.Fatalf("generated source = %#v", source)
	}
	if got := source.Spec.Nodes[0].Management.Address; got != "192.0.2.11" {
		t.Fatalf("generated management address = %q", got)
	}
	if !source.Spec.Nodes[0].ControlPlane || source.Spec.Nodes[1].ControlPlane {
		t.Fatalf("generated control-plane choices = %#v", source.Spec.Nodes)
	}
	rendered := stdout.String()
	for _, internalDefault := range []string{"katlosImage:", "wipeTarget:", "systemRoleDefaults:", "kubeadmConfigs:", "nodeClasses:", "overrides:", "bundle:", "catalogRef:", "hostname:"} {
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
	if configbundle.SourceControlPlaneEndpoint(source) != "api.home.arpa:6443" || source.Spec.Kubernetes.Version != "v1.36.2" {
		t.Fatalf("explicit intent = %#v", source.Spec)
	}
}

func TestConfigInitRejectsEmptyKubernetesVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{
		"config", "init",
		"--kubernetes-version", "",
		"--node", "cp-1=control-plane,192.0.2.11,/dev/disk/by-id/ata-cp-root",
	}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "--kubernetes-version is required") {
		t.Fatalf("run() error = %v, want required concrete Kubernetes version", err)
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
	keys, _ := source.Spec.Defaults.Access.SSH.AuthorizedKeys.Get()
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
	keys, _ := source.Spec.Defaults.Access.SSH.AuthorizedKeys.Get()
	if len(keys) != 0 {
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
	if _, _, err := ensureManagementIdentity("lab", io.Discard); err != nil {
		t.Fatal(err)
	}
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
	if topology.Management == nil {
		t.Fatal("saved context has no automatic management credentials")
	}
	identityPath, err := managementIdentityPath("lab")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := readManagementIdentity(identityPath)
	if err != nil {
		t.Fatal(err)
	}
	if topology.Management.ClientPrivateKey != identity.Operator.PrivateKey || topology.Management.ClientCertificate != identity.Operator.Certificate {
		t.Fatal("saved context did not retain the issued operator identity")
	}
	for name, secret := range map[string]string{"CA private key": identity.CertificateAuthority.PrivateKey, "node private key": identity.Nodes["cp-1"].PrivateKey} {
		if topology.Management.ClientPrivateKey == secret {
			t.Fatalf("saved context uses %s as its client identity", name)
		}
	}
	fake.nodeStatus = &agentapi.NodeStatus{InventoryNodeName: "cp-1", EnrollmentId: "replacement-enrollment", MachineId: "replacement-machine"}
	err = run(context.Background(), []string{"context", "save", "--config", sourcePath, "--context-file", configPath}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "--replace-node cp-1") {
		t.Fatalf("replacement refusal = %v", err)
	}
	unchanged, err := workstation.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	unchangedTopology, err := unchanged.SelectedTopology("lab")
	if err != nil {
		t.Fatal(err)
	}
	if unchangedTopology.Nodes[0].EnrollmentID == "replacement-enrollment" {
		t.Fatal("unacknowledged replacement changed the saved context")
	}
	var replacementOutput bytes.Buffer
	if err := run(context.Background(), []string{"context", "save", "--config", sourcePath, "--context-file", configPath, "--replace-node", "cp-1"}, &replacementOutput, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(replacementOutput.String(), "replaced enrollment for cp-1") {
		t.Fatalf("replacement output = %q", replacementOutput.String())
	}
	replaced, err := workstation.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	replacedTopology, err := replaced.SelectedTopology("lab")
	if err != nil {
		t.Fatal(err)
	}
	if replacedTopology.Nodes[0].EnrollmentID != "replacement-enrollment" || replacedTopology.Nodes[0].MachineID != "replacement-machine" {
		t.Fatalf("replacement topology = %#v", replacedTopology.Nodes[0])
	}
	err = run(context.Background(), []string{"context", "save", "--config", sourcePath, "--context-file", configPath, "--replace-node", "cp-1"}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "saved enrollment has not changed") {
		t.Fatalf("redundant replacement error = %v", err)
	}
	var shown bytes.Buffer
	if err := run(context.Background(), []string{"context", "show", "--context-file", configPath, "--output", "json"}, &shown, io.Discard); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(shown.Bytes(), []byte("PRIVATE KEY")) || bytes.Contains(shown.Bytes(), []byte("clientPrivateKey")) || bytes.Contains(shown.Bytes(), []byte("clientCertificate")) {
		t.Fatalf("context show exposed management credentials:\n%s", shown.String())
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
		nodeStatus:     &agentapi.NodeStatus{MachineId: "machine-cp-1", EnrollmentId: "enrollment-cp-1", InventoryNodeName: "cp-1", CurrentGenerationId: "generation-0"},
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
