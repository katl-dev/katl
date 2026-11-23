package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/katl-dev/katl/internal/bootstrap/cluster"
	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/katlctl/workstation"
)

// A management.address may name the agent port, as it does when NAT forwards
// a different public port. Enrollment saves that endpoint, and bootstrap and
// wipe from the saved context dial it.
func TestManagementPortSurvivesSavedContext(t *testing.T) {
	contextPath := filepath.Join(t.TempDir(), "katlctl.yaml")
	t.Setenv("KATLCTL_CONFIG", contextPath)
	t.Setenv("KATLCTL_CONFIG_DIR", "")
	if _, _, err := ensureManagementIdentity("lab", io.Discard); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(t.TempDir(), "cluster.yaml")
	source := strings.Replace(configBundleSource(), "address: 10.0.0.11\n", "address: 198.18.0.11:19443\n", 1)
	if err := os.WriteFile(sourcePath, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}

	var dialed []string
	fake := &fakeKatlcAgentClient{nodeStatus: &agentapi.NodeStatus{InventoryNodeName: "cp-1", EnrollmentId: "enrollment-cp-1", MachineId: "machine-cp-1"}}
	oldDial := dialKatlcAgent
	dialKatlcAgent = func(_ context.Context, endpoint string) (katlcAgentConnection, error) {
		dialed = append(dialed, endpoint)
		return katlcAgentConnection{Client: fake, Close: func() error { return nil }}, nil
	}
	t.Cleanup(func() { dialKatlcAgent = oldDial })

	var stderr bytes.Buffer
	if err := run(context.Background(), []string{"context", "save", "--config", sourcePath, "--context-file", contextPath}, &bytes.Buffer{}, &stderr); err != nil {
		t.Fatalf("context save: %v\n%s", err, stderr.String())
	}
	if !reflect.DeepEqual(dialed, []string{"198.18.0.11:19443"}) {
		t.Fatalf("context save dialed %q, want the configured agent port", dialed)
	}
	saved, err := workstation.Load(contextPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := saved.Clusters[0].Nodes[0].ManagementEndpoint; got != "198.18.0.11:19443" {
		t.Fatalf("saved management endpoint = %q, want 198.18.0.11:19443", got)
	}

	var request cluster.Request
	oldBootstrap := runAgentBootstrap
	runAgentBootstrap = func(_ context.Context, got cluster.Request, _ cluster.AgentBootstrapDependencies) (cluster.Result, error) {
		request = got
		return cluster.Result{DryRun: true}, nil
	}
	t.Cleanup(func() { runAgentBootstrap = oldBootstrap })
	if err := run(context.Background(), []string{"cluster", "bootstrap", "--config", sourcePath, "--dry-run"}, &bytes.Buffer{}, &stderr); err != nil {
		t.Fatalf("cluster bootstrap: %v\n%s", err, stderr.String())
	}
	if len(request.Inventory.Nodes) != 1 || fmt.Sprint(request.Inventory.Nodes[0].Address) != "198.18.0.11:19443" {
		t.Fatalf("bootstrap inventory = %+v, want cp-1 at its saved endpoint", request.Inventory.Nodes)
	}
}

// node wipe removes a control plane's Node object through a surviving
// control plane's kube-apiserver. The workstation reaches that node only at
// its management host, which is not in the serving certificate, so kubectl
// must verify the control-plane endpoint name instead.
func TestWipeNodeDeletesThroughTranslatedCoordinator(t *testing.T) {
	contextPath := writeKatlctlConfig(t, `currentContext: lab
contexts:
- name: lab
  cluster: katl-lab
clusters:
- name: katl-lab
  controlPlaneEndpoint: 10.0.0.11:6443
  nodes:
  - name: cp-1
    managementEndpoint: 198.18.0.11:19443
    systemRole: control-plane
    enrollmentID: enrollment-cp-1
    machineID: machine-cp-1
  - name: cp-2
    managementEndpoint: 198.18.0.12:19443
    systemRole: control-plane
    enrollmentID: enrollment-cp-2
    machineID: machine-cp-2
  - name: cp-3
    managementEndpoint: 198.18.0.13:19443
    systemRole: control-plane
    enrollmentID: enrollment-cp-3
    machineID: machine-cp-3
`)
	cp3 := readyWipeClusterClient("machine-cp-3")
	cp3.nodeStatus.Kubernetes = &agentapi.KubernetesStatus{State: "ready", Role: "control-plane"}
	cp3.nodeStatus.ApiProxy = &agentapi.APIProxyStatus{State: "ready", Backends: []*agentapi.APIProxyBackendStatus{
		{Name: "cp-1", Address: "10.0.0.11:6443"},
		{Name: "cp-3", Address: "10.0.0.13:6443", Local: true},
	}}
	cp1 := readyWipeClusterClient("machine-cp-1")
	cp1.etcdStatus = healthyTwoMemberEtcdStatus()
	connector := &addressRecordingConnector{next: newFakeWipeClusterConnector(map[string]*fakeKatlcAgentClient{"cp-1": cp1, "cp-3": cp3})}
	oldConnector := newWipeClusterConnector
	newWipeClusterConnector = func() cluster.AgentConnector { return connector }
	oldKubectl := operatorKubectlRunner
	kubectl := &fakeKubectlRunner{}
	operatorKubectlRunner = kubectl
	t.Cleanup(func() {
		newWipeClusterConnector = oldConnector
		operatorKubectlRunner = oldKubectl
	})

	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{
		"node", "wipe", "cp-3", "--context-file", contextPath, "--kubeconfig", "admin.conf", "--output", "json",
	}, &stdout, &stderr); err != nil {
		t.Fatalf("run() error = %v, stderr = %s\n%s", err, stderr.String(), stdout.String())
	}
	var report wipeNodeReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.EtcdCoordinator != "cp-1" || report.KubernetesCleanup != "succeeded" {
		t.Fatalf("wipe report = %+v", report)
	}

	for name, address := range connector.addresses() {
		if !strings.HasSuffix(address, ":19443") {
			t.Errorf("wipe dialed %s at %s, want its saved agent port", name, address)
		}
	}
	want := []string{"kubectl", "--kubeconfig", "admin.conf", "--server=https://198.18.0.11:6443", "--tls-server-name=10.0.0.11", "delete", "node", "cp-3", "--ignore-not-found=true"}
	if len(kubectl.calls) == 0 || !reflect.DeepEqual(kubectl.calls[len(kubectl.calls)-1], want) {
		t.Fatalf("kubectl calls = %q, want the last to be %q", kubectl.calls, want)
	}
}

// addressRecordingConnector records the management address each node is
// dialed at.
type addressRecordingConnector struct {
	next cluster.AgentConnector
	mu   sync.Mutex
	seen map[string]string
}

func (c *addressRecordingConnector) Connect(ctx context.Context, node inventory.PlannedNode) (cluster.AgentConnection, error) {
	c.mu.Lock()
	if c.seen == nil {
		c.seen = map[string]string{}
	}
	c.seen[node.Name] = fmt.Sprint(node.Address)
	c.mu.Unlock()
	return c.next.Connect(ctx, node)
}

func (c *addressRecordingConnector) addresses() map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.seen
}
