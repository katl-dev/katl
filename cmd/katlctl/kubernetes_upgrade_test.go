package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/installer/artifact"
	"github.com/katl-dev/katl/internal/installer/kubernetesbundle"
	"github.com/katl-dev/katl/internal/installer/operation"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/katlctl/workstation"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestKubernetesUpgradePlansAndRunsControlPlanesBeforeWorkers(t *testing.T) {
	nodes := []workstation.Node{
		{Name: "worker-1", ManagementEndpoint: "192.0.2.4:9443", SystemRole: inventory.RoleWorker, EnrollmentID: "enrollment-worker-1", MachineID: "machine-worker-1"},
		{Name: "cp-2", ManagementEndpoint: "192.0.2.2:9443", SystemRole: inventory.RoleControlPlane, EnrollmentID: "enrollment-cp-2", MachineID: "machine-cp-2"},
		{Name: "cp-1", ManagementEndpoint: "192.0.2.1:9443", SystemRole: inventory.RoleControlPlane, EnrollmentID: "enrollment-cp-1", MachineID: "machine-cp-1"},
	}
	clusterConfig, configPath := writeKubernetesUpgradeConfig(t, "v1.36.1", nodes...)
	clients := map[string]*fakeKatlcAgentClient{}
	var executionOrder []string
	for _, name := range []string{"cp-1", "cp-2", "worker-1"} {
		name := name
		client := &fakeKatlcAgentClient{
			nodeStatus:      &agentapi.NodeStatus{MachineId: "machine-" + name, EnrollmentId: "enrollment-" + name, InventoryNodeName: name, AgentStartId: "before-" + name, CurrentGenerationId: "gen-1"},
			generation:      &agentapi.Generation{GenerationId: "gen-1", CommitState: "committed", BootState: "good", HealthState: "healthy", Sysexts: []*agentapi.ExtensionRef{{Name: "kubernetes", PayloadVersion: "v1.36.0", Sha256: strings.Repeat("a", 64)}}},
			submitAccepted:  &agentapi.OperationAccepted{OperationId: "internal-" + name},
			operationStatus: &agentapi.OperationStatus{Terminal: true, Result: operation.ResultSucceeded, Phase: "healthy"},
		}
		client.onSubmit = func(req *agentapi.SubmitOperationRequest) {
			if !req.DryRun {
				executionOrder = append(executionOrder, name)
			}
		}
		clients[name] = client
	}
	byEndpoint := map[string]*fakeKatlcAgentClient{"192.0.2.1:9443": clients["cp-1"], "192.0.2.2:9443": clients["cp-2"], "192.0.2.4:9443": clients["worker-1"]}
	previousDial := dialKatlcAgent
	previousNow := kubernetesUpgradeNow
	previousEndpointDial := dialKubernetesEndpoint
	defer func() {
		dialKatlcAgent = previousDial
		kubernetesUpgradeNow = previousNow
		dialKubernetesEndpoint = previousEndpointDial
	}()
	dialKatlcAgent = func(_ context.Context, endpoint string) (katlcAgentConnection, error) {
		return katlcAgentConnection{Client: byEndpoint[endpoint], Close: func() error { return nil }}, nil
	}
	kubernetesUpgradeNow = func() time.Time { return time.Unix(42, 0).UTC() }
	dialKubernetesEndpoint = func(context.Context, string) error { return nil }
	var stdout bytes.Buffer
	if err := runKubernetesUpgrade(context.Background(), kubernetesUpgradeOptions{clusterConfig: clusterConfig, configPath: configPath, timeout: time.Minute, output: "json"}, &stdout, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout.String(), "internal-") || strings.Contains(strings.ToLower(stdout.String()), "digest") {
		t.Fatalf("output exposed internal operation data: %s", stdout.String())
	}
	var report kubernetesUpgradeReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.SourceVersion != "v1.36.0" || report.TargetVersion != "v1.36.1" || len(report.Nodes) != 3 || !strings.Contains(report.NextAction, "complete") {
		t.Fatalf("report = %#v", report)
	}
	upgradeRoles := []string{"apply", "control-plane", "worker"}
	reportRoles := []string{"control-plane", "control-plane", "worker"}
	for i, name := range []string{"cp-1", "cp-2", "worker-1"} {
		if report.Nodes[i].Name != name || report.Nodes[i].Role != reportRoles[i] || report.Nodes[i].Result != operation.ResultSucceeded || report.Nodes[i].Phase != "healthy" {
			t.Fatalf("node report %d = %#v", i, report.Nodes[i])
		}
		requests := clients[name].submitRequests
		if len(requests) == 0 || requests[len(requests)-1].DryRun {
			t.Fatalf("%s requests = %#v", name, requests)
		}
		body := requests[len(requests)-1].KubernetesSysextUpdate
		if body.UpgradeRole != upgradeRoles[i] || body.SourcePayloadVersion != "v1.36.0" || body.TargetPayloadVersion != "v1.36.1" {
			t.Fatalf("%s body = %#v", name, body)
		}
		if body.TargetSysextPath != "" || body.TargetSysextSha256 != "" || body.SnapshotDigest != "" || body.CandidateGenerationId == "" {
			t.Fatalf("%s request exposed internal artifact or snapshot inputs: %#v", name, body)
		}
		if len(clients[name].rebootRequests) != 0 {
			t.Fatalf("%s unexpectedly rebooted: %#v", name, clients[name].rebootRequests)
		}
	}
	if want := []string{"cp-1", "cp-2", "worker-1"}; !reflect.DeepEqual(executionOrder, want) {
		t.Fatalf("execution order = %v, want %v", executionOrder, want)
	}
}

func TestKubernetesUpgradeBundleUsesReleaseCompatibility(t *testing.T) {
	bundle, err := kubernetesUpgradeBundle("v1.36.1", "")
	if err != nil {
		t.Fatalf("kubernetesUpgradeBundle() error = %v", err)
	}
	image, err := kubernetesbundle.ParseImageReference(bundle)
	if err != nil {
		t.Fatalf("ParseImageReference() error = %v", err)
	}
	if image.PayloadVersion != "v1.36.1" || image.ArtifactVersion == "" || image.ManifestDigest == "" {
		t.Fatalf("image = %#v", image)
	}
	if _, err := kubernetesUpgradeBundle("v9.99.9", ""); err == nil || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("unavailable version error = %v", err)
	}
}

func TestKubernetesUpgradePlanDoesNotExecute(t *testing.T) {
	clusterConfig, contextPath := writeKubernetesUpgradeConfig(t, "v1.36.1", workstation.Node{Name: "cp-1", ManagementEndpoint: "192.0.2.1:9443", SystemRole: inventory.RoleControlPlane, EnrollmentID: "enrollment-cp-1", MachineID: "machine"})
	client := &fakeKatlcAgentClient{nodeStatus: &agentapi.NodeStatus{MachineId: "machine", EnrollmentId: "enrollment-cp-1", InventoryNodeName: "cp-1", CurrentGenerationId: "gen-1"}, generation: &agentapi.Generation{GenerationId: "gen-1", CommitState: "committed", HealthState: "healthy", Sysexts: []*agentapi.ExtensionRef{{Name: "kubernetes", PayloadVersion: "v1.36.0"}}}}
	previous := dialKatlcAgent
	defer func() { dialKatlcAgent = previous }()
	dialKatlcAgent = func(context.Context, string) (katlcAgentConnection, error) {
		return katlcAgentConnection{Client: client, Close: func() error { return nil }}, nil
	}
	var stdout bytes.Buffer
	if err := run(context.Background(), []string{"kubernetes", "upgrade", "--config", clusterConfig, "--context-file", contextPath, "--plan", "--timeout", "1m", "--output", "json"}, &stdout, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if len(client.submitRequests) != 1 || !client.submitRequests[0].DryRun || !strings.Contains(stdout.String(), `"role": "control-plane"`) || strings.Contains(stdout.String(), `"role": "apply"`) || !strings.Contains(stdout.String(), `"result": "planned"`) {
		t.Fatalf("requests=%#v output=%s", client.submitRequests, stdout.String())
	}
}

func TestKubernetesUpgradeConfigVersionChangeProducesPlan(t *testing.T) {
	clusterConfig, contextPath := writeKubernetesUpgradeConfig(t, "v1.36.1", workstation.Node{Name: "cp-1", ManagementEndpoint: "192.0.2.1:9443", SystemRole: inventory.RoleControlPlane, EnrollmentID: "enrollment-cp-1", MachineID: "machine"})
	data, err := os.ReadFile(clusterConfig)
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.Replace(data, []byte("version: v1.36.1"), []byte("version: v1.36.2"), 1)
	if err := os.WriteFile(clusterConfig, data, 0o600); err != nil {
		t.Fatal(err)
	}
	client := healthyKubernetesUpgradeClient()
	previous := dialKatlcAgent
	t.Cleanup(func() { dialKatlcAgent = previous })
	dialKatlcAgent = func(context.Context, string) (katlcAgentConnection, error) {
		return katlcAgentConnection{Client: client, Close: func() error { return nil }}, nil
	}
	var stdout bytes.Buffer
	if err := run(context.Background(), []string{"kubernetes", "upgrade", "--config", clusterConfig, "--context-file", contextPath, "--plan", "--timeout", "1m", "--output", "json"}, &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(client.submitRequests) != 1 || !client.submitRequests[0].DryRun {
		t.Fatalf("requests = %#v", client.submitRequests)
	}
	body := client.submitRequests[0].KubernetesSysextUpdate
	if body.SourcePayloadVersion != "v1.36.1" || body.TargetPayloadVersion != "v1.36.2" {
		t.Fatalf("upgrade body = %#v", body)
	}
}

func TestKubernetesUpgradeRejectsPositionalVersionBeforeConnecting(t *testing.T) {
	clusterConfig, _ := writeKubernetesUpgradeConfig(t, "v1.36.2", workstation.Node{Name: "cp-1", ManagementEndpoint: "192.0.2.1:9443", SystemRole: inventory.RoleControlPlane})
	dialed := false
	previous := dialKatlcAgent
	t.Cleanup(func() { dialKatlcAgent = previous })
	dialKatlcAgent = func(context.Context, string) (katlcAgentConnection, error) {
		dialed = true
		return katlcAgentConnection{}, fmt.Errorf("unexpected dial")
	}
	err := run(context.Background(), []string{"kubernetes", "upgrade", "v1.36.1", "--config", clusterConfig, "--plan"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), `unknown command "v1.36.1"`) {
		t.Fatalf("run() error = %v", err)
	}
	if dialed {
		t.Fatal("positional version connected to a node")
	}
}

func TestKubernetesUpgradeResumeKeepsConfiguredSourceAndTarget(t *testing.T) {
	nodes := []workstation.Node{
		{Name: "cp-1", ManagementEndpoint: "192.0.2.1:9443", SystemRole: inventory.RoleControlPlane, EnrollmentID: "enrollment-cp-1", MachineID: "machine-cp-1"},
		{Name: "cp-2", ManagementEndpoint: "192.0.2.2:9443", SystemRole: inventory.RoleControlPlane, EnrollmentID: "enrollment-cp-2", MachineID: "machine-cp-2"},
		{Name: "worker-1", ManagementEndpoint: "192.0.2.3:9443", SystemRole: inventory.RoleWorker, EnrollmentID: "enrollment-worker-1", MachineID: "machine-worker-1"},
	}
	clusterConfig, contextPath := writeKubernetesUpgradeConfig(t, "v1.36.2", nodes...)
	clients := map[string]*fakeKatlcAgentClient{}
	for _, node := range nodes {
		version := "v1.36.1"
		if node.Name == "cp-1" {
			version = "v1.36.2"
		}
		clients[node.ManagementEndpoint] = &fakeKatlcAgentClient{
			nodeStatus: &agentapi.NodeStatus{MachineId: node.MachineID, EnrollmentId: node.EnrollmentID, InventoryNodeName: node.Name, CurrentGenerationId: "gen-1"},
			generation: &agentapi.Generation{GenerationId: "gen-1", CommitState: "committed", HealthState: "healthy", Sysexts: []*agentapi.ExtensionRef{{Name: "kubernetes", PayloadVersion: version}}},
		}
	}
	previous := dialKatlcAgent
	t.Cleanup(func() { dialKatlcAgent = previous })
	dialKatlcAgent = func(_ context.Context, endpoint string) (katlcAgentConnection, error) {
		return katlcAgentConnection{Client: clients[endpoint], Close: func() error { return nil }}, nil
	}
	var stdout bytes.Buffer
	if err := run(context.Background(), []string{"kubernetes", "upgrade", "--config", clusterConfig, "--context-file", contextPath, "--plan", "--timeout", "1m", "--output", "json"}, &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(clients["192.0.2.1:9443"].submitRequests) != 0 {
		t.Fatalf("completed node requests = %#v", clients["192.0.2.1:9443"].submitRequests)
	}
	for _, endpoint := range []string{"192.0.2.2:9443", "192.0.2.3:9443"} {
		requests := clients[endpoint].submitRequests
		if len(requests) != 1 || !requests[0].DryRun {
			t.Fatalf("%s requests = %#v", endpoint, requests)
		}
		body := requests[0].KubernetesSysextUpdate
		if body.SourcePayloadVersion != "v1.36.1" || body.TargetPayloadVersion != "v1.36.2" {
			t.Fatalf("%s upgrade body = %#v", endpoint, body)
		}
	}
}

func TestKubernetesUpgradeLocalArtifactPlanDoesNotUpload(t *testing.T) {
	clusterConfig, contextPath := writeKubernetesUpgradeConfig(t, "v1.36.2", workstation.Node{Name: "cp-1", ManagementEndpoint: "192.0.2.1:9443", SystemRole: inventory.RoleControlPlane, EnrollmentID: "enrollment-cp-1", MachineID: "machine"})
	local := writeKubernetesUpgradeArtifact(t, "v1.36.2")
	client := healthyKubernetesUpgradeClient()
	previous := dialKatlcAgent
	t.Cleanup(func() { dialKatlcAgent = previous })
	dialKatlcAgent = func(context.Context, string) (katlcAgentConnection, error) {
		return katlcAgentConnection{Client: client, Close: func() error { return nil }}, nil
	}
	var stdout bytes.Buffer
	err := run(context.Background(), []string{"kubernetes", "upgrade", "--config", clusterConfig, "--context-file", contextPath, "--artifact", local.Path, "--plan", "--timeout", "1m", "--output", "json"}, &stdout, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(client.stageArtifact) != 0 {
		t.Fatalf("plan uploaded %d artifact chunks", len(client.stageArtifact))
	}
	if len(client.submitRequests) != 1 || !client.submitRequests[0].DryRun {
		t.Fatalf("requests = %#v", client.submitRequests)
	}
	body := client.submitRequests[0].KubernetesSysextUpdate
	if body.TargetSysextPath != kubernetesUpgradeArtifactLogicalPath(kubernetesUpgradeArtifactLocalRef(local.SHA256)) || body.TargetSysextSha256 != local.SHA256 || body.TargetSysextSizeBytes != local.SizeBytes || body.KubernetesBundleRef != "" {
		t.Fatalf("local plan body = %#v", body)
	}
	var report kubernetesUpgradeReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Artifact != local.Path || report.Bundle != "" || !report.Plan {
		t.Fatalf("report = %#v", report)
	}
}

func TestKubernetesUpgradeUploadsLocalArtifactBeforeOnlineOperation(t *testing.T) {
	clusterConfig, configPath := writeKubernetesUpgradeConfig(t, "v1.36.2", workstation.Node{Name: "cp-1", ManagementEndpoint: "192.0.2.1:9443", SystemRole: inventory.RoleControlPlane, EnrollmentID: "enrollment-cp-1", MachineID: "machine"})
	local := writeKubernetesUpgradeArtifact(t, "v1.36.2")
	client := healthyKubernetesUpgradeClient()
	previousDial := dialKatlcAgent
	previousEndpoint := dialKubernetesEndpoint
	t.Cleanup(func() {
		dialKatlcAgent = previousDial
		dialKubernetesEndpoint = previousEndpoint
	})
	dialKatlcAgent = func(context.Context, string) (katlcAgentConnection, error) {
		return katlcAgentConnection{Client: client, Close: func() error { return nil }}, nil
	}
	dialKubernetesEndpoint = func(context.Context, string) error { return nil }
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{"kubernetes", "upgrade", "--config", clusterConfig, "--context-file", configPath, "--artifact", local.Path, "--timeout", "1m", "--output", "json"}, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if len(client.stageArtifact) != 1 {
		t.Fatalf("staged chunks = %d, want 1", len(client.stageArtifact))
	}
	first := client.stageArtifact[0]
	if first.Kind != "StageKubernetesUpgradeArtifactRequest" || first.Actor != "katlctl kubernetes upgrade" || first.ExpectedMachineId != "machine" || first.Sha256 != local.SHA256 || first.SizeBytes != local.SizeBytes || string(first.Chunk) != "local kubernetes upgrade" {
		t.Fatalf("first staged chunk = %#v", first)
	}
	if len(client.submitRequests) != 2 || !client.submitRequests[0].DryRun || client.submitRequests[1].DryRun {
		t.Fatalf("requests = %#v", client.submitRequests)
	}
	body := client.submitRequests[1].KubernetesSysextUpdate
	if body.TargetSysextPath != kubernetesUpgradeArtifactLogicalPath(kubernetesUpgradeArtifactLocalRef(local.SHA256)) || body.KubernetesBundleRef != "" {
		t.Fatalf("operation body = %#v", body)
	}
	if len(client.rebootRequests) != 0 {
		t.Fatalf("online upgrade rebooted: %#v", client.rebootRequests)
	}
	if !strings.Contains(stderr.String(), "uploading local Kubernetes v1.36.2 image") {
		t.Fatalf("progress = %q", stderr.String())
	}
}

func TestKubernetesUpgradeRejectsMismatchedLocalArtifactVersion(t *testing.T) {
	clusterConfig, contextPath := writeKubernetesUpgradeConfig(t, "v1.36.1", workstation.Node{Name: "cp-1", ManagementEndpoint: "192.0.2.1:9443", SystemRole: inventory.RoleControlPlane, EnrollmentID: "enrollment-cp-1", MachineID: "machine"})
	local := writeKubernetesUpgradeArtifact(t, "v1.36.2")
	dialed := false
	previous := dialKatlcAgent
	t.Cleanup(func() { dialKatlcAgent = previous })
	dialKatlcAgent = func(context.Context, string) (katlcAgentConnection, error) {
		dialed = true
		return katlcAgentConnection{}, fmt.Errorf("unexpected dial")
	}
	err := runKubernetesUpgrade(context.Background(), kubernetesUpgradeOptions{clusterConfig: clusterConfig, configPath: contextPath, artifact: local.Path, timeout: time.Minute, output: "text"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "does not match spec.kubernetes.version") {
		t.Fatalf("runKubernetesUpgrade() error = %v", err)
	}
	if dialed {
		t.Fatal("mismatched artifact connected to a node")
	}
}

func TestKubernetesUpgradeLocalArtifactExplainsAgentUpgradeRequirement(t *testing.T) {
	local := writeKubernetesUpgradeArtifact(t, "v1.36.2")
	client := &fakeKatlcAgentClient{stageArtifactErr: status.Error(codes.Unimplemented, "old agent")}
	target := kubernetesUpgradeTarget{machineID: "machine", generation: "generation-1", node: workstation.TopologyNode{Name: "cp-1", EnrollmentID: "enrollment"}}
	_, err := stageKubernetesUpgradeArtifact(context.Background(), client, target, local, "cp-1", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "upgrade KatlOS to a build with local Kubernetes artifact support") {
		t.Fatalf("stageKubernetesUpgradeArtifact() error = %v", err)
	}
}

func TestKubernetesUpgradeResolvesClusterConfigWithoutContext(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "cluster.yaml")
	config := strings.Replace(configBundleSource(), "  controlPlaneEndpoint:\n    host: api.katl.test\n    port: 6443\n", "", 1)
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	topology, version, err := resolveKubernetesUpgradeTopology(kubernetesUpgradeOptions{clusterConfig: configPath})
	if err != nil {
		t.Fatal(err)
	}
	if len(topology.Nodes) != 1 || topology.Nodes[0].Name != "cp-1" || topology.Nodes[0].ManagementEndpoint != "10.0.0.11:9443" || topology.ControlPlaneEndpoint != "10.0.0.11:6443" {
		t.Fatalf("topology = %#v", topology)
	}
	if version != "v1.36.1" {
		t.Fatalf("version = %q", version)
	}
}

func TestKubernetesUpgradeCordonRequiresKubeconfig(t *testing.T) {
	err := runKubernetesUpgrade(context.Background(), kubernetesUpgradeOptions{cordon: true, timeout: time.Minute, output: "json"}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "--kubeconfig is required with --cordon") {
		t.Fatalf("runKubernetesUpgrade() error = %v", err)
	}
}

func TestKubernetesUpgradeCordonIsExplicitAndNonDraining(t *testing.T) {
	runner := &fakeKubectlRunner{}
	previous := operatorKubectlRunner
	operatorKubectlRunner = runner
	t.Cleanup(func() { operatorKubectlRunner = previous })
	client := &fakeKatlcAgentClient{
		submitAccepted:  &agentapi.OperationAccepted{OperationId: "upgrade-worker-1"},
		operationStatus: &agentapi.OperationStatus{Terminal: true, Result: operation.ResultSucceeded, Phase: "healthy"},
	}
	image, err := kubernetesbundle.ParseImageReference("ghcr.io/katl-dev/kubernetes:v1.36.1-katl.1")
	if err != nil {
		t.Fatal(err)
	}
	report, err := runKubernetesUpgradeTarget(context.Background(), workstation.ResolvedTopology{}, kubernetesUpgradeOptions{cordon: true, kubeconfig: "/tmp/admin.conf", timeout: time.Minute}, kubernetesUpgradeTarget{
		node: workstation.TopologyNode{Name: "worker-1", SystemRole: "worker"}, upgradeRole: "worker", conn: katlcAgentConnection{Client: client}, machineID: "machine-worker-1", generation: "gen0", source: "v1.36.0", candidate: "gen1",
	}, image, nil, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Result != operation.ResultSucceeded || report.Phase != "healthy" {
		t.Fatalf("report = %#v", report)
	}
	want := [][]string{
		{"kubectl", "--kubeconfig", "/tmp/admin.conf", "cordon", "worker-1"},
		{"kubectl", "--kubeconfig", "/tmp/admin.conf", "uncordon", "worker-1"},
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("kubectl calls = %#v, want %#v", runner.calls, want)
	}
	for _, call := range runner.calls {
		if strings.Contains(strings.Join(call, " "), "drain") {
			t.Fatalf("cordon option invoked drain: %#v", runner.calls)
		}
	}
}

func TestKubernetesUpgradeStopsAfterNodeFailure(t *testing.T) {
	nodes := []workstation.Node{
		{Name: "cp-1", ManagementEndpoint: "192.0.2.1:9443", SystemRole: inventory.RoleControlPlane, EnrollmentID: "enrollment-cp-1", MachineID: "machine-cp-1"},
		{Name: "cp-2", ManagementEndpoint: "192.0.2.2:9443", SystemRole: inventory.RoleControlPlane, EnrollmentID: "enrollment-cp-2", MachineID: "machine-cp-2"},
		{Name: "worker-1", ManagementEndpoint: "192.0.2.3:9443", SystemRole: inventory.RoleWorker, EnrollmentID: "enrollment-worker-1", MachineID: "machine-worker-1"},
	}
	clusterConfig, configPath := writeKubernetesUpgradeConfig(t, "v1.36.1", nodes...)
	clients := map[string]*fakeKatlcAgentClient{}
	for _, node := range nodes {
		client := &fakeKatlcAgentClient{
			nodeStatus:     &agentapi.NodeStatus{MachineId: node.MachineID, EnrollmentId: node.EnrollmentID, InventoryNodeName: node.Name, AgentStartId: "before-" + node.Name, CurrentGenerationId: "gen-1"},
			generation:     &agentapi.Generation{GenerationId: "gen-1", CommitState: "committed", BootState: "good", HealthState: "healthy", Sysexts: []*agentapi.ExtensionRef{{Name: "kubernetes", PayloadVersion: "v1.36.0"}}},
			submitAccepted: &agentapi.OperationAccepted{OperationId: "upgrade-" + node.Name},
			operationStatus: &agentapi.OperationStatus{Terminal: true, Result: operation.ResultSucceeded,
				Phase: "healthy"},
		}
		clients[node.ManagementEndpoint] = client
	}
	clients["192.0.2.2:9443"].operationStatus = &agentapi.OperationStatus{Terminal: true, Result: operation.ResultFailedNeedsRepair, Phase: "failed", FailureReason: "kubeadm failed", RecoveryRequired: true}
	oldDial := dialKatlcAgent
	oldEndpointDial := dialKubernetesEndpoint
	dialKatlcAgent = func(_ context.Context, endpoint string) (katlcAgentConnection, error) {
		return katlcAgentConnection{Client: clients[endpoint], Close: func() error { return nil }}, nil
	}
	dialKubernetesEndpoint = func(context.Context, string) error { return nil }
	t.Cleanup(func() { dialKatlcAgent = oldDial; dialKubernetesEndpoint = oldEndpointDial })

	var stdout bytes.Buffer
	err := runKubernetesUpgrade(context.Background(), kubernetesUpgradeOptions{clusterConfig: clusterConfig, configPath: configPath, timeout: time.Minute, output: "json"}, &stdout, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "stopped at node cp-2") {
		t.Fatalf("runKubernetesUpgrade() error = %v", err)
	}
	for _, request := range clients["192.0.2.3:9443"].submitRequests {
		if !request.DryRun {
			t.Fatalf("worker executed after failure: %#v", request)
		}
	}
}

func writeKubernetesUpgradeConfig(t *testing.T, version string, nodes ...workstation.Node) (string, string) {
	t.Helper()
	contextPath := writeTestEnrollmentContext(t, "home", nodes...)
	var source strings.Builder
	_, _ = fmt.Fprintf(&source, "apiVersion: config.katl.dev/v1alpha1\nkind: ClusterConfig\nmetadata:\n  name: home\nspec:\n  controlPlaneEndpoint:\n    host: 192.0.2.10\n    port: 6443\n  kubernetes:\n    version: %s\n  defaults:\n    install:\n      systemDisk:\n        minSizeMiB: 32768\n    access:\n      ssh:\n        authorizedKeys:\n          - ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDAxMjM0NTY3ODlhYmNkZWYwMTIzNDU2Nzg5YWJjZGVm katl@example\n  nodes:\n", version)
	for _, node := range nodes {
		address := strings.TrimSuffix(node.ManagementEndpoint, ":9443")
		_, _ = fmt.Fprintf(&source, "    - name: %s\n      controlPlane: %t\n      management:\n        address: %s\n      install:\n        systemDisk:\n          byID: /dev/disk/by-id/ata-%s-root\n", node.Name, node.SystemRole == inventory.RoleControlPlane, address, node.Name)
	}
	path := filepath.Join(t.TempDir(), "cluster.yaml")
	if err := os.WriteFile(path, []byte(source.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, contextPath
}

func healthyKubernetesUpgradeClient() *fakeKatlcAgentClient {
	return &fakeKatlcAgentClient{
		nodeStatus: &agentapi.NodeStatus{MachineId: "machine", EnrollmentId: "enrollment-cp-1", InventoryNodeName: "cp-1", CurrentGenerationId: "gen-1"},
		generation: &agentapi.Generation{
			GenerationId: "gen-1", CommitState: "committed", BootState: "good", HealthState: "healthy",
			RuntimeArchitecture: "x86_64",
			Sysexts:             []*agentapi.ExtensionRef{{Name: "kubernetes", PayloadVersion: "v1.36.1"}},
		},
		submitAccepted:  &agentapi.OperationAccepted{OperationId: "upgrade-cp-1"},
		operationStatus: &agentapi.OperationStatus{Terminal: true, Result: operation.ResultSucceeded, Phase: "healthy"},
	}
}

func writeKubernetesUpgradeArtifact(t *testing.T, payloadVersion string) kubernetesUpgradeArtifact {
	t.Helper()
	path := filepath.Join(t.TempDir(), "katl-kubernetes-"+payloadVersion+".raw")
	contents := []byte("local kubernetes upgrade")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(contents))
	meta := artifact.LocalMeta{
		Name: "kubernetes", Kind: artifact.ArtifactSysext, Format: "sysext",
		Path: filepath.Base(path), SizeBytes: int64(len(contents)), SHA256: digest,
		Version: payloadVersion + "-katl.1", PayloadVersion: payloadVersion, Architecture: "x86_64",
		SourceRepo:       &artifact.SourceRepo{ID: "kubernetes", BaseURL: "https://pkgs.k8s.io/core:/stable:/v1.36/rpm/", Minor: "v1.36"},
		PackageVersions:  map[string]string{"kubeadm": "0:1.36.2-150500.2.1", "kubelet": "0:1.36.2-150500.2.1", "kubectl": "0:1.36.2-150500.2.1", "cri-tools": "0:1.36.0-150500.1.1"},
		RuntimeInterface: "katl-runtime-1", Created: "2026-07-24T00:00:00Z",
	}
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".json", append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	return kubernetesUpgradeArtifact{Path: path, PayloadVersion: payloadVersion, Architecture: "x86_64", SHA256: digest, SizeBytes: uint64(len(contents))}
}
