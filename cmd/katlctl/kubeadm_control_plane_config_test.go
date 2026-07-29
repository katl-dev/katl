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

	"github.com/katl-dev/katl/internal/bootstrap/cluster"
	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/installer/configapply"
	"github.com/katl-dev/katl/internal/installer/generation"
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
	for index, name := range []string{"cp-1", "cp-2", "cp-3"} {
		payloadDigest := strings.Repeat(string(rune('a'+index)), 64)
		clients[name] = &fakeKatlcAgentClient{nodeStatus: &agentapi.NodeStatus{MachineId: "machine-" + name}, generation: &agentapi.Generation{GenerationId: "gen-2", CommitState: "committed", HealthState: "healthy", ConfigApply: &agentapi.ConfigApplyStatus{KubeadmActionRequired: true, SelectedKubeadmConfigName: "control-plane"}, Sysexts: []*agentapi.ExtensionRef{{Name: "kubernetes", PayloadVersion: "v1.36.1", Sha256: payloadDigest}}}, submitAccepted: &agentapi.OperationAccepted{OperationId: "op-" + name, RequestDigest: strings.Repeat("f", 64)}, operationStatus: &agentapi.OperationStatus{Terminal: true, Result: operation.ResultSucceeded}}
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
	var stdout, stderr bytes.Buffer
	client := &fakeKatlcAgentClient{
		nodeStatus: &agentapi.NodeStatus{MachineId: "machine-cp-1", CurrentGenerationId: "generation-1"},
		validateResult: &agentapi.ConfigValidationResult{
			Accepted:          true,
			AcceptedApplyMode: "live",
			ChangedDomains:    []string{configapply.DomainKubeadmConfig},
		},
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
	client.onSubmit = func(request *agentapi.SubmitOperationRequest) {
		if request.DryRun {
			return
		}
		var required string
		switch request.OperationKind {
		case "generation-apply":
			required = "phase=node-config node=cp-1 status=started"
		case "kubeadm-control-plane-config":
			component := strings.TrimPrefix(request.KubeadmControlPlaneConfig.SupportedFieldDelta[0], "component/")
			required = "component=" + component + " node=cp-1 phase=apply status=started"
		}
		if required != "" && !strings.Contains(stderr.String(), required) {
			t.Fatalf("progress before %s submission = %q, missing %q", request.OperationKind, stderr.String(), required)
		}
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

	if err := runClusterApply(context.Background(), kubeadmControlPlaneConfigOptions{configPath: configPath}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{`"result":"succeeded"`, `"control-plane"`, `"kubelet"`} {
		if !strings.Contains(stdout.String(), required) {
			t.Fatalf("stdout = %s, missing %s", stdout.String(), required)
		}
	}
	for _, required := range []string{
		"phase=configuration status=started nodes=1",
		"phase=config-validation node=cp-1 status=succeeded",
		"phase=node-config node=cp-1 step=pending status=running",
		"phase=node-config node=cp-1 status=succeeded",
		"component=control-plane phase=preflight status=succeeded nodes=1",
		"component=control-plane node=cp-1 phase=pending status=running",
		"component=kubelet node=cp-1 phase=complete status=succeeded",
	} {
		if !strings.Contains(stderr.String(), required) {
			t.Fatalf("stderr = %s, missing %s", stderr.String(), required)
		}
	}
	if strings.Contains(stdout.String(), "cluster apply ") {
		t.Fatalf("structured stdout contains progress output: %s", stdout.String())
	}
	if strings.Contains(stderr.String(), "operation kind=") || strings.Contains(stderr.String(), "operation-id") {
		t.Fatalf("progress exposed internal operation bookkeeping: %s", stderr.String())
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

func TestRunClusterApplySkipsKubernetesComponentsBeforeBootstrap(t *testing.T) {
	configPath := writeClusterConfig(t)
	var stdout, stderr bytes.Buffer
	client := &fakeKatlcAgentClient{
		nodeStatus: &agentapi.NodeStatus{
			MachineId:           "machine-cp-1",
			CurrentGenerationId: "generation-0",
			Kubernetes:          &agentapi.KubernetesStatus{State: "not-configured"},
		},
		validateResult: &agentapi.ConfigValidationResult{Accepted: true, AcceptedApplyMode: "live", NoChanges: true},
	}
	previousDial := dialKatlcAgent
	defer func() { dialKatlcAgent = previousDial }()
	dialKatlcAgent = func(_ context.Context, endpoint string) (katlcAgentConnection, error) {
		if endpoint != "10.0.0.11:9443" {
			t.Fatalf("endpoint = %q", endpoint)
		}
		return katlcAgentConnection{Client: client, Close: func() error { return nil }}, nil
	}

	if err := runClusterApply(context.Background(), kubeadmControlPlaneConfigOptions{configPath: configPath}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		`"result":"succeeded"`,
		`"control-plane":{"component":"control-plane","reason":"kubernetes-not-configured","result":"skipped"}`,
		`"kubelet":{"component":"kubelet","reason":"kubernetes-not-configured","result":"skipped"}`,
	} {
		if !strings.Contains(stdout.String(), required) {
			t.Fatalf("stdout = %s, missing %s", stdout.String(), required)
		}
	}
	for _, component := range []string{"control-plane", "kubelet"} {
		required := "component=" + component + " status=skipped reason=kubernetes-not-configured"
		if !strings.Contains(stderr.String(), required) {
			t.Fatalf("stderr = %s, missing %s", stderr.String(), required)
		}
	}
	if strings.Contains(stderr.String(), "component=control-plane status=started") ||
		strings.Contains(stderr.String(), "component=kubelet status=started") {
		t.Fatalf("pre-bootstrap apply started Kubernetes reconciliation: %s", stderr.String())
	}
	if len(client.submitRequests) != 0 {
		t.Fatalf("pre-bootstrap no-op submitted mutations: %#v", client.submitRequests)
	}
}

func TestRunClusterApplyStagesPostBootstrapHostOnlyChangeAndRepeatsWithoutKubeadm(t *testing.T) {
	configPath := writeClusterConfig(t)
	client := &fakeKatlcAgentClient{
		nodeStatus: &agentapi.NodeStatus{
			MachineId:           "machine-cp-1",
			CurrentGenerationId: "generation-1",
			Kubernetes:          &agentapi.KubernetesStatus{State: "ready"},
		},
		validateResult: &agentapi.ConfigValidationResult{
			Accepted:          true,
			AcceptedApplyMode: generation.ApplyModeNextBoot,
			ChangedDomains:    []string{configapply.DomainHostConfiguration},
		},
		submitAccepted: &agentapi.OperationAccepted{OperationId: "stage-cp-1", RequestDigest: strings.Repeat("e", 64)},
		operationStatus: &agentapi.OperationStatus{
			OperationId:           "stage-cp-1",
			CandidateGenerationId: "cluster-config-42",
			Terminal:              true,
			Result:                operation.ResultSucceeded,
		},
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

	var stdout, stderr bytes.Buffer
	if err := runClusterApply(context.Background(), kubeadmControlPlaneConfigOptions{configPath: configPath}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{`"rebootRequired":true`, `"stagedNodes":["cp-1"]`, `"kubernetes":{}`} {
		if !strings.Contains(stdout.String(), required) {
			t.Fatalf("staged stdout = %s, missing %s", stdout.String(), required)
		}
	}
	if len(client.submitRequests) != 1 || client.submitRequests[0].OperationKind != "generation-stage" {
		t.Fatalf("staged submit requests = %#v, want one generation-stage", client.submitRequests)
	}
	if client.generationRequest != nil || strings.Contains(stderr.String(), "component=control-plane status=started") || strings.Contains(stderr.String(), "component=kubelet status=started") {
		t.Fatalf("host-only stage reconciled Kubernetes: generation=%#v progress=%s", client.generationRequest, stderr.String())
	}

	client.nodeStatus.CurrentGenerationId = "cluster-config-42"
	client.validateResult = &agentapi.ConfigValidationResult{Accepted: true, AcceptedApplyMode: generation.ApplyModeLive, NoChanges: true}
	stdout.Reset()
	stderr.Reset()
	if err := runClusterApply(context.Background(), kubeadmControlPlaneConfigOptions{configPath: configPath}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), `"kubernetes":{}`) || strings.Contains(stdout.String(), `"rebootRequired"`) {
		t.Fatalf("repeat stdout = %s", stdout.String())
	}
	if len(client.submitRequests) != 1 || client.generationRequest != nil {
		t.Fatalf("repeat apply mutated state: submits=%#v generation=%#v", client.submitRequests, client.generationRequest)
	}
}

func TestActivateClusterConfigPlansOneFreshReplacementNode(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "cluster.yaml")
	source := configBundleSource() + `    - name: cp-2
      controlPlane: true
      management:
        address: 10.0.0.12
      install:
        systemDisk:
          byID: /dev/disk/by-id/ata-cp-2-root
    - name: cp-3
      controlPlane: true
      management:
        address: 10.0.0.13
      install:
        systemDisk:
          byID: /dev/disk/by-id/ata-cp-3-root
`
	if err := os.WriteFile(configPath, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	first := &fakeKatlcAgentClient{
		nodeStatus: &agentapi.NodeStatus{
			MachineId:           "machine-cp-1",
			CurrentGenerationId: "generation-0",
			Kubernetes:          &agentapi.KubernetesStatus{State: "not-configured"},
		},
		validateResult: &agentapi.ConfigValidationResult{Accepted: true, AcceptedApplyMode: "live", NoChanges: true},
	}
	second := &fakeKatlcAgentClient{
		nodeStatus: &agentapi.NodeStatus{
			MachineId:           "machine-cp-2",
			CurrentGenerationId: "generation-1",
			Kubernetes:          &agentapi.KubernetesStatus{State: "ready"},
		},
		validateResult: &agentapi.ConfigValidationResult{Accepted: true, AcceptedApplyMode: "live", NoChanges: true},
	}
	third := &fakeKatlcAgentClient{
		nodeStatus: &agentapi.NodeStatus{
			MachineId:           "machine-cp-3",
			CurrentGenerationId: "generation-1",
			Kubernetes:          &agentapi.KubernetesStatus{State: "ready"},
		},
		validateResult: &agentapi.ConfigValidationResult{Accepted: true, AcceptedApplyMode: "live", NoChanges: true},
	}
	previousDial := dialKatlcAgent
	defer func() { dialKatlcAgent = previousDial }()
	dialKatlcAgent = func(_ context.Context, endpoint string) (katlcAgentConnection, error) {
		clients := map[string]*fakeKatlcAgentClient{"10.0.0.11:9443": first, "10.0.0.12:9443": second, "10.0.0.13:9443": third}
		return katlcAgentConnection{Client: clients[endpoint], Close: func() error { return nil }}, nil
	}

	activated, err := activateClusterConfig(context.Background(), kubeadmControlPlaneConfigOptions{configPath: configPath, rolloutID: "rollout-1", coordinator: "cp-3"}, []inventory.Node{
		{Name: "cp-1", Address: "10.0.0.11", SystemRole: inventory.RoleControlPlane, KubeadmConfig: inventory.KubeadmConfig{Ref: "control-plane"}},
		{Name: "cp-2", Address: "10.0.0.12", SystemRole: inventory.RoleControlPlane, KubeadmConfig: inventory.KubeadmConfig{Ref: "control-plane"}},
		{Name: "cp-3", Address: "10.0.0.13", SystemRole: inventory.RoleControlPlane, KubeadmConfig: inventory.KubeadmConfig{Ref: "control-plane"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(activated.joinNodes, []string{"cp-1"}) || activated.joinCoordinator != "cp-3" {
		t.Fatalf("replacement plan = nodes %#v coordinator %q", activated.joinNodes, activated.joinCoordinator)
	}
	if len(first.submitRequests) != 0 || len(second.submitRequests) != 0 || len(third.submitRequests) != 0 {
		t.Fatalf("no-op config unexpectedly mutated nodes: first=%#v second=%#v third=%#v", first.submitRequests, second.submitRequests, third.submitRequests)
	}
}

func TestRunClusterApplyRefreshesReplacementGenerationAfterJoin(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "cluster.yaml")
	source := configBundleSource() + `    - name: cp-2
      controlPlane: true
      management:
        address: 10.0.0.12
      install:
        systemDisk:
          byID: /dev/disk/by-id/ata-cp-2-root
`
	if err := os.WriteFile(configPath, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	generation := func(id string) *agentapi.Generation {
		return &agentapi.Generation{
			GenerationId: id,
			CommitState:  "committed",
			HealthState:  "healthy",
			ConfigApply:  &agentapi.ConfigApplyStatus{SelectedKubeadmConfigName: "control-plane"},
			Sysexts:      []*agentapi.ExtensionRef{{Name: "kubernetes", PayloadVersion: "v1.36.1", Sha256: strings.Repeat("c", 64)}},
		}
	}
	cp1 := &fakeKatlcAgentClient{
		nodeStatus:      &agentapi.NodeStatus{MachineId: "machine-cp-1", CurrentGenerationId: "generation-cp-1", Kubernetes: &agentapi.KubernetesStatus{State: "ready"}},
		validateResult:  &agentapi.ConfigValidationResult{Accepted: true, AcceptedApplyMode: "live", NoChanges: true},
		generation:      generation("generation-cp-1"),
		submitAccepted:  &agentapi.OperationAccepted{OperationId: "operation-cp-1", RequestDigest: strings.Repeat("e", 64)},
		operationStatus: &agentapi.OperationStatus{Terminal: true, Result: operation.ResultSucceeded},
	}
	cp2StatusCalls := 0
	cp2 := &fakeKatlcAgentClient{
		nodeStatus:      &agentapi.NodeStatus{MachineId: "machine-cp-2", CurrentGenerationId: "generation-0", Kubernetes: &agentapi.KubernetesStatus{State: "not-configured"}},
		validateResult:  &agentapi.ConfigValidationResult{Accepted: true, AcceptedApplyMode: "live", NoChanges: true},
		generation:      generation("generation-joined"),
		submitAccepted:  &agentapi.OperationAccepted{OperationId: "operation-cp-2", RequestDigest: strings.Repeat("e", 64)},
		operationStatus: &agentapi.OperationStatus{Terminal: true, Result: operation.ResultSucceeded},
	}
	cp2.onGetNodeStatus = func() {
		cp2StatusCalls++
		if cp2StatusCalls == 2 {
			cp2.nodeStatus.CurrentGenerationId = "generation-joined"
			cp2.nodeStatus.Kubernetes.State = "ready"
		}
	}
	clients := map[string]*fakeKatlcAgentClient{"10.0.0.11:9443": cp1, "10.0.0.12:9443": cp2}
	previousDial := dialKatlcAgent
	previousJoin := runAgentNodeJoin
	defer func() {
		dialKatlcAgent = previousDial
		runAgentNodeJoin = previousJoin
	}()
	dialKatlcAgent = func(_ context.Context, endpoint string) (katlcAgentConnection, error) {
		client := clients[endpoint]
		if client == nil {
			return katlcAgentConnection{}, os.ErrNotExist
		}
		return katlcAgentConnection{Client: client, Close: func() error { return nil }}, nil
	}
	runAgentNodeJoin = func(_ context.Context, _ cluster.Request, nodeName string, _ cluster.AgentBootstrapDependencies) (cluster.Result, error) {
		if nodeName != "cp-2" {
			t.Fatalf("join node = %q, want cp-2", nodeName)
		}
		return cluster.Result{}, nil
	}

	var stdout, stderr bytes.Buffer
	if err := runClusterApply(context.Background(), kubeadmControlPlaneConfigOptions{configPath: configPath}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), `"joined":["cp-2"]`) {
		t.Fatalf("stdout = %s, missing replacement join", stdout.String())
	}
	if cp2.generationRequest == nil || cp2.generationRequest.GenerationId != "generation-joined" {
		t.Fatalf("replacement generation request = %#v, want generation-joined", cp2.generationRequest)
	}
}

func TestActivateClusterConfigValidatesEveryNodeBeforeMutation(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "cluster.yaml")
	source := configBundleSource() + `    - name: cp-2
      controlPlane: true
      management:
        address: 10.0.0.12
      install:
        systemDisk:
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
		nodeStatus: &agentapi.NodeStatus{MachineId: "machine-cp-1", CurrentGenerationId: "generation-1"},
		validateResult: &agentapi.ConfigValidationResult{
			Accepted:          true,
			AcceptedApplyMode: "live",
			ChangedDomains:    []string{configapply.DomainKubeadmConfig},
		},
		submitAccepted:  &agentapi.OperationAccepted{OperationId: "stage-cp-1", RequestDigest: strings.Repeat("e", 64)},
		operationStatus: &agentapi.OperationStatus{OperationId: "stage-cp-1", Terminal: true, Result: operation.ResultSucceeded},
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
	for _, required := range []string{"identity:", "controlPlaneEndpoint:", "kubeadmConfigs:"} {
		if !strings.Contains(client.validateRequest.ConfigYaml, required) {
			t.Fatalf("whole cluster config is missing %q:\n%s", required, client.validateRequest.ConfigYaml)
		}
	}
	if strings.Contains(client.validateRequest.ConfigYaml, "networkd:") {
		t.Fatalf("whole cluster config contains removed networkd API:\n%s", client.validateRequest.ConfigYaml)
	}
	if client.submitRequest == nil || client.submitRequest.OperationKind != "generation-apply" || client.submitRequest.ConfigApply == nil {
		t.Fatalf("submit request = %#v", client.submitRequest)
	}
}

func TestActivateClusterConfigStagesNextBootHostConfiguration(t *testing.T) {
	configPath := writeClusterConfig(t)
	client := &fakeKatlcAgentClient{
		nodeStatus: &agentapi.NodeStatus{MachineId: "machine-cp-1", CurrentGenerationId: "generation-1"},
		validateResult: &agentapi.ConfigValidationResult{
			Accepted:          true,
			AcceptedApplyMode: generation.ApplyModeNextBoot,
			ChangedDomains:    []string{configapply.DomainHostConfiguration},
		},
		submitAccepted:  &agentapi.OperationAccepted{OperationId: "stage-cp-1", RequestDigest: strings.Repeat("e", 64)},
		operationStatus: &agentapi.OperationStatus{OperationId: "stage-cp-1", CandidateGenerationId: "cluster-config-42", Terminal: true, Result: operation.ResultSucceeded},
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
	if !reflect.DeepEqual(activated.stagedNodes, []string{"cp-1"}) {
		t.Fatalf("staged nodes = %#v", activated.stagedNodes)
	}
	if client.submitRequest == nil || client.submitRequest.OperationKind != "generation-stage" {
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
