package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/generation"
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
		name        string
		status      *agentapi.NodeStatus
		requirement nodeRecoveryRequirement
		state       string
		reason      string
		ready       bool
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
			name:        "node not ready",
			status:      &agentapi.NodeStatus{Kubernetes: &agentapi.KubernetesStatus{State: "waiting-for-node", Role: "worker", KubeletActive: true, FailureReason: "Kubernetes node worker-1 is not Ready"}},
			requirement: nodeRecoveryRequirement{KubernetesConfigured: true, NodeReady: true},
			state:       "waiting-for-node", reason: "Kubernetes node worker-1 is not Ready",
		},
		{
			name: "pre CNI state is preserved",
			status: &agentapi.NodeStatus{Kubernetes: &agentapi.KubernetesStatus{
				State: "waiting-for-node", Role: "control-plane", KubeletActive: true, ControlPlaneComponentsReady: true,
				FailureReason: "Kubernetes node cp-1 is not Ready",
			}},
			requirement: nodeRecoveryRequirement{KubernetesConfigured: true},
			state:       "waiting-for-node", reason: "Kubernetes node cp-1 is not Ready", ready: true,
		},
		{
			name:        "configured state is not lost",
			status:      &agentapi.NodeStatus{Kubernetes: &agentapi.KubernetesStatus{State: "not-configured"}},
			requirement: nodeRecoveryRequirement{KubernetesConfigured: true},
			state:       "not-configured", reason: "Kubernetes was configured before the reboot but is no longer configured",
		},
		{
			name: "managed endpoint not ready",
			status: &agentapi.NodeStatus{
				Kubernetes:           readyKubernetes,
				ControlPlaneEndpoint: &agentapi.ControlPlaneEndpointStatus{State: "waiting-for-apiserver"},
			},
			requirement: nodeRecoveryRequirement{ManagedEndpointReady: true},
			state:       "waiting-for-managed-endpoint", reason: "managed API endpoint is waiting-for-apiserver",
		},
		{
			// Node Ready lags kubelet losing the API, so the proxy decides.
			name: "API proxy has no eligible backend",
			status: &agentapi.NodeStatus{
				Kubernetes: readyKubernetes,
				ApiProxy:   &agentapi.APIProxyStatus{State: "no-eligible-backends", FailureReason: "no eligible Kubernetes API backend"},
			},
			requirement: nodeRecoveryRequirement{KubernetesConfigured: true, NodeReady: true, APIProxyReady: true, LocalAPIEligible: true},
			state:       "waiting-for-api-proxy", reason: "Kubernetes API proxy: no eligible Kubernetes API backend",
		},
		{
			name: "API proxy lost its local backend",
			status: &agentapi.NodeStatus{
				Kubernetes: readyKubernetes,
				ApiProxy: &agentapi.APIProxyStatus{State: "ready", Backends: []*agentapi.APIProxyBackendStatus{
					{Name: "cp-1", Address: "192.0.2.10:6443", Local: true, Reason: "dial timeout"},
					{Name: "cp-2", Address: "10.0.1.11:6443", Eligible: true},
				}},
			},
			requirement: nodeRecoveryRequirement{APIProxyReady: true, LocalAPIEligible: true},
			state:       "waiting-for-api-proxy", reason: "Kubernetes API proxy does not reach the local API server at 192.0.2.10:6443: dial timeout",
		},
		{
			name: "API proxy was not serving before the reboot",
			status: &agentapi.NodeStatus{
				Kubernetes: readyKubernetes,
				ApiProxy:   &agentapi.APIProxyStatus{State: "no-eligible-backends"},
			},
			state: "ready", ready: true,
		},
		{
			name: "ready",
			status: &agentapi.NodeStatus{
				Kubernetes: readyKubernetes,
				ControlPlaneEndpoint: &agentapi.ControlPlaneEndpointStatus{
					State: "active", LocalApiReady: true, LocalVipOwned: true,
				},
			},
			state: "ready", ready: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := nodeUpgradeRecovery(test.status, test.requirement)
			if got.State != test.state || got.Reason != test.reason || got.Ready != test.ready {
				t.Fatalf("recovery = %#v", got)
			}
		})
	}
}

func TestNodeRecoveryRequirementRecordsServingAPIProxy(t *testing.T) {
	serving := &agentapi.NodeStatus{ApiProxy: &agentapi.APIProxyStatus{State: "ready", LocalApiEligible: true}}
	if got := nodeRecoveryRequirementFor(serving); !got.APIProxyReady || !got.LocalAPIEligible {
		t.Fatalf("requirement for serving proxy = %#v", got)
	}
	failing := &agentapi.NodeStatus{ApiProxy: &agentapi.APIProxyStatus{State: "no-eligible-backends"}}
	if got := nodeRecoveryRequirementFor(failing); got.APIProxyReady || got.LocalAPIEligible {
		t.Fatalf("requirement for failing proxy = %#v", got)
	}
}

func TestWaitNodeBootHealthWaitsForKubernetesRecovery(t *testing.T) {
	fake := &fakeKatlcAgentClient{
		nodeStatus: &agentapi.NodeStatus{
			AgentStartId:         "after",
			CurrentGenerationId:  "katlos-next",
			SelectedGenerationId: "katlos-next",
			BootHealthState:      "healthy",
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
			State: "active", LocalApiReady: true, LocalVipOwned: true,
		}
	}
	installKatlcDial(t, func(endpoint string) {
		if endpoint != "10.0.0.11:9443" {
			t.Fatalf("endpoint = %q", endpoint)
		}
	}, fake)
	fastBootHealthPolls(t)

	var progress bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	requirement := nodeRecoveryRequirement{KubernetesConfigured: true, NodeReady: true}
	conn, verified, err := waitNodeBootHealth(ctx, bootWaitTarget{nodeName: "cp-1", endpoints: []string{"10.0.0.11:9443"}}, "before", "katlos-next", requirement, "upgrade node=cp-1", &progress)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if polls < 3 || !nodeUpgradeRecovery(verified.Status, requirement).Ready {
		t.Fatalf("polls = %d, status = %#v", polls, verified.Status)
	}
	if output := progress.String(); !strings.Contains(output, "waiting-for-kubernetes state=waiting-for-node") || !strings.Contains(output, "Kubernetes node cp-1 is not Ready") {
		t.Fatalf("progress = %q", output)
	}
}

// bootObservation is what the node reports at one poll after a reboot request.
type bootObservation struct {
	agentStart    string
	running       string
	health        string
	diagnostic    string
	nextBoot      string
	candidate     string
	candidateBoot string
}

func (o bootObservation) status() *agentapi.NodeStatus {
	return &agentapi.NodeStatus{
		AgentStartId:           o.agentStart,
		CurrentGenerationId:    o.running,
		SelectedGenerationId:   o.running,
		BootTargetGenerationId: firstNonEmpty(o.nextBoot, o.running),
		BootHealthState:        o.health,
		BootHealthDiagnostic:   o.diagnostic,
		Kubernetes:             &agentapi.KubernetesStatus{State: "not-configured"},
	}
}

func TestWaitNodeBootHealthFollowsCurrentBoot(t *testing.T) {
	const skipped = "katl-boot-health.service did not run in this boot because a unit it requires did not start; inspect the boot journal"
	healthy, unhealthy := generation.HealthStateHealthy, generation.HealthStateUnhealthy
	tests := []struct {
		name string
		// observations are reported in order; the last repeats.
		observations []bootObservation
		// settled is the observation the wait must end on.
		settled         int
		err             []string
		progress        []string
		noRebootAdvice  bool
		nextUnavailable string
	}{
		{
			name: "same generation waits for its own boot",
			observations: []bootObservation{
				{agentStart: "before", running: "gen-a", health: "healthy", candidate: healthy},
				{agentStart: "after", running: "gen-a", health: "pending", candidate: healthy},
				{agentStart: "after", running: "gen-a", health: "healthy", candidate: healthy},
			},
			settled: 2,
		},
		{
			name: "reboot waits for management link recovery",
			observations: []bootObservation{
				{agentStart: "after", running: "gen-a", health: "unhealthy", diagnostic: "systemd-networkd reports required link enp1s0 not online", candidate: healthy},
				{agentStart: "after", running: "gen-a", health: "unknown", candidate: healthy},
				{agentStart: "after", running: "gen-a", health: "healthy", candidate: healthy},
			},
			settled:  2,
			progress: []string{"systemd-networkd", "enp1s0"},
		},
		{
			name: "failed boot of a previously healthy generation",
			observations: []bootObservation{
				{agentStart: "before", running: "gen-a", health: "healthy", candidate: healthy},
				{agentStart: "after", running: "gen-a", health: "pending", candidate: healthy},
				{agentStart: "after", running: "gen-a", health: "failed", diagnostic: skipped, nextBoot: "gen-old", candidate: healthy},
				{agentStart: "after", running: "gen-a", health: "failed", diagnostic: skipped, nextBoot: "gen-old", candidate: healthy},
			},
			settled:        2,
			err:            []string{"node cp-1 failed boot health on generation gen-a: " + skipped, "Generation gen-old is recorded for next boot", "'katlctl node generations list cp-1 --config cluster.yaml'", "'katlctl node logs cp-1 --config cluster.yaml'"},
			noRebootAdvice: true,
		},
		{
			name: "recorded fallback with indeterminate bootability",
			observations: []bootObservation{
				{agentStart: "after", running: "gen-a", health: "failed", diagnostic: skipped, nextBoot: "gen-old", candidate: unhealthy},
			},
			err:             []string{"Generation gen-old is recorded for next boot (bootability indeterminate: input/output error)", "'katlctl node generations list cp-1 --config cluster.yaml'"},
			noRebootAdvice:  true,
			nextUnavailable: "bootability indeterminate: input/output error",
		},
		{
			name: "failed boot without another generation",
			observations: []bootObservation{
				{agentStart: "after", running: "gen-a", health: "failed", diagnostic: skipped, candidate: unhealthy},
			},
			err: []string{"failed boot health on generation gen-a", "'katlctl node generations select GENERATION cp-1 --config cluster.yaml'"},
		},
		{
			name: "rejected trial waits for its fallback boot",
			observations: []bootObservation{
				{agentStart: "trial", running: "gen-a", health: "pending", nextBoot: "gen-old", candidate: unhealthy},
				{agentStart: "fallback", running: "gen-old", health: "pending", candidate: unhealthy},
			},
			settled: 1,
			err:     []string{"rejected generation gen-a during boot health and returned on generation gen-old"},
		},
		{
			name: "fallback lost a management link",
			observations: []bootObservation{
				{agentStart: "fallback", running: "gen-old", health: "unhealthy", diagnostic: "systemd-networkd reports required link enp1s0 not online", candidate: unhealthy},
			},
			err: []string{"rejected generation gen-a during boot health and returned on generation gen-old"},
		},
		{
			name: "fallback with a recorded boot failure on the trial",
			observations: []bootObservation{
				{agentStart: "fallback", running: "gen-old", health: "unhealthy", candidate: generation.HealthStateUnknown, candidateBoot: generation.BootStateFailed},
			},
			err: []string{"rejected generation gen-a during boot health and returned on generation gen-old"},
		},
		{
			name: "failed fallback boot",
			observations: []bootObservation{
				{agentStart: "fallback", running: "gen-old", health: "failed", diagnostic: skipped, candidate: unhealthy},
			},
			err: []string{"booted generation gen-old instead of gen-a, and it failed boot health"},
		},
		{
			name: "another generation booted",
			observations: []bootObservation{
				{agentStart: "after", running: "gen-old", health: "healthy", candidate: healthy},
			},
			err: []string{"booted generation gen-old instead of gen-a", "'katlctl node generations list cp-1 --config cluster.yaml'"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeKatlcAgentClient{}
			observed := -1
			fake.onGetNodeStatus = func() {
				observed = min(observed+1, len(test.observations)-1)
				current := test.observations[observed]
				fake.nodeStatus = current.status()
				fake.generationList = &agentapi.ListGenerationsResponse{
					NextBootGenerationId: current.nextBoot,
					Generations:          []*agentapi.Generation{{GenerationId: current.nextBoot, UnavailableReason: test.nextUnavailable}},
				}
				fake.generation = &agentapi.Generation{GenerationId: "gen-a", CommitState: generation.CommitStateCommitted, BootState: generation.BootStateGood, HealthState: current.candidate}
				if current.candidateBoot != "" {
					fake.generation.BootState = current.candidateBoot
				}
			}
			installKatlcDial(t, nil, fake)
			fastBootHealthPolls(t)

			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			var progress strings.Builder
			_, verified, err := waitNodeBootHealth(ctx, bootWaitTarget{nodeName: "cp-1", endpoints: []string{"10.0.0.11:9443"}, flags: " --config cluster.yaml"}, "before", "gen-a", nodeRecoveryRequirement{}, "reboot node=cp-1", &progress)

			for _, want := range test.progress {
				if !strings.Contains(progress.String(), want) {
					t.Errorf("progress = %q, want %q", progress.String(), want)
				}
			}
			if errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("wait timed out: %v", err)
			}
			if observed != test.settled {
				t.Fatalf("wait ended on observation %d, want %d (err = %v)", observed, test.settled, err)
			}
			if len(test.err) == 0 {
				if err != nil || verified.Status.GetBootHealthState() != "healthy" {
					t.Fatalf("wait = %+v, %v; want healthy boot", verified.Status, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("wait succeeded, want error containing %q", test.err)
			}
			if test.noRebootAdvice && strings.Contains(err.Error(), "'katlctl node reboot") {
				t.Errorf("error recommends rebooting with indeterminate bootability: %v", err)
			}
			for _, want := range test.err {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %v, want %q", err, want)
				}
			}
		})
	}
}

func TestWaitNodeBootHealthTimeoutExplainsPendingBoot(t *testing.T) {
	fake := &fakeKatlcAgentClient{
		nodeStatus: bootObservation{agentStart: "after", running: "gen-a", health: "pending", diagnostic: "katl-boot-health.service has not finished for this boot; inspect the units it requires"}.status(),
		generation: &agentapi.Generation{GenerationId: "gen-a"},
	}
	installKatlcDial(t, nil, fake)
	fastBootHealthPolls(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, _, err := waitNodeBootHealth(ctx, bootWaitTarget{nodeName: "cp-1", endpoints: []string{"10.0.0.11:9443"}}, "before", "gen-a", nodeRecoveryRequirement{}, "reboot node=cp-1", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "did not finish boot health on generation gen-a: katl-boot-health.service has not finished") || !strings.Contains(err.Error(), "katlctl node status cp-1") {
		t.Fatalf("error = %v", err)
	}
}

func fastBootHealthPolls(t *testing.T) {
	t.Helper()
	previous := upgradeRebootPollInterval
	upgradeRebootPollInterval = time.Millisecond
	t.Cleanup(func() { upgradeRebootPollInterval = previous })
}

func TestObservedBootHealth(t *testing.T) {
	good := &agentapi.Generation{GenerationId: "gen-a", CommitState: generation.CommitStateCommitted, BootState: generation.BootStateGood, HealthState: generation.HealthStateHealthy}
	tests := []struct {
		name   string
		status *agentapi.NodeStatus
		want   string
	}{
		{name: "another generation is running", status: &agentapi.NodeStatus{CurrentGenerationId: "gen-a", SelectedGenerationId: "trial", BootHealthState: "failed"}, want: "unknown"},
		{name: "another generation is running with a link down", status: &agentapi.NodeStatus{CurrentGenerationId: "gen-a", SelectedGenerationId: "fallback", BootHealthState: "unhealthy"}, want: "unknown"},
		{name: "running boot failed", status: &agentapi.NodeStatus{CurrentGenerationId: "gen-a", SelectedGenerationId: "gen-a", BootHealthState: "failed"}, want: "failed"},
		{name: "running boot pending", status: &agentapi.NodeStatus{CurrentGenerationId: "gen-a", SelectedGenerationId: "gen-a", BootHealthState: "pending"}, want: "unknown"},
		{name: "running boot healthy", status: &agentapi.NodeStatus{CurrentGenerationId: "gen-a", SelectedGenerationId: "gen-a", BootHealthState: "healthy"}, want: "healthy"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := observedBootHealth(verifiedNodeBoot{Status: test.status, Generation: good}, "gen-a"); got != test.want {
				t.Fatalf("boot health = %q, want %q", got, test.want)
			}
		})
	}
}

func TestCurrentHostUpgradeAcceptsPreservedPreCNIState(t *testing.T) {
	const generationID = "katlos-2026.7.0-alpha.9"
	fake := &fakeKatlcAgentClient{
		nodeStatus: &agentapi.NodeStatus{
			MachineId:           "machine-cp-1",
			EnrollmentId:        "enrollment-cp-1",
			InventoryNodeName:   "cp-1",
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
			RuntimeVersion:      "2026.7.0-alpha.9",
			CommitState:         generation.CommitStateCommitted,
			BootState:           generation.BootStateGood,
			HealthState:         generation.HealthStateHealthy,
		},
	}
	polls := 0
	fake.onGetNodeStatus = func() { polls++ }
	installKatlcDial(t, nil, fake)

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{"node", "upgrade", "2026.7.0-alpha.9", "cp-1", "--config", writeClusterConfig(t), "--timeout", "1s"}, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if fake.submitRequest != nil || len(fake.rebootRequests) != 0 {
		t.Fatalf("submitted = %#v, reboots = %d", fake.submitRequest, len(fake.rebootRequests))
	}
	if polls != 1 || !strings.Contains(stdout.String(), "Kubernetes waiting-for-node") || stderr.Len() != 0 {
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
	_, _, err := waitNodeKubernetesRecovery(ctx, "cp-1", "10.0.0.11:9443", nodeRecoveryRequirement{KubernetesConfigured: true}, "upgrade node=cp-1", &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "local etcd component is not running") || !strings.Contains(err.Error(), "do not schedule workloads") || !strings.Contains(err.Error(), "katlctl node status") {
		t.Fatalf("error = %v", err)
	}
}

func TestWaitNodeBootHealthReportsPhaseChanges(t *testing.T) {
	healthy := generation.HealthStateHealthy
	const down = "down"
	tests := []struct {
		name string
		// observations are reported in order; the last repeats. An agent start of down means no answer.
		observations []bootObservation
		want         string
		wantErr      bool
	}{
		{
			name: "reboot through a down period",
			observations: []bootObservation{
				{agentStart: "before", running: "gen-a", health: "healthy", candidate: healthy},
				{agentStart: "before", running: "gen-a", health: "healthy", candidate: healthy},
				{agentStart: down},
				{agentStart: down},
				{agentStart: "after", running: "gen-a", health: "pending", candidate: healthy},
				{agentStart: "after", running: "gen-a", health: "pending", candidate: healthy},
				{agentStart: "after", running: "gen-a", health: "healthy", candidate: healthy},
			},
			want: "reboot node=cp-1 node-down\nreboot node=cp-1 agent-restarted\nreboot node=cp-1 boot-health-pending\nreboot node=cp-1 boot-health-healthy\n",
		},
		{
			name: "reboot faster than a poll",
			observations: []bootObservation{
				{agentStart: "before", running: "gen-a", health: "healthy", candidate: healthy},
				{agentStart: "after", running: "gen-a", health: "healthy", candidate: healthy},
			},
			want: "reboot node=cp-1 agent-restarted\nreboot node=cp-1 boot-health-healthy\n",
		},
		{
			name: "unreadable boot health recovers",
			observations: []bootObservation{
				{agentStart: "after", running: "gen-a", health: "unknown", diagnostic: "cannot read boot health for this boot from systemd", candidate: healthy},
				{agentStart: "after", running: "gen-a", health: "healthy", candidate: healthy},
			},
			want: "reboot node=cp-1 agent-restarted\nreboot node=cp-1 boot-health-unknown\nreboot node=cp-1 boot-health-healthy\n",
		},
		{
			name: "failing boot health",
			observations: []bootObservation{
				{agentStart: "before", running: "gen-a", health: "healthy", candidate: healthy},
				{agentStart: down},
				{agentStart: "after", running: "gen-a", health: "pending", candidate: healthy},
				{agentStart: "after", running: "gen-a", health: "failed", diagnostic: "katl-boot-health.service failed", candidate: healthy},
			},
			want:    "reboot node=cp-1 node-down\nreboot node=cp-1 agent-restarted\nreboot node=cp-1 boot-health-pending\n",
			wantErr: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeKatlcAgentClient{}
			observed := -1
			fake.onGetNodeStatus = func() {
				observed = min(observed+1, len(test.observations)-1)
				current := test.observations[observed]
				if current.agentStart == down {
					fake.nodeStatus, fake.nodeStatusErr = nil, errors.New("connection refused")
					return
				}
				fake.nodeStatus, fake.nodeStatusErr = current.status(), nil
				fake.generation = &agentapi.Generation{GenerationId: "gen-a", CommitState: generation.CommitStateCommitted, BootState: generation.BootStateGood, HealthState: current.candidate}
			}
			installKatlcDial(t, nil, fake)
			fastBootHealthPolls(t)

			var stderr bytes.Buffer
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, _, err := waitNodeBootHealth(ctx, bootWaitTarget{nodeName: "cp-1", endpoints: []string{"10.0.0.11:9443"}}, "before", "gen-a", nodeRecoveryRequirement{}, "reboot node=cp-1", &stderr)

			if (err != nil) != test.wantErr || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("wait error = %v, want error %v", err, test.wantErr)
			}
			if got := stderr.String(); got != test.want {
				t.Fatalf("progress:\n%s\nwant:\n%s", got, test.want)
			}
		})
	}
}

func TestWaitNodeBootHealthTimeoutReportsLastDiagnostic(t *testing.T) {
	const diagnostic = "cannot read boot health for this boot from systemd"
	fake := &fakeKatlcAgentClient{nodeStatus: bootObservation{
		agentStart: "after", running: "gen-a", health: "unknown", diagnostic: diagnostic,
	}.status()}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	fake.onGetNodeStatus = cancel
	installKatlcDial(t, nil, fake)

	_, _, err := waitNodeBootHealth(ctx, bootWaitTarget{nodeName: "cp-1", endpoints: []string{"10.0.0.11:9443"}}, "before", "gen-a", nodeRecoveryRequirement{}, "reboot node=cp-1", &bytes.Buffer{})
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), diagnostic) {
		t.Fatalf("wait error = %v, want context error with %q", err, diagnostic)
	}
}

func TestWaitNodeBootHealthStaysQuietWhileOldAddressAnswers(t *testing.T) {
	fake := &fakeKatlcAgentClient{}
	polls := 0
	fake.onGetNodeStatus = func() {
		polls++
		if polls < 4 {
			fake.nodeStatus = bootObservation{agentStart: "before", running: "gen-a", health: "healthy"}.status()
			return
		}
		fake.nodeStatus = bootObservation{agentStart: "after", running: "gen-a", health: "healthy"}.status()
	}
	fake.generation = &agentapi.Generation{GenerationId: "gen-a", CommitState: generation.CommitStateCommitted, BootState: generation.BootStateGood, HealthState: generation.HealthStateHealthy}
	oldDial := dialKatlcAgent
	dialKatlcAgent = func(_ context.Context, endpoint string) (katlcAgentConnection, error) {
		if endpoint == "10.0.0.12:9443" && polls < 4 {
			return katlcAgentConnection{}, errors.New("no route to host")
		}
		return katlcAgentConnection{Client: fake, Close: func() error { return nil }}, nil
	}
	t.Cleanup(func() { dialKatlcAgent = oldDial })
	fastBootHealthPolls(t)

	var stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	target := bootWaitTarget{nodeName: "cp-1", endpoints: []string{"10.0.0.12:9443", "10.0.0.11:9443"}}
	if _, _, err := waitNodeBootHealth(ctx, target, "before", "gen-a", nodeRecoveryRequirement{}, "reboot node=cp-1", &stderr); err != nil {
		t.Fatal(err)
	}

	if strings.Contains(stderr.String(), "node-down") {
		t.Fatalf("progress reports the node down while its previous address still answers:\n%s", stderr.String())
	}
}
