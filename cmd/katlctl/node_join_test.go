package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/bootstrap/cluster"
	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/katlctl/workstation"
)

func TestNodeJoin(t *testing.T) {
	for _, test := range []struct {
		name   string
		role   inventory.SystemRole
		resume bool
	}{
		{name: "control plane", role: inventory.RoleControlPlane},
		{name: "worker", role: inventory.RoleWorker},
		{name: "resume trial boot", role: inventory.RoleControlPlane, resume: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := writeClusterConfig(t)
			writeTestEnrollmentContext(t, "lab",
				workstation.Node{Name: "cp-1", ManagementEndpoint: "10.0.0.11:9443", SystemRole: inventory.RoleControlPlane, EnrollmentID: "enrollment-cp-1", MachineID: "machine-cp-1"},
				workstation.Node{Name: "node-2", ManagementEndpoint: "10.0.0.12:9443", SystemRole: test.role, EnrollmentID: "enrollment-node-2", MachineID: "machine-node-2"},
				workstation.Node{Name: "offline", ManagementEndpoint: "192.0.2.99:9443", SystemRole: inventory.RoleControlPlane, EnrollmentID: "enrollment-offline", MachineID: "machine-offline"},
			)
			source := configBundleSource() + fmt.Sprintf(`    - name: node-2
      controlPlane: %t
      management:
        address: 10.0.0.12
      install:
        systemDisk:
          byID: /dev/disk/by-id/ata-node-2-root
    - name: offline
      controlPlane: true
      management:
        address: 192.0.2.99
      install:
        systemDisk:
          byID: /dev/disk/by-id/ata-offline-root
`, test.role == inventory.RoleControlPlane)
			if err := os.WriteFile(path, []byte(source), 0600); err != nil {
				t.Fatal(err)
			}
			coordinator := &fakeKatlcAgentClient{
				nodeStatus: &agentapi.NodeStatus{MachineId: "machine-cp-1", CurrentGenerationId: "current", Kubernetes: &agentapi.KubernetesStatus{State: "ready", Role: "control-plane", NodeName: "cp-1", KubeletActive: true, ControlPlaneComponentsReady: true}},
				generation: &agentapi.Generation{GenerationId: "current", CommitState: "committed", HealthState: "healthy", BootState: "good"},
			}
			target := &fakeKatlcAgentClient{
				nodeStatus: &agentapi.NodeStatus{MachineId: "machine-node-2", CurrentGenerationId: "0", AgentStartId: "before", Kubernetes: &agentapi.KubernetesStatus{State: "not-configured"}},
				generation: &agentapi.Generation{GenerationId: "0", CommitState: "committed", HealthState: "healthy", BootState: "good"},
			}
			joined := func() {
				target.nodeStatus.CurrentGenerationId = "bootstrap-join-example-candidate"
				target.nodeStatus.Kubernetes = &agentapi.KubernetesStatus{State: "ready", Role: string(test.role), NodeName: "node-2", KubeletActive: true, ControlPlaneComponentsReady: true, NodeReady: true}
				target.generation = &agentapi.Generation{GenerationId: target.nodeStatus.CurrentGenerationId, CommitState: "committed", HealthState: "healthy", BootState: "good"}
			}
			if test.resume {
				joined()
				target.generation.CommitState = "trial"
				target.generation.HealthState = "unknown"
				target.nodeStatus.Kubernetes.KubeletActive = false
				target.onReboot = func(*agentapi.RebootRequest) { joined(); target.nodeStatus.AgentStartId = "after" }
			}
			previousDial, previousJoin := dialKatlcAgent, runAgentNodeJoin
			t.Cleanup(func() { dialKatlcAgent = previousDial; runAgentNodeJoin = previousJoin })
			repeating := false
			dialKatlcAgent = func(_ context.Context, endpoint string) (katlcAgentConnection, error) {
				client := target
				switch endpoint {
				case "10.0.0.12:9443":
				case "10.0.0.11:9443":
					if repeating || test.resume {
						t.Fatal("completed/resuming join contacted a coordinator")
					}
					client = coordinator
				default:
					t.Fatalf("contacted unrelated endpoint %s", endpoint)
				}
				return katlcAgentConnection{Client: client, Close: func() error { return nil }}, nil
			}
			joins := 0
			runAgentNodeJoin = func(_ context.Context, request cluster.Request, node string, deps cluster.AgentBootstrapDependencies) (cluster.Result, error) {
				joins++
				if test.resume || repeating {
					t.Fatal("repeated membership transition")
				}
				if node != "node-2" || request.InitNode != "cp-1" || deps.Actor != "katlctl node join" {
					t.Fatalf("join node=%s request=%+v actor=%s", node, request, deps.Actor)
				}
				for _, node := range request.Inventory.Nodes {
					if node.Name != "cp-1" && node.Name != "node-2" {
						t.Fatalf("unrelated join participant %s", node.Name)
					}
				}
				joined()
				return cluster.Result{}, nil
			}

			var stdout, stderr bytes.Buffer
			args := []string{"node", "join", "node-2", "--config", path, "--output", "json"}
			if err := run(context.Background(), args, &stdout, &stderr); err != nil {
				t.Fatalf("join: %v\n%s", err, stderr.String())
			}
			if !strings.Contains(stdout.String(), `"result":"joined"`) {
				t.Fatalf("report = %s", stdout.String())
			}
			if test.resume {
				if joins != 0 || len(target.rebootRequests) != 1 {
					t.Fatalf("resume joins=%d reboots=%d", joins, len(target.rebootRequests))
				}
			} else if joins != 1 || len(target.rebootRequests) != 0 {
				t.Fatalf("join joins=%d reboots=%d", joins, len(target.rebootRequests))
			}
			if coordinator.validateRequest != nil || target.validateRequest != nil || len(coordinator.submitRequests) != 0 || len(target.submitRequests) != 0 {
				t.Fatal("join applied node configuration")
			}

			repeating = true
			stdout.Reset()
			if err := run(context.Background(), args, &stdout, &stderr); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(stdout.String(), `"result":"unchanged"`) {
				t.Fatalf("repeat report = %s", stdout.String())
			}
		})
	}
}

func TestNodeJoinRefusals(t *testing.T) {
	for _, test := range []struct{ name, state, health, next, want string }{
		{name: "no existing cluster", state: "not-configured", health: "healthy", want: "no ready control plane"},
		{name: "staged config", state: "not-configured", health: "healthy", next: "next", want: "reboot it before joining"},
		{name: "unhealthy generation", state: "not-configured", health: "unhealthy", want: "healthy committed generation"},
		{name: "configured but unhealthy", state: "waiting-for-kubelet", health: "healthy", want: "already configured but is not healthy"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := writeClusterConfig(t)
			client := &fakeKatlcAgentClient{
				nodeStatus: &agentapi.NodeStatus{MachineId: "machine-cp-1", CurrentGenerationId: "current", BootTargetGenerationId: test.next, Kubernetes: &agentapi.KubernetesStatus{State: test.state}},
				generation: &agentapi.Generation{GenerationId: "current", CommitState: "committed", HealthState: test.health},
			}
			oldDial, oldJoin := dialKatlcAgent, runAgentNodeJoin
			t.Cleanup(func() { dialKatlcAgent = oldDial; runAgentNodeJoin = oldJoin })
			dialKatlcAgent = func(context.Context, string) (katlcAgentConnection, error) {
				return katlcAgentConnection{Client: client, Close: func() error { return nil }}, nil
			}
			runAgentNodeJoin = func(context.Context, cluster.Request, string, cluster.AgentBootstrapDependencies) (cluster.Result, error) {
				t.Fatal("refused request attempted membership change")
				return cluster.Result{}, nil
			}
			var stdout, stderr bytes.Buffer
			err := run(context.Background(), []string{"node", "join", "cp-1", "--config", path}, &stdout, &stderr)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want %q", err, test.want)
			}
			if len(client.submitRequests) != 0 || len(client.rebootRequests) != 0 {
				t.Fatal("refused request mutated node")
			}
		})
	}
}
