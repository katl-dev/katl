package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/installer/operation"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
)

func TestOrderControlPlanesChangesCoordinatorLast(t *testing.T) {
	nodes := []inventory.Node{{Name: "cp-3"}, {Name: "cp-1"}, {Name: "cp-2"}}
	ordered, err := orderControlPlanes(nodes, "cp-2")
	if err != nil {
		t.Fatal(err)
	}
	got := []string{ordered[0].Name, ordered[1].Name, ordered[2].Name}
	if want := []string{"cp-1", "cp-3", "cp-2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestRunKubeadmControlPlaneConfigSubmitsSerialCoordinatorLast(t *testing.T) {
	root := t.TempDir()
	inventoryPath := filepath.Join(root, "inventory.yaml")
	content := "nodes:\n  - name: cp-3\n    address: 192.0.2.3\n    systemRole: control-plane\n  - name: cp-1\n    address: 192.0.2.1\n    systemRole: control-plane\n  - name: cp-2\n    address: 192.0.2.2\n    systemRole: control-plane\n"
	if err := os.WriteFile(inventoryPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	clients := map[string]*fakeKatlcAgentClient{}
	for _, name := range []string{"cp-1", "cp-2", "cp-3"} {
		clients[name] = &fakeKatlcAgentClient{nodeStatus: &agentapi.NodeStatus{MachineId: "machine-" + name}, generation: &agentapi.Generation{GenerationId: "gen-2", CommitState: "committed", HealthState: "healthy", ConfigApply: &agentapi.ConfigApplyStatus{KubeadmActionRequired: true, SelectedKubeadmConfigName: "control-plane"}, Sysexts: []*agentapi.ExtensionRef{{Name: "kubernetes", PayloadVersion: "v1.36.1", Sha256: strings.Repeat("c", 64)}}}, submitAccepted: &agentapi.OperationAccepted{OperationId: "op-" + name, RequestDigest: strings.Repeat("f", 64)}, operationStatus: &agentapi.OperationStatus{Terminal: true, Result: operation.ResultSucceeded}}
	}
	byEndpoint := map[string]*fakeKatlcAgentClient{"192.0.2.1:9443": clients["cp-1"], "192.0.2.2:9443": clients["cp-2"], "192.0.2.3:9443": clients["cp-3"]}
	previous := dialKatlcAgent
	defer func() { dialKatlcAgent = previous }()
	dialKatlcAgent = func(_ context.Context, endpoint, token string) (katlcAgentConnection, error) {
		client := byEndpoint[endpoint]
		if client == nil {
			return katlcAgentConnection{}, os.ErrNotExist
		}
		return katlcAgentConnection{Client: client, Close: func() error { return nil }}, nil
	}
	opts := kubeadmControlPlaneConfigOptions{inventoryPath: inventoryPath, coordinator: "cp-3", generationID: "gen-2", configName: "control-plane", rolloutID: "rollout-1"}
	var stdout bytes.Buffer
	if err := runKubeadmControlPlaneConfig(context.Background(), opts, &stdout); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout.String(), "requestDigest") {
		t.Fatalf("stdout exposed request digest: %s", stdout.String())
	}
	for index, name := range []string{"cp-1", "cp-2", "cp-3"} {
		requests := clients[name].submitRequests
		if len(requests) != 2 || !requests[0].DryRun || requests[1].DryRun {
			t.Fatalf("%s requests=%#v", name, requests)
		}
		req := requests[1]
		if req == nil || req.KubeadmControlPlaneConfig.NodePosition != uint32(index+1) || req.KubeadmControlPlaneConfig.CoordinatorUpload != (name == "cp-3") {
			t.Fatalf("%s request=%#v", name, req)
		}
		body := req.KubeadmControlPlaneConfig
		if body.DesiredConfigSha256 != "" || body.ExpectedLiveConfigSha256 != "" || body.KubernetesPayloadSha256 != "" || body.SnapshotDigest != "" || len(body.SupportedFieldDelta) != 0 {
			t.Fatalf("%s request exposed operator-derived state: %#v", name, body)
		}
	}
}

func TestOrderControlPlanesRejectsUnknownCoordinator(t *testing.T) {
	_, err := orderControlPlanes([]inventory.Node{{Name: "cp-1"}, {Name: "cp-2"}, {Name: "cp-3"}}, "cp-4")
	if err == nil {
		t.Fatal("orderControlPlanes() error = nil")
	}
}
