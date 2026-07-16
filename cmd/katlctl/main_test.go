package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/bootstrap/cluster"
	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/bootstrap/readiness"
	"github.com/katl-dev/katl/internal/installer/configapply"
	"github.com/katl-dev/katl/internal/installer/configbundle"
	"github.com/katl-dev/katl/internal/installer/generation"
	"github.com/katl-dev/katl/internal/installer/operation"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/katlctl/workstation"
	"github.com/katl-dev/katl/internal/vmtest"
	vmtestpb "github.com/katl-dev/katl/internal/vmtest/proto"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
)

func TestVersion(t *testing.T) {
	oldVersion, oldCommit, oldDate := version, commit, date
	version, commit, date = "dev", "abc123", "2026-06-05T00:00:00Z"
	t.Cleanup(func() {
		version, commit, date = oldVersion, oldCommit, oldDate
	})

	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"--version"}, &stdout, &stderr); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if got, want := stdout.String(), "katlctl version=dev commit=abc123 date=2026-06-05T00:00:00Z\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if err := run(context.Background(), []string{"version"}, &stdout, &stderr); err != nil {
		t.Fatalf("run(version) error = %v", err)
	}
	if got, want := stdout.String(), "katlctl version=dev commit=abc123 date=2026-06-05T00:00:00Z\n"; got != want {
		t.Fatalf("version stdout = %q, want %q", got, want)
	}
	if stderr.Len() != 0 {
		t.Fatalf("version stderr = %q, want empty", stderr.String())
	}
}

func TestRootHelpShowsCommandGroups(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"--help"}, &stdout, &stderr); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	out := stdout.String()
	for _, want := range []string{
		"katlctl installs and manages KatlOS nodes",
		"cluster     Cluster lifecycle operations",
		"config      Create and compile ClusterConfig",
		"context     Inspect enrolled workstation context",
		"node        Manage individual KatlOS nodes",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout = %q, missing %q", out, want)
		}
	}
	if strings.Contains(out, "completion") {
		t.Fatalf("stdout = %q, want no implicit completion command", out)
	}
	for _, obsolete := range []string{"\n  host ", "\n  wipe "} {
		if strings.Contains(out, obsolete) {
			t.Fatalf("stdout = %q, contains obsolete top-level command %q", out, obsolete)
		}
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestCommandGroupsExposeOneSupportedPath(t *testing.T) {
	tests := []struct {
		group     string
		required  []string
		forbidden []string
	}{
		{group: "node", required: []string{"apply", "reboot", "status", "upgrade", "wipe"}},
		{group: "cluster", required: []string{"bootstrap", "enroll", "wipe"}, forbidden: []string{"kubeadm-control-plane-config"}},
		{group: "kubernetes", required: []string{"apply-config", "upgrade"}},
		{group: "config", required: []string{"bundle", "init", "schema", "validate"}, forbidden: []string{"apply", "path", "topology"}},
		{group: "context", required: []string{"path", "show"}},
	}
	for _, test := range tests {
		t.Run(test.group, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if err := run(context.Background(), []string{test.group, "--help"}, &stdout, &stderr); err != nil {
				t.Fatalf("run() error = %v", err)
			}
			out := stdout.String()
			for _, command := range test.required {
				if !strings.Contains(out, command) {
					t.Fatalf("stdout = %q, missing command %q", out, command)
				}
			}
			for _, command := range test.forbidden {
				if strings.Contains(out, command) {
					t.Fatalf("stdout = %q, contains obsolete command %q", out, command)
				}
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}
		})
	}
}

func TestRootWithoutArgumentsPrintsHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), nil, &stdout, &stderr); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if !strings.Contains(stdout.String(), "Usage:") || !strings.Contains(stdout.String(), "katlctl install discover") {
		t.Fatalf("stdout = %q, want useful root help", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestOutputFormatValidation(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{"context", "show", "--output", "yaml"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), `--output = "yaml", want json`) {
		t.Fatalf("run() error = %v, want output validation", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
}

func TestConfigPathUsesXDGDefault(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("KATLCTL_CONFIG", "")
	t.Setenv("KATLCTL_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", configHome)

	path, err := workstationConfigPath()
	if err != nil {
		t.Fatalf("workstationConfigPath() error = %v", err)
	}
	if want := filepath.Join(configHome, "katl", "katlctl.yaml"); path != want {
		t.Fatalf("workstationConfigPath() = %q, want %q", path, want)
	}
}

func TestConfigPathEnvOverrides(t *testing.T) {
	configHome := t.TempDir()
	configDir := filepath.Join(t.TempDir(), "katlctl-config")
	configFile := filepath.Join(t.TempDir(), "custom.yaml")
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("KATLCTL_CONFIG_DIR", configDir)
	t.Setenv("KATLCTL_CONFIG", "")

	path, err := workstationConfigPath()
	if err != nil {
		t.Fatalf("workstationConfigPath() error = %v", err)
	}
	if want := filepath.Join(configDir, "katlctl.yaml"); path != want {
		t.Fatalf("workstationConfigPath() = %q, want %q", path, want)
	}

	t.Setenv("KATLCTL_CONFIG", configFile)
	path, err = workstationConfigPath()
	if err != nil {
		t.Fatalf("workstationConfigPath() with file override error = %v", err)
	}
	if path != configFile {
		t.Fatalf("workstationConfigPath() = %q, want %q", path, configFile)
	}
}

func TestConfigPathCommandPrintsResolvedPath(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("KATLCTL_CONFIG", "")
	t.Setenv("KATLCTL_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", configHome)

	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"context", "path"}, &stdout, &stderr); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if got, want := strings.TrimSpace(stdout.String()), filepath.Join(configHome, "katl", "katlctl.yaml"); got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestConfigBundleCommandWritesBundle(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "cluster.yaml")
	outputPath := filepath.Join(dir, "homelab.katlcfg")
	if err := os.WriteFile(sourcePath, []byte(configBundleSource()), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"config", "bundle", sourcePath, "--output", outputPath}, &stdout, &stderr); err != nil {
		t.Fatalf("run() error = %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	info, err := os.Stat(outputPath)
	if err != nil {
		t.Fatalf("stat output: %v", err)
	}
	if info.Size() == 0 {
		t.Fatalf("output bundle is empty")
	}
	var report struct {
		Kind   string `json:"kind"`
		Output string `json:"output"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode stdout: %v\n%s", err, stdout.String())
	}
	if report.Kind != "ConfigBundleReport" || report.Output != outputPath || strings.Contains(stdout.String(), "Digest") {
		t.Fatalf("report = %#v", report)
	}
}

func TestConfigBundleCommandBindsPXEImageAsOperationInput(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "cluster.yaml")
	metadataPath := filepath.Join(dir, "katlos-install.squashfs.json")
	outputPath := filepath.Join(dir, "homelab.katlcfg")
	if err := os.WriteFile(sourcePath, []byte(configBundleSource()), 0o644); err != nil {
		t.Fatal(err)
	}
	metadata := `{"apiVersion":"katl.dev/v1alpha1","kind":"KatlOSImageArtifact","imageRole":"install","format":"squashfs","version":"2026.7.0-test","architecture":"x86_64","runtimeInterface":"katl-runtime-1","sizeBytes":1234,"sha256":"` + strings.Repeat("a", 64) + `"}`
	if err := os.WriteFile(metadataPath, []byte(metadata), 0o644); err != nil {
		t.Fatal(err)
	}
	imageURL := "https://boot.example.test/katlos-install.squashfs"
	if err := run(context.Background(), []string{
		"config", "bundle", sourcePath,
		"--output", outputPath,
		"--katlos-image-url", imageURL,
		"--katlos-image-metadata", metadataPath,
	}, io.Discard, io.Discard); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	selected, err := configbundle.ReadSelectedNodeFile(outputPath, configbundle.ReadOptions{NodeName: "cp-1"})
	if err != nil {
		t.Fatalf("ReadSelectedNodeFile() error = %v", err)
	}
	if selected.InstallManifest.KatlosImage.URL != imageURL || selected.InstallManifest.KatlosImage.Version != "2026.7.0-test" {
		t.Fatalf("PXE image = %#v", selected.InstallManifest.KatlosImage)
	}
}

func TestConfigRenderNodeFromSource(t *testing.T) {
	sourcePath := filepath.Join(t.TempDir(), "cluster.yaml")
	if err := os.WriteFile(sourcePath, []byte(configBundleSource()), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{
		"config", "render-node",
		"--source", sourcePath,
		"--node", "cp-1",
		"--desired-version", "2",
	}, &stdout, &stderr); err != nil {
		t.Fatalf("run() error = %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	request, err := configapply.DecodeNodeConfigurationChange(strings.NewReader(stdout.String()), configapply.TrustedBundleRequest{})
	if err != nil {
		t.Fatalf("decode rendered node config: %v\n%s", err, stdout.String())
	}
	if request.SourceID != "lab" || request.DesiredVersion != "2" || request.ApplyMode != generation.ApplyModeAuto {
		t.Fatalf("rendered request metadata = %#v", request)
	}
	overlay := request.NodeOverrides["cp-1"]
	if overlay.Identity == nil || overlay.Identity.Hostname != "cp-1" || len(overlay.Identity.AuthorizedKeys) != 1 {
		t.Fatalf("rendered node overlay = %#v", overlay)
	}
	if overlay.SystemRole != "control-plane" || overlay.Kubernetes == nil || overlay.Kubernetes.Kubeadm.ConfigRef != "control-plane" {
		t.Fatalf("rendered node overlay operation state = %#v", overlay)
	}
}

func TestConfigRenderNodeFromMediaBundle(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "cluster.yaml")
	if err := os.WriteFile(sourcePath, []byte(configBundleSource()), 0o644); err != nil {
		t.Fatal(err)
	}
	archive, _, err := configbundle.BuildArchive(configbundle.BuildRequest{SourcePath: sourcePath})
	if err != nil {
		t.Fatalf("BuildArchive() error = %v", err)
	}
	bundlePath := filepath.Join(dir, "cluster.katlcfg")
	if err := os.WriteFile(bundlePath, archive, 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{
		"config", "render-node",
		"--config-bundle", bundlePath,
		"--node", "cp-1",
		"--desired-version", "2",
	}, &stdout, &stderr); err != nil {
		t.Fatalf("run() error = %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	request, err := configapply.DecodeNodeConfigurationChange(strings.NewReader(stdout.String()), configapply.TrustedBundleRequest{})
	if err != nil {
		t.Fatalf("decode rendered node config: %v\n%s", err, stdout.String())
	}
	if request.SourceID != "lab" || request.DesiredVersion != "2" {
		t.Fatalf("rendered request metadata = %#v", request)
	}
}

func TestConfigValidateResolvesWithoutWriting(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "cluster.yaml")
	outputPath := filepath.Join(dir, "homelab.katlcfg")
	if err := os.WriteFile(sourcePath, []byte(configBundleSource()), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"config", "validate", sourcePath}, &stdout, &stderr); err != nil {
		t.Fatalf("run() error = %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read temp directory: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "cluster.yaml" {
		t.Fatalf("validation wrote files: %#v", entries)
	}
	var report configValidationReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode stdout: %v\n%s", err, stdout.String())
	}
	if report.Kind != "ClusterConfigValidation" || report.ClusterName != "lab" || report.Source != sourcePath {
		t.Fatalf("report = %#v", report)
	}
	if strings.Contains(stdout.String(), "Digest") || strings.Contains(stdout.String(), "artifactVersion") {
		t.Fatalf("report exposes integrity plumbing = %s", stdout.String())
	}
	if len(report.Nodes) != 1 || report.Nodes[0] != (configValidationNode{Name: "cp-1", SystemRole: "control-plane"}) {
		t.Fatalf("resolved nodes = %#v", report.Nodes)
	}

	stdout.Reset()
	if err := run(context.Background(), []string{"config", "bundle", sourcePath, "--output", outputPath}, &stdout, &stderr); err != nil {
		t.Fatalf("bundle run() error = %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	var bundleReport configBundleReport
	if err := json.Unmarshal(stdout.Bytes(), &bundleReport); err != nil {
		t.Fatalf("decode bundle stdout: %v\n%s", err, stdout.String())
	}
	if bundleReport.Output != outputPath || strings.Contains(stdout.String(), "Digest") {
		t.Fatalf("bundle report = %#v\n%s", bundleReport, stdout.String())
	}
}

func TestConfigValidateReportsNestedFieldPath(t *testing.T) {
	sourcePath := filepath.Join(t.TempDir(), "cluster.yaml")
	source := strings.Replace(configBundleSource(), "targetDisk:\n          byID:", "targetDisk:\n          unsupportedSelector: true\n          byID:", 1)
	if err := os.WriteFile(sourcePath, []byte(source), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{"config", "validate", sourcePath}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "spec.nodes[0].install.targetDisk.unsupportedSelector: field is not supported") {
		t.Fatalf("run() error = %v, want nested field path", err)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("stdout=%q stderr=%q, want empty", stdout.String(), stderr.String())
	}
}

func TestConfigSchemaCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"config", "schema"}, &stdout, &stderr); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	var schema struct {
		ID    string `json:"$id"`
		Title string `json:"title"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &schema); err != nil {
		t.Fatalf("decode schema: %v", err)
	}
	if schema.ID != "https://katl.dev/schemas/config.katl.dev/v1alpha1/cluster-config.json" || schema.Title != "config.katl.dev/v1alpha1 ClusterConfig" {
		t.Fatalf("schema identity = %#v", schema)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestLoadInventoryPreservesKubernetesBundleSelection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inventory.yaml")
	bundleRef := "ghcr.io/katl-dev/kubernetes:v1.36.1-katl.1@sha256:" + strings.Repeat("a", 64)
	if err := os.WriteFile(path, []byte(`
controlPlaneEndpoint: api.katl.test:6443
kubernetesVersion: v1.36.1
kubernetesBundle: `+bundleRef+`
nodes:
- name: cp-1
  address: 192.0.2.10
  systemRole: control-plane
  access:
    method: katlc-agent
    user: root
    credentialRef: file:/tmp/token
  kubeadmConfig:
    ref: control-plane
    path: /etc/katl/kubeadm/control-plane/config.yaml
    intent: init
`), 0o600); err != nil {
		t.Fatalf("write inventory: %v", err)
	}
	inv, err := loadInventory(path)
	if err != nil {
		t.Fatalf("loadInventory() error = %v", err)
	}
	if inv.KubernetesBundleSource != "https://ghcr.io/v2/katl-dev/kubernetes" || inv.KubernetesBundleRef != bundleRef {
		t.Fatalf("bundle selection = %q %q", inv.KubernetesBundleSource, inv.KubernetesBundleRef)
	}
}

func TestConfigTopologyCommandPrintsResolvedContext(t *testing.T) {
	configPath := writeKatlctlConfig(t, `currentContext: prod
contexts:
- name: prod
  cluster: katl-prod
- name: stage
  cluster: katl-stage
clusters:
- name: katl-prod
  controlPlaneEndpoint: api.prod.test:6443
  nodes:
  - name: cp-1
    managementEndpoint: cp-1.prod.test:9443
    systemRole: control-plane
    credentialRef: file:/secure/katl/cp-1.token
- name: katl-stage
  nodes:
  - name: stage-cp
    managementEndpoint: stage-cp.test:9443
    systemRole: control-plane
    credentialRef: file:/secure/katl/stage-cp.token
`)
	t.Setenv("KATLCTL_CONFIG", configPath)
	t.Setenv("KATLCTL_CONFIG_DIR", "")

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{
		"context", "show",
		"--context", "stage",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run() error = %v, stderr = %s", err, stderr.String())
	}
	var resolved workstation.ResolvedTopology
	if err := json.Unmarshal(stdout.Bytes(), &resolved); err != nil {
		t.Fatalf("decode topology: %v\n%s", err, stdout.String())
	}
	if resolved.Source != workstation.SourceConfigContext || resolved.ContextName != "stage" || resolved.ClusterName != "katl-stage" {
		t.Fatalf("topology = %#v", resolved)
	}
	if len(resolved.Nodes) != 1 || resolved.Nodes[0].Name != "stage-cp" || resolved.Nodes[0].ManagementEndpoint != "stage-cp.test:9443" {
		t.Fatalf("nodes = %#v", resolved.Nodes)
	}
	if resolved.Nodes[0].CredentialRef != "file:/secure/katl/stage-cp.token" {
		t.Fatalf("credential ref = %q", resolved.Nodes[0].CredentialRef)
	}
}

func TestClusterBootstrapParsesFlagsAndPrintsNextStep(t *testing.T) {
	inventoryPath := writeInventory(t)
	var got cluster.Request
	var gotDeps cluster.Dependencies
	old := runBootstrap
	runBootstrap = func(_ context.Context, request cluster.Request, deps cluster.Dependencies) (cluster.Result, error) {
		got = request
		gotDeps = deps
		return cluster.Result{
			Plan: inventory.Plan{
				InitNode: "cp-1",
				AddressOverrides: []inventory.AddressOverride{{
					Node:    "worker-1",
					Before:  "10.0.0.21",
					Address: "10.0.0.22",
				}},
				Nodes: []inventory.PlannedNode{{Name: "cp-1"}},
			},
			Phases: []cluster.Phase{
				{Name: "plan", Status: "passed"},
				{Name: "dry-run", Status: "passed"},
			},
			NextStep: "kubectl --kubeconfig out.conf get nodes",
		}, nil
	}
	t.Cleanup(func() { runBootstrap = old })

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{
		"cluster", "bootstrap",
		"--inventory", inventoryPath,
		"--init-node", "cp-1",
		"--node-address", "worker-1=10.0.0.22",
		"--control-plane-endpoint", "api.override.test:6443",
		"--kubeconfig-out", "out.conf",
		"--overwrite-kubeconfig",
		"--dry-run",
		"--vmtest-transcript-dir", "artifacts/transcripts",
		"--bootstrap-manifest", "01-cni.yaml",
		"--bootstrap-manifest", "02-flux.yaml",
		"--bootstrap-pre-wait", "nodes-ready",
		"--bootstrap-wait", "api-ready",
		"--bootstrap-wait", "resource-exists:kube-system:daemonset/cilium",
		"--bootstrap-wait", "rollout-status:kube-system:daemonset/cilium",
		"--bootstrap-wait", "pods-ready:kube-system:k8s-app=kube-dns",
		"--bootstrap-wait", "condition:kube-system:deployment/cilium-operator:Available",
		"--bootstrap-wait", "nodes-ready",
		"--bootstrap-stable-endpoint", "api.stable.test:6443",
		"--bootstrap-stable-endpoint-before-manifests",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run() error = %v, stderr = %s", err, stderr.String())
	}
	if got.InitNode != "cp-1" || got.ControlPlaneEndpoint != "api.override.test:6443" || got.KubeconfigOut != "out.conf" || !got.OverwriteKubeconfig || !got.DryRun {
		t.Fatalf("request = %#v", got)
	}
	if got.Inventory.Nodes[1].Access.CredentialRef != "agent/worker-1" {
		t.Fatalf("inventory = %#v", got.Inventory)
	}
	if got.AddressOverrides["worker-1"] != "10.0.0.22" {
		t.Fatalf("address overrides = %#v", got.AddressOverrides)
	}
	if got.Bootstrap.StableEndpoint != "api.stable.test:6443" {
		t.Fatalf("bootstrap stable endpoint = %q", got.Bootstrap.StableEndpoint)
	}
	if !got.Bootstrap.StableEndpointBeforeManifests {
		t.Fatal("bootstrap stable endpoint before manifests = false")
	}
	if len(got.Bootstrap.Manifests) != 2 || got.Bootstrap.Manifests[0].Path != "01-cni.yaml" || got.Bootstrap.Manifests[1].Path != "02-flux.yaml" {
		t.Fatalf("bootstrap manifests = %#v", got.Bootstrap.Manifests)
	}
	wantPreWaits := []cluster.BootstrapWait{
		{Kind: cluster.BootstrapWaitNodesReady},
	}
	if !reflect.DeepEqual(got.Bootstrap.PreWaits, wantPreWaits) {
		t.Fatalf("bootstrap pre-waits = %#v, want %#v", got.Bootstrap.PreWaits, wantPreWaits)
	}
	wantWaits := []cluster.BootstrapWait{
		{Kind: cluster.BootstrapWaitAPIReady},
		{Kind: cluster.BootstrapWaitResourceExists, Namespace: "kube-system", Name: "daemonset/cilium"},
		{Kind: cluster.BootstrapWaitRolloutStatus, Namespace: "kube-system", Name: "daemonset/cilium"},
		{Kind: cluster.BootstrapWaitPodsReady, Namespace: "kube-system", Selector: "k8s-app=kube-dns"},
		{Kind: cluster.BootstrapWaitCondition, Namespace: "kube-system", Name: "deployment/cilium-operator", Condition: "Available"},
		{Kind: cluster.BootstrapWaitNodesReady},
	}
	if !reflect.DeepEqual(got.Bootstrap.Waits, wantWaits) {
		t.Fatalf("bootstrap waits = %#v, want %#v", got.Bootstrap.Waits, wantWaits)
	}
	runner, ok := gotDeps.NodeRunner.(cluster.TransportRunner)
	if !ok {
		t.Fatalf("NodeRunner = %T", gotDeps.NodeRunner)
	}
	transport, ok := runner.Transport.(vmtestAgentTransport)
	if !ok {
		t.Fatalf("Transport = %T", runner.Transport)
	}
	if transport.TranscriptDir != "artifacts/transcripts" {
		t.Fatalf("TranscriptDir = %q", transport.TranscriptDir)
	}
	if _, ok := gotDeps.BootstrapRunner.(cluster.KubectlBootstrapRunner); !ok {
		t.Fatalf("BootstrapRunner = %T", gotDeps.BootstrapRunner)
	}
	out := stdout.String()
	for _, want := range []string{
		"init-node=cp-1",
		"address-override node=worker-1 before=10.0.0.21 after=10.0.0.22",
		"phase=plan status=passed",
		"next: kubectl --kubeconfig out.conf get nodes",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout = %q, missing %q", out, want)
		}
	}
}

func TestClusterBootstrapDefaultsToAgentBootstrap(t *testing.T) {
	inventoryPath := writeInventory(t)
	tokenPath := filepath.Join(t.TempDir(), "agent.token")
	if err := os.WriteFile(tokenPath, []byte("test-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var got cluster.Request
	var gotDeps cluster.AgentBootstrapDependencies
	old := runAgentBootstrap
	runAgentBootstrap = func(_ context.Context, request cluster.Request, deps cluster.AgentBootstrapDependencies) (cluster.Result, error) {
		got = request
		gotDeps = deps
		return cluster.Result{
			Plan: inventory.Plan{
				InitNode: "cp-1",
				Nodes:    []inventory.PlannedNode{{Name: "cp-1"}},
			},
			Phases:   []cluster.Phase{{Name: "plan", Status: "passed"}},
			NextStep: "katlc agent accepted bootstrap-init",
		}, nil
	}
	t.Cleanup(func() { runAgentBootstrap = old })

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{
		"cluster", "bootstrap",
		"--inventory", inventoryPath,
		"--init-node", "cp-1",
		"--agent-token-file", tokenPath,
		"--node-address", "cp-1=cp-1.override.test",
		"--control-plane-endpoint", "api.override.test:6443",
		"--dry-run",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run() error = %v, stderr = %s", err, stderr.String())
	}
	if got.InitNode != "cp-1" || got.ControlPlaneEndpoint != "api.override.test:6443" || !got.DryRun {
		t.Fatalf("request = %#v", got)
	}
	if got.AddressOverrides["cp-1"] != "cp-1.override.test" {
		t.Fatalf("address overrides = %#v", got.AddressOverrides)
	}
	connector, ok := gotDeps.Connector.(cluster.TCPAgentConnector)
	if !ok {
		t.Fatalf("Connector = %T", gotDeps.Connector)
	}
	if connector.AuthToken != "test-token" {
		t.Fatalf("AuthToken = %q", connector.AuthToken)
	}
	nodeTokenPath := filepath.Join(t.TempDir(), "node-agent.token")
	if err := os.WriteFile(nodeTokenPath, []byte("node-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	token, err := connector.AuthTokenForNode(inventory.PlannedNode{
		Name: "worker-1",
		Access: inventory.Access{
			Method:        "agent",
			CredentialRef: "file:" + nodeTokenPath,
		},
	})
	if err != nil {
		t.Fatalf("AuthTokenForNode() error = %v", err)
	}
	if token != "node-token" {
		t.Fatalf("AuthTokenForNode() = %q", token)
	}
	if gotDeps.Actor != "katlctl cluster bootstrap" {
		t.Fatalf("Actor = %q", gotDeps.Actor)
	}
	if !strings.Contains(stdout.String(), "next: katlc agent accepted bootstrap-init") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestClusterBootstrapJoinsFreshWorkerWithoutInit(t *testing.T) {
	inventoryPath := writeInventory(t)
	oldJoin := runAgentWorkerJoin
	oldBootstrap := runAgentBootstrap
	calledBootstrap := false
	runAgentBootstrap = func(context.Context, cluster.Request, cluster.AgentBootstrapDependencies) (cluster.Result, error) {
		calledBootstrap = true
		return cluster.Result{}, nil
	}
	var gotWorker string
	runAgentWorkerJoin = func(_ context.Context, request cluster.Request, worker string, _ cluster.AgentBootstrapDependencies) (cluster.Result, error) {
		gotWorker = worker
		return cluster.Result{Plan: inventory.Plan{InitNode: request.InitNode}, Phases: []cluster.Phase{{Name: "worker-join", Node: worker, Status: "passed"}}}, nil
	}
	t.Cleanup(func() {
		runAgentWorkerJoin = oldJoin
		runAgentBootstrap = oldBootstrap
	})
	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"cluster", "bootstrap", "--inventory", inventoryPath, "--init-node", "cp-1", "--join-worker", "worker-1"}, &stdout, &stderr); err != nil {
		t.Fatalf("run() error = %v, stderr = %s", err, stderr.String())
	}
	if gotWorker != "worker-1" || calledBootstrap {
		t.Fatalf("worker = %q, full bootstrap called = %v", gotWorker, calledBootstrap)
	}
	if !strings.Contains(stdout.String(), "phase=worker-join node=worker-1 status=passed") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestClusterBootstrapReturnsAgentBootstrapError(t *testing.T) {
	inventoryPath := writeInventory(t)
	old := runAgentBootstrap
	runAgentBootstrap = func(_ context.Context, _ cluster.Request, _ cluster.AgentBootstrapDependencies) (cluster.Result, error) {
		return cluster.Result{
			Plan: inventory.Plan{
				InitNode: "cp-1",
				Nodes:    []inventory.PlannedNode{{Name: "cp-1"}},
			},
			Phases: []cluster.Phase{
				{Name: "plan", Status: "passed"},
				{Name: "readiness", Status: "failed"},
			},
		}, errors.New("cp-1 katlc-agent: operation lock is held")
	}
	t.Cleanup(func() { runAgentBootstrap = old })

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{
		"cluster", "bootstrap",
		"--inventory", inventoryPath,
		"--init-node", "cp-1",
	}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "operation lock is held") {
		t.Fatalf("run() error = %v, want agent bootstrap error", err)
	}
	if !strings.Contains(stdout.String(), "phase=readiness status=failed") {
		t.Fatalf("stdout = %q, want failed readiness phase", stdout.String())
	}
}

func TestClusterBootstrapRequiresInput(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{"cluster", "bootstrap"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "exactly one cluster config SOURCE") {
		t.Fatalf("run() error = %v, want input error", err)
	}
}

func TestClusterBootstrapCompilesSourceInventory(t *testing.T) {
	sourcePath := writeClusterConfig(t)
	var got cluster.Request
	old := runAgentBootstrap
	runAgentBootstrap = func(_ context.Context, request cluster.Request, _ cluster.AgentBootstrapDependencies) (cluster.Result, error) {
		got = request
		return cluster.Result{Plan: inventory.Plan{InitNode: request.InitNode}}, nil
	}
	t.Cleanup(func() { runAgentBootstrap = old })

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{
		"cluster", "bootstrap", sourcePath,
		"--init-node", "cp-1",
		"--dry-run",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run() error = %v, stderr = %s", err, stderr.String())
	}
	if got.Inventory.ControlPlaneEndpoint != "api.katl.test:6443" || got.Inventory.KubernetesVersion != "v1.36.1" || len(got.Inventory.Nodes) != 1 {
		t.Fatalf("source inventory = %#v", got.Inventory)
	}
}

func TestClusterBootstrapUsesConfigBundleInventory(t *testing.T) {
	bundlePath, _ := writeConfigBundle(t)
	var got cluster.Request
	old := runAgentBootstrap
	runAgentBootstrap = func(_ context.Context, request cluster.Request, _ cluster.AgentBootstrapDependencies) (cluster.Result, error) {
		got = request
		return cluster.Result{Plan: inventory.Plan{InitNode: request.InitNode}}, nil
	}
	t.Cleanup(func() { runAgentBootstrap = old })

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{
		"cluster", "bootstrap",
		"--config-bundle", bundlePath,
		"--init-node", "cp-1",
		"--dry-run",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run() error = %v, stderr = %s", err, stderr.String())
	}
	if got.Inventory.ControlPlaneEndpoint != "api.katl.test:6443" || got.Inventory.KubernetesVersion != "v1.36.1" || len(got.Inventory.Nodes) != 1 {
		t.Fatalf("bundle inventory = %#v", got.Inventory)
	}
	if got.Inventory.KubernetesBundleRef == "" || got.Inventory.Nodes[0].Access.CredentialRef != "agent/cp-1" {
		t.Fatalf("bundle selection = %#v", got.Inventory)
	}
}

func TestClusterBootstrapRejectsConfigBundleConflicts(t *testing.T) {
	bundlePath, _ := writeConfigBundle(t)
	sourcePath := writeClusterConfig(t)
	inventoryPath := writeInventory(t)
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "inventory", args: []string{"--config-bundle", bundlePath, "--inventory", inventoryPath}, want: "exactly one"},
		{name: "source and bundle", args: []string{sourcePath, "--config-bundle", bundlePath}, want: "exactly one"},
		{name: "Kubernetes", args: []string{"--config-bundle", bundlePath, "--kubernetes-bundle", "ghcr.io/katl-dev/kubernetes:v1.36.1-katl.2"}, want: "conflicts with the selection embedded"},
		{name: "endpoint", args: []string{"--config-bundle", bundlePath, "--control-plane-endpoint", "other.test:6443"}, want: "conflicts with the endpoint embedded"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := append([]string{"cluster", "bootstrap"}, tt.args...)
			err := run(context.Background(), args, io.Discard, io.Discard)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("run() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestClusterBootstrapRejectsInvalidBootstrapWait(t *testing.T) {
	inventoryPath := writeInventory(t)
	old := runAgentBootstrap
	runAgentBootstrap = func(context.Context, cluster.Request, cluster.AgentBootstrapDependencies) (cluster.Result, error) {
		t.Fatal("runAgentBootstrap should not be called for invalid bootstrap wait")
		return cluster.Result{}, nil
	}
	t.Cleanup(func() { runAgentBootstrap = old })

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{
		"cluster", "bootstrap",
		"--inventory", inventoryPath,
		"--bootstrap-wait", "condition:kube-system:deployment/cilium:",
	}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "bootstrap wait condition") {
		t.Fatalf("run() error = %v, want wait validation failure", err)
	}

	err = run(context.Background(), []string{
		"cluster", "bootstrap",
		"--inventory", inventoryPath,
		"--bootstrap-wait", "resource-exists:pods",
	}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "target must be kind/name") {
		t.Fatalf("run() error = %v, want kind/name validation failure", err)
	}

	err = run(context.Background(), []string{
		"cluster", "bootstrap",
		"--inventory", inventoryPath,
		"--bootstrap-wait", "pods-ready:kube-system:app = coredns",
	}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "bootstrap wait pods-ready") {
		t.Fatalf("run() error = %v, want selector validation failure", err)
	}
}

func TestClusterBootstrapAllowsPreManifestStableEndpointFromInventory(t *testing.T) {
	inventoryPath := filepath.Join(t.TempDir(), "cluster.yaml")
	data := `controlPlaneEndpoint: api.katl.test:6443
kubernetesVersion: v1.36.1
bootstrap:
  stableEndpoint: api.inventory.test:6443
nodes:
- name: cp-1
  address: 10.0.0.11
  systemRole: control-plane
  access:
    method: agent
    credentialRef: agent/cp-1
  kubeadmConfig:
    ref: control-plane
    path: /etc/katl/kubeadm/control-plane/config.yaml
    intent: control-plane
  kubernetesVersion: v1.36.1
`
	if err := os.WriteFile(inventoryPath, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	var got cluster.Request
	old := runAgentBootstrap
	runAgentBootstrap = func(_ context.Context, request cluster.Request, _ cluster.AgentBootstrapDependencies) (cluster.Result, error) {
		got = request
		return cluster.Result{}, nil
	}
	t.Cleanup(func() { runAgentBootstrap = old })

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{
		"cluster", "bootstrap",
		"--inventory", inventoryPath,
		"--bootstrap-stable-endpoint-before-manifests",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run() error = %v, stderr = %s", err, stderr.String())
	}
	if got.Bootstrap.StableEndpoint != "" {
		t.Fatalf("request bootstrap stable endpoint = %q, want CLI unset", got.Bootstrap.StableEndpoint)
	}
	if !got.Bootstrap.StableEndpointBeforeManifests {
		t.Fatal("request bootstrap stable endpoint before manifests = false")
	}
	if got.Inventory.Bootstrap == nil || got.Inventory.Bootstrap.StableEndpoint != "api.inventory.test:6443" {
		t.Fatalf("inventory bootstrap = %#v", got.Inventory.Bootstrap)
	}
}

func TestWipeCommandsExplainConsequenceWithoutConfirmationCeremony(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"node", "wipe", "--help"}, &stdout, &stderr); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "must boot installer media or PXE") {
		t.Fatalf("stdout = %q, want reinstall consequence", out)
	}
	if strings.Contains(out, "acknowledge") || strings.Contains(out, "confirm-destructive") {
		t.Fatalf("stdout = %q, want no confirmation ceremony", out)
	}
}

func TestWipeClusterRefusesPartialTargetWithoutOverride(t *testing.T) {
	inventoryPath := writeInventory(t)
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{
		"cluster", "wipe",
		"--inventory", inventoryPath,
		"--node", "cp-1",
		"--client-request-id", "wipe-req",
	}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "partial cluster wipe requires --allow-partial-cluster") {
		t.Fatalf("run() error = %v, want partial target refusal", err)
	}
	var report wipeClusterReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode report: %v\n%s", err, stdout.String())
	}
	if !report.PartialCluster || len(report.Targets) != 1 || report.Targets[0].Name != "cp-1" {
		t.Fatalf("report targets = %#v", report)
	}
	if len(report.Refusals) != 1 || !strings.Contains(report.Refusals[0], "partial cluster wipe") {
		t.Fatalf("report refusals = %#v", report.Refusals)
	}
}

func TestWipeClusterPlanPrintsNodeLocalOperations(t *testing.T) {
	inventoryPath := writeInventory(t)
	connector := newFakeWipeClusterConnector(map[string]*fakeKatlcAgentClient{
		"cp-1":     readyWipeClusterClient("cp-machine"),
		"worker-1": readyWipeClusterClient("worker-machine"),
	})
	old := newWipeClusterConnector
	newWipeClusterConnector = func(token string) cluster.AgentConnector {
		if token != "" {
			t.Fatalf("token = %q, want empty", token)
		}
		return connector
	}
	t.Cleanup(func() { newWipeClusterConnector = old })

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{
		"cluster", "wipe",
		"--inventory", inventoryPath,
		"--all",
		"--plan",
		"--client-request-id", "wipe-req",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run() error = %v, stderr = %s", err, stderr.String())
	}
	var report wipeClusterReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode report: %v\n%s", err, stdout.String())
	}
	if !report.Plan || report.PartialCluster {
		t.Fatalf("report flags = %#v", report)
	}
	if len(report.Targets) != 2 || len(report.NodeLocalOperations) != 2 {
		t.Fatalf("report targets/operations = %#v", report)
	}
	if report.NodeLocalOperations[0].OperationKind != wipeClusterOperationKind || !report.NodeLocalOperations[0].DiscardClusterIdentity {
		t.Fatalf("node-local operation = %#v", report.NodeLocalOperations[0])
	}
	for name, client := range connector.clients {
		if client.submitRequest != nil {
			t.Fatalf("%s submit request = %+v, want nil for plan", name, client.submitRequest)
		}
	}
}

func TestWipeClusterPlanAcceptsClusterConfig(t *testing.T) {
	sourcePath := writeClusterConfig(t)
	connector := newFakeWipeClusterConnector(map[string]*fakeKatlcAgentClient{
		"cp-1": readyWipeClusterClient("cp-machine"),
	})
	old := newWipeClusterConnector
	newWipeClusterConnector = func(string) cluster.AgentConnector { return connector }
	t.Cleanup(func() { newWipeClusterConnector = old })

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{
		"cluster", "wipe", sourcePath,
		"--all",
		"--plan",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run() error = %v, stderr = %s", err, stderr.String())
	}
	var report wipeClusterReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode report: %v\n%s", err, stdout.String())
	}
	if !report.Plan || len(report.Targets) != 1 || report.Targets[0].Name != "cp-1" {
		t.Fatalf("report = %#v", report)
	}
}

func TestWipeClusterUsesEnrolledContextWithoutSource(t *testing.T) {
	configPath := writeKatlctlConfig(t, `currentContext: lab
contexts:
- name: lab
  cluster: katl-lab
clusters:
- name: katl-lab
  controlPlaneEndpoint: api.lab.test:6443
  nodes:
  - name: cp-1
    managementEndpoint: 192.0.2.11:9443
    systemRole: control-plane
    credentialRef: file:/secure/katl/cp-1.token
  - name: worker-1
    managementEndpoint: 192.0.2.21:9443
    systemRole: worker
    credentialRef: file:/secure/katl/worker-1.token
`)
	connector := newFakeWipeClusterConnector(map[string]*fakeKatlcAgentClient{
		"cp-1":     readyWipeClusterClient("cp-machine"),
		"worker-1": readyWipeClusterClient("worker-machine"),
	})
	oldConnector := newWipeClusterConnector
	newWipeClusterConnector = func(string) cluster.AgentConnector { return connector }
	t.Cleanup(func() { newWipeClusterConnector = oldConnector })

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{
		"cluster", "wipe",
		"--context-file", configPath,
		"--all",
		"--plan",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run() error = %v, stderr = %s", err, stderr.String())
	}
	var report wipeClusterReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode report: %v\n%s", err, stdout.String())
	}
	if len(report.Targets) != 2 || report.PartialCluster {
		t.Fatalf("report = %#v", report)
	}
}

func TestWipeClusterSubmitsDestructiveResetToAllNodes(t *testing.T) {
	inventoryPath := writeInventory(t)
	connector := newFakeWipeClusterConnector(map[string]*fakeKatlcAgentClient{
		"cp-1":     readyWipeClusterClient("cp-machine"),
		"worker-1": readyWipeClusterClient("worker-machine"),
	})
	old := newWipeClusterConnector
	newWipeClusterConnector = func(token string) cluster.AgentConnector {
		return connector
	}
	t.Cleanup(func() { newWipeClusterConnector = old })

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{
		"cluster", "wipe",
		"--inventory", inventoryPath,
		"--all",
		"--client-request-id", "wipe-req",
		"--timeout", "10m",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run() error = %v, stderr = %s", err, stderr.String())
	}
	var report wipeClusterReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode report: %v\n%s", err, stdout.String())
	}
	if len(report.Nodes) != 2 {
		t.Fatalf("report nodes = %#v", report.Nodes)
	}
	for name, client := range connector.clients {
		if client.submitRequest == nil {
			t.Fatalf("%s submit request = nil", name)
		}
		req := client.submitRequest
		if req.OperationKind != wipeClusterOperationKind || req.ClientRequestId != "wipe-req" || req.Actor != "katlctl cluster wipe" || req.OperationTimeout != "10m" {
			t.Fatalf("%s submit request = %+v", name, req)
		}
		reset := req.GetDestructiveReset()
		if reset == nil || reset.InventoryNodeName != name || reset.ResetScope != "cluster" || reset.TargetGenerationId != "" || !reset.DiscardClusterIdentity {
			t.Fatalf("%s destructive reset = %+v", name, reset)
		}
		if !reflect.DeepEqual(reset.WipeSurfaces, []string{"katlos-boot-artifacts", "disk-boot-path"}) {
			t.Fatalf("%s wipe surfaces = %#v", name, reset.WipeSurfaces)
		}
	}
	for _, node := range report.Nodes {
		if !node.Accepted || node.OperationKind != wipeClusterOperationKind || node.OperationID != "" || !node.Terminal || node.Result != "succeeded" {
			t.Fatalf("node result = %#v", node)
		}
	}
}

func TestWipeNodeRequiresExactlyOneTarget(t *testing.T) {
	inventoryPath := writeInventory(t)
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{
		"node", "wipe",
		"--inventory", inventoryPath,
		"--client-request-id", "wipe-node-req",
		"--kubeconfig", "admin.conf",
	}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "accepts between 1 and 2 arg") {
		t.Fatalf("run() error = %v, want exact node target validation", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %s, want empty", stdout.String())
	}
}

func TestNodeWipeSubmitsWithNodeActor(t *testing.T) {
	inventoryPath := writeInventory(t)
	connector := newFakeWipeClusterConnector(map[string]*fakeKatlcAgentClient{
		"worker-1": readyWipeClusterClient("worker-machine"),
	})
	oldConnector := newWipeClusterConnector
	newWipeClusterConnector = func(token string) cluster.AgentConnector {
		return connector
	}
	oldKubectl := operatorKubectlRunner
	kubectl := &fakeKubectlRunner{}
	operatorKubectlRunner = kubectl
	t.Cleanup(func() {
		newWipeClusterConnector = oldConnector
		operatorKubectlRunner = oldKubectl
	})

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{
		"node", "wipe", "worker-1",
		"--inventory", inventoryPath,
		"--kubeconfig", "admin.conf",
		"--client-request-id", "cluster-wipe-node-req",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run() error = %v, stderr = %s", err, stderr.String())
	}
	req := connector.clients["worker-1"].submitRequest
	if req == nil {
		t.Fatal("submit request = nil")
	}
	reset := req.GetDestructiveReset()
	if req.Actor != "katlctl node wipe" || reset == nil || reset.ResetScope != "node" {
		t.Fatalf("submit request = %+v reset=%+v", req, reset)
	}
	var report wipeNodeReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode report: %v\n%s", err, stdout.String())
	}
	if report.Command != "katlctl node wipe" {
		t.Fatalf("report command = %q", report.Command)
	}
}

func TestWipeNodePlanPrintsUnknownKubernetesCleanupWithoutKubeconfig(t *testing.T) {
	inventoryPath := writeInventory(t)
	connector := newFakeWipeClusterConnector(map[string]*fakeKatlcAgentClient{
		"worker-1": readyWipeClusterClient("worker-machine"),
	})
	oldConnector := newWipeClusterConnector
	newWipeClusterConnector = func(token string) cluster.AgentConnector {
		return connector
	}
	oldKubectl := operatorKubectlRunner
	kubectl := &fakeKubectlRunner{}
	operatorKubectlRunner = kubectl
	t.Cleanup(func() {
		newWipeClusterConnector = oldConnector
		operatorKubectlRunner = oldKubectl
	})

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{
		"node", "wipe", "worker-1",
		"--inventory", inventoryPath,
		"--plan",
		"--client-request-id", "wipe-node-req",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run() error = %v, stderr = %s", err, stderr.String())
	}
	var report wipeNodeReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode report: %v\n%s", err, stdout.String())
	}
	if !report.Plan || report.Kind != "WipeNodeReport" || report.Command != "katlctl node wipe" {
		t.Fatalf("report identity = %#v", report)
	}
	if report.KubernetesCleanup != "unknown" {
		t.Fatalf("kubernetes cleanup = %q, want unknown", report.KubernetesCleanup)
	}
	if len(report.NodeLocalOperations) != 1 || report.NodeLocalOperations[0].ResetScope != "node" {
		t.Fatalf("node local operations = %#v", report.NodeLocalOperations)
	}
	if len(kubectl.calls) != 0 {
		t.Fatalf("kubectl calls = %#v, want none for plan without kubeconfig", kubectl.calls)
	}
}

func TestWipeNodeUsesEnrolledContextWithoutClusterSource(t *testing.T) {
	configPath := writeKatlctlConfig(t, `currentContext: lab
contexts:
- name: lab
  cluster: katl-lab
clusters:
- name: katl-lab
  controlPlaneEndpoint: api.lab.test:6443
  nodes:
  - name: cp-1
    managementEndpoint: 192.0.2.11:9443
    systemRole: control-plane
    credentialRef: file:/secure/katl/cp-1.token
  - name: worker-1
    managementEndpoint: 192.0.2.21:9443
    systemRole: worker
    credentialRef: file:/secure/katl/worker-1.token
`)
	connector := newFakeWipeClusterConnector(map[string]*fakeKatlcAgentClient{
		"worker-1": readyWipeClusterClient("worker-machine"),
	})
	oldConnector := newWipeClusterConnector
	newWipeClusterConnector = func(string) cluster.AgentConnector { return connector }
	t.Cleanup(func() { newWipeClusterConnector = oldConnector })

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{
		"node", "wipe", "worker-1",
		"--context-file", configPath,
		"--plan",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run() error = %v, stderr = %s", err, stderr.String())
	}
	var report wipeNodeReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode report: %v\n%s", err, stdout.String())
	}
	if len(report.Targets) != 1 || report.Targets[0].Name != "worker-1" || report.Targets[0].Address != "192.0.2.21" {
		t.Fatalf("targets = %#v", report.Targets)
	}
}

func TestWipeNodeSubmitsAfterKubernetesCleanup(t *testing.T) {
	inventoryPath := writeInventory(t)
	connector := newFakeWipeClusterConnector(map[string]*fakeKatlcAgentClient{
		"worker-1": readyWipeClusterClient("worker-machine"),
	})
	oldConnector := newWipeClusterConnector
	newWipeClusterConnector = func(token string) cluster.AgentConnector {
		return connector
	}
	oldKubectl := operatorKubectlRunner
	kubectl := &fakeKubectlRunner{}
	operatorKubectlRunner = kubectl
	t.Cleanup(func() {
		newWipeClusterConnector = oldConnector
		operatorKubectlRunner = oldKubectl
	})

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{
		"node", "wipe", "worker-1",
		"--inventory", inventoryPath,
		"--kubeconfig", "admin.conf",
		"--client-request-id", "wipe-node-req",
		"--timeout", "7m",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run() error = %v, stderr = %s", err, stderr.String())
	}
	var report wipeNodeReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode report: %v\n%s", err, stdout.String())
	}
	if report.KubernetesCleanup != "succeeded" || len(report.KubernetesDiagnostics) != 0 {
		t.Fatalf("kubernetes cleanup = %q diagnostics %#v", report.KubernetesCleanup, report.KubernetesDiagnostics)
	}
	wantCalls := [][]string{
		{"kubectl", "--kubeconfig", "admin.conf", "cordon", "worker-1"},
		{"kubectl", "--kubeconfig", "admin.conf", "drain", "worker-1", "--ignore-daemonsets", "--delete-emptydir-data", "--force", "--timeout=7m"},
		{"kubectl", "--kubeconfig", "admin.conf", "delete", "node", "worker-1", "--ignore-not-found=true"},
	}
	if !reflect.DeepEqual(kubectl.calls, wantCalls) {
		t.Fatalf("kubectl calls = %#v, want %#v", kubectl.calls, wantCalls)
	}
	req := connector.clients["worker-1"].submitRequest
	if req == nil {
		t.Fatal("submit request = nil")
	}
	reset := req.GetDestructiveReset()
	if reset == nil || reset.ResetScope != "node" || reset.InventoryNodeName != "worker-1" || !reset.DiscardClusterIdentity {
		t.Fatalf("destructive reset = %+v", reset)
	}
	if req.OperationKind != wipeClusterOperationKind || req.ClientRequestId != "wipe-node-req" || req.OperationTimeout != "7m" {
		t.Fatalf("submit request = %+v", req)
	}
}

func TestWipeNodeReportsRecoveryRequiredBeforeLocalReset(t *testing.T) {
	inventoryPath := writeInventory(t)
	connector := newFakeWipeClusterConnector(map[string]*fakeKatlcAgentClient{
		"worker-1": readyWipeClusterClient("worker-machine"),
	})
	oldConnector := newWipeClusterConnector
	newWipeClusterConnector = func(token string) cluster.AgentConnector {
		return connector
	}
	oldKubectl := operatorKubectlRunner
	kubectl := &fakeKubectlRunner{results: []readiness.CommandResult{
		{ExitStatus: 0},
		{ExitStatus: 1, Stderr: "drain timed out with token abcdef.abcdefghijklmnop"},
		{ExitStatus: 1, Stderr: "delete failed with Bearer secret-token"},
	}}
	operatorKubectlRunner = kubectl
	t.Cleanup(func() {
		newWipeClusterConnector = oldConnector
		operatorKubectlRunner = oldKubectl
	})

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{
		"node", "wipe", "worker-1",
		"--inventory", inventoryPath,
		"--kubeconfig", "admin.conf",
		"--client-request-id", "wipe-node-req",
	}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "Kubernetes cleanup failed") {
		t.Fatalf("run() error = %v, want Kubernetes cleanup failure", err)
	}
	var report wipeNodeReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode report: %v\n%s", err, stdout.String())
	}
	if report.KubernetesCleanup != "recovery-required" {
		t.Fatalf("kubernetes cleanup = %q, want recovery-required", report.KubernetesCleanup)
	}
	diagnostics := strings.Join(report.KubernetesDiagnostics, "\n")
	if !strings.Contains(diagnostics, "[REDACTED BOOTSTRAP TOKEN]") || !strings.Contains(diagnostics, "Bearer [REDACTED]") {
		t.Fatalf("diagnostics were not redacted: %#v", report.KubernetesDiagnostics)
	}
	if connector.clients["worker-1"].submitRequest != nil {
		t.Fatalf("submit request = %+v, want nil", connector.clients["worker-1"].submitRequest)
	}
}

func TestWipeNodeRefusesControlPlaneBeforeMutation(t *testing.T) {
	inventoryPath := writeInventory(t)
	connector := newFakeWipeClusterConnector(map[string]*fakeKatlcAgentClient{
		"cp-1": readyWipeClusterClient("cp-machine"),
	})
	oldConnector := newWipeClusterConnector
	newWipeClusterConnector = func(token string) cluster.AgentConnector {
		return connector
	}
	oldKubectl := operatorKubectlRunner
	kubectl := &fakeKubectlRunner{}
	operatorKubectlRunner = kubectl
	t.Cleanup(func() {
		newWipeClusterConnector = oldConnector
		operatorKubectlRunner = oldKubectl
	})

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{
		"node", "wipe", "cp-1",
		"--inventory", inventoryPath,
		"--kubeconfig", "admin.conf",
		"--client-request-id", "wipe-node-req",
	}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "etcd membership coordination") {
		t.Fatalf("run() error = %v, want etcd coordinator refusal", err)
	}
	var report wipeNodeReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode report: %v\n%s", err, stdout.String())
	}
	if report.KubernetesCleanup != "refused" || len(report.Refusals) != 1 {
		t.Fatalf("report = %#v", report)
	}
	if len(kubectl.calls) != 0 {
		t.Fatalf("kubectl calls = %#v, want none before control-plane refusal", kubectl.calls)
	}
	if connector.clients["cp-1"].submitRequest != nil {
		t.Fatalf("submit request = %+v, want nil", connector.clients["cp-1"].submitRequest)
	}
}

func TestConfigApplyStatusReportsActiveAndNextBootJSON(t *testing.T) {
	root := t.TempDir()
	writeConfigApplyFixture(t, root, configApplyFixture{
		GenerationID:       "2026.06.05-002",
		PreviousGeneration: "2026.06.05-001",
		Mode:               generation.ApplyModeLive,
		Phase:              generation.ConfigApplyPhaseActive,
		Domains:            []string{"networkd", "tmpfiles"},
	})
	writeConfigApplyFixture(t, root, configApplyFixture{
		GenerationID:       "2026.06.05-003",
		PreviousGeneration: "2026.06.05-002",
		Mode:               generation.ApplyModeNextBoot,
		Phase:              generation.ConfigApplyPhaseNextBoot,
		Domains:            []string{"node-identity"},
	})

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{
		"node", "apply", "status",
		"--root", root,
		"--active-generation", "2026.06.05-002",
		"--next-boot-generation", "2026.06.05-003",
		"--output", "json",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run() error = %v, stderr = %s", err, stderr.String())
	}
	var report configApplyReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode report: %v\n%s", err, stdout.String())
	}
	if report.ActiveGenerationID != "2026.06.05-002" || report.NextBootGenerationID != "2026.06.05-003" {
		t.Fatalf("report ids = %#v", report)
	}
	if report.Active == nil || report.Active.Phase != generation.ConfigApplyPhaseActive || strings.Join(report.Active.ChangedDomains, ",") != "networkd,tmpfiles" {
		t.Fatalf("active report = %#v", report.Active)
	}
	if report.NextBoot == nil || report.NextBoot.Phase != generation.ConfigApplyPhaseNextBoot || report.NextBoot.AcceptedApplyMode != generation.ApplyModeNextBoot {
		t.Fatalf("next-boot report = %#v", report.NextBoot)
	}
}

func TestConfigApplySubmitsStageGenerationToAgent(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("apiVersion: katl.dev/v1alpha1\nkind: NodeConfigurationChange\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := &fakeKatlcAgentClient{
		stageAccepted: &agentapi.OperationAccepted{
			OperationId:   "generation-stage-01",
			OperationKind: "generation-stage",
			RequestDigest: strings.Repeat("a", 64),
		},
	}
	oldDial := dialKatlcAgent
	dialKatlcAgent = func(ctx context.Context, endpoint string, token string) (katlcAgentConnection, error) {
		if endpoint != "node-a.example.test:9443" || token != "" {
			t.Fatalf("dial endpoint=%q token=%q", endpoint, token)
		}
		return katlcAgentConnection{Client: fake, Close: func() error { return nil }}, nil
	}
	t.Cleanup(func() { dialKatlcAgent = oldDial })

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{
		"node", "apply",
		"--endpoint", "node-a.example.test:9443",
		"--file", configPath,
		"--mode", generation.ApplyModeNextBoot,
		"--candidate-generation", "generation-1",
		"--client-request-id", "req-stage",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run() error = %v, stderr = %s", err, stderr.String())
	}
	if fake.stageRequest == nil || fake.stageRequest.CandidateGenerationId != "generation-1" || fake.stageRequest.ClientRequestId != "req-stage" || fake.stageRequest.Actor != "katlctl node apply" {
		t.Fatalf("stage request = %+v", fake.stageRequest)
	}
	assertSuccessfulMutationOutput(t, stdout.Bytes())
}

func TestHostUpgradeSubmitsSingleImageOperation(t *testing.T) {
	fake := &fakeKatlcAgentClient{
		nodeStatus: &agentapi.NodeStatus{MachineId: "machine-a", CurrentGenerationId: "generation-current"},
		stageAccepted: &agentapi.OperationAccepted{
			OperationId:   "host-upgrade-01",
			OperationKind: "host-upgrade",
			RequestDigest: strings.Repeat("a", 64),
		},
	}
	oldDial := dialKatlcAgent
	dialKatlcAgent = func(_ context.Context, endpoint string, token string) (katlcAgentConnection, error) {
		if endpoint != "node-a.example.test:9443" || token != "" {
			t.Fatalf("dial endpoint=%q token=%q", endpoint, token)
		}
		return katlcAgentConnection{Client: fake, Close: func() error { return nil }}, nil
	}
	t.Cleanup(func() { dialKatlcAgent = oldDial })

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{
		"node", "upgrade",
		"--endpoint", "node-a.example.test:9443",
		"--image-url", "https://updates.example.test/katlos-upgrade.squashfs",
		"--candidate-generation", "generation-upgrade-1",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run() error = %v, stderr = %s", err, stderr.String())
	}
	request := fake.submitRequest
	if request == nil || request.OperationKind != "host-upgrade" || request.GetHostUpgrade() == nil {
		t.Fatalf("submit request = %+v", request)
	}
	if !strings.HasPrefix(request.ClientRequestId, "katlctl-") {
		t.Fatalf("generated client request id = %q", request.ClientRequestId)
	}
	if request.HostUpgrade.ImageUrl != "https://updates.example.test/katlos-upgrade.squashfs" || request.HostUpgrade.ImageSha256 != "" || request.HostUpgrade.ImageSizeBytes != 0 || request.HostUpgrade.CandidateGenerationId != "generation-upgrade-1" {
		t.Fatalf("host upgrade request = %+v", request.HostUpgrade)
	}
	assertSuccessfulMutationOutput(t, stdout.Bytes())
}

func TestHostUpgradeVersionStagesRebootsAndVerifiesHealth(t *testing.T) {
	fake := &fakeKatlcAgentClient{
		nodeStatus:      &agentapi.NodeStatus{MachineId: "machine-a", AgentStartId: "before", CurrentGenerationId: "generation-current"},
		generation:      &agentapi.Generation{GenerationId: "generation-current", Sysexts: []*agentapi.ExtensionRef{{Name: "kubernetes", Architecture: "x86_64"}}},
		submitAccepted:  &agentapi.OperationAccepted{OperationId: "host-upgrade-01", OperationKind: "host-upgrade"},
		operationStatus: &agentapi.OperationStatus{Terminal: true, Result: operation.ResultSucceeded, Phase: "arm-trial-boot"},
	}
	fake.onReboot = func(req *agentapi.RebootRequest) {
		fake.nodeStatus.AgentStartId = "after"
		fake.nodeStatus.CurrentGenerationId = req.TargetGenerationId
		fake.generation = &agentapi.Generation{GenerationId: req.TargetGenerationId, CommitState: generation.CommitStateCommitted, BootState: generation.BootStateGood, HealthState: generation.HealthStateHealthy}
	}
	oldDial := dialKatlcAgent
	dialKatlcAgent = func(context.Context, string, string) (katlcAgentConnection, error) {
		return katlcAgentConnection{Client: fake, Close: func() error { return nil }}, nil
	}
	t.Cleanup(func() { dialKatlcAgent = oldDial })

	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"node", "upgrade", "v2026.7.0-alpha.9", "--endpoint", "node-a.example.test:9443", "--timeout", "1m"}, &stdout, &stderr); err != nil {
		t.Fatalf("run() error = %v, stderr = %s", err, stderr.String())
	}
	request := fake.submitRequest.GetHostUpgrade()
	if request.GetImageUrl() != "https://github.com/katl-dev/katl/releases/download/v2026.7.0-alpha.9/katlos-upgrade-2026.7.0-alpha.9-x86_64.squashfs" || request.GetCandidateGenerationId() != "katlos-2026.7.0-alpha.9" {
		t.Fatalf("host upgrade request = %#v", request)
	}
	var report hostUpgradeReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Result != operation.ResultSucceeded || !report.Rebooted || report.BootHealth != generation.HealthStateHealthy {
		t.Fatalf("report = %#v", report)
	}
}

func TestConfigApplyDefaultsAutoAndSubmitsAcceptedOperationKind(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("apiVersion: katl.dev/v1alpha1\nkind: NodeConfigurationChange\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := &fakeKatlcAgentClient{
		validateResult: &agentapi.ConfigValidationResult{
			Accepted:              true,
			RequestDigest:         strings.Repeat("c", 64),
			RequestedApplyMode:    generation.ApplyModeAuto,
			AcceptedApplyMode:     generation.ApplyModeLive,
			CandidateGenerationId: "generation-auto",
			ChangedDomains:        []string{"sysctl"},
		},
		stageAccepted: &agentapi.OperationAccepted{
			OperationId:   "generation-apply-auto-01",
			OperationKind: "generation-apply",
			RequestDigest: strings.Repeat("a", 64),
		},
	}
	oldDial := dialKatlcAgent
	dialKatlcAgent = func(ctx context.Context, endpoint string, token string) (katlcAgentConnection, error) {
		if endpoint != "node-a.example.test:9443" || token != "" {
			t.Fatalf("dial endpoint=%q token=%q", endpoint, token)
		}
		return katlcAgentConnection{Client: fake, Close: func() error { return nil }}, nil
	}
	t.Cleanup(func() { dialKatlcAgent = oldDial })

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{
		"node", "apply",
		"--endpoint", "node-a.example.test:9443",
		"--file", configPath,
		"--candidate-generation", "generation-auto",
		"--client-request-id", "req-auto",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run() error = %v, stderr = %s", err, stderr.String())
	}
	if fake.validateRequest == nil || fake.validateRequest.ApplyMode != generation.ApplyModeAuto || fake.validateRequest.CandidateGenerationId != "generation-auto" || fake.validateRequest.Actor != "katlctl node apply" {
		t.Fatalf("validate request = %+v", fake.validateRequest)
	}
	if fake.submitRequest == nil || fake.submitRequest.OperationKind != "generation-apply" || fake.submitRequest.Actor != "katlctl node apply" || fake.submitRequest.GetConfigApply().GetApplyMode() != generation.ApplyModeAuto {
		t.Fatalf("submit request = %+v", fake.submitRequest)
	}
	if fake.stageRequest != nil || fake.applyRequest != nil {
		t.Fatalf("direct mutation request was sent: stage=%+v apply=%+v", fake.stageRequest, fake.applyRequest)
	}
	assertSuccessfulMutationOutput(t, stdout.Bytes())
}

func TestConfigApplyPlanValidatesWithAgent(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("apiVersion: katl.dev/v1alpha1\nkind: NodeConfigurationChange\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := &fakeKatlcAgentClient{
		validateResult: &agentapi.ConfigValidationResult{
			Accepted:              true,
			RequestDigest:         strings.Repeat("c", 64),
			CandidateGenerationId: "generation-plan",
			ChangedDomains:        []string{"networkd"},
		},
	}
	oldDial := dialKatlcAgent
	dialKatlcAgent = func(ctx context.Context, endpoint string, token string) (katlcAgentConnection, error) {
		if endpoint != "node-a.example.test:9443" {
			t.Fatalf("dial endpoint=%q", endpoint)
		}
		return katlcAgentConnection{Client: fake, Close: func() error { return nil }}, nil
	}
	t.Cleanup(func() { dialKatlcAgent = oldDial })

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{
		"node", "apply", "validate",
		"--endpoint", "node-a.example.test:9443",
		"--file", configPath,
		"--mode", generation.ApplyModeNextBoot,
		"--candidate-generation", "generation-plan",
		"--client-request-id", "req-plan",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run() error = %v, stderr = %s", err, stderr.String())
	}
	if fake.validateRequest == nil || fake.validateRequest.CandidateGenerationId != "generation-plan" || fake.validateRequest.ClientRequestId != "req-plan" || fake.validateRequest.Actor != "katlctl node apply validate" || fake.validateRequest.ApplyMode != generation.ApplyModeNextBoot {
		t.Fatalf("validate request = %+v", fake.validateRequest)
	}
	if fake.stageRequest != nil || fake.applyRequest != nil {
		t.Fatalf("mutation request was sent: stage=%+v apply=%+v", fake.stageRequest, fake.applyRequest)
	}
	var output map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("decode stdout = %v: %s", err, stdout.String())
	}
	if output["accepted"] != true || output["candidateGenerationId"] != "generation-plan" || !strings.Contains(stdout.String(), `"networkd"`) || strings.Contains(stdout.String(), "requestDigest") {
		t.Fatalf("stdout = %s", stdout.String())
	}
}

func TestConfigApplyRendersVerifiedBundleNode(t *testing.T) {
	bundlePath, _ := writeConfigBundle(t)
	fake := &fakeKatlcAgentClient{
		validateResult: &agentapi.ConfigValidationResult{
			Accepted:              true,
			RequestDigest:         strings.Repeat("c", 64),
			AcceptedApplyMode:     generation.ApplyModeLive,
			CandidateGenerationId: "generation-bundle",
			ChangedDomains:        []string{"node-identity", "networkd"},
		},
		stageAccepted: &agentapi.OperationAccepted{
			OperationId:   "generation-bundle-apply",
			OperationKind: "generation-apply",
			RequestDigest: strings.Repeat("d", 64),
		},
	}
	oldDial := dialKatlcAgent
	dialKatlcAgent = func(_ context.Context, endpoint string, token string) (katlcAgentConnection, error) {
		if endpoint != "node-a.example.test:9443" || token != "" {
			t.Fatalf("dial endpoint=%q token=%q", endpoint, token)
		}
		return katlcAgentConnection{Client: fake, Close: func() error { return nil }}, nil
	}
	t.Cleanup(func() { dialKatlcAgent = oldDial })

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{
		"node", "apply",
		"--endpoint", "node-a.example.test:9443",
		"--config-bundle", bundlePath,
		"--node", "cp-1",
		"--desired-version", "2",
		"--candidate-generation", "generation-bundle",
		"--client-request-id", "req-bundle",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run() error = %v, stderr = %s", err, stderr.String())
	}
	if fake.validateRequest == nil || fake.validateRequest.NodeName != "cp-1" {
		t.Fatalf("validate request = %+v", fake.validateRequest)
	}
	if fake.submitRequest == nil || fake.submitRequest.GetConfigApply().GetNodeName() != "cp-1" {
		t.Fatalf("submit request = %+v", fake.submitRequest)
	}
	request, err := configapply.DecodeNodeConfigurationChange(strings.NewReader(fake.validateRequest.ConfigYaml), configapply.TrustedBundleRequest{})
	if err != nil {
		t.Fatalf("decode rendered config: %v\n%s", err, fake.validateRequest.ConfigYaml)
	}
	if request.SourceID != "lab" || request.DesiredVersion != "2" || request.NodeOverrides["cp-1"].Identity == nil {
		t.Fatalf("rendered request = %#v", request)
	}
	assertSuccessfulMutationOutput(t, stdout.Bytes())
}

func assertSuccessfulMutationOutput(t *testing.T, data []byte) {
	t.Helper()
	var status agentapi.OperationStatus
	if err := protojson.Unmarshal(data, &status); err != nil {
		t.Fatalf("decode mutation result: %v\n%s", err, data)
	}
	if !status.Terminal || status.Result != operation.ResultSucceeded || status.OperationId != "" || status.RequestDigest != "" {
		t.Fatalf("mutation result = %+v", &status)
	}
}

func TestConfigApplyStatusQueriesGenerationFromAgent(t *testing.T) {
	fake := &fakeKatlcAgentClient{
		generation: &agentapi.Generation{
			GenerationId: "generation-1",
			ConfigApply: &agentapi.ConfigApplyStatus{
				Phase:              generation.ConfigApplyPhaseNextBoot,
				AcceptedApplyMode:  generation.ApplyModeNextBoot,
				ChangedDomains:     []string{"networkd"},
				RequestedApplyMode: generation.ApplyModeNextBoot,
			},
		},
	}
	oldDial := dialKatlcAgent
	dialKatlcAgent = func(ctx context.Context, endpoint string, token string) (katlcAgentConnection, error) {
		if endpoint != "node-a.example.test:9443" {
			t.Fatalf("dial endpoint=%q", endpoint)
		}
		return katlcAgentConnection{Client: fake, Close: func() error { return nil }}, nil
	}
	t.Cleanup(func() { dialKatlcAgent = oldDial })

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{
		"node", "apply", "status",
		"--endpoint", "node-a.example.test:9443",
		"--generation", "generation-1",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run() error = %v, stderr = %s", err, stderr.String())
	}
	if fake.generationRequest == nil || fake.generationRequest.GenerationId != "generation-1" || !fake.generationRequest.IncludeConfigApply {
		t.Fatalf("generation request = %+v", fake.generationRequest)
	}
	if !strings.Contains(stdout.String(), `"generationId"`) || !strings.Contains(stdout.String(), `"generation-1"`) || !strings.Contains(stdout.String(), `"phase"`) || !strings.Contains(stdout.String(), `"next-boot"`) {
		t.Fatalf("stdout = %s", stdout.String())
	}
}

func TestConfigApplyStatusReportsFailureRollbackAndKubeadmRedacted(t *testing.T) {
	root := t.TempDir()
	secret := "abcdef.0123456789abcdef"
	writeConfigApplyFixture(t, root, configApplyFixture{
		GenerationID:       "2026.06.05-004",
		PreviousGeneration: "2026.06.05-003",
		Mode:               generation.ApplyModeNextBoot,
		Phase:              generation.ConfigApplyPhaseFailed,
		Domains:            []string{"kubeadm-config"},
		FailureReason:      "desired kubeadm input contains join token " + secret,
		Kubeadm: generation.KubeadmActionRequired{
			Required: true,
			Reason:   "operator must run kubeadm with token " + secret,
		},
	})
	writeConfigApplyFixture(t, root, configApplyFixture{
		GenerationID:       "2026.06.05-005",
		PreviousGeneration: "2026.06.05-004",
		Mode:               generation.ApplyModeLive,
		Phase:              generation.ConfigApplyPhaseRolledBack,
		Domains:            []string{"networkd"},
		RollbackTarget:     "2026.06.05-004",
	})

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{
		"node", "apply", "status",
		"--root", root,
		"--active-generation", "2026.06.05-004",
		"--next-boot-generation", "2026.06.05-005",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run() error = %v, stderr = %s", err, stderr.String())
	}
	if strings.Contains(stdout.String(), secret) {
		t.Fatalf("status output leaked secret:\n%s", stdout.String())
	}
	var report configApplyReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode report: %v\n%s", err, stdout.String())
	}
	if report.Active == nil || report.Active.Phase != generation.ConfigApplyPhaseFailed {
		t.Fatalf("active report = %#v", report.Active)
	}
	if !report.Active.KubeadmActionRequired.Required || !strings.Contains(report.Active.KubeadmActionRequired.Reason, "[REDACTED BOOTSTRAP TOKEN]") {
		t.Fatalf("kubeadm report = %#v", report.Active.KubeadmActionRequired)
	}
	if !strings.Contains(report.Active.FailureReason, "[REDACTED BOOTSTRAP TOKEN]") {
		t.Fatalf("failure reason = %q", report.Active.FailureReason)
	}
	if report.NextBoot == nil || report.NextBoot.Phase != generation.ConfigApplyPhaseRolledBack || report.NextBoot.RollbackTarget != "2026.06.05-004" {
		t.Fatalf("rolled-back report = %#v", report.NextBoot)
	}
}

func TestAddressOverrideValidation(t *testing.T) {
	var overrides addressOverrides
	if err := overrides.Set("bad"); err == nil {
		t.Fatal("Set() error = nil, want node=address validation")
	}
	if err := overrides.Set("node=10.0.0.10"); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	if overrides.values["node"] != "10.0.0.10" {
		t.Fatalf("values = %#v", overrides.values)
	}
}

func TestPrintBootstrapResultIncludesOperationReference(t *testing.T) {
	var stdout bytes.Buffer
	printBootstrapResult(&stdout, cluster.Result{Phases: []cluster.Phase{{
		Name:        "bootstrap-init",
		Node:        "cp-1",
		Status:      "failed",
		OperationID: "bootstrap-init-1",
	}}})
	if got := stdout.String(); !strings.Contains(got, "operation-id=bootstrap-init-1") || strings.Contains(got, "digest") {
		t.Fatalf("stdout = %q", got)
	}
}

func TestOperatorCommandsHideIntegrityDigestFlags(t *testing.T) {
	for _, args := range [][]string{
		{"install", "apply", "--help"},
		{"config", "render-node", "--help"},
		{"node", "apply", "--help"},
		{"cluster", "bootstrap", "--help"},
		{"kubernetes", "apply-config", "--help"},
		{"node", "upgrade", "--help"},
		{"operations", "status", "--help"},
	} {
		var stdout, stderr bytes.Buffer
		if err := run(context.Background(), args, &stdout, &stderr); err != nil {
			t.Fatalf("run(%v) error = %v", args, err)
		}
		for _, hidden := range []string{"config-bundle-digest", "request-digest", "image-sha256", "image-size-bytes", "desired-config-sha256", "expected-live-sha256", "kubernetes-sha256", "snapshot-sha256", "member-list-sha256", "field-delta"} {
			if !strings.Contains(stdout.String(), hidden) {
				continue
			}
			t.Fatalf("run(%v) exposed digest plumbing:\n%s", args, stdout.String())
		}
	}
}

type configApplyFixture struct {
	GenerationID       string
	PreviousGeneration string
	Mode               string
	Phase              string
	Domains            []string
	FailureReason      string
	RollbackTarget     string
	Kubeadm            generation.KubeadmActionRequired
}

type fakeKatlcAgentClient struct {
	stageAccepted     *agentapi.OperationAccepted
	stageRequest      *agentapi.GenerationApplyRequest
	applyRequest      *agentapi.GenerationApplyRequest
	validateResult    *agentapi.ConfigValidationResult
	validateRequest   *agentapi.ValidateConfigRequest
	submitRequest     *agentapi.SubmitOperationRequest
	submitRequests    []*agentapi.SubmitOperationRequest
	submitAccepted    *agentapi.OperationAccepted
	nodeStatus        *agentapi.NodeStatus
	nodeStatusErr     error
	generation        *agentapi.Generation
	generationRequest *agentapi.GetGenerationRequest
	operationStatus   *agentapi.OperationStatus
	operationRequest  *agentapi.GetOperationRequest
	operations        *agentapi.ListOperationsResponse
	operationsRequest *agentapi.ListOperationsRequest
	onSubmit          func(*agentapi.SubmitOperationRequest)
	rebootRequests    []*agentapi.RebootRequest
	onReboot          func(*agentapi.RebootRequest)
}

type fakeWipeClusterConnector struct {
	clients map[string]*fakeKatlcAgentClient
}

type fakeKubectlRunner struct {
	calls   [][]string
	results []readiness.CommandResult
	errs    []error
}

func (r *fakeKubectlRunner) Run(_ context.Context, argv []string) (readiness.CommandResult, error) {
	r.calls = append(r.calls, append([]string(nil), argv...))
	var result readiness.CommandResult
	if len(r.results) > 0 {
		result = r.results[0]
		r.results = r.results[1:]
	}
	var err error
	if len(r.errs) > 0 {
		err = r.errs[0]
		r.errs = r.errs[1:]
	}
	return result, err
}

func newFakeWipeClusterConnector(clients map[string]*fakeKatlcAgentClient) *fakeWipeClusterConnector {
	return &fakeWipeClusterConnector{clients: clients}
}

func (c *fakeWipeClusterConnector) Connect(_ context.Context, node inventory.PlannedNode) (cluster.AgentConnection, error) {
	client := c.clients[node.Name]
	if client == nil {
		return cluster.AgentConnection{}, errors.New("missing fake katlc agent for " + node.Name)
	}
	return cluster.AgentConnection{
		Endpoint: node.Address + ":9443",
		Client:   client,
		Close:    func() error { return nil },
	}, nil
}

func readyWipeClusterClient(machineID string) *fakeKatlcAgentClient {
	return &fakeKatlcAgentClient{
		nodeStatus: &agentapi.NodeStatus{
			ApiVersion:              operation.APIVersion,
			MachineId:               machineID,
			SupportedOperationKinds: []string{wipeClusterOperationKind},
		},
		submitAccepted: &agentapi.OperationAccepted{
			OperationId:   "wipe-" + machineID,
			OperationKind: wipeClusterOperationKind,
			RequestDigest: strings.Repeat("a", 64),
			InitialStatus: &agentapi.OperationStatus{Phase: "completed", Terminal: true, Result: "succeeded"},
		},
	}
}

func (c *fakeKatlcAgentClient) GetNodeStatus(context.Context, *agentapi.GetNodeStatusRequest, ...grpc.CallOption) (*agentapi.NodeStatus, error) {
	return c.nodeStatus, c.nodeStatusErr
}

func (c *fakeKatlcAgentClient) Reboot(_ context.Context, req *agentapi.RebootRequest, _ ...grpc.CallOption) (*agentapi.RebootAccepted, error) {
	c.rebootRequests = append(c.rebootRequests, req)
	if c.onReboot != nil {
		c.onReboot(req)
	}
	return &agentapi.RebootAccepted{Scheduled: true, TargetGenerationId: req.TargetGenerationId}, nil
}

func (c *fakeKatlcAgentClient) ValidateConfig(_ context.Context, req *agentapi.ValidateConfigRequest, _ ...grpc.CallOption) (*agentapi.ConfigValidationResult, error) {
	c.validateRequest = req
	return c.validateResult, nil
}

func (c *fakeKatlcAgentClient) ApplyGeneration(_ context.Context, req *agentapi.GenerationApplyRequest, _ ...grpc.CallOption) (*agentapi.OperationAccepted, error) {
	c.applyRequest = req
	return c.stageAccepted, nil
}

func (c *fakeKatlcAgentClient) StageGeneration(_ context.Context, req *agentapi.GenerationApplyRequest, _ ...grpc.CallOption) (*agentapi.OperationAccepted, error) {
	c.stageRequest = req
	return c.stageAccepted, nil
}

func (c *fakeKatlcAgentClient) SubmitOperation(_ context.Context, req *agentapi.SubmitOperationRequest, _ ...grpc.CallOption) (*agentapi.OperationAccepted, error) {
	if c.onSubmit != nil {
		c.onSubmit(req)
	}
	c.submitRequest = req
	c.submitRequests = append(c.submitRequests, req)
	if req.DryRun {
		return &agentapi.OperationAccepted{
			OperationKind: req.OperationKind,
			RequestDigest: strings.Repeat("d", 64),
			InitialStatus: &agentapi.OperationStatus{Phase: "dry-run"},
		}, nil
	}
	if c.submitAccepted != nil {
		return c.submitAccepted, nil
	}
	return c.stageAccepted, nil
}

func (c *fakeKatlcAgentClient) CreateWorkerJoinMaterial(context.Context, *agentapi.CreateWorkerJoinMaterialRequest, ...grpc.CallOption) (*agentapi.CreateWorkerJoinMaterialResponse, error) {
	return nil, nil
}

func (c *fakeKatlcAgentClient) GetOperation(_ context.Context, req *agentapi.GetOperationRequest, _ ...grpc.CallOption) (*agentapi.OperationStatus, error) {
	c.operationRequest = req
	if c.operationStatus == nil {
		accepted := c.stageAccepted
		if c.submitAccepted != nil {
			accepted = c.submitAccepted
		}
		status := &agentapi.OperationStatus{OperationId: req.OperationId, Phase: "completed", Terminal: true, Result: "succeeded"}
		if accepted != nil {
			status.OperationKind = accepted.GetOperationKind()
		}
		return status, nil
	}
	return c.operationStatus, nil
}

func (c *fakeKatlcAgentClient) ListOperations(_ context.Context, req *agentapi.ListOperationsRequest, _ ...grpc.CallOption) (*agentapi.ListOperationsResponse, error) {
	c.operationsRequest = req
	if c.operations == nil {
		return &agentapi.ListOperationsResponse{}, nil
	}
	return c.operations, nil
}

func (c *fakeKatlcAgentClient) WatchOperation(context.Context, *agentapi.WatchOperationRequest, ...grpc.CallOption) (agentapi.KatlcAgent_WatchOperationClient, error) {
	return nil, nil
}

func (c *fakeKatlcAgentClient) ListGenerations(context.Context, *agentapi.ListGenerationsRequest, ...grpc.CallOption) (*agentapi.ListGenerationsResponse, error) {
	return nil, nil
}

func (c *fakeKatlcAgentClient) GetGeneration(_ context.Context, req *agentapi.GetGenerationRequest, _ ...grpc.CallOption) (*agentapi.Generation, error) {
	c.generationRequest = req
	return c.generation, nil
}

func writeConfigApplyFixture(t *testing.T, root string, fixture configApplyFixture) {
	t.Helper()
	previous := configApplyBaseRecord(fixture.PreviousGeneration)
	record, err := generation.NewRuntimeConfigRecord(generation.RuntimeConfigRequest{
		GenerationID:       fixture.GenerationID,
		Previous:           previous,
		SourceDigest:       strings.Repeat("d", 64),
		GeneratedConfext:   configApplyConfext(fixture.GenerationID),
		ChangedDomains:     fixture.Domains,
		RequestedApplyMode: fixture.Mode,
		AcceptedApplyMode:  fixture.Mode,
		Kubeadm:            fixture.Kubeadm,
		CreatedAt:          time.Date(2026, 6, 5, 18, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("NewRuntimeConfigRecord() error = %v", err)
	}
	metadataPath, err := generation.MetadataPath(root, fixture.GenerationID)
	if err != nil {
		t.Fatalf("MetadataPath() error = %v", err)
	}
	if err := generation.WriteRecord(metadataPath, record); err != nil {
		t.Fatalf("WriteRecord() error = %v", err)
	}
	status, err := generation.NewConfigApplyStatus(generation.ConfigApplyStatusRequest{
		GenerationID:       fixture.GenerationID,
		PreviousGeneration: fixture.PreviousGeneration,
		RequestedApplyMode: fixture.Mode,
		AcceptedApplyMode:  fixture.Mode,
		ChangedDomains:     fixture.Domains,
		HealthState:        "unknown",
		Kubeadm:            fixture.Kubeadm,
		UpdatedAt:          time.Date(2026, 6, 5, 18, 5, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("NewConfigApplyStatus() error = %v", err)
	}
	status.Phase = fixture.Phase
	status.FailureReason = fixture.FailureReason
	status.DomainActions = []generation.ConfigApplyDomainAction{{
		Domain: fixture.Domains[0],
		Action: "fixture",
		Status: generation.ConfigApplyActionPassed,
	}}
	if fixture.RollbackTarget != "" {
		status.Rollback = &generation.ConfigApplyRollback{
			TargetGenerationID: fixture.RollbackTarget,
			Result:             generation.ConfigApplyActionPassed,
			Reason:             "fixture rollback",
		}
	}
	statusPath, err := generation.ConfigApplyStatusPath(root, fixture.GenerationID)
	if err != nil {
		t.Fatalf("ConfigApplyStatusPath() error = %v", err)
	}
	if err := generation.WriteConfigApplyStatus(statusPath, status); err != nil {
		t.Fatalf("WriteConfigApplyStatus() error = %v", err)
	}
}

func configApplyBaseRecord(id string) generation.Record {
	return generation.Record{
		APIVersion:     generation.APIVersion,
		Kind:           generation.Kind,
		GenerationID:   id,
		RuntimeVersion: "0.1.0",
		Root: generation.RootSelection{
			Slot:                  "root-a",
			PartitionUUID:         "11111111-2222-3333-4444-555555555555",
			RuntimeVersion:        "0.1.0",
			RuntimeInterface:      "katl-runtime-1",
			Architecture:          "x86_64",
			RuntimeArtifactSHA256: strings.Repeat("a", 64),
		},
		Boot: generation.BootSelection{UKIPath: "/efi/EFI/Linux/katl-" + id + ".efi"},
		Sysexts: []generation.ExtensionRef{{
			Name:            "kubernetes",
			Path:            "/var/lib/katl/generations/" + id + "/sysext/kubernetes.raw",
			ActivationPath:  "/run/extensions/kubernetes.raw",
			SHA256:          strings.Repeat("b", 64),
			ArtifactVersion: "k8s-v1.36.1",
			PayloadVersion:  "v1.36.1",
			Architecture:    "x86_64",
			Compatibility: generation.ExtensionCompatibility{
				RuntimeInterfaces: []string{"katl-runtime-1"},
			},
		}},
		Confexts: []generation.GeneratedConfext{configApplyConfext(id)},
		KernelCommandLine: []string{
			"root=PARTUUID=11111111-2222-3333-4444-555555555555",
			"rootfstype=squashfs",
			"ro",
		},
		CreatedAt:   time.Date(2026, 6, 5, 17, 0, 0, 0, time.UTC),
		BootState:   "good",
		HealthState: "healthy",
	}
}

func configApplyConfext(id string) generation.GeneratedConfext {
	return generation.GeneratedConfext{
		Name:           "katl-node",
		Path:           "/var/lib/katl/generations/" + id + "/confext",
		ActivationPath: "/run/confexts/katl-node",
		SHA256:         strings.Repeat("d", 64),
		Compatibility: generation.ConfextCompatibility{
			ID:           "katl",
			VersionID:    "0.1.0",
			ConfextLevel: 1,
		},
	}
}

func TestParseVSockCredentialRef(t *testing.T) {
	cid, port, err := parseVSockCredentialRef("vsock:1234:10240")
	if err != nil {
		t.Fatalf("parseVSockCredentialRef() error = %v", err)
	}
	if cid != 1234 || port != 10240 {
		t.Fatalf("cid=%d port=%d", cid, port)
	}
	for _, value := range []string{"agent/cp-1", "vsock:0:10240", "vsock:abc:10240"} {
		if _, _, err := parseVSockCredentialRef(value); err == nil {
			t.Fatalf("parseVSockCredentialRef(%q) error = nil, want validation", value)
		}
	}
}

func TestVMTestAgentTransportWritesPerNodeTranscript(t *testing.T) {
	transcriptDir := t.TempDir()
	guestDir := t.TempDir()
	secretPath := filepath.Join(guestDir, "admin.conf")
	if err := os.WriteFile(secretPath, []byte("secret"), 0o600); err != nil {
		t.Fatalf("write secret fixture: %v", err)
	}
	oldDial := dialVMTestAgent
	dialVMTestAgent = func(_ context.Context, cid, port uint32, transcript string) (*vmtest.AgentClient, error) {
		nameByCID := map[uint32]string{
			1234: "cp-1",
			5678: "worker-1",
		}
		nodeName, ok := nameByCID[cid]
		if !ok || port != 10240 {
			t.Fatalf("dial cid=%d port=%d", cid, port)
		}
		if transcript != filepath.Join(transcriptDir, nodeName+".jsonl") {
			t.Fatalf("transcript = %q", transcript)
		}
		serverConn, clientConn := net.Pipe()
		server := vmtest.NewAgentServer("test")
		server.AllowedFilePaths = []string{guestDir + string(os.PathSeparator)}
		server.CommandRunner = commandRunnerFunc(func(context.Context, *vmtestpb.RunCommandRequest) (*vmtestpb.CommandResult, error) {
			return &vmtestpb.CommandResult{ExitStatus: 0, Stdout: []byte("ok"), StdoutBytes: 2}, nil
		})
		done := make(chan error, 1)
		go func() { done <- server.Serve(context.Background(), serverConn) }()
		client := vmtest.NewAgentClient(clientConn, transcript)
		t.Cleanup(func() {
			_ = client.Close()
			if err := <-done; err != nil {
				t.Fatalf("agent server: %v", err)
			}
		})
		return client, nil
	}
	t.Cleanup(func() { dialVMTestAgent = oldDial })

	transport := vmtestAgentTransport{TranscriptDir: transcriptDir}
	_, err := transport.RunCommand(context.Background(), inventory.PlannedNode{
		Name:   "cp-1",
		Access: inventory.Access{Method: "agent", CredentialRef: "vsock:1234:10240"},
	}, readiness.CommandRequest{
		Argv:            []string{"kubeadm", "init"},
		SensitiveOutput: true,
	})
	if err != nil {
		t.Fatalf("RunCommand() error = %v", err)
	}
	_, err = transport.ReadFile(context.Background(), inventory.PlannedNode{
		Name:   "cp-1",
		Access: inventory.Access{Method: "agent", CredentialRef: "vsock:1234:10240"},
	}, readiness.FileRequest{
		Path:      secretPath,
		Sensitive: true,
	})
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	_, err = transport.RunCommand(context.Background(), inventory.PlannedNode{
		Name:   "worker-1",
		Access: inventory.Access{Method: "agent", CredentialRef: "vsock:5678:10240"},
	}, readiness.CommandRequest{
		Argv:            []string{"kubeadm", "join"},
		SensitiveOutput: true,
	})
	if err != nil {
		t.Fatalf("worker RunCommand() error = %v", err)
	}
	entries := readTranscript(t, filepath.Join(transcriptDir, "cp-1.jsonl"))
	if len(entries) != 2 {
		t.Fatalf("transcript entries = %#v", entries)
	}
	if entries[0].Method != "RunCommand" || entries[0].Redaction != "output" || entries[0].StdoutBytes != 2 {
		t.Fatalf("transcript entry = %#v", entries[0])
	}
	if entries[1].Method != "ReadFile" || entries[1].Redaction != "sensitive" || !entries[1].SensitiveOutput {
		t.Fatalf("file transcript entry = %#v", entries[1])
	}
	workerEntries := readTranscript(t, filepath.Join(transcriptDir, "worker-1.jsonl"))
	if len(workerEntries) != 1 {
		t.Fatalf("worker transcript entries = %#v", workerEntries)
	}
	if workerEntries[0].Method != "RunCommand" || workerEntries[0].Redaction != "output" || !workerEntries[0].SensitiveOutput {
		t.Fatalf("worker transcript entry = %#v", workerEntries[0])
	}
}

type commandRunnerFunc func(context.Context, *vmtestpb.RunCommandRequest) (*vmtestpb.CommandResult, error)

func (f commandRunnerFunc) Run(ctx context.Context, req *vmtestpb.RunCommandRequest) (*vmtestpb.CommandResult, error) {
	return f(ctx, req)
}

type transcriptEntry struct {
	Method          string   `json:"method"`
	Argv            []string `json:"argv,omitempty"`
	Redaction       string   `json:"redaction,omitempty"`
	StdoutBytes     uint32   `json:"stdoutBytes,omitempty"`
	SensitiveOutput bool     `json:"sensitiveOutput,omitempty"`
}

func readTranscript(t *testing.T, path string) []transcriptEntry {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open transcript: %v", err)
	}
	defer file.Close()
	var entries []transcriptEntry
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var entry transcriptEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			t.Fatalf("decode transcript: %v", err)
		}
		entries = append(entries, entry)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan transcript: %v", err)
	}
	return entries
}

func writeInventory(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cluster.yaml")
	data := `controlPlaneEndpoint: api.katl.test:6443
kubernetesVersion: v1.36.1
nodes:
- name: cp-1
  address: 10.0.0.11
  systemRole: control-plane
  access:
    method: agent
    credentialRef: agent/cp-1
  kubeadmConfig:
    ref: control-plane
    path: /etc/katl/kubeadm/control-plane/config.yaml
    intent: control-plane
  kubernetesVersion: v1.36.1
- name: worker-1
  address: 10.0.0.21
  systemRole: worker
  access:
    method: agent
    credentialRef: agent/worker-1
  kubeadmConfig:
    ref: worker
    path: /etc/katl/kubeadm/worker/config.yaml
    intent: worker
  kubernetesVersion: v1.36.1
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeKatlctlConfig(t *testing.T, data string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "katlctl.yaml")
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func configBundleSource() string {
	return `apiVersion: config.katl.dev/v1alpha1
kind: ClusterConfig
metadata:
  name: lab
spec:
  controlPlaneEndpoint: api.katl.test:6443
  kubernetes:
    version: v1.36.1
  defaults:
    install:
      targetDiskDefaults:
        minSizeMiB: 32768
    identity:
      ssh:
        authorizedKeys:
          - ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDAxMjM0NTY3ODlhYmNkZWYwMTIzNDU2Nzg5YWJjZGVm katl@example
  nodes:
    - name: cp-1
      systemRole: control-plane
      bootstrap:
        address: 10.0.0.11
      install:
        targetDisk:
          byID: /dev/disk/by-id/ata-cp-root
`
}

func writeConfigBundle(t *testing.T) (string, string) {
	t.Helper()
	sourcePath := writeClusterConfig(t)
	archive, result, err := configbundle.BuildArchive(configbundle.BuildRequest{SourcePath: sourcePath})
	if err != nil {
		t.Fatalf("BuildArchive() error = %v", err)
	}
	bundlePath := filepath.Join(filepath.Dir(sourcePath), "cluster.katlcfg")
	if err := os.WriteFile(bundlePath, archive, 0o600); err != nil {
		t.Fatal(err)
	}
	return bundlePath, result.Digest
}

func writeClusterConfig(t *testing.T) string {
	t.Helper()
	sourcePath := filepath.Join(t.TempDir(), "cluster.yaml")
	if err := os.WriteFile(sourcePath, []byte(configBundleSource()), 0o644); err != nil {
		t.Fatal(err)
	}
	return sourcePath
}
