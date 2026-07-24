package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/installer/generation"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
)

func TestNodeUpgradeRecoveryRequiresKubernetesAndManagedRouting(t *testing.T) {
	readyKubernetes := &agentapi.KubernetesStatus{
		State:                       "ready",
		Role:                        "control-plane",
		NodeName:                    "cp-1",
		KubeletActive:               true,
		NodeReady:                   true,
		ControlPlaneComponentsReady: true,
	}
	tests := []struct {
		name   string
		status *agentapi.NodeStatus
		state  string
		reason string
		ready  bool
	}{
		{
			name: "status unsupported", status: &agentapi.NodeStatus{},
			state: "unknown", reason: "Kubernetes status is not reported by the node agent",
		},
		{
			name: "not bootstrapped", status: &agentapi.NodeStatus{Kubernetes: &agentapi.KubernetesStatus{State: "not-configured"}},
			state: "not-configured", ready: true,
		},
		{
			name:   "node not ready",
			status: &agentapi.NodeStatus{Kubernetes: &agentapi.KubernetesStatus{State: "waiting-for-node", Role: "worker", KubeletActive: true, FailureReason: "Kubernetes node worker-1 is not Ready"}},
			state:  "waiting-for-node", reason: "Kubernetes node worker-1 is not Ready",
		},
		{
			name: "managed endpoint not ready",
			status: &agentapi.NodeStatus{
				Kubernetes:           readyKubernetes,
				ControlPlaneEndpoint: &agentapi.ControlPlaneEndpointStatus{State: "waiting-for-apiserver"},
			},
			state: "waiting-for-managed-endpoint", reason: "managed API endpoint is waiting-for-apiserver",
		},
		{
			name: "passive optional route exchange",
			status: &agentapi.NodeStatus{
				Kubernetes: readyKubernetes,
				ControlPlaneEndpoint: &agentapi.ControlPlaneEndpointStatus{
					State: "advertised", LocalApiReady: true, RouteOriginated: true,
					RouteExchange: []*agentapi.ControlPlaneEndpointRouteExchangeStatus{{Name: "cilium", State: "passive"}},
				},
			},
			state: "ready", ready: true,
		},
		{
			name: "ready",
			status: &agentapi.NodeStatus{
				Kubernetes: readyKubernetes,
				ControlPlaneEndpoint: &agentapi.ControlPlaneEndpointStatus{
					State: "advertised", LocalApiReady: true, RouteOriginated: true,
					RouteExchange: []*agentapi.ControlPlaneEndpointRouteExchangeStatus{{Name: "cilium", State: "established"}},
				},
			},
			state: "ready", ready: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := nodeUpgradeRecovery(test.status)
			if got.State != test.state || got.Reason != test.reason || got.Ready != test.ready {
				t.Fatalf("recovery = %#v", got)
			}
		})
	}
}

func TestWaitNodeBootHealthWaitsForKubernetesRecovery(t *testing.T) {
	fake := &fakeKatlcAgentClient{
		nodeStatus: &agentapi.NodeStatus{
			AgentStartId:        "after",
			CurrentGenerationId: "katlos-next",
			Kubernetes: &agentapi.KubernetesStatus{
				State:         "waiting-for-node",
				Role:          "control-plane",
				NodeName:      "cp-1",
				KubeletActive: true,
				FailureReason: "Kubernetes node cp-1 is not Ready",
			},
		},
		generation: &agentapi.Generation{
			GenerationId: "katlos-next",
			CommitState:  generation.CommitStateCommitted,
			BootState:    generation.BootStateGood,
			HealthState:  generation.HealthStateHealthy,
		},
	}
	polls := 0
	fake.onGetNodeStatus = func() {
		polls++
		if polls < 3 {
			return
		}
		fake.nodeStatus.Kubernetes = &agentapi.KubernetesStatus{
			State:                       "ready",
			Role:                        "control-plane",
			NodeName:                    "cp-1",
			KubeletActive:               true,
			NodeReady:                   true,
			ControlPlaneComponentsReady: true,
		}
		fake.nodeStatus.ControlPlaneEndpoint = &agentapi.ControlPlaneEndpointStatus{
			State: "advertised", LocalApiReady: true, RouteOriginated: true,
			RouteExchange: []*agentapi.ControlPlaneEndpointRouteExchangeStatus{{Name: "cilium", State: "established"}},
		}
	}
	installKatlcDial(t, func(endpoint string) {
		if endpoint != "10.0.0.11:9443" {
			t.Fatalf("endpoint = %q", endpoint)
		}
	}, fake)
	previousPollInterval := upgradeRebootPollInterval
	upgradeRebootPollInterval = time.Millisecond
	t.Cleanup(func() { upgradeRebootPollInterval = previousPollInterval })

	var progress bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, verified, err := waitNodeBootHealth(ctx, "cp-1", "10.0.0.11:9443", "before", "katlos-next", &progress)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if polls < 3 || !nodeUpgradeRecovery(verified.Status).Ready {
		t.Fatalf("polls = %d, status = %#v", polls, verified.Status)
	}
	if output := progress.String(); !strings.Contains(output, "waiting-for-kubernetes state=waiting-for-node") || !strings.Contains(output, "Kubernetes node cp-1 is not Ready") {
		t.Fatalf("progress = %q", output)
	}
}

func TestCurrentHostUpgradeWaitsForKubernetesRecovery(t *testing.T) {
	const generationID = "katlos-2026.7.0-alpha.9"
	fake := &fakeKatlcAgentClient{
		nodeStatus: &agentapi.NodeStatus{
			CurrentGenerationId: generationID,
			Kubernetes: &agentapi.KubernetesStatus{
				State:         "waiting-for-node",
				Role:          "worker",
				NodeName:      "cp-1",
				KubeletActive: true,
				FailureReason: "Kubernetes node cp-1 is not Ready",
			},
		},
		generation: &agentapi.Generation{
			GenerationId:        generationID,
			RuntimeArchitecture: "x86_64",
			CommitState:         generation.CommitStateCommitted,
			BootState:           generation.BootStateGood,
			HealthState:         generation.HealthStateHealthy,
		},
	}
	polls := 0
	fake.onGetNodeStatus = func() {
		polls++
		if polls >= 3 {
			fake.nodeStatus.Kubernetes.State = "ready"
			fake.nodeStatus.Kubernetes.NodeReady = true
			fake.nodeStatus.Kubernetes.FailureReason = ""
		}
	}
	installKatlcDial(t, nil, fake)
	previousPollInterval := upgradeRebootPollInterval
	upgradeRebootPollInterval = time.Millisecond
	t.Cleanup(func() { upgradeRebootPollInterval = previousPollInterval })

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{"node", "upgrade", "2026.7.0-alpha.9", "cp-1", "--config", writeClusterConfig(t), "--timeout", "1s"}, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if fake.submitRequest != nil || len(fake.rebootRequests) != 0 {
		t.Fatalf("submitted = %#v, reboots = %d", fake.submitRequest, len(fake.rebootRequests))
	}
	if polls < 3 || !strings.Contains(stdout.String(), "Kubernetes ready") || !strings.Contains(stderr.String(), "waiting-for-kubernetes") {
		t.Fatalf("polls = %d, stdout = %q, stderr = %q", polls, stdout.String(), stderr.String())
	}
}

func TestWaitNodeKubernetesRecoveryTimeoutIsActionable(t *testing.T) {
	fake := &fakeKatlcAgentClient{nodeStatus: &agentapi.NodeStatus{Kubernetes: &agentapi.KubernetesStatus{
		State: "waiting-for-control-plane", Role: "control-plane", KubeletActive: true, NodeReady: true,
		FailureReason: "local etcd component is not running",
	}}}
	installKatlcDial(t, nil, fake)
	previousPollInterval := upgradeRebootPollInterval
	upgradeRebootPollInterval = time.Millisecond
	t.Cleanup(func() { upgradeRebootPollInterval = previousPollInterval })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, _, err := waitNodeKubernetesRecovery(ctx, "cp-1", "10.0.0.11:9443", &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "local etcd component is not running") || !strings.Contains(err.Error(), "do not schedule workloads") || !strings.Contains(err.Error(), "katlctl node status") {
		t.Fatalf("error = %v", err)
	}
}
