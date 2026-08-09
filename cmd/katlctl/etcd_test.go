package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/katl-dev/katl/internal/bootstrap/cluster"
	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
)

func TestWipeNodePlansSafeControlPlaneMembershipRemoval(t *testing.T) {
	inventoryPath := writeThreeControlPlaneInventory(t)
	cp3 := readyWipeClusterClient("machine-cp-3")
	cp3.nodeStatus.Kubernetes = &agentapi.KubernetesStatus{State: "ready", Role: "control-plane"}
	cp1 := readyWipeClusterClient("machine-cp-1")
	cp1.etcdStatus = healthyThreeMemberEtcdStatus()
	connector := newFakeWipeClusterConnector(map[string]*fakeKatlcAgentClient{"cp-1": cp1, "cp-3": cp3})
	oldConnector := newWipeClusterConnector
	newWipeClusterConnector = func() cluster.AgentConnector { return connector }
	t.Cleanup(func() { newWipeClusterConnector = oldConnector })

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{
		"node", "wipe", "cp-3", "--inventory", inventoryPath, "--plan", "--output", "json",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	var report wipeNodeReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.EtcdCleanup != "planned" || report.EtcdCoordinator != "cp-1" || report.EtcdMemberID != "3" {
		t.Fatalf("etcd plan = %+v", report)
	}
	if cp1.submitRequest != nil || cp3.submitRequest != nil {
		t.Fatalf("plan submitted operations: cp1=%+v cp3=%+v", cp1.submitRequest, cp3.submitRequest)
	}
}

func TestWipeNodeResumesAfterEtcdMemberWasRemoved(t *testing.T) {
	inventoryPath := writeThreeControlPlaneInventory(t)
	cp3 := readyWipeClusterClient("machine-cp-3")
	cp3.nodeStatus.Kubernetes = &agentapi.KubernetesStatus{State: "waiting-for-control-plane", Role: "control-plane"}
	cp1 := readyWipeClusterClient("machine-cp-1")
	cp1.etcdStatus = healthyTwoMemberEtcdStatus()
	connector := newFakeWipeClusterConnector(map[string]*fakeKatlcAgentClient{"cp-1": cp1, "cp-3": cp3})
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
	err := run(context.Background(), []string{
		"node", "wipe", "cp-3", "--inventory", inventoryPath, "--kubeconfig", "admin.conf", "--output", "json",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run() error = %v, stderr = %s", err, stderr.String())
	}
	var report wipeNodeReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.EtcdCleanup != "already-removed" || report.EtcdCoordinator != "cp-1" || report.EtcdMemberID != "" || report.KubernetesCleanup != "succeeded" {
		t.Fatalf("wipe report = %+v", report)
	}
	if cp1.submitRequest != nil {
		t.Fatalf("coordinator submitted another operation: %+v", cp1.submitRequest)
	}
	if cp3.submitRequest == nil || cp3.submitRequest.GetDestructiveReset() == nil {
		t.Fatalf("target wipe request = %+v", cp3.submitRequest)
	}
	wantCalls := [][]string{
		{"kubectl", "--kubeconfig", "admin.conf", "cordon", "cp-3"},
		{"kubectl", "--kubeconfig", "admin.conf", "drain", "cp-3", "--ignore-daemonsets", "--delete-emptydir-data", "--force", "--timeout=25m"},
		{"kubectl", "--kubeconfig", "admin.conf", "--server=https://10.0.0.11:6443", "delete", "node", "cp-3", "--ignore-not-found=true"},
	}
	if !reflect.DeepEqual(kubectl.calls, wantCalls) {
		t.Fatalf("kubectl calls = %#v, want %#v", kubectl.calls, wantCalls)
	}
}

func TestWipeNodeTextReportsKubernetesCleanupFailure(t *testing.T) {
	report := wipeNodeReport{wipeClusterReport: wipeClusterReport{Output: "text", Targets: []wipeClusterTarget{{Name: "cp-3", SystemRole: string(inventory.RoleControlPlane), Address: "10.0.0.13"}}}}
	report.EtcdCleanup = "succeeded"
	report.EtcdCoordinator = "cp-1"
	report.EtcdMemberID = "3"
	report.KubernetesCleanup = "recovery-required"
	report.KubernetesDiagnostics = []string{"delete node failed: API unavailable"}
	var output bytes.Buffer
	if err := printWipeNodeReport(&output, report); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"etcd cleanup=succeeded coordinator=cp-1 member=3", "Kubernetes cleanup=recovery-required", "Kubernetes: delete node failed: API unavailable"} {
		if !bytes.Contains(output.Bytes(), []byte(want)) {
			t.Fatalf("output = %q, want %q", output.String(), want)
		}
	}
}

func TestEtcdRemoveRequiresObservedMemberID(t *testing.T) {
	err := runEtcdRemove(context.Background(), etcdRemoveOptions{
		etcdOptions: etcdOptions{configPath: writeThreeControlPlaneInventory(t), output: "text"},
		targetNode:  "cp-3",
	}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("runEtcdRemove() error = nil")
	}
}

func healthyThreeMemberEtcdStatus() *agentapi.EtcdStatus {
	return &agentapi.EtcdStatus{
		ClusterId: "64", LocalMemberId: "1", LeaderId: "1", Quorum: 2, HealthyMembers: 3,
		Members: []*agentapi.EtcdMember{
			{Id: "1", Name: "cp-1", PeerUrls: []string{"https://10.0.0.11:2380"}, ClientUrls: []string{"https://10.0.0.11:2379"}, Healthy: true, Leader: true},
			{Id: "2", Name: "cp-2", PeerUrls: []string{"https://10.0.0.12:2380"}, ClientUrls: []string{"https://10.0.0.12:2379"}, Healthy: true},
			{Id: "3", Name: "cp-3", PeerUrls: []string{"https://10.0.0.13:2380"}, ClientUrls: []string{"https://10.0.0.13:2379"}, Healthy: true},
		},
	}
}

func healthyTwoMemberEtcdStatus() *agentapi.EtcdStatus {
	status := healthyThreeMemberEtcdStatus()
	status.Members = status.Members[:2]
	status.HealthyMembers = 2
	status.Quorum = 2
	return status
}

func writeThreeControlPlaneInventory(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cluster.yaml")
	data := `controlPlaneEndpoint: api.katl.test:6443
kubernetesVersion: v1.36.1
nodes:
- name: cp-1
  address: 10.0.0.11
  systemRole: control-plane
  access: {method: agent}
  kubeadmConfig: {ref: control-plane, path: /etc/katl/kubeadm/control-plane/config.yaml, intent: control-plane}
  kubernetesVersion: v1.36.1
  enrollmentID: enrollment-cp-1
  machineID: machine-cp-1
- name: cp-2
  address: 10.0.0.12
  systemRole: control-plane
  access: {method: agent}
  kubeadmConfig: {ref: control-plane, path: /etc/katl/kubeadm/control-plane/config.yaml, intent: control-plane}
  kubernetesVersion: v1.36.1
  enrollmentID: enrollment-cp-2
  machineID: machine-cp-2
- name: cp-3
  address: 10.0.0.13
  systemRole: control-plane
  access: {method: agent}
  kubeadmConfig: {ref: control-plane, path: /etc/katl/kubeadm/control-plane/config.yaml, intent: control-plane}
  kubernetesVersion: v1.36.1
  enrollmentID: enrollment-cp-3
  machineID: machine-cp-3
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
