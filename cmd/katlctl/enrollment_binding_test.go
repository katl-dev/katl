package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/bootstrap/cluster"
	"github.com/katl-dev/katl/internal/installer/operation"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/katlctl/workstation"
)

func TestSwappedManagementAddressRefusesConfigPlanAndMutation(t *testing.T) {
	contextPath := writeEnrollmentContext(t)
	changePath := filepath.Join(t.TempDir(), "change.yaml")
	if err := os.WriteFile(changePath, []byte("apiVersion: katl.dev/v1alpha1\nkind: NodeConfigurationChange\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"node", "apply", "--plan", "--context-file", contextPath, "--node", "cp-1", "--config", changePath},
		{"node", "apply", "--context-file", contextPath, "--node", "cp-1", "--config", changePath, "--acknowledge-storage-wipe", "cp-1/data"},
	} {
		fake := swappedNodeClient()
		installKatlcDial(t, nil, fake)
		err := run(context.Background(), args, &bytes.Buffer{}, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), `address answered as enrolled node "cp-2"`) {
			t.Fatalf("run(%v) error = %v, want swapped-address refusal", args, err)
		}
		if fake.validateRequest != nil || fake.submitRequest != nil || fake.stageRequest != nil || fake.applyRequest != nil {
			t.Fatalf("swapped address reached acceptance: validate=%+v submit=%+v stage=%+v apply=%+v", fake.validateRequest, fake.submitRequest, fake.stageRequest, fake.applyRequest)
		}
	}
}

func TestSwappedManagementAddressRefusesShutdownAndWipePlan(t *testing.T) {
	contextPath := writeEnrollmentContext(t)
	swapped := swappedNodeClient()
	installKatlcDial(t, nil, swapped)
	err := run(context.Background(), []string{"node", "shutdown", "cp-1", "--context-file", contextPath, "--no-wait"}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), `address answered as enrolled node "cp-2"`) {
		t.Fatalf("shutdown error = %v, want swapped-address refusal", err)
	}
	if len(swapped.shutdownRequests) != 0 {
		t.Fatalf("shutdown requests = %d, want none", len(swapped.shutdownRequests))
	}

	connector := newFakeWipeClusterConnector(map[string]*fakeKatlcAgentClient{"cp-1": swapped})
	oldConnector := newWipeClusterConnector
	newWipeClusterConnector = func() cluster.AgentConnector { return connector }
	t.Cleanup(func() { newWipeClusterConnector = oldConnector })
	err = run(context.Background(), []string{"node", "wipe", "cp-1", "--context-file", contextPath, "--plan"}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "node-local preflight failed") {
		t.Fatalf("wipe plan error = %v, want identity preflight refusal", err)
	}
	if swapped.submitRequest != nil {
		t.Fatalf("wipe plan submitted mutation: %+v", swapped.submitRequest)
	}
}

func TestContextRebindVerifiesIdentityBeforeSavingAddress(t *testing.T) {
	contextPath := writeEnrollmentContext(t)
	matching := &fakeKatlcAgentClient{nodeStatus: enrolledStatus("cp-1", "enrollment-cp-1", "machine-cp-1")}
	installKatlcDial(t, nil, matching)
	var stdout bytes.Buffer
	if err := run(context.Background(), []string{"context", "rebind", "--context-file", contextPath, "--node", "cp-1", "--endpoint", "10.0.0.91"}, &stdout, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	cfg, err := workstation.Load(contextPath)
	if err != nil {
		t.Fatal(err)
	}
	topology, err := cfg.SelectedTopology("")
	if err != nil {
		t.Fatal(err)
	}
	if got := topology.Nodes[0].ManagementEndpoint; got != "10.0.0.91:9443" {
		t.Fatalf("rebound endpoint = %q", got)
	}
	if !strings.Contains(stdout.String(), "after verifying enrollment and machine identity") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestContextRebindRefusesDifferentEnrollment(t *testing.T) {
	contextPath := writeEnrollmentContext(t)
	wrong := &fakeKatlcAgentClient{nodeStatus: enrolledStatus("cp-1", "different-enrollment", "machine-cp-1")}
	installKatlcDial(t, nil, wrong)
	err := run(context.Background(), []string{"context", "rebind", "--context-file", contextPath, "--node", "cp-1", "--endpoint", "10.0.0.91"}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "enrollment identity does not match") {
		t.Fatalf("rebind error = %v, want identity mismatch", err)
	}
	cfg, loadErr := workstation.Load(contextPath)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	topology, selectErr := cfg.SelectedTopology("")
	if selectErr != nil {
		t.Fatal(selectErr)
	}
	if got := topology.Nodes[0].ManagementEndpoint; got != "10.0.0.11:9443" {
		t.Fatalf("refused rebind changed endpoint to %q", got)
	}
}

func TestContextRebindRequiresPositiveTimeout(t *testing.T) {
	err := run(context.Background(), []string{"context", "rebind", "--node", "cp-1", "--endpoint", "10.0.0.91", "--timeout=0"}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "--timeout must be positive") {
		t.Fatalf("rebind error = %v", err)
	}
}

func swappedNodeClient() *fakeKatlcAgentClient {
	return &fakeKatlcAgentClient{nodeStatus: enrolledStatus("cp-2", "enrollment-cp-2", "machine-cp-2")}
}

func enrolledStatus(name, enrollmentID, machineID string) *agentapi.NodeStatus {
	return &agentapi.NodeStatus{
		ApiVersion: operation.APIVersion, InventoryNodeName: name, EnrollmentId: enrollmentID,
		MachineId: machineID, CurrentGenerationId: "generation-0", SupportedOperationKinds: []string{wipeClusterOperationKind},
		Kubernetes: &agentapi.KubernetesStatus{State: "not-configured"},
	}
}

func writeEnrollmentContext(t *testing.T) string {
	t.Helper()
	return writeKatlctlConfig(t, `currentContext: lab
contexts:
- name: lab
  cluster: lab
clusters:
- name: lab
  nodes:
  - name: cp-1
    managementEndpoint: 10.0.0.11:9443
    systemRole: control-plane
    enrollmentID: enrollment-cp-1
    machineID: machine-cp-1
  - name: cp-2
    managementEndpoint: 10.0.0.12:9443
    systemRole: control-plane
    enrollmentID: enrollment-cp-2
    machineID: machine-cp-2
`)
}
