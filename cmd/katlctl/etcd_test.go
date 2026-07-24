package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/katl-dev/katl/internal/bootstrap/cluster"
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
- name: cp-2
  address: 10.0.0.12
  systemRole: control-plane
  access: {method: agent}
  kubeadmConfig: {ref: control-plane, path: /etc/katl/kubeadm/control-plane/config.yaml, intent: control-plane}
  kubernetesVersion: v1.36.1
- name: cp-3
  address: 10.0.0.13
  systemRole: control-plane
  access: {method: agent}
  kubeadmConfig: {ref: control-plane, path: /etc/katl/kubeadm/control-plane/config.yaml, intent: control-plane}
  kubernetesVersion: v1.36.1
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
