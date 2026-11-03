package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/bootstrap/cluster"
	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/katlctl/workstation"
)

func TestExternalWorkerJoined(t *testing.T) {
	for _, trial := range []bool{false, true} {
		t.Run(fmt.Sprintf("trial=%t", trial), func(t *testing.T) {
			path := writeClusterConfig(t)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			data = bytes.ReplaceAll(data, []byte("cp-1"), []byte("worker-1"))
			data = bytes.Replace(data, []byte("  controlPlaneEndpoint:\n    host: api.katl.test\n    port: 6443\n"), nil, 1)
			data = bytes.Replace(data, []byte("      controlPlane: true\n"), nil, 1)
			data = bytes.Replace(data, []byte("    version: v1.36.1"), []byte("    version: v1.36.1\n    external:\n      endpoint: https://api.external.test:6443"), 1)
			data = bytes.Replace(data, []byte("  defaults:\n"), []byte("  defaults:\n    kubernetes:\n      kubelet:\n        configFile: kubelet.yaml\n"), 1)
			if err := os.WriteFile(filepath.Join(filepath.Dir(path), "kubelet.yaml"), []byte("apiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\nclusterDNS: [10.96.0.10]\nclusterDomain: cluster.local\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			target := inventory.Node{Name: "worker-1", Address: "10.0.0.21", SystemRole: inventory.RoleWorker, EnrollmentID: "enrollment-worker-1", MachineID: "machine-worker-1"}
			contextPath := filepath.Join(t.TempDir(), "katlctl.yaml")
			if err := workstation.Save(contextPath, workstation.Config{CurrentContext: "external", Contexts: []workstation.Context{{Name: "external", Cluster: "lab"}}, Clusters: []workstation.Cluster{{Name: "lab", ExternalControlPlane: true, ControlPlaneEndpoint: "api.external.test:6443", Nodes: []workstation.Node{{Name: target.Name, ManagementEndpoint: target.Address + ":9443", SystemRole: inventory.RoleWorker, EnrollmentID: "enrollment-worker-1", MachineID: "machine-worker-1"}}}}}); err != nil {
				t.Fatal(err)
			}
			t.Setenv("KATLCTL_CONFIG", contextPath)
			client := &fakeKatlcAgentClient{nodeStatus: &agentapi.NodeStatus{MachineId: "machine-worker-1", EnrollmentId: "enrollment-worker-1", InventoryNodeName: "worker-1", CurrentGenerationId: "bootstrap-join-external-candidate", AgentStartId: "before", Kubernetes: &agentapi.KubernetesStatus{State: "ready", Role: "worker", NodeName: "worker-1", KubeletActive: true, NodeReady: true, ExternalEndpoint: "api.external.test:6443"}}, generation: &agentapi.Generation{GenerationId: "bootstrap-join-external-candidate", CommitState: "committed", HealthState: "healthy", BootState: "good"}}
			if trial {
				client.generation.CommitState = "trial"
				client.generation.HealthState = "unknown"
				client.onReboot = func(*agentapi.RebootRequest) {
					client.generation.CommitState = "committed"
					client.generation.HealthState = "healthy"
					client.nodeStatus.AgentStartId = "after"
				}
			}
			oldDial, oldJoin := dialKatlcAgent, runAgentExternalWorkerJoin
			t.Cleanup(func() { dialKatlcAgent = oldDial; runAgentExternalWorkerJoin = oldJoin })
			dialKatlcAgent = func(_ context.Context, endpoint string) (katlcAgentConnection, error) {
				if endpoint != "10.0.0.21:9443" {
					t.Fatalf("contacted coordinator: %s", endpoint)
				}
				return katlcAgentConnection{Client: client, Close: func() error { return nil }}, nil
			}
			runAgentExternalWorkerJoin = func(context.Context, cluster.Request, string, []byte, cluster.AgentBootstrapDependencies) (cluster.Result, error) {
				t.Fatal("re-enrolled an existing worker")
				return cluster.Result{}, nil
			}
			var stdout, stderr bytes.Buffer
			err = run(context.Background(), []string{"node", "join", target.Name, "--config", path}, &stdout, &stderr)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(stdout.String(), "joined") {
				t.Fatalf("stdout=%s", stdout.String())
			}
		})
	}
}
