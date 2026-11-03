package main

import (
	"bytes"
	"context"
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
	for _, test := range []struct {
		name                                             string
		trial, pending, unavailable, receipt, superseded bool
	}{
		{name: "committed"},
		{name: "promoted with interrupted receipt", receipt: true},
		{name: "trial", trial: true},
		{name: "promotion pending", pending: true},
		{name: "promotion superseded source", pending: true, superseded: true},
		{name: "API unavailable", unavailable: true},
	} {
		t.Run(test.name, func(t *testing.T) {
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
			if test.trial {
				client.generation.CommitState = "trial"
				client.generation.HealthState = "unknown"
				client.onReboot = func(*agentapi.RebootRequest) {
					client.generation.CommitState = "committed"
					client.generation.HealthState = "healthy"
					client.nodeStatus.AgentStartId = "after"
				}
			}
			if test.receipt {
				client.operations = &agentapi.ListOperationsResponse{Operations: []*agentapi.OperationStatus{{OperationId: "interrupted-receipt", OperationKind: "bootstrap-join-worker", PreviousGenerationId: "0", CandidateGenerationId: "bootstrap-join-external-candidate", Terminal: true, Result: "failed-needs-repair", RecoveryRequired: true, ResumeSupported: true}}}
			}
			if test.pending {
				client.nodeStatus.CurrentGenerationId, client.generation.GenerationId = "0", "0"
				client.nodeStatus.BootTargetGenerationId = "bootstrap-join-external-candidate"
				client.operations = &agentapi.ListOperationsResponse{Operations: []*agentapi.OperationStatus{{OperationId: "failed-promotion", OperationKind: "bootstrap-join-worker", PreviousGenerationId: "0", CandidateGenerationId: "bootstrap-join-external-candidate", Terminal: true, Result: "failed-needs-repair", RecoveryRequired: true, ResumeSupported: true}}}
			}
			if test.superseded {
				client.generation.CommitState = "superseded"
			}
			if test.unavailable {
				client.nodeStatus.Kubernetes.State, client.nodeStatus.Kubernetes.NodeReady = "waiting-for-node", false
			}
			oldDial, oldJoin := dialKatlcAgent, runAgentExternalWorkerJoin
			t.Cleanup(func() { dialKatlcAgent = oldDial; runAgentExternalWorkerJoin = oldJoin })
			dialKatlcAgent = func(_ context.Context, endpoint string) (katlcAgentConnection, error) {
				if endpoint != "10.0.0.21:9443" {
					t.Fatalf("contacted coordinator: %s", endpoint)
				}
				return katlcAgentConnection{Client: client, Close: func() error { return nil }}, nil
			}
			resumed := false
			runAgentExternalWorkerJoin = func(_ context.Context, _ cluster.Request, _ string, bootstrap []byte, _ cluster.AgentBootstrapDependencies) (cluster.Result, error) {
				if (!test.pending && !test.receipt) || len(bootstrap) != 0 {
					t.Fatal("re-enrolled an existing worker or required revoked credentials")
				}
				resumed = true
				client.generation.CommitState = "committed"
				client.nodeStatus.CurrentGenerationId, client.generation.GenerationId = "bootstrap-join-external-candidate", "bootstrap-join-external-candidate"
				client.operations.Operations[0].Result, client.operations.Operations[0].RecoveryRequired = "succeeded", false
				return cluster.Result{}, nil
			}
			var stdout, stderr bytes.Buffer
			err = run(context.Background(), []string{"node", "join", target.Name, "--config", path}, &stdout, &stderr)
			if test.unavailable {
				if err == nil || !strings.Contains(err.Error(), "already configured but is not healthy") {
					t.Fatalf("configured worker requested enrollment instead of reporting readiness: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(stdout.String(), "joined") {
				t.Fatalf("stdout=%s", stdout.String())
			}
			if resumed != (test.pending || test.receipt) {
				t.Fatalf("unfinished promotion resumed=%t", resumed)
			}
		})
	}
}
