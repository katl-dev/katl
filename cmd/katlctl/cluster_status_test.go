package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/installer/generation"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
)

func TestClusterStatusKeepsHealthyAndUnreachableNodes(t *testing.T) {
	contextPath := writeKatlctlConfig(t, `currentContext: lab
contexts:
- name: lab
  cluster: lab
clusters:
- name: lab
  nodes:
  - name: cp-1
    managementEndpoint: 192.0.2.11:9443
    systemRole: control-plane
  - name: worker-1
    managementEndpoint: 192.0.2.21:9443
    systemRole: worker
`)
	client := &fakeKatlcAgentClient{
		nodeStatus: &agentapi.NodeStatus{
			CurrentGenerationId: "generation-1",
			Kubernetes:          &agentapi.KubernetesStatus{State: "ready", Role: "control-plane", NodeName: "cp-1", KubeletActive: true, NodeReady: true, ControlPlaneComponentsReady: true},
		},
		generation: &agentapi.Generation{GenerationId: "generation-1", RuntimeVersion: "2026.7.0-alpha.15", CommitState: generation.CommitStateCommitted, BootState: generation.BootStateGood, HealthState: generation.HealthStateHealthy},
	}
	oldDial := dialKatlcAgent
	dialKatlcAgent = func(_ context.Context, endpoint string) (katlcAgentConnection, error) {
		if endpoint == "192.0.2.21:9443" {
			return katlcAgentConnection{}, fmt.Errorf("connection refused")
		}
		return katlcAgentConnection{Client: client, Close: func() error { return nil }}, nil
	}
	t.Cleanup(func() { dialKatlcAgent = oldDial })

	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"cluster", "status", "--context-file", contextPath, "--output", "json"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	var report clusterStatusReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Nodes) != 2 || !report.Nodes[0].Reachable || report.Nodes[0].Health != "OK" || report.Nodes[1].Reachable || !strings.Contains(report.Nodes[1].Error, "connection refused") {
		t.Fatalf("report = %#v", report)
	}
	if report.Nodes[0].Kubernetes == nil || report.Nodes[0].Kubernetes.State != "ready" || !report.Nodes[0].Kubernetes.NodeReady {
		t.Fatalf("Kubernetes report = %#v", report.Nodes[0].Kubernetes)
	}
}

func TestClusterStatusResolvesClusterConfigWithoutContext(t *testing.T) {
	topology, err := resolveClusterTopology(clusterStatusOptions{clusterConfig: writeClusterConfig(t)})
	if err != nil {
		t.Fatal(err)
	}
	if len(topology.Nodes) != 1 || topology.Nodes[0].Name != "cp-1" || topology.Nodes[0].ManagementEndpoint != "10.0.0.11:9443" {
		t.Fatalf("topology = %#v", topology)
	}
}

func TestStableEndpointReachabilityUsesOperatorNetworkPath(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	if failure := stableEndpointFailure(context.Background(), listener.Addr().String()); failure != "" {
		t.Fatalf("reachable endpoint failure = %q", failure)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if failure := stableEndpointFailure(context.Background(), address); failure == "" {
		t.Fatal("closed stable endpoint reported reachable")
	}
}
