package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/installer/kubernetesbundle"
	"github.com/katl-dev/katl/internal/installer/operation"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/katlctl/workstation"
)

func TestKubernetesUpgradePlansAndRunsControlPlanesBeforeWorkers(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "katlctl.yaml")
	var nodes strings.Builder
	for _, node := range []struct{ name, endpoint, role string }{{"worker-1", "192.0.2.4:9443", "worker"}, {"cp-2", "192.0.2.2:9443", "control-plane"}, {"cp-1", "192.0.2.1:9443", "control-plane"}} {
		_, _ = nodes.WriteString("      - name: " + node.name + "\n        managementEndpoint: " + node.endpoint + "\n        systemRole: " + node.role + "\n")
	}
	config := "currentContext: lab\ncontexts:\n  - name: lab\n    cluster: home\nclusters:\n  - name: home\n    controlPlaneEndpoint: 192.0.2.10:6443\n    nodes:\n" + nodes.String()
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	clients := map[string]*fakeKatlcAgentClient{}
	var executionOrder []string
	for _, name := range []string{"cp-1", "cp-2", "worker-1"} {
		name := name
		client := &fakeKatlcAgentClient{
			nodeStatus:      &agentapi.NodeStatus{MachineId: "machine-" + name, AgentStartId: "before-" + name, CurrentGenerationId: "gen-1"},
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
	if err := runKubernetesUpgrade(context.Background(), kubernetesUpgradeOptions{configPath: configPath, version: "v1.36.1", timeout: time.Minute, output: "json"}, &stdout, &bytes.Buffer{}); err != nil {
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
	roles := []string{"apply", "control-plane", "worker"}
	for i, name := range []string{"cp-1", "cp-2", "worker-1"} {
		if report.Nodes[i].Name != name || report.Nodes[i].Result != operation.ResultSucceeded || report.Nodes[i].Phase != "healthy" {
			t.Fatalf("node report %d = %#v", i, report.Nodes[i])
		}
		requests := clients[name].submitRequests
		if len(requests) == 0 || requests[len(requests)-1].DryRun {
			t.Fatalf("%s requests = %#v", name, requests)
		}
		body := requests[len(requests)-1].KubernetesSysextUpdate
		if body.UpgradeRole != roles[i] || body.SourcePayloadVersion != "v1.36.0" || body.TargetPayloadVersion != "v1.36.1" {
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
	bundle, err := kubernetesUpgradeBundle("1.36.1", "")
	if err != nil {
		t.Fatalf("kubernetesUpgradeBundle() error = %v", err)
	}
	if !strings.Contains(bundle, "v1.36.1-katl.1@sha256:") {
		t.Fatalf("bundle = %q", bundle)
	}
	if _, err := kubernetesUpgradeBundle("v1.36.2", ""); err == nil || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("unavailable version error = %v", err)
	}
}

func TestKubernetesUpgradePlanDoesNotExecute(t *testing.T) {
	root := t.TempDir()
	inv := filepath.Join(root, "inventory.yaml")
	if err := os.WriteFile(inv, []byte("nodes:\n  - name: cp-1\n    address: 192.0.2.1\n    systemRole: control-plane\n    access:\n      method: agent\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := &fakeKatlcAgentClient{nodeStatus: &agentapi.NodeStatus{MachineId: "machine", CurrentGenerationId: "gen-1"}, generation: &agentapi.Generation{GenerationId: "gen-1", CommitState: "committed", HealthState: "healthy", Sysexts: []*agentapi.ExtensionRef{{Name: "kubernetes", PayloadVersion: "v1.36.0"}}}}
	previous := dialKatlcAgent
	defer func() { dialKatlcAgent = previous }()
	dialKatlcAgent = func(context.Context, string) (katlcAgentConnection, error) {
		return katlcAgentConnection{Client: client, Close: func() error { return nil }}, nil
	}
	var stdout bytes.Buffer
	if err := run(context.Background(), []string{"kubernetes", "upgrade", "v1.36.1", "--inventory", inv, "--plan", "--timeout", "1m", "--output", "json"}, &stdout, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if len(client.submitRequests) != 1 || !client.submitRequests[0].DryRun || !strings.Contains(stdout.String(), `"result": "planned"`) {
		t.Fatalf("requests=%#v output=%s", client.submitRequests, stdout.String())
	}
}

func TestKubernetesUpgradeResolvesClusterConfigWithoutContext(t *testing.T) {
	topology, err := resolveKubernetesUpgradeTopology(kubernetesUpgradeOptions{clusterConfig: writeClusterConfig(t)})
	if err != nil {
		t.Fatal(err)
	}
	if len(topology.Nodes) != 1 || topology.Nodes[0].Name != "cp-1" || topology.Nodes[0].ManagementEndpoint != "10.0.0.11:9443" {
		t.Fatalf("topology = %#v", topology)
	}
}

func TestKubernetesUpgradeCordonRequiresKubeconfig(t *testing.T) {
	err := runKubernetesUpgrade(context.Background(), kubernetesUpgradeOptions{version: "v1.36.1", cordon: true, timeout: time.Minute, output: "json"}, &bytes.Buffer{}, &bytes.Buffer{})
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
		node: workstation.TopologyNode{Name: "worker-1", SystemRole: "worker"}, role: "worker", conn: katlcAgentConnection{Client: client}, machineID: "machine-worker-1", generation: "gen0", source: "v1.36.0", candidate: "gen1",
	}, image, &bytes.Buffer{})
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
	root := t.TempDir()
	configPath := filepath.Join(root, "katlctl.yaml")
	var nodes strings.Builder
	clients := map[string]*fakeKatlcAgentClient{}
	for _, node := range []struct{ name, endpoint, role string }{{"cp-1", "192.0.2.1:9443", "control-plane"}, {"cp-2", "192.0.2.2:9443", "control-plane"}, {"worker-1", "192.0.2.3:9443", "worker"}} {
		_, _ = nodes.WriteString("      - name: " + node.name + "\n        managementEndpoint: " + node.endpoint + "\n        systemRole: " + node.role + "\n")
		client := &fakeKatlcAgentClient{
			nodeStatus:     &agentapi.NodeStatus{MachineId: "machine-" + node.name, AgentStartId: "before-" + node.name, CurrentGenerationId: "gen-1"},
			generation:     &agentapi.Generation{GenerationId: "gen-1", CommitState: "committed", BootState: "good", HealthState: "healthy", Sysexts: []*agentapi.ExtensionRef{{Name: "kubernetes", PayloadVersion: "v1.36.0"}}},
			submitAccepted: &agentapi.OperationAccepted{OperationId: "upgrade-" + node.name},
			operationStatus: &agentapi.OperationStatus{Terminal: true, Result: operation.ResultSucceeded,
				Phase: "healthy"},
		}
		clients[node.endpoint] = client
	}
	clients["192.0.2.2:9443"].operationStatus = &agentapi.OperationStatus{Terminal: true, Result: operation.ResultFailedNeedsRepair, Phase: "failed", FailureReason: "kubeadm failed", RecoveryRequired: true}
	config := "currentContext: lab\ncontexts:\n  - name: lab\n    cluster: home\nclusters:\n  - name: home\n    controlPlaneEndpoint: 192.0.2.10:6443\n    nodes:\n" + nodes.String()
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	oldDial := dialKatlcAgent
	oldEndpointDial := dialKubernetesEndpoint
	dialKatlcAgent = func(_ context.Context, endpoint string) (katlcAgentConnection, error) {
		return katlcAgentConnection{Client: clients[endpoint], Close: func() error { return nil }}, nil
	}
	dialKubernetesEndpoint = func(context.Context, string) error { return nil }
	t.Cleanup(func() { dialKatlcAgent = oldDial; dialKubernetesEndpoint = oldEndpointDial })

	var stdout bytes.Buffer
	err := runKubernetesUpgrade(context.Background(), kubernetesUpgradeOptions{configPath: configPath, version: "v1.36.1", timeout: time.Minute, output: "json"}, &stdout, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "stopped at node cp-2") {
		t.Fatalf("runKubernetesUpgrade() error = %v", err)
	}
	for _, request := range clients["192.0.2.3:9443"].submitRequests {
		if !request.DryRun {
			t.Fatalf("worker executed after failure: %#v", request)
		}
	}
}
