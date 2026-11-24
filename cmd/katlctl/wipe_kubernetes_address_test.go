package main

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/katl-dev/katl/internal/bootstrap/cluster"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
)

func TestWipeNodeFromContextObservesKubernetesAddress(t *testing.T) {
	// The saved context records only management endpoints, and cp-3 is
	// managed through NAT at 198.18.0.13 while etcd advertises 10.0.0.13.
	for _, tc := range []struct {
		name    string
		backend string
		want    string
	}{
		{name: "matching member", backend: "10.0.0.13:6443", want: "planned"},
		{name: "different member", backend: "10.0.0.99:6443", want: "refused"},
		{name: "no API proxy status", want: "refused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configPath := writeKatlctlConfig(t, `currentContext: lab
contexts:
- name: lab
  cluster: katl-lab
clusters:
- name: katl-lab
  controlPlaneEndpoint: 10.0.0.11:6443
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
  - name: cp-3
    managementEndpoint: 198.18.0.13:9443
    systemRole: control-plane
    enrollmentID: enrollment-cp-3
    machineID: machine-cp-3
`)
			cp3 := readyWipeClusterClient("machine-cp-3")
			cp3.nodeStatus.Kubernetes = &agentapi.KubernetesStatus{State: "ready", Role: "control-plane"}
			if tc.backend != "" {
				cp3.nodeStatus.ApiProxy = &agentapi.APIProxyStatus{State: "ready", Backends: []*agentapi.APIProxyBackendStatus{
					{Name: "cp-1", Address: "10.0.0.11:6443"},
					{Name: "cp-3", Address: tc.backend, Local: true},
				}}
			}
			cp1 := readyWipeClusterClient("machine-cp-1")
			cp1.etcdStatus = healthyThreeMemberEtcdStatus()
			connector := newFakeWipeClusterConnector(map[string]*fakeKatlcAgentClient{"cp-1": cp1, "cp-3": cp3})
			oldConnector := newWipeClusterConnector
			newWipeClusterConnector = func() cluster.AgentConnector { return connector }
			t.Cleanup(func() { newWipeClusterConnector = oldConnector })

			var stdout, stderr bytes.Buffer
			err := run(context.Background(), []string{
				"node", "wipe", "cp-3", "--context-file", configPath, "--plan", "--output", "json",
			}, &stdout, &stderr)
			var report wipeNodeReport
			if decodeErr := json.Unmarshal(stdout.Bytes(), &report); decodeErr != nil {
				t.Fatalf("decode report: %v (run error %v, stderr %s)\n%s", decodeErr, err, stderr.String(), stdout.String())
			}
			if report.EtcdCleanup != tc.want || (err == nil) != (tc.want == "planned") {
				t.Fatalf("etcd cleanup = %q, error = %v, want %s; report = %+v", report.EtcdCleanup, err, tc.want, report)
			}
			if tc.want == "planned" && report.EtcdMemberID != "3" {
				t.Fatalf("etcd member = %q, want 3", report.EtcdMemberID)
			}
		})
	}
}
