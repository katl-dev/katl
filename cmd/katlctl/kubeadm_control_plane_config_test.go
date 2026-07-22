package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

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
	dialKatlcAgent = func(_ context.Context, endpoint string) (katlcAgentConnection, error) {
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
	if strings.Contains(stdout.String(), "requestDigest") || strings.Contains(stdout.String(), "operationID") || strings.Contains(stdout.String(), "rolloutID") {
		t.Fatalf("stdout exposed internal operation metadata: %s", stdout.String())
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
		if body.DesiredConfigSha256 != "" || body.ExpectedLiveConfigSha256 != "" || body.KubernetesPayloadSha256 != "" || body.SnapshotDigest != "" || !reflect.DeepEqual(body.SupportedFieldDelta, []string{kubeadmConfigComponentControlPlane}) {
			t.Fatalf("%s request exposed operator-derived state: %#v", name, body)
		}
	}
}

func TestRunClusterApplyReconcilesWholeConfigAndAllKubernetesComponents(t *testing.T) {
	configPath := writeClusterConfig(t)
	client := &fakeKatlcAgentClient{
		nodeStatus:      &agentapi.NodeStatus{MachineId: "machine-cp-1", CurrentGenerationId: "generation-1"},
		validateResult:  &agentapi.ConfigValidationResult{Accepted: true, AcceptedApplyMode: "live"},
		submitAccepted:  &agentapi.OperationAccepted{OperationId: "operation-1", RequestDigest: strings.Repeat("e", 64)},
		operationStatus: &agentapi.OperationStatus{OperationId: "operation-1", Terminal: true, Result: operation.ResultSucceeded},
		generation: &agentapi.Generation{
			GenerationId: "cluster-config-42",
			CommitState:  "committed",
			HealthState:  "healthy",
			ConfigApply:  &agentapi.ConfigApplyStatus{SelectedKubeadmConfigName: "control-plane"},
			Sysexts:      []*agentapi.ExtensionRef{{Name: "kubernetes", PayloadVersion: "v1.36.1", Sha256: strings.Repeat("c", 64)}},
		},
	}
	previousDial := dialKatlcAgent
	previousNow := kubeadmConfigNow
	defer func() {
		dialKatlcAgent = previousDial
		kubeadmConfigNow = previousNow
	}()
	dialKatlcAgent = func(_ context.Context, endpoint string) (katlcAgentConnection, error) {
		if endpoint != "10.0.0.11:9443" {
			t.Fatalf("endpoint = %q", endpoint)
		}
		return katlcAgentConnection{Client: client, Close: func() error { return nil }}, nil
	}
	kubeadmConfigNow = func() time.Time { return time.Unix(0, 42).UTC() }

	var stdout bytes.Buffer
	if err := runClusterApply(context.Background(), kubeadmControlPlaneConfigOptions{configPath: configPath}, &stdout); err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{`"result":"succeeded"`, `"control-plane"`, `"kubelet"`} {
		if !strings.Contains(stdout.String(), required) {
			t.Fatalf("stdout = %s, missing %s", stdout.String(), required)
		}
	}
	if client.validateRequest == nil || !strings.Contains(client.validateRequest.ConfigYaml, "identity:") || !strings.Contains(client.validateRequest.ConfigYaml, "kubeadmConfigs:") {
		t.Fatalf("cluster validation did not receive the whole node config: %#v", client.validateRequest)
	}
	if len(client.submitRequests) != 5 {
		t.Fatalf("submit requests = %d, want generation apply plus dry-run and execution for both Kubernetes components: %#v", len(client.submitRequests), client.submitRequests)
	}
	components := []string{
		client.submitRequests[1].KubeadmControlPlaneConfig.SupportedFieldDelta[0],
		client.submitRequests[3].KubeadmControlPlaneConfig.SupportedFieldDelta[0],
	}
	if want := []string{kubeadmConfigComponentControlPlane, kubeadmConfigComponentKubelet}; !reflect.DeepEqual(components, want) {
		t.Fatalf("components = %#v, want %#v", components, want)
	}
}

func TestActivateClusterConfigValidatesEveryNodeBeforeMutation(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "cluster.yaml")
	source := configBundleSource() + `    - name: cp-2
      controlPlane: true
      bootstrap:
        address: 10.0.0.12
      install:
        targetDisk:
          byID: /dev/disk/by-id/ata-cp-2-root
`
	if err := os.WriteFile(configPath, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	first := &fakeKatlcAgentClient{
		nodeStatus:     &agentapi.NodeStatus{MachineId: "machine-cp-1", CurrentGenerationId: "generation-1"},
		validateResult: &agentapi.ConfigValidationResult{Accepted: true, AcceptedApplyMode: "live"},
	}
	second := &fakeKatlcAgentClient{
		nodeStatus:     &agentapi.NodeStatus{MachineId: "machine-cp-2", CurrentGenerationId: "generation-1"},
		validateResult: &agentapi.ConfigValidationResult{Accepted: false, FailureReason: "unsupported online Kubernetes field"},
	}
	previousDial := dialKatlcAgent
	defer func() { dialKatlcAgent = previousDial }()
	dialKatlcAgent = func(_ context.Context, endpoint string) (katlcAgentConnection, error) {
		clients := map[string]*fakeKatlcAgentClient{"10.0.0.11:9443": first, "10.0.0.12:9443": second}
		return katlcAgentConnection{Client: clients[endpoint], Close: func() error { return nil }}, nil
	}

	_, err := activateClusterConfig(context.Background(), kubeadmControlPlaneConfigOptions{configPath: configPath, rolloutID: "rollout-1"}, []inventory.Node{
		{Name: "cp-1", Address: "10.0.0.11", SystemRole: inventory.RoleControlPlane, KubeadmConfig: inventory.KubeadmConfig{Ref: "control-plane"}},
		{Name: "cp-2", Address: "10.0.0.12", SystemRole: inventory.RoleControlPlane, KubeadmConfig: inventory.KubeadmConfig{Ref: "control-plane"}},
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported online Kubernetes field") {
		t.Fatalf("activateClusterConfig() error = %v", err)
	}
	if len(first.submitRequests) != 0 || len(second.submitRequests) != 0 {
		t.Fatalf("mutation started before cluster-wide validation completed: first=%#v second=%#v", first.submitRequests, second.submitRequests)
	}
}

func TestActivateClusterConfigDiscoversKubeProxyPhase(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "cluster.yaml")
	source := strings.Replace(configBundleSource(), "    version: v1.36.1\n", "    version: v1.36.1\n    kubeadm:\n      configFile: ./kubeadm.yaml\n", 1)
	if err := os.WriteFile(configPath, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	kubeadm := "apiVersion: kubeadm.k8s.io/v1beta4\nkind: InitConfiguration\n---\napiVersion: kubeproxy.config.k8s.io/v1alpha1\nkind: KubeProxyConfiguration\nmode: nftables\n"
	if err := os.WriteFile(filepath.Join(root, "kubeadm.yaml"), []byte(kubeadm), 0o600); err != nil {
		t.Fatal(err)
	}
	client := &fakeKatlcAgentClient{
		nodeStatus:     &agentapi.NodeStatus{MachineId: "machine-cp-1", CurrentGenerationId: "generation-1"},
		validateResult: &agentapi.ConfigValidationResult{Accepted: true, AcceptedApplyMode: "live", NoChanges: true},
	}
	previousDial := dialKatlcAgent
	defer func() { dialKatlcAgent = previousDial }()
	dialKatlcAgent = func(context.Context, string) (katlcAgentConnection, error) {
		return katlcAgentConnection{Client: client, Close: func() error { return nil }}, nil
	}
	inv, err := kubeadmConfigInventory(kubeadmControlPlaneConfigOptions{configPath: configPath})
	if err != nil {
		t.Fatal(err)
	}
	activated, err := activateClusterConfig(context.Background(), kubeadmControlPlaneConfigOptions{configPath: configPath, rolloutID: "rollout-1"}, inv.Nodes)
	if err != nil {
		t.Fatal(err)
	}
	if !activated.components["kube-proxy"] {
		t.Fatalf("components = %#v, missing kube-proxy", activated.components)
	}
}

func TestOrderKubeletNodesChangesCoordinatorFirst(t *testing.T) {
	nodes := []inventory.Node{{Name: "worker-1", SystemRole: inventory.RoleWorker}, {Name: "cp-2", SystemRole: inventory.RoleControlPlane}, {Name: "cp-1", SystemRole: inventory.RoleControlPlane}}
	ordered, err := orderKubeletNodes(nodes, nodes[1:], "cp-2")
	if err != nil {
		t.Fatal(err)
	}
	got := []string{ordered[0].Name, ordered[1].Name, ordered[2].Name}
	if want := []string{"cp-2", "cp-1", "worker-1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestActivateClusterConfigUsesOneLiveWholeNodeGeneration(t *testing.T) {
	configPath := writeClusterConfig(t)
	client := &fakeKatlcAgentClient{
		nodeStatus:      &agentapi.NodeStatus{MachineId: "machine-cp-1", CurrentGenerationId: "generation-1"},
		validateResult:  &agentapi.ConfigValidationResult{Accepted: true, AcceptedApplyMode: "live"},
		submitAccepted:  &agentapi.OperationAccepted{OperationId: "stage-cp-1", RequestDigest: strings.Repeat("e", 64)},
		operationStatus: &agentapi.OperationStatus{OperationId: "stage-cp-1", Terminal: true, Result: operation.ResultSucceeded},
	}
	previousDial := dialKatlcAgent
	previousNow := kubeadmConfigNow
	defer func() {
		dialKatlcAgent = previousDial
		kubeadmConfigNow = previousNow
	}()
	dialKatlcAgent = func(_ context.Context, endpoint string) (katlcAgentConnection, error) {
		if endpoint != "10.0.0.11:9443" {
			t.Fatalf("endpoint = %q", endpoint)
		}
		return katlcAgentConnection{Client: client, Close: func() error { return nil }}, nil
	}
	kubeadmConfigNow = func() time.Time { return time.Unix(0, 42).UTC() }
	inv, err := kubeadmConfigInventory(kubeadmControlPlaneConfigOptions{configPath: configPath})
	if err != nil {
		t.Fatal(err)
	}
	activated, err := activateClusterConfig(context.Background(), kubeadmControlPlaneConfigOptions{configPath: configPath, rolloutID: "rollout-1"}, inv.Nodes)
	if err != nil {
		t.Fatal(err)
	}
	if activated.generations["cp-1"] != "cluster-config-42" {
		t.Fatalf("generations = %#v", activated.generations)
	}
	if client.validateRequest == nil || client.validateRequest.ApplyMode != "auto" || client.validateRequest.CandidateGenerationId != "cluster-config-42" {
		t.Fatalf("validate request = %#v", client.validateRequest)
	}
	for _, required := range []string{"identity:", "networkd:", "controlPlaneEndpoint:", "kubeadmConfigs:"} {
		if !strings.Contains(client.validateRequest.ConfigYaml, required) {
			t.Fatalf("whole cluster config is missing %q:\n%s", required, client.validateRequest.ConfigYaml)
		}
	}
	if client.submitRequest == nil || client.submitRequest.OperationKind != "generation-apply" || client.submitRequest.ConfigApply == nil {
		t.Fatalf("submit request = %#v", client.submitRequest)
	}
}

func TestActivateClusterConfigKeepsCurrentGenerationAfterLateNoop(t *testing.T) {
	configPath := writeClusterConfig(t)
	client := &fakeKatlcAgentClient{
		nodeStatus:      &agentapi.NodeStatus{MachineId: "machine-cp-1", CurrentGenerationId: "generation-current"},
		validateResult:  &agentapi.ConfigValidationResult{Accepted: true, AcceptedApplyMode: "live"},
		submitAccepted:  &agentapi.OperationAccepted{OperationId: "stage-cp-1", RequestDigest: strings.Repeat("e", 64)},
		operationStatus: &agentapi.OperationStatus{OperationId: "stage-cp-1", Phase: "desired-state-current", Terminal: true, Result: operation.ResultSucceeded, GenerationCommitState: operation.GenerationCommitAbandoned},
	}
	previousDial := dialKatlcAgent
	previousNow := kubeadmConfigNow
	defer func() {
		dialKatlcAgent = previousDial
		kubeadmConfigNow = previousNow
	}()
	dialKatlcAgent = func(context.Context, string) (katlcAgentConnection, error) {
		return katlcAgentConnection{Client: client, Close: func() error { return nil }}, nil
	}
	kubeadmConfigNow = func() time.Time { return time.Unix(0, 42).UTC() }
	inv, err := kubeadmConfigInventory(kubeadmControlPlaneConfigOptions{configPath: configPath})
	if err != nil {
		t.Fatal(err)
	}
	activated, err := activateClusterConfig(context.Background(), kubeadmControlPlaneConfigOptions{configPath: configPath, rolloutID: "rollout-1"}, inv.Nodes)
	if err != nil {
		t.Fatal(err)
	}
	if got := activated.generations["cp-1"]; got != "generation-current" {
		t.Fatalf("activated generation = %q, want current generation", got)
	}
}

func TestOrderControlPlanesRejectsUnknownCoordinator(t *testing.T) {
	_, err := orderControlPlanes([]inventory.Node{{Name: "cp-1"}, {Name: "cp-2"}, {Name: "cp-3"}}, "cp-4")
	if err == nil {
		t.Fatal("orderControlPlanes() error = nil")
	}
}
