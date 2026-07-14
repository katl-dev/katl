package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/installer/generation"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
)

func TestHostStatusUsesContextAndPrintsOperatorView(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "cp-1.token")
	if err := os.WriteFile(tokenPath, []byte("node-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := writeKatlctlConfig(t, fmt.Sprintf(`currentContext: lab
contexts:
- name: lab
  cluster: homelab
clusters:
- name: homelab
  nodes:
  - name: cp-1
    managementEndpoint: 192.0.2.10:9443
    systemRole: control-plane
    credentialRef: file:%s
`, tokenPath))
	fake := healthyHostClient("machine-secret", "agent-secret", "generation-0")
	fake.nodeStatus.OperationLockHeld = true
	fake.nodeStatus.ActiveOperationIds = []string{"operation-secret"}
	fake.nodeStatus.BootTargetGenerationId = "generation-staged"
	fake.generation.RuntimeVersion = "2026.7.0-alpha.10"
	installKatlcDial(t, func(endpoint, token string) {
		if endpoint != "192.0.2.10:9443" || token != "node-token" {
			t.Fatalf("dial endpoint=%q token=%q", endpoint, token)
		}
	}, fake)

	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"host", "status", "cp-1", "--config", configPath}, &stdout, &stderr); err != nil {
		t.Fatalf("run() error = %v, stderr = %s", err, stderr.String())
	}
	output := stdout.String()
	for _, want := range []string{"NODE", "HEALTH", "KATLOS", "GENERATION", "NEXT BOOT", "ACTIVITY", "cp-1", "OK", "2026.7.0-alpha.10", "generation-0", "generation-staged", "busy"} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q:\n%s", want, output)
		}
	}
	for _, internal := range []string{"machine-secret", "agent-secret", "operation-secret"} {
		if strings.Contains(output, internal) {
			t.Fatalf("output exposes %q:\n%s", internal, output)
		}
	}
}

func TestHostStatusJSON(t *testing.T) {
	fake := healthyHostClient("machine-a", "agent-a", "generation-0")
	installKatlcDial(t, nil, fake)

	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"host", "status", "node-a", "--endpoint", "node-a.test:9443", "--output", "json"}, &stdout, &stderr); err != nil {
		t.Fatalf("run() error = %v, stderr = %s", err, stderr.String())
	}
	var report hostStatusReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode status: %v\n%s", err, stdout.String())
	}
	if report.Node != "node-a" || report.Endpoint != "node-a.test:9443" || report.Health != "OK" || report.Generation != "generation-0" || report.Activity != "idle" {
		t.Fatalf("report = %#v", report)
	}
}

func TestHostRebootHonorsBootTargetAndWaits(t *testing.T) {
	fake := healthyHostClient("machine-a", "before", "generation-0")
	fake.nodeStatus.BootTargetGenerationId = "generation-staged"
	fake.onReboot = func(req *agentapi.RebootRequest) {
		fake.nodeStatus.AgentStartId = "after"
		fake.nodeStatus.CurrentGenerationId = req.GetTargetGenerationId()
		fake.generation.GenerationId = req.GetTargetGenerationId()
	}
	installKatlcDial(t, nil, fake)

	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"host", "reboot", "node-a", "--endpoint", "node-a.test:9443", "--timeout", "1s"}, &stdout, &stderr); err != nil {
		t.Fatalf("run() error = %v, stderr = %s", err, stderr.String())
	}
	if len(fake.rebootRequests) != 1 {
		t.Fatalf("reboot requests = %d, want 1", len(fake.rebootRequests))
	}
	request := fake.rebootRequests[0]
	if request.GetActor() != "katlctl host reboot" || request.GetExpectedMachineId() != "machine-a" || request.GetTargetGenerationId() != "generation-staged" {
		t.Fatalf("reboot request = %#v", request)
	}
	if got := stdout.String(); got != "node-a rebooted successfully; health OK\n" {
		t.Fatalf("stdout = %q", got)
	}
	if got := stderr.String(); !strings.Contains(got, "Reboot scheduled for node-a") || strings.Contains(got, "agent=") {
		t.Fatalf("stderr = %q", got)
	}
}

func TestHostRebootNoWaitJSON(t *testing.T) {
	fake := healthyHostClient("machine-a", "before", "generation-0")
	installKatlcDial(t, nil, fake)

	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"host", "reboot", "node-a", "--endpoint", "node-a.test:9443", "--no-wait", "--output", "json"}, &stdout, &stderr); err != nil {
		t.Fatalf("run() error = %v, stderr = %s", err, stderr.String())
	}
	var report hostRebootReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode reboot: %v\n%s", err, stdout.String())
	}
	if report.Node != "node-a" || report.Result != "scheduled" || report.Generation != "generation-0" || report.Health != "" {
		t.Fatalf("report = %#v", report)
	}
}

func TestHostRebootReportsUnhealthyReturn(t *testing.T) {
	fake := healthyHostClient("machine-a", "before", "generation-0")
	fake.onReboot = func(*agentapi.RebootRequest) {
		fake.nodeStatus.AgentStartId = "after"
		fake.generation.HealthState = generation.HealthStateUnhealthy
	}
	installKatlcDial(t, nil, fake)

	err := run(context.Background(), []string{"host", "reboot", "node-a", "--endpoint", "node-a.test:9443", "--timeout", "1s"}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "reported generation generation-0 unhealthy after reboot") {
		t.Fatalf("run() error = %v, want unhealthy boot error", err)
	}
}

func TestHostRebootTimesOutWhenAgentDoesNotRestart(t *testing.T) {
	fake := healthyHostClient("machine-a", "before", "generation-0")
	installKatlcDial(t, nil, fake)
	oldInterval := upgradeRebootPollInterval
	upgradeRebootPollInterval = time.Millisecond
	t.Cleanup(func() { upgradeRebootPollInterval = oldInterval })

	err := run(context.Background(), []string{"host", "reboot", "node-a", "--endpoint", "node-a.test:9443", "--timeout", "10ms"}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "node node-a did not return healthy") {
		t.Fatalf("run() error = %v, want reboot timeout", err)
	}
}

func TestHostManagementRejectsDuplicateNodeSelection(t *testing.T) {
	err := run(context.Background(), []string{"host", "status", "cp-1", "--node", "worker-1"}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "NODE cannot be combined with --node") {
		t.Fatalf("run() error = %v", err)
	}
}

func healthyHostClient(machineID, agentStartID, generationID string) *fakeKatlcAgentClient {
	return &fakeKatlcAgentClient{
		nodeStatus: &agentapi.NodeStatus{
			MachineId:           machineID,
			AgentStartId:        agentStartID,
			CurrentGenerationId: generationID,
		},
		generation: &agentapi.Generation{
			GenerationId: generationID,
			CommitState:  generation.CommitStateCommitted,
			BootState:    generation.BootStateGood,
			HealthState:  generation.HealthStateHealthy,
		},
	}
}

func installKatlcDial(t *testing.T, inspect func(endpoint, token string), client agentapi.KatlcAgentClient) {
	t.Helper()
	oldDial := dialKatlcAgent
	dialKatlcAgent = func(_ context.Context, endpoint, token string) (katlcAgentConnection, error) {
		if inspect != nil {
			inspect(endpoint, token)
		}
		return katlcAgentConnection{Client: client, Close: func() error { return nil }}, nil
	}
	t.Cleanup(func() { dialKatlcAgent = oldDial })
}
