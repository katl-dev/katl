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

	"github.com/katl-dev/katl/internal/installer/operation"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
)

func TestKubernetesUpgradePlansAndRunsControlPlanesBeforeWorkers(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "katlctl.yaml")
	var nodes strings.Builder
	for _, node := range []struct{ name, endpoint, role string }{{"worker-1", "192.0.2.4:9443", "worker"}, {"cp-2", "192.0.2.2:9443", "control-plane"}, {"cp-1", "192.0.2.1:9443", "control-plane"}} {
		token := filepath.Join(root, node.name+".token")
		if err := os.WriteFile(token, []byte("token-"+node.name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, _ = nodes.WriteString("      - name: " + node.name + "\n        managementEndpoint: " + node.endpoint + "\n        systemRole: " + node.role + "\n        credentialRef: file:" + token + "\n")
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
			nodeStatus:      &agentapi.NodeStatus{MachineId: "machine-" + name, CurrentGenerationId: "gen-1"},
			generation:      &agentapi.Generation{GenerationId: "gen-1", CommitState: "committed", HealthState: "healthy", Sysexts: []*agentapi.ExtensionRef{{Name: "kubernetes", PayloadVersion: "v1.36.0", Sha256: strings.Repeat("a", 64)}}},
			submitAccepted:  &agentapi.OperationAccepted{OperationId: "internal-" + name},
			operationStatus: &agentapi.OperationStatus{Terminal: true, Result: operation.ResultSucceeded, Phase: "healthy", NextAction: "reboot for boot health"},
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
	defer func() { dialKatlcAgent = previousDial; kubernetesUpgradeNow = previousNow }()
	dialKatlcAgent = func(_ context.Context, endpoint, token string) (katlcAgentConnection, error) {
		return katlcAgentConnection{Client: byEndpoint[endpoint], Close: func() error { return nil }}, nil
	}
	kubernetesUpgradeNow = func() time.Time { return time.Unix(42, 0).UTC() }
	run := func() kubernetesUpgradeReport {
		t.Helper()
		var stdout bytes.Buffer
		if err := runKubernetesUpgrade(context.Background(), kubernetesUpgradeOptions{configPath: configPath, bundle: "ghcr.io/katl-dev/kubernetes:v1.36.1-katl.1", timeout: time.Minute, output: "json"}, &stdout, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(stdout.String(), "internal-") || strings.Contains(strings.ToLower(stdout.String()), "digest") {
			t.Fatalf("output exposed internal operation data: %s", stdout.String())
		}
		var report kubernetesUpgradeReport
		if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		return report
	}

	roles := []string{"apply", "control-plane", "worker"}
	for i, name := range []string{"cp-1", "cp-2", "worker-1"} {
		report := run()
		if report.SourceVersion != "v1.36.0" || report.TargetVersion != "v1.36.1" || len(report.Nodes) != 1 || report.Nodes[0].Name != name || report.NextAction == "" {
			t.Fatalf("run %d report = %#v", i+1, report)
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
		clients[name].generation.Sysexts[0].PayloadVersion = "v1.36.1"
	}
	if want := []string{"cp-1", "cp-2", "worker-1"}; !reflect.DeepEqual(executionOrder, want) {
		t.Fatalf("execution order = %v, want %v", executionOrder, want)
	}
	report := run()
	if len(report.Nodes) != 0 || !strings.Contains(report.NextAction, "already") {
		t.Fatalf("completed report = %#v", report)
	}
}

func TestKubernetesUpgradePlanDoesNotExecute(t *testing.T) {
	root := t.TempDir()
	token := filepath.Join(root, "token")
	if err := os.WriteFile(token, []byte("token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	inv := filepath.Join(root, "inventory.yaml")
	if err := os.WriteFile(inv, []byte("nodes:\n  - name: cp-1\n    address: 192.0.2.1\n    systemRole: control-plane\n    access:\n      method: agent\n      credentialRef: file:"+token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := &fakeKatlcAgentClient{nodeStatus: &agentapi.NodeStatus{MachineId: "machine", CurrentGenerationId: "gen-1"}, generation: &agentapi.Generation{GenerationId: "gen-1", CommitState: "committed", HealthState: "healthy", Sysexts: []*agentapi.ExtensionRef{{Name: "kubernetes", PayloadVersion: "v1.36.0"}}}}
	previous := dialKatlcAgent
	defer func() { dialKatlcAgent = previous }()
	dialKatlcAgent = func(context.Context, string, string) (katlcAgentConnection, error) {
		return katlcAgentConnection{Client: client, Close: func() error { return nil }}, nil
	}
	var stdout bytes.Buffer
	if err := runKubernetesUpgrade(context.Background(), kubernetesUpgradeOptions{inventoryPath: inv, bundle: "ghcr.io/katl-dev/kubernetes:v1.36.1-katl.1", plan: true, timeout: time.Minute, output: "json"}, &stdout, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if len(client.submitRequests) != 1 || !client.submitRequests[0].DryRun || !strings.Contains(stdout.String(), `"result": "planned"`) {
		t.Fatalf("requests=%#v output=%s", client.submitRequests, stdout.String())
	}
}
