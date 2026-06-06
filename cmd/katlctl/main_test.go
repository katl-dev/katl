package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zariel/katl/internal/bootstrap/cluster"
	"github.com/zariel/katl/internal/bootstrap/inventory"
	"github.com/zariel/katl/internal/bootstrap/readiness"
	"github.com/zariel/katl/internal/installer/generation"
	"github.com/zariel/katl/internal/vmtest"
	vmtestpb "github.com/zariel/katl/internal/vmtest/proto"
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

func TestClusterBootstrapRequiresInventory(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{"cluster", "bootstrap"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "--inventory is required") {
		t.Fatalf("run() error = %v, want inventory error", err)
	}
}

func TestClusterBootstrapRejectsInvalidBootstrapWait(t *testing.T) {
	inventoryPath := writeInventory(t)
	old := runBootstrap
	runBootstrap = func(context.Context, cluster.Request, cluster.Dependencies) (cluster.Result, error) {
		t.Fatal("runBootstrap should not be called for invalid bootstrap wait")
		return cluster.Result{}, nil
	}
	t.Cleanup(func() { runBootstrap = old })

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
	old := runBootstrap
	runBootstrap = func(_ context.Context, request cluster.Request, _ cluster.Dependencies) (cluster.Result, error) {
		got = request
		return cluster.Result{}, nil
	}
	t.Cleanup(func() { runBootstrap = old })

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
		"config", "apply", "status",
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
		"config", "apply", "status",
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
