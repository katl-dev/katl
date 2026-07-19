package cluster

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/bootstrap/kubeconfig"
	"github.com/katl-dev/katl/internal/bootstrap/readiness"
)

const (
	testCA            = "Y2EtZGF0YQ=="
	testCert          = "Y2VydC1kYXRh"
	testKey           = "a2V5LWRhdGE="
	testDiscoveryHash = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

func TestRunBootstrapsInitWorkerAndKubeconfig(t *testing.T) {
	out := filepath.Join(t.TempDir(), "operator.conf")
	nodeRunner := &fakeNodeRunner{
		credentials: AdminCredentials{
			CertificateAuthorityData: testCA,
			ClientCertificateData:    testCert,
			ClientKeyData:            testKey,
		},
		join: JoinMaterial{Argv: []string{"kubeadm", "join", "api.katl.test:6443", "--token", "abcdef.0123456789abcdef", "--discovery-token-ca-cert-hash", testDiscoveryHash}},
	}
	result, err := Run(context.Background(), Request{
		Inventory:           validInventory(),
		InitNode:            "cp-1",
		AddressOverrides:    map[string]string{"worker-1": "10.0.0.22"},
		KubeconfigOut:       out,
		OverwriteKubeconfig: true,
	}, Dependencies{
		ReadinessChecker: readyChecker{},
		NodeRunner:       nodeRunner,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Plan.InitNode != "cp-1" {
		t.Fatalf("init node = %q", result.Plan.InitNode)
	}
	if len(result.Plan.AddressOverrides) != 1 || result.Plan.AddressOverrides[0].Address != "10.0.0.22" {
		t.Fatalf("address overrides = %#v", result.Plan.AddressOverrides)
	}
	wantPhases := []string{"plan", "readiness", "kubeadm-init", "api-ready", "join-material", "worker-join", "worker-ready", "api-ready-after-join", "kubeconfig"}
	if got := phaseNames(result.Phases); !reflect.DeepEqual(got, wantPhases) {
		t.Fatalf("phases = %#v, want %#v", got, wantPhases)
	}
	if got, want := nodeRunner.calls, []string{
		"init:cp-1",
		"ready:cp-1",
		"join-material:cp-1",
		"join-worker:worker-1",
		"ready-worker:worker-1",
		"ready:cp-1",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %#v, want %#v", got, want)
	}
	if result.Kubeconfig.Path != out || result.Kubeconfig.Server != "https://api.katl.test:6443" {
		t.Fatalf("kubeconfig result = %#v", result.Kubeconfig)
	}
	if result.NextStep != "kubectl --kubeconfig "+out+" get nodes" {
		t.Fatalf("next step = %q", result.NextStep)
	}
	info, err := os.Stat(out)
	if err != nil {
		t.Fatalf("stat kubeconfig: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("kubeconfig mode = %o, want 0600", got)
	}
}

func TestRunVerifiesManagedEndpointBeforeCreatingJoinMaterial(t *testing.T) {
	inv := validInventory()
	inv.ControlPlaneEndpointManaged = true
	var events []string
	nodeRunner := &fakeNodeRunner{
		events: &events,
		credentials: AdminCredentials{
			CertificateAuthorityData: testCA,
			ClientCertificateData:    testCert,
			ClientKeyData:            testKey,
		},
	}
	bootstrapRunner := &fakeBootstrapRunner{
		events: &events,
		result: BootstrapResult{StableEndpointReady: true},
	}
	result, err := Run(context.Background(), Request{
		Inventory:           inv,
		KubeconfigOut:       filepath.Join(t.TempDir(), "operator.conf"),
		OverwriteKubeconfig: true,
	}, Dependencies{ReadinessChecker: readyChecker{}, NodeRunner: nodeRunner, BootstrapRunner: bootstrapRunner})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := events[:3], []string{"init:cp-1", "ready:cp-1", "bootstrap"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events before joins = %#v, want %#v", got, want)
	}
	if got := phaseNames(result.Phases); !containsString(got, "stable-endpoint") {
		t.Fatalf("phases = %#v, want stable endpoint proof", got)
	}
	if len(bootstrapRunner.requests) != 1 {
		t.Fatalf("endpoint checks = %d, want 1", len(bootstrapRunner.requests))
	}
	request := bootstrapRunner.requests[0]
	if request.Server != "10.0.0.11:6443" || request.StableEndpoint != "api.katl.test:6443" {
		t.Fatalf("endpoint check request = %#v", request)
	}
	if len(request.PreWaits) != 1 || request.PreWaits[0].Kind != BootstrapWaitStableEndpoint {
		t.Fatalf("endpoint pre-waits = %#v", request.PreWaits)
	}
}

func TestRunAppliesUserBootstrapAfterAPIReadinessAndUsesStableEndpoint(t *testing.T) {
	out := filepath.Join(t.TempDir(), "operator.conf")
	manifestOne := writeBootstrapManifest(t, "01-cni.yaml", "cni")
	manifestTwo := writeBootstrapManifest(t, "02-flux.yaml", "flux")
	inv := validSingleNodeInventory()
	inv.Bootstrap = &inventory.Bootstrap{
		Manifests: []inventory.BootstrapManifest{
			{Path: manifestOne},
			{Path: manifestTwo},
		},
		Waits: []inventory.BootstrapWait{{
			Kind:      BootstrapWaitCondition,
			Namespace: "kube-system",
			Name:      "deployment/cilium-operator",
			Condition: "Available",
		}, {
			Kind:      BootstrapWaitPodsReady,
			Namespace: "kube-system",
			Selector:  "k8s-app=kube-dns",
		}},
		StableEndpoint: "api.stable.test:6443",
	}
	var events []string
	nodeRunner := &fakeNodeRunner{
		events: &events,
		credentials: AdminCredentials{
			CertificateAuthorityData: testCA,
			ClientCertificateData:    testCert,
			ClientKeyData:            testKey,
		},
	}
	bootstrapRunner := &fakeBootstrapRunner{
		events: &events,
		result: BootstrapResult{StableEndpointReady: true},
	}
	result, err := Run(context.Background(), Request{
		Inventory:           inv,
		KubeconfigOut:       out,
		OverwriteKubeconfig: true,
	}, Dependencies{
		ReadinessChecker: readyChecker{},
		NodeRunner:       nodeRunner,
		BootstrapRunner:  bootstrapRunner,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	wantPhases := []string{"plan", "readiness", "kubeadm-init", "api-ready", "api-ready-after-join", "user-bootstrap", "kubeconfig"}
	if got := phaseNames(result.Phases); !reflect.DeepEqual(got, wantPhases) {
		t.Fatalf("phases = %#v, want %#v", got, wantPhases)
	}
	if got, want := events, []string{"init:cp-1", "ready:cp-1", "ready:cp-1", "bootstrap"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %#v, want %#v", got, want)
	}
	if len(bootstrapRunner.requests) != 1 {
		t.Fatalf("bootstrap calls = %d, want 1", len(bootstrapRunner.requests))
	}
	request := bootstrapRunner.requests[0]
	if request.Server != "10.0.0.11:6443" {
		t.Fatalf("bootstrap server = %q", request.Server)
	}
	if result.Plan.Bootstrap.Manifests[0].Path != manifestOne {
		t.Fatalf("plan bootstrap = %#v", result.Plan.Bootstrap)
	}
	if got, want := manifestPaths(request.Manifests), []string{manifestOne, manifestTwo}; !reflect.DeepEqual(got, want) {
		t.Fatalf("manifest order = %#v, want %#v", got, want)
	}
	wantWaits := []BootstrapWait{
		{Kind: BootstrapWaitCondition, Namespace: "kube-system", Name: "deployment/cilium-operator", Condition: "Available"},
		{Kind: BootstrapWaitPodsReady, Namespace: "kube-system", Selector: "k8s-app=kube-dns"},
		{Kind: BootstrapWaitStableEndpoint, Name: "api.stable.test:6443"},
	}
	if !reflect.DeepEqual(request.Waits, wantWaits) {
		t.Fatalf("waits = %#v, want %#v", request.Waits, wantWaits)
	}
	if result.Kubeconfig.Server != "https://api.stable.test:6443" {
		t.Fatalf("kubeconfig server = %q, want stable endpoint", result.Kubeconfig.Server)
	}
}

func TestRunAppliesStableEndpointWaitBeforeUserBootstrapManifests(t *testing.T) {
	out := filepath.Join(t.TempDir(), "operator.conf")
	bootstrapRunner := &fakeBootstrapRunner{result: BootstrapResult{StableEndpointReady: true}}
	result, err := Run(context.Background(), Request{
		Inventory:           validSingleNodeInventory(),
		KubeconfigOut:       out,
		OverwriteKubeconfig: true,
		Bootstrap: UserBootstrap{
			StableEndpoint:                "api.stable.test:6443",
			StableEndpointBeforeManifests: true,
			Manifests: []BootstrapManifest{{
				Path:    "cni.yaml",
				Content: []byte(validBootstrapManifest("cni")),
			}},
		},
	}, Dependencies{
		ReadinessChecker: readyChecker{},
		NodeRunner: &fakeNodeRunner{credentials: AdminCredentials{
			CertificateAuthorityData: testCA,
			ClientCertificateData:    testCert,
			ClientKeyData:            testKey,
		}},
		BootstrapRunner: bootstrapRunner,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Kubeconfig.Server != "https://api.stable.test:6443" {
		t.Fatalf("kubeconfig server = %q, want stable endpoint", result.Kubeconfig.Server)
	}
	if len(bootstrapRunner.requests) != 1 {
		t.Fatalf("bootstrap calls = %d, want 1", len(bootstrapRunner.requests))
	}
	request := bootstrapRunner.requests[0]
	if len(request.PreWaits) != 1 || request.PreWaits[0].Kind != BootstrapWaitStableEndpoint || request.PreWaits[0].Name != "api.stable.test:6443" {
		t.Fatalf("pre waits = %#v, want stable endpoint", request.PreWaits)
	}
	if len(request.Waits) != 0 {
		t.Fatalf("post waits = %#v, want no duplicate stable endpoint wait", request.Waits)
	}
}

func TestRunAppliesExplicitPreWaitsBeforeUserBootstrapManifests(t *testing.T) {
	bootstrapRunner := &fakeBootstrapRunner{}
	_, err := Run(context.Background(), Request{
		Inventory:           validSingleNodeInventory(),
		KubeconfigOut:       filepath.Join(t.TempDir(), "operator.conf"),
		OverwriteKubeconfig: true,
		Bootstrap: UserBootstrap{
			PreWaits: []BootstrapWait{{Kind: BootstrapWaitNodesReady}},
			Manifests: []BootstrapManifest{{
				Path:    "workload.yaml",
				Content: []byte(validBootstrapManifest("workload")),
			}},
		},
	}, Dependencies{
		ReadinessChecker: readyChecker{},
		NodeRunner: &fakeNodeRunner{credentials: AdminCredentials{
			CertificateAuthorityData: testCA,
			ClientCertificateData:    testCert,
			ClientKeyData:            testKey,
		}},
		BootstrapRunner: bootstrapRunner,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(bootstrapRunner.requests) != 1 {
		t.Fatalf("bootstrap calls = %d, want 1", len(bootstrapRunner.requests))
	}
	request := bootstrapRunner.requests[0]
	if len(request.PreWaits) != 1 || request.PreWaits[0].Kind != BootstrapWaitNodesReady {
		t.Fatalf("pre waits = %#v, want nodes-ready", request.PreWaits)
	}
	if len(request.Waits) != 0 {
		t.Fatalf("post waits = %#v, want none", request.Waits)
	}
}

func TestRunUsesInventoryStableEndpointForRequestedPreManifestWait(t *testing.T) {
	inv := validSingleNodeInventory()
	inv.Bootstrap = &inventory.Bootstrap{StableEndpoint: "api.inventory.test:6443"}
	bootstrapRunner := &fakeBootstrapRunner{result: BootstrapResult{StableEndpointReady: true}}
	_, err := Run(context.Background(), Request{
		Inventory:           inv,
		KubeconfigOut:       filepath.Join(t.TempDir(), "operator.conf"),
		OverwriteKubeconfig: true,
		Bootstrap:           UserBootstrap{StableEndpointBeforeManifests: true},
	}, Dependencies{
		ReadinessChecker: readyChecker{},
		NodeRunner: &fakeNodeRunner{credentials: AdminCredentials{
			CertificateAuthorityData: testCA,
			ClientCertificateData:    testCert,
			ClientKeyData:            testKey,
		}},
		BootstrapRunner: bootstrapRunner,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(bootstrapRunner.requests) != 1 {
		t.Fatalf("bootstrap calls = %d, want 1", len(bootstrapRunner.requests))
	}
	request := bootstrapRunner.requests[0]
	if len(request.PreWaits) != 1 || request.PreWaits[0].Name != "api.inventory.test:6443" {
		t.Fatalf("pre waits = %#v, want inventory stable endpoint", request.PreWaits)
	}
	if request.StableEndpoint != "api.inventory.test:6443" {
		t.Fatalf("stable endpoint = %q, want inventory stable endpoint", request.StableEndpoint)
	}
}

func TestRunRejectsPreManifestStableEndpointWithoutEndpoint(t *testing.T) {
	result, err := Run(context.Background(), Request{
		Inventory: validSingleNodeInventory(),
		Bootstrap: UserBootstrap{StableEndpointBeforeManifests: true},
	}, Dependencies{
		ReadinessChecker: readyChecker{},
		NodeRunner:       &fakeNodeRunner{},
		BootstrapRunner:  &fakeBootstrapRunner{},
	})
	if err == nil || !strings.Contains(err.Error(), "requires stable endpoint") {
		t.Fatalf("Run() error = %v, want stable endpoint validation", err)
	}
	if got := phaseNames(result.Phases); !reflect.DeepEqual(got, []string{"plan"}) {
		t.Fatalf("phases = %#v, want plan only", got)
	}
}

func TestRunStopsAfterPreManifestStableEndpointFailureAndRedactsSecret(t *testing.T) {
	out := filepath.Join(t.TempDir(), "operator.conf")
	secret := "abcdef.0123456789abcdef"
	bootstrapRunner := &fakeBootstrapRunner{err: errors.New("stable endpoint failed with token " + secret)}
	result, err := Run(context.Background(), Request{
		Inventory:           validSingleNodeInventory(),
		KubeconfigOut:       out,
		OverwriteKubeconfig: true,
		Bootstrap: UserBootstrap{
			StableEndpoint:                "api.stable.test:6443",
			StableEndpointBeforeManifests: true,
			Manifests: []BootstrapManifest{{
				Path:    "cni.yaml",
				Content: []byte(validBootstrapManifest("cni")),
			}},
		},
	}, Dependencies{
		ReadinessChecker: readyChecker{},
		NodeRunner: &fakeNodeRunner{credentials: AdminCredentials{
			CertificateAuthorityData: testCA,
			ClientCertificateData:    testCert,
			ClientKeyData:            testKey,
		}},
		BootstrapRunner: bootstrapRunner,
	})
	if err == nil {
		t.Fatal("Run() error = nil, want stable endpoint failure")
	}
	if strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "[REDACTED BOOTSTRAP TOKEN]") {
		t.Fatalf("error = %q, want redacted token", err.Error())
	}
	if got := phaseNames(result.Phases); containsString(got, "kubeconfig") {
		t.Fatalf("phases = %#v, kubeconfig should not run after pre wait failure", got)
	}
}

func TestRunRejectsInvalidBootstrapYAMLBeforeKubeadm(t *testing.T) {
	nodeRunner := &fakeNodeRunner{}
	bootstrapRunner := &fakeBootstrapRunner{}
	_, err := Run(context.Background(), Request{
		Inventory: validSingleNodeInventory(),
		Bootstrap: UserBootstrap{Manifests: []BootstrapManifest{{
			Path:    "bad.yaml",
			Content: []byte("apiVersion: ["),
		}}},
	}, Dependencies{
		ReadinessChecker: readyChecker{},
		NodeRunner:       nodeRunner,
		BootstrapRunner:  bootstrapRunner,
	})
	if err == nil || !strings.Contains(err.Error(), "decode YAML") {
		t.Fatalf("Run() error = %v, want YAML validation failure", err)
	}
	if len(nodeRunner.calls) != 0 || len(bootstrapRunner.requests) != 0 {
		t.Fatalf("execution happened after invalid YAML: node=%#v bootstrap=%#v", nodeRunner.calls, bootstrapRunner.requests)
	}
}

func TestRunDoesNotApplyUserBootstrapBeforeAPIReadiness(t *testing.T) {
	nodeRunner := &fakeNodeRunner{apiErr: errors.New("not ready yet")}
	bootstrapRunner := &fakeBootstrapRunner{}
	_, err := Run(context.Background(), Request{
		Inventory: validSingleNodeInventory(),
		Bootstrap: UserBootstrap{Manifests: []BootstrapManifest{{
			Path:    "cni.yaml",
			Content: []byte(validBootstrapManifest("cni")),
		}}},
	}, Dependencies{
		ReadinessChecker: readyChecker{},
		NodeRunner:       nodeRunner,
		BootstrapRunner:  bootstrapRunner,
	})
	if err == nil || !strings.Contains(err.Error(), "wait for API readiness") {
		t.Fatalf("Run() error = %v, want API readiness failure", err)
	}
	if len(bootstrapRunner.requests) != 0 {
		t.Fatalf("bootstrap runner was called before API readiness: %#v", bootstrapRunner.requests)
	}
}

func TestRunStopsAfterUserBootstrapServerFailureAndRedactsSecret(t *testing.T) {
	out := filepath.Join(t.TempDir(), "operator.conf")
	secret := "abcdef.0123456789abcdef"
	bootstrapRunner := &fakeBootstrapRunner{err: errors.New("server-side apply failed with token " + secret)}
	result, err := Run(context.Background(), Request{
		Inventory:           validSingleNodeInventory(),
		KubeconfigOut:       out,
		OverwriteKubeconfig: true,
		Bootstrap: UserBootstrap{Manifests: []BootstrapManifest{{
			Path:    "cni.yaml",
			Content: []byte(validBootstrapManifest("cni")),
		}}},
	}, Dependencies{
		ReadinessChecker: readyChecker{},
		NodeRunner: &fakeNodeRunner{credentials: AdminCredentials{
			CertificateAuthorityData: testCA,
			ClientCertificateData:    testCert,
			ClientKeyData:            testKey,
		}},
		BootstrapRunner: bootstrapRunner,
	})
	if err == nil {
		t.Fatal("Run() error = nil, want bootstrap failure")
	}
	if strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "[REDACTED BOOTSTRAP TOKEN]") {
		t.Fatalf("error = %q, want redacted token", err.Error())
	}
	if got := phaseNames(result.Phases); containsString(got, "kubeconfig") {
		t.Fatalf("phases = %#v, kubeconfig should not run after bootstrap failure", got)
	}
	if _, statErr := os.Stat(out); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("kubeconfig output stat error = %v, want not exist", statErr)
	}
}

func TestRunStopsAfterUserBootstrapWaitFailure(t *testing.T) {
	bootstrapRunner := &fakeBootstrapRunner{err: errors.New("wait condition deployment/cilium failed")}
	result, err := Run(context.Background(), Request{
		Inventory: validSingleNodeInventory(),
		Bootstrap: UserBootstrap{
			Waits: []BootstrapWait{{
				Kind:      BootstrapWaitCondition,
				Namespace: "kube-system",
				Name:      "deployment/cilium",
				Condition: "Available",
			}},
		},
	}, Dependencies{
		ReadinessChecker: readyChecker{},
		NodeRunner: &fakeNodeRunner{credentials: AdminCredentials{
			CertificateAuthorityData: testCA,
			ClientCertificateData:    testCert,
			ClientKeyData:            testKey,
		}},
		BootstrapRunner: bootstrapRunner,
	})
	if err == nil || !strings.Contains(err.Error(), "wait condition") {
		t.Fatalf("Run() error = %v, want wait failure", err)
	}
	if got := phaseNames(result.Phases); containsString(got, "kubeconfig") {
		t.Fatalf("phases = %#v, kubeconfig should not run after wait failure", got)
	}
}

func TestRunRefusesNotReadyNodesBeforeKubeadm(t *testing.T) {
	nodeRunner := &fakeNodeRunner{}
	_, err := Run(context.Background(), Request{Inventory: validInventory()}, Dependencies{
		ReadinessChecker: failingChecker{snapshot: inventory.ReadinessSnapshot{
			KatlKubeadmReadyTarget: false,
			KubernetesSysextActive: true,
			KubeadmConfigExists:    true,
			ContainerdActive:       true,
			CRIResponsive:          true,
			KubeletInstalled:       true,
			EtcKubernetesWritable:  true,
			EtcKubernetesProjected: true,
			KubernetesVersion:      "v1.36.1",
			SystemRole:             inventory.RoleControlPlane,
			KubeadmConfigIntent:    inventory.IntentControlPlane,
		}},
		NodeRunner: nodeRunner,
	})
	if err == nil || !strings.Contains(err.Error(), "katl-kubeadm-ready.target") {
		t.Fatalf("Run() error = %v, want readiness failure", err)
	}
	if len(nodeRunner.calls) != 0 {
		t.Fatalf("node runner was called before readiness passed: %#v", nodeRunner.calls)
	}
}

func TestRunDryRunSkipsKubeadm(t *testing.T) {
	result, err := Run(context.Background(), Request{Inventory: validInventory(), DryRun: true}, Dependencies{
		ReadinessChecker: readyChecker{},
		NodeRunner:       &fakeNodeRunner{err: errors.New("must not run")},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !result.DryRun {
		t.Fatal("result.DryRun = false")
	}
	if got := phaseNames(result.Phases); !reflect.DeepEqual(got, []string{"plan", "readiness", "dry-run"}) {
		t.Fatalf("phases = %#v", got)
	}
}

func TestRunJoinsAdditionalControlPlanesSeriallyBeforeWorkers(t *testing.T) {
	inv := validInventory()
	inv.Nodes = append(inv.Nodes, inventory.Node{
		Name:              "cp-2",
		Address:           "10.0.0.12",
		SystemRole:        inventory.RoleControlPlane,
		Access:            inventory.Access{Method: "agent"},
		KubeadmConfig:     inventory.KubeadmConfig{Ref: "control-plane", Path: "/etc/katl/kubeadm/control-plane/config.yaml", Intent: inventory.IntentControlPlane},
		KubernetesVersion: "v1.36.1",
	})
	inv.Nodes = append(inv.Nodes, inventory.Node{
		Name:              "cp-3",
		Address:           "10.0.0.13",
		SystemRole:        inventory.RoleControlPlane,
		Access:            inventory.Access{Method: "agent"},
		KubeadmConfig:     inventory.KubeadmConfig{Ref: "control-plane", Path: "/etc/katl/kubeadm/control-plane/config.yaml", Intent: inventory.IntentControlPlane},
		KubernetesVersion: "v1.36.1",
	})
	nodeRunner := &fakeNodeRunner{
		credentials: AdminCredentials{
			CertificateAuthorityData: testCA,
			ClientCertificateData:    testCert,
			ClientKeyData:            testKey,
		},
		join:             JoinMaterial{Argv: []string{"kubeadm", "join", "api.katl.test:6443", "--token", "abcdef.0123456789abcdef", "--discovery-token-ca-cert-hash", testDiscoveryHash}},
		controlPlaneJoin: JoinMaterial{Argv: []string{"kubeadm", "join", "api.katl.test:6443", "--token", "abcdef.0123456789abcdef", "--discovery-token-ca-cert-hash", testDiscoveryHash, "--control-plane", "--certificate-key", strings.Repeat("a", 64)}},
	}
	result, err := Run(context.Background(), Request{
		Inventory:           inv,
		InitNode:            "cp-1",
		KubeconfigOut:       filepath.Join(t.TempDir(), "operator.conf"),
		OverwriteKubeconfig: true,
	}, Dependencies{
		ReadinessChecker: readyChecker{},
		NodeRunner:       nodeRunner,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	wantPhases := []string{
		"plan", "readiness", "kubeadm-init", "api-ready",
		"control-plane-join-material", "control-plane-join", "control-plane-ready",
		"control-plane-join", "control-plane-ready",
		"join-material", "worker-join", "worker-ready", "api-ready-after-join", "kubeconfig",
	}
	if got := phaseNames(result.Phases); !reflect.DeepEqual(got, wantPhases) {
		t.Fatalf("phases = %#v, want %#v", got, wantPhases)
	}
	if got, want := nodeRunner.calls, []string{
		"init:cp-1",
		"ready:cp-1",
		"join-control-plane-material:cp-1",
		"join-control-plane:cp-2",
		"ready-control-plane:cp-2",
		"join-control-plane:cp-3",
		"ready-control-plane:cp-3",
		"join-material:cp-1",
		"join-worker:worker-1",
		"ready-worker:worker-1",
		"ready:cp-1",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %#v, want %#v", got, want)
	}
}

func TestRunStopsAfterControlPlaneJoinHealthFailure(t *testing.T) {
	inv := validInventory()
	inv.Nodes = append(inv.Nodes, inventory.Node{
		Name:              "cp-2",
		Address:           "10.0.0.12",
		SystemRole:        inventory.RoleControlPlane,
		Access:            inventory.Access{Method: "agent"},
		KubeadmConfig:     inventory.KubeadmConfig{Ref: "control-plane", Path: "/etc/katl/kubeadm/control-plane/config.yaml", Intent: inventory.IntentControlPlane},
		KubernetesVersion: "v1.36.1",
	})
	secret := "abcdef.0123456789abcdef"
	nodeRunner := &fakeNodeRunner{
		credentials:          AdminCredentials{CertificateAuthorityData: testCA, ClientCertificateData: testCert, ClientKeyData: testKey},
		controlPlaneJoin:     JoinMaterial{Argv: []string{"kubeadm", "join", "api.katl.test:6443", "--token", secret, "--discovery-token-ca-cert-hash", testDiscoveryHash, "--control-plane", "--certificate-key", strings.Repeat("a", 64)}},
		controlPlaneReadyErr: errors.New("etcd unhealthy with token " + secret),
	}
	result, err := Run(context.Background(), Request{Inventory: inv, InitNode: "cp-1"}, Dependencies{
		ReadinessChecker: readyChecker{},
		NodeRunner:       nodeRunner,
	})
	if err == nil {
		t.Fatal("Run() error = nil, want control-plane readiness failure")
	}
	if strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "[REDACTED BOOTSTRAP TOKEN]") {
		t.Fatalf("error = %q, want redacted token", err.Error())
	}
	if got := phaseNames(result.Phases); containsString(got, "join-material") {
		t.Fatalf("phases = %#v, worker join material should not be created after control-plane failure", got)
	}
}

func TestRunRedactsKubeadmFailure(t *testing.T) {
	secret := "abcdef.0123456789abcdef"
	_, err := Run(context.Background(), Request{Inventory: validInventory()}, Dependencies{
		ReadinessChecker: readyChecker{},
		NodeRunner:       &fakeNodeRunner{err: errors.New("kubeadm failed with token " + secret)},
	})
	if err == nil {
		t.Fatal("Run() error = nil, want kubeadm failure")
	}
	if strings.Contains(err.Error(), secret) || !strings.Contains(inventory.Redact(err.Error()), "[REDACTED BOOTSTRAP TOKEN]") {
		t.Fatalf("error was not redacted by caller helpers: %q", inventory.Redact(err.Error()))
	}
}

func TestRunRefusesDifferentExistingKubeconfig(t *testing.T) {
	out := filepath.Join(t.TempDir(), "operator.conf")
	if err := os.WriteFile(out, []byte("different\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Run(context.Background(), Request{Inventory: validSingleNodeInventory(), KubeconfigOut: out}, Dependencies{
		ReadinessChecker: readyChecker{},
		NodeRunner: &fakeNodeRunner{credentials: AdminCredentials{
			CertificateAuthorityData: testCA,
			ClientCertificateData:    testCert,
			ClientKeyData:            testKey,
		}},
	})
	if !errors.Is(err, kubeconfig.ErrExists) {
		t.Fatalf("Run() error = %v, want ErrExists", err)
	}
}

func TestTransportRunnerRunsKubeadmAndRedactsCommandErrors(t *testing.T) {
	transport := newFakeTransport()
	transport.commands[commandKey("kubeadm", "init", "--config", "/etc/katl/kubeadm/control-plane/config.yaml")] = readiness.CommandResult{ExitStatus: 0}
	transport.files[adminKubeconfigPath] = []byte(adminKubeconfig())
	runner := TransportRunner{Transport: transport}
	credentials, err := runner.RunKubeadmInit(context.Background(), validPlannedNode("cp-1", inventory.ActionInit), "")
	if err != nil {
		t.Fatalf("RunKubeadmInit() error = %v", err)
	}
	if credentials.ClientKeyData != testKey {
		t.Fatalf("credentials = %#v", credentials)
	}

	transport = newFakeTransport()
	secret := "abcdef.0123456789abcdef"
	transport.commands[commandKey("kubeadm", "token", "create", "--print-join-command", "--kubeconfig", adminKubeconfigPath)] = readiness.CommandResult{
		ExitStatus: 1,
		Stderr:     "token " + secret,
	}
	_, err = (TransportRunner{Transport: transport}).CreateWorkerJoin(context.Background(), validPlannedNode("cp-1", inventory.ActionInit))
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "[REDACTED BOOTSTRAP TOKEN]") {
		t.Fatalf("CreateWorkerJoin() error = %v, want redacted failure", err)
	}

	transport = newFakeTransport()
	transport.files["/etc/katl/kubeadm/worker/config.yaml"] = []byte(testWorkerJoinConfig())
	workerJoinConfigPath := generatedJoinConfigPath(validPlannedNode("worker-1", inventory.ActionWorkerJoin))
	transport.commands[commandKey("kubeadm", "join", "--config", workerJoinConfigPath)] = readiness.CommandResult{
		ExitStatus: 1,
		Stderr:     "join failed with token " + secret,
	}
	err = (TransportRunner{Transport: transport}).RunWorkerJoin(context.Background(), validPlannedNode("worker-1", inventory.ActionWorkerJoin), JoinMaterial{
		Argv: []string{"kubeadm", "join", "api.katl.test:6443", "--token", secret, "--discovery-token-ca-cert-hash", testDiscoveryHash},
	})
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "[REDACTED BOOTSTRAP TOKEN]") {
		t.Fatalf("RunWorkerJoin() error = %v, want redacted argv failure", err)
	}
	if uploaded := string(transport.writes[workerJoinConfigPath]); !strings.Contains(uploaded, "apiServerEndpoint: api.katl.test:6443") || !strings.Contains(uploaded, "token: "+secret) || !strings.Contains(uploaded, "- "+testDiscoveryHash) {
		t.Fatalf("uploaded worker join config = %q", uploaded)
	}
}

func TestTransportRunnerContinuesWhenInitAlreadyCompleted(t *testing.T) {
	transport := newFakeTransport()
	transport.commands[commandKey("kubeadm", "init", "--config", "/etc/katl/kubeadm/control-plane/config.yaml")] = readiness.CommandResult{
		ExitStatus: 1,
		Stderr:     "this node is already initialized",
	}
	transport.files[adminKubeconfigPath] = []byte(adminKubeconfig())
	credentials, err := (TransportRunner{Transport: transport}).RunKubeadmInit(context.Background(), validPlannedNode("cp-1", inventory.ActionInit), "")
	if err != nil {
		t.Fatalf("RunKubeadmInit() error = %v", err)
	}
	if credentials.ClientCertificateData != testCert {
		t.Fatalf("credentials = %#v", credentials)
	}
}

func TestTransportRunnerRunKubeadmInitWritesEndpointConfig(t *testing.T) {
	transport := newFakeTransport()
	node := validPlannedNode("cp-1", inventory.ActionInit)
	initConfigPath := generatedInitConfigPath(node)
	transport.files["/etc/katl/kubeadm/control-plane/config.yaml"] = []byte(testControlPlaneInitConfig())
	transport.files[adminKubeconfigPath] = []byte(adminKubeconfig())
	transport.commands[commandKey("kubeadm", "init", "--config", initConfigPath)] = readiness.CommandResult{ExitStatus: 0}

	base := transport.files["/etc/katl/kubeadm/control-plane/config.yaml"]
	rendered, err := RenderInitConfig(base, "api.katl.test:6443", "192.0.2.10")
	if err != nil {
		t.Fatalf("RenderInitConfig() error = %v", err)
	}
	uploaded := string(rendered)
	for _, want := range []string{
		"kind: InitConfiguration",
		"kind: ClusterConfiguration",
		"controlPlaneEndpoint: api.katl.test:6443",
		"- api.katl.test",
		"- 192.0.2.10",
	} {
		if !strings.Contains(uploaded, want) {
			t.Fatalf("uploaded init config = %q, want %q", uploaded, want)
		}
	}
}

func TestTransportRunnerSkipsAlreadyJoinedWorker(t *testing.T) {
	transport := newFakeTransport()
	node := validPlannedNode("worker-1", inventory.ActionWorkerJoin)
	joinConfigPath := generatedJoinConfigPath(node)
	transport.files["/etc/katl/kubeadm/worker/config.yaml"] = []byte(testWorkerJoinConfig())
	transport.commands[commandKey("kubeadm", "join", "--config", joinConfigPath)] = readiness.CommandResult{
		ExitStatus: 1,
		Stderr:     "node is already joined",
	}
	transport.commands[commandKey("test", "-f", "/etc/kubernetes/kubelet.conf")] = readiness.CommandResult{ExitStatus: 0}
	transport.commands[commandKey("systemctl", "is-active", "--quiet", "kubelet.service")] = readiness.CommandResult{ExitStatus: 0}
	err := (TransportRunner{Transport: transport}).RunWorkerJoin(context.Background(), node, JoinMaterial{
		Argv: []string{"kubeadm", "join", "api.katl.test:6443", "--token", "abcdef.0123456789abcdef", "--discovery-token-ca-cert-hash", testDiscoveryHash},
	})
	if err != nil {
		t.Fatalf("RunWorkerJoin() error = %v", err)
	}
}

func TestTransportRunnerRejectsAmbiguousJoinAlreadyExists(t *testing.T) {
	transport := newFakeTransport()
	node := validPlannedNode("worker-1", inventory.ActionWorkerJoin)
	joinConfigPath := generatedJoinConfigPath(node)
	transport.files["/etc/katl/kubeadm/worker/config.yaml"] = []byte(testWorkerJoinConfig())
	transport.commands[commandKey("kubeadm", "join", "--config", joinConfigPath)] = readiness.CommandResult{
		ExitStatus: 1,
		Stderr:     "/etc/kubernetes/kubelet.conf already exists",
	}
	err := (TransportRunner{Transport: transport}).RunWorkerJoin(context.Background(), node, JoinMaterial{
		Argv: []string{"kubeadm", "join", "api.katl.test:6443", "--token", "abcdef.0123456789abcdef", "--discovery-token-ca-cert-hash", testDiscoveryHash},
	})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("RunWorkerJoin() error = %v, want non-idempotent failure", err)
	}
}

func TestTransportRunnerRejectsControlPlaneMaterialForWorkerJoin(t *testing.T) {
	transport := newFakeTransport()
	certificateKey := strings.Repeat("a", 64)
	err := (TransportRunner{Transport: transport}).RunWorkerJoin(context.Background(), validPlannedNode("worker-1", inventory.ActionWorkerJoin), JoinMaterial{
		Argv: []string{"kubeadm", "join", "api.katl.test:6443", "--token", "abcdef.0123456789abcdef", "--control-plane", "--certificate-key", certificateKey},
	})
	if err == nil || !strings.Contains(err.Error(), "must not include --control-plane") {
		t.Fatalf("RunWorkerJoin() error = %v, want control-plane material rejection", err)
	}
	if len(transport.commandCalls) != 0 {
		t.Fatalf("commands = %#v, want no remote command before material validation", transport.commandCalls)
	}
}

func TestTransportRunnerCreatesControlPlaneJoinMaterial(t *testing.T) {
	transport := newFakeTransport()
	certificateKey := strings.Repeat("a", 64)
	transport.commands[commandKey("kubeadm", "init", "phase", "upload-certs", "--upload-certs", "--kubeconfig", adminKubeconfigPath)] = readiness.CommandResult{
		ExitStatus: 0,
		Stdout:     "sha256:" + strings.Repeat("b", 64) + "\nUsing certificate key:\n" + certificateKey + "\n",
	}
	transport.commands[commandKey("kubeadm", "token", "create", "--print-join-command", "--certificate-key", certificateKey, "--kubeconfig", adminKubeconfigPath)] = readiness.CommandResult{
		ExitStatus: 0,
		Stdout:     "kubeadm join api.katl.test:6443 --token abcdef.0123456789abcdef --discovery-token-ca-cert-hash " + testDiscoveryHash + "\n",
	}
	material, err := (TransportRunner{Transport: transport}).CreateControlPlaneJoin(context.Background(), validPlannedNode("cp-1", inventory.ActionInit))
	if err != nil {
		t.Fatalf("CreateControlPlaneJoin() error = %v", err)
	}
	if !reflect.DeepEqual(material.Argv, []string{"kubeadm", "join", "api.katl.test:6443", "--token", "abcdef.0123456789abcdef", "--discovery-token-ca-cert-hash", testDiscoveryHash, "--control-plane", "--certificate-key", certificateKey}) {
		t.Fatalf("join argv = %#v", material.Argv)
	}
}

func TestTransportRunnerRejectsUploadCertsWithoutStandaloneCertificateKey(t *testing.T) {
	transport := newFakeTransport()
	transport.commands[commandKey("kubeadm", "init", "phase", "upload-certs", "--upload-certs", "--kubeconfig", adminKubeconfigPath)] = readiness.CommandResult{
		ExitStatus: 0,
		Stdout:     "sha256:" + strings.Repeat("b", 64) + "\n",
	}
	_, err := (TransportRunner{Transport: transport}).CreateControlPlaneJoin(context.Background(), validPlannedNode("cp-1", inventory.ActionInit))
	if err == nil || !strings.Contains(err.Error(), "certificate key") {
		t.Fatalf("CreateControlPlaneJoin() error = %v, want missing certificate key", err)
	}
}

func TestTransportRunnerRequiresControlPlaneJoinMaterialFlags(t *testing.T) {
	tests := []struct {
		name     string
		material JoinMaterial
		want     string
	}{
		{
			name: "missing control-plane flag",
			material: JoinMaterial{
				Argv: []string{"kubeadm", "join", "api.katl.test:6443", "--token", "abcdef.0123456789abcdef", "--certificate-key", strings.Repeat("a", 64)},
			},
			want: "must include --control-plane",
		},
		{
			name: "missing certificate key",
			material: JoinMaterial{
				Argv: []string{"kubeadm", "join", "api.katl.test:6443", "--token", "abcdef.0123456789abcdef", "--control-plane"},
			},
			want: "must include --certificate-key",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			transport := newFakeTransport()
			err := (TransportRunner{Transport: transport}).RunControlPlaneJoin(context.Background(), validPlannedNode("cp-2", inventory.ActionControlPlaneJoin), tt.material)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("RunControlPlaneJoin() error = %v, want %q", err, tt.want)
			}
			if len(transport.commandCalls) != 0 {
				t.Fatalf("commands = %#v, want no remote command before material validation", transport.commandCalls)
			}
		})
	}
}

func TestTransportRunnerRunControlPlaneJoinRedactsCertificateKey(t *testing.T) {
	transport := newFakeTransport()
	certificateKey := strings.Repeat("a", 64)
	node := validPlannedNode("cp-2", inventory.ActionControlPlaneJoin)
	joinConfigPath := generatedJoinConfigPath(node)
	transport.files["/etc/katl/kubeadm/control-plane/config.yaml"] = []byte(testControlPlaneJoinConfig())
	transport.commands[commandKey("kubeadm", "join", "--config", joinConfigPath)] = readiness.CommandResult{
		ExitStatus: 1,
		Stderr:     "join failed with certificate-key " + certificateKey,
	}
	err := (TransportRunner{Transport: transport}).RunControlPlaneJoin(context.Background(), node, JoinMaterial{
		Argv: []string{"kubeadm", "join", "api.katl.test:6443", "--token", "abcdef.0123456789abcdef", "--discovery-token-ca-cert-hash", testDiscoveryHash, "--control-plane", "--certificate-key", certificateKey},
	})
	if err == nil {
		t.Fatal("RunControlPlaneJoin() error = nil, want join failure")
	}
	if strings.Contains(err.Error(), certificateKey) || !strings.Contains(err.Error(), "certificate-key[REDACTED]") {
		t.Fatalf("RunControlPlaneJoin() error = %q, want redacted certificate key", err.Error())
	}
	if uploaded := string(transport.writes[joinConfigPath]); !strings.Contains(uploaded, "certificateKey: "+certificateKey) {
		t.Fatalf("uploaded control-plane join config = %q", uploaded)
	}
}

func TestTransportRunnerRunControlPlaneJoinSynthesizesJoinConfig(t *testing.T) {
	transport := newFakeTransport()
	certificateKey := strings.Repeat("a", 64)
	node := validPlannedNode("cp-2", inventory.ActionControlPlaneJoin)
	joinConfigPath := generatedJoinConfigPath(node)
	transport.files["/etc/katl/kubeadm/control-plane/config.yaml"] = []byte(testControlPlaneInitConfig())
	transport.commands[commandKey("kubeadm", "join", "--config", joinConfigPath)] = readiness.CommandResult{ExitStatus: 0}

	err := (TransportRunner{Transport: transport}).RunControlPlaneJoin(context.Background(), node, JoinMaterial{
		Argv: []string{"kubeadm", "join", "api.katl.test:6443", "--token", "abcdef.0123456789abcdef", "--discovery-token-ca-cert-hash", testDiscoveryHash, "--control-plane", "--certificate-key", certificateKey},
	})
	if err != nil {
		t.Fatalf("RunControlPlaneJoin() error = %v", err)
	}
	uploaded := string(transport.writes[joinConfigPath])
	for _, want := range []string{
		"kind: JoinConfiguration",
		"name: cp-2",
		"apiServerEndpoint: api.katl.test:6443",
		"certificateKey: " + certificateKey,
	} {
		if !strings.Contains(uploaded, want) {
			t.Fatalf("uploaded control-plane join config = %q, want %q", uploaded, want)
		}
	}
	if strings.Contains(uploaded, "kind: InitConfiguration") || strings.Contains(uploaded, "kind: ClusterConfiguration") {
		t.Fatalf("uploaded control-plane join config = %q, want only join config", uploaded)
	}
}

func TestTransportRunnerWaitsForControlPlaneJoinReady(t *testing.T) {
	transport := newFakeTransport()
	transport.commands[commandKey("kubectl", "--kubeconfig", adminKubeconfigPath, "get", "--raw=/readyz")] = readiness.CommandResult{ExitStatus: 0, Stdout: "ok\n"}
	transport.commands[commandKey("kubectl", "--kubeconfig", adminKubeconfigPath, "get", "node", "cp-2")] = readiness.CommandResult{ExitStatus: 0, Stdout: "cp-2\n"}
	for _, name := range []string{"kube-apiserver", "kube-controller-manager", "kube-scheduler", "etcd"} {
		transport.commands[commandKey("crictl", "ps", "--name", name, "--state", "Running", "--quiet")] = readiness.CommandResult{ExitStatus: 0, Stdout: name + "-container\n"}
	}
	transport.commands[commandKey(etcdctlArgs("etcd-container", "endpoint", "health", "--cluster", "--write-out=json")...)] = readiness.CommandResult{
		ExitStatus: 0,
		Stdout:     `[{"endpoint":"https://127.0.0.1:2379","health":true}]`,
	}
	transport.commands[commandKey(etcdctlArgs("etcd-container", "endpoint", "status", "--cluster", "--write-out=json")...)] = readiness.CommandResult{
		ExitStatus: 0,
		Stdout:     `[{"Endpoint":"https://127.0.0.1:2379","Status":{"header":{"member_id":"a1"},"version":"3.5.12","leader":"a1"}}]`,
	}
	transport.commands[commandKey(etcdctlArgs("etcd-container", "member", "list", "--write-out=json")...)] = readiness.CommandResult{
		ExitStatus: 0,
		Stdout:     `{"members":[{"ID":"a1","name":"cp-1"},{"ID":"b2","name":"cp-2"},{"ID":"c3","name":"cp-3"}]}`,
	}

	err := (TransportRunner{Transport: transport, APITimeout: time.Second, APIPollInterval: time.Millisecond}).WaitControlPlaneJoinReady(
		context.Background(),
		validPlannedNode("cp-1", inventory.ActionInit),
		validPlannedNode("cp-2", inventory.ActionControlPlaneJoin),
	)
	if err != nil {
		t.Fatalf("WaitControlPlaneJoinReady() error = %v", err)
	}
}

func TestTransportRunnerWaitControlPlaneJoinReadyRequiresEtcdMember(t *testing.T) {
	transport := newFakeTransport()
	transport.commands[commandKey("kubectl", "--kubeconfig", adminKubeconfigPath, "get", "--raw=/readyz")] = readiness.CommandResult{ExitStatus: 0, Stdout: "ok\n"}
	transport.commands[commandKey("kubectl", "--kubeconfig", adminKubeconfigPath, "get", "node", "cp-2")] = readiness.CommandResult{ExitStatus: 0, Stdout: "cp-2\n"}
	for _, name := range []string{"kube-apiserver", "kube-controller-manager", "kube-scheduler", "etcd"} {
		transport.commands[commandKey("crictl", "ps", "--name", name, "--state", "Running", "--quiet")] = readiness.CommandResult{ExitStatus: 0, Stdout: name + "-container\n"}
	}
	transport.commands[commandKey(etcdctlArgs("etcd-container", "endpoint", "health", "--cluster", "--write-out=json")...)] = readiness.CommandResult{
		ExitStatus: 0,
		Stdout:     `[{"endpoint":"https://127.0.0.1:2379","health":true}]`,
	}
	transport.commands[commandKey(etcdctlArgs("etcd-container", "endpoint", "status", "--cluster", "--write-out=json")...)] = readiness.CommandResult{
		ExitStatus: 0,
		Stdout:     `[{"Endpoint":"https://127.0.0.1:2379","Status":{"header":{"member_id":"a1"},"version":"3.5.12","leader":"a1"}}]`,
	}
	transport.commands[commandKey(etcdctlArgs("etcd-container", "member", "list", "--write-out=json")...)] = readiness.CommandResult{
		ExitStatus: 0,
		Stdout:     `{"members":[{"ID":"a1","name":"cp-1"}]}`,
	}

	err := (TransportRunner{Transport: transport, APITimeout: time.Millisecond, APIPollInterval: time.Millisecond}).WaitControlPlaneJoinReady(
		context.Background(),
		validPlannedNode("cp-1", inventory.ActionInit),
		validPlannedNode("cp-2", inventory.ActionControlPlaneJoin),
	)
	if err == nil || !strings.Contains(err.Error(), "member cp-2 is missing") {
		t.Fatalf("WaitControlPlaneJoinReady() error = %v, want missing member", err)
	}
}

func TestTransportRunnerWaitsForAPIReady(t *testing.T) {
	transport := newFakeTransport()
	transport.commandResults[commandKey("kubectl", "--kubeconfig", adminKubeconfigPath, "get", "--raw=/readyz")] = []readiness.CommandResult{
		{ExitStatus: 1, Stderr: "not ready"},
		{ExitStatus: 0, Stdout: "ok\n"},
	}
	err := (TransportRunner{Transport: transport, APITimeout: time.Second, APIPollInterval: time.Millisecond}).WaitAPIReady(context.Background(), validPlannedNode("cp-1", inventory.ActionInit))
	if err != nil {
		t.Fatalf("WaitAPIReady() error = %v", err)
	}
	if got := transport.commandCount(commandKey("kubectl", "--kubeconfig", adminKubeconfigPath, "get", "--raw=/readyz")); got != 2 {
		t.Fatalf("readyz command count = %d, want 2", got)
	}
}

func TestKubectlBootstrapRunnerAppliesManifestsAndWaits(t *testing.T) {
	dir := t.TempDir()
	commands := &fakeKubectlCommandRunner{}
	result, err := (KubectlBootstrapRunner{
		CommandRunner: commands,
		TempDir:       dir,
	}).RunUserBootstrap(context.Background(), BootstrapRequest{
		Server:         "10.0.0.11:6443",
		StableEndpoint: "api.stable.test:6443",
		Credentials: AdminCredentials{
			CertificateAuthorityData: testCA,
			ClientCertificateData:    testCert,
			ClientKeyData:            testKey,
		},
		Manifests: []BootstrapManifest{{
			Path:    "01-cni.yaml",
			Content: []byte(validBootstrapManifest("cni")),
		}},
		Waits: []BootstrapWait{
			{Kind: BootstrapWaitAPIReady},
			{Kind: BootstrapWaitRolloutStatus, Namespace: "kube-system", Name: "daemonset/cilium"},
			{Kind: BootstrapWaitRolloutStatus, Namespace: "kube-system", Name: "deployment/coredns"},
			{Kind: BootstrapWaitPodsReady, Namespace: "kube-system", Selector: "k8s-app=kube-dns"},
			{Kind: BootstrapWaitCondition, Namespace: "kube-system", Name: "deployment/cilium-operator", Condition: "Available"},
			{Kind: BootstrapWaitNodesReady},
			{Kind: BootstrapWaitStableEndpoint, Name: "api.stable.test:6443"},
		},
	})
	if err != nil {
		t.Fatalf("RunUserBootstrap() error = %v", err)
	}
	if len(result.AppliedManifests) != 1 || len(result.Waits) != 7 || !result.StableEndpointReady {
		t.Fatalf("result = %#v", result)
	}
	if len(commands.calls) != 8 {
		t.Fatalf("kubectl calls = %#v, want 8 calls", commands.calls)
	}
	if got := commands.calls[0][5:]; !reflect.DeepEqual(got, []string{"apply", "-f", filepath.Join(dir, "0000.yaml")}) {
		t.Fatalf("first kubectl args = %#v, want apply", commands.calls[0])
	}
	if got := commands.calls[1][5:]; !reflect.DeepEqual(got, []string{"get", "--raw=/readyz"}) {
		t.Fatalf("api ready args = %#v", commands.calls[1])
	}
	if got := commands.calls[2][5:]; !reflect.DeepEqual(got, []string{"-n", "kube-system", "rollout", "status", "daemonset/cilium", "--timeout=2m"}) {
		t.Fatalf("cilium rollout wait args = %#v", commands.calls[2])
	}
	if got := commands.calls[3][5:]; !reflect.DeepEqual(got, []string{"-n", "kube-system", "rollout", "status", "deployment/coredns", "--timeout=2m"}) {
		t.Fatalf("coredns rollout wait args = %#v", commands.calls[3])
	}
	if got := commands.calls[4][5:]; !reflect.DeepEqual(got, []string{"-n", "kube-system", "wait", "--for=condition=Ready", "pod", "-l", "k8s-app=kube-dns", "--timeout=2m"}) {
		t.Fatalf("pods-ready wait args = %#v", commands.calls[4])
	}
	if got := commands.calls[5][5:]; !reflect.DeepEqual(got, []string{"-n", "kube-system", "wait", "--for=condition=Available", "deployment/cilium-operator", "--timeout=2m"}) {
		t.Fatalf("condition wait args = %#v", commands.calls[5])
	}
	if got := commands.calls[6][5:]; !reflect.DeepEqual(got, []string{"wait", "--for=condition=Ready", "nodes", "--all", "--timeout=2m"}) {
		t.Fatalf("nodes wait args = %#v", commands.calls[6])
	}
	for _, call := range commands.calls {
		if len(call) < 5 || call[3] != "--context" || call[4] != "katl-bootstrap" {
			t.Fatalf("kubectl call lacks explicit context: %#v", call)
		}
	}
	initialKubeconfig := readFileString(t, commands.calls[0][2])
	if !strings.Contains(initialKubeconfig, "server: https://10.0.0.11:6443") {
		t.Fatalf("initial bootstrap kubeconfig did not normalize server:\n%s", initialKubeconfig)
	}
	stableKubeconfig := readFileString(t, commands.calls[7][2])
	if !strings.Contains(stableKubeconfig, "server: https://api.stable.test:6443") {
		t.Fatalf("stable bootstrap kubeconfig did not normalize server:\n%s", stableKubeconfig)
	}
}

func TestPrepareBootstrapRejectsInvalidPodSelectorWait(t *testing.T) {
	_, err := prepareBootstrap(UserBootstrap{
		Waits: []BootstrapWait{{Kind: BootstrapWaitPodsReady, Namespace: "kube-system", Selector: "app = coredns"}},
	})
	if err == nil || !strings.Contains(err.Error(), "bootstrap wait pods-ready") {
		t.Fatalf("prepareBootstrap() error = %v, want selector validation failure", err)
	}
}

func TestKubectlBootstrapRunnerStopsOnRolloutFailure(t *testing.T) {
	commands := &fakeKubectlCommandRunner{
		defaultResult: &readiness.CommandResult{ExitStatus: 1, Stderr: "deployment exceeded progress deadline"},
	}
	_, err := (KubectlBootstrapRunner{
		CommandRunner: commands,
		TempDir:       t.TempDir(),
	}).RunUserBootstrap(context.Background(), BootstrapRequest{
		Server: "10.0.0.11:6443",
		Credentials: AdminCredentials{
			CertificateAuthorityData: testCA,
			ClientCertificateData:    testCert,
			ClientKeyData:            testKey,
		},
		Waits: []BootstrapWait{{Kind: BootstrapWaitRolloutStatus, Namespace: "kube-system", Name: "deployment/coredns"}},
	})
	if err == nil || !strings.Contains(err.Error(), "deployment exceeded progress deadline") {
		t.Fatalf("RunUserBootstrap() error = %v, want rollout failure", err)
	}
	if len(commands.calls) != 1 {
		t.Fatalf("kubectl calls = %#v, want one rollout status call", commands.calls)
	}
	if got := commands.calls[0][5:]; !reflect.DeepEqual(got, []string{"-n", "kube-system", "rollout", "status", "deployment/coredns", "--timeout=2m"}) {
		t.Fatalf("rollout wait args = %#v", commands.calls[0])
	}
}

func TestKubectlBootstrapRunnerStopsOnPodReadinessFailure(t *testing.T) {
	commands := &fakeKubectlCommandRunner{
		defaultResult: &readiness.CommandResult{ExitStatus: 1, Stderr: "timed out waiting for condition Ready on pods"},
	}
	_, err := (KubectlBootstrapRunner{
		CommandRunner: commands,
		TempDir:       t.TempDir(),
	}).RunUserBootstrap(context.Background(), BootstrapRequest{
		Server: "10.0.0.11:6443",
		Credentials: AdminCredentials{
			CertificateAuthorityData: testCA,
			ClientCertificateData:    testCert,
			ClientKeyData:            testKey,
		},
		Waits: []BootstrapWait{
			{Kind: BootstrapWaitPodsReady, Namespace: "kube-system", Selector: "k8s-app=kube-dns"},
			{Kind: BootstrapWaitNodesReady},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "timed out waiting for condition Ready") {
		t.Fatalf("RunUserBootstrap() error = %v, want pod readiness failure", err)
	}
	if len(commands.calls) != 1 {
		t.Fatalf("kubectl calls = %#v, want one pod readiness call and no later wait", commands.calls)
	}
	if got := commands.calls[0][5:]; !reflect.DeepEqual(got, []string{"-n", "kube-system", "wait", "--for=condition=Ready", "pod", "-l", "k8s-app=kube-dns", "--timeout=2m"}) {
		t.Fatalf("pods-ready wait args = %#v", commands.calls[0])
	}
}

func TestKubectlBootstrapRunnerRunsPreWaitBeforeManifests(t *testing.T) {
	dir := t.TempDir()
	commands := &fakeKubectlCommandRunner{}
	result, err := (KubectlBootstrapRunner{
		CommandRunner: commands,
		TempDir:       dir,
	}).RunUserBootstrap(context.Background(), BootstrapRequest{
		Server:         "10.0.0.11:6443",
		StableEndpoint: "api.stable.test:6443",
		Credentials: AdminCredentials{
			CertificateAuthorityData: testCA,
			ClientCertificateData:    testCert,
			ClientKeyData:            testKey,
		},
		PreWaits: []BootstrapWait{{Kind: BootstrapWaitStableEndpoint, Name: "api.stable.test:6443"}},
		Manifests: []BootstrapManifest{{
			Path:    "01-cni.yaml",
			Content: []byte(validBootstrapManifest("cni")),
		}},
	})
	if err != nil {
		t.Fatalf("RunUserBootstrap() error = %v", err)
	}
	if !result.StableEndpointReady || len(result.AppliedManifests) != 1 || len(result.Waits) != 1 {
		t.Fatalf("result = %#v", result)
	}
	if len(commands.calls) != 2 {
		t.Fatalf("kubectl calls = %#v, want pre wait then apply", commands.calls)
	}
	if got := commands.calls[0][5:]; !reflect.DeepEqual(got, []string{"get", "--raw=/readyz"}) {
		t.Fatalf("first kubectl args = %#v, want stable endpoint readyz", commands.calls[0])
	}
	if got := commands.calls[1][5:]; !reflect.DeepEqual(got, []string{"apply", "-f", filepath.Join(dir, "0000.yaml")}) {
		t.Fatalf("second kubectl args = %#v, want apply", commands.calls[1])
	}
	stableKubeconfig := readFileString(t, commands.calls[0][2])
	if !strings.Contains(stableKubeconfig, "server: https://api.stable.test:6443") {
		t.Fatalf("pre-wait kubeconfig did not use stable endpoint:\n%s", stableKubeconfig)
	}
}

func TestKubectlBootstrapRunnerStopsWhenPreWaitFails(t *testing.T) {
	commands := &fakeKubectlCommandRunner{
		defaultResult: &readiness.CommandResult{ExitStatus: 1, Stderr: "stable endpoint still not ready"},
		results: []readiness.CommandResult{
			{ExitStatus: 1, Stderr: "stable endpoint not ready"},
			{ExitStatus: 1, Stderr: "stable endpoint still not ready"},
		},
	}
	_, err := (KubectlBootstrapRunner{
		CommandRunner: commands,
		TempDir:       t.TempDir(),
		Timeout:       2 * time.Millisecond,
		PollInterval:  time.Millisecond,
		ProbeTimeout:  time.Millisecond,
	}).RunUserBootstrap(context.Background(), BootstrapRequest{
		Server:         "10.0.0.11:6443",
		StableEndpoint: "api.stable.test:6443",
		Credentials: AdminCredentials{
			CertificateAuthorityData: testCA,
			ClientCertificateData:    testCert,
			ClientKeyData:            testKey,
		},
		PreWaits: []BootstrapWait{{Kind: BootstrapWaitStableEndpoint, Name: "api.stable.test:6443"}},
		Manifests: []BootstrapManifest{{
			Path:    "01-cni.yaml",
			Content: []byte(validBootstrapManifest("cni")),
		}},
	})
	if err == nil {
		t.Fatal("RunUserBootstrap() error = nil, want pre-wait failure")
	}
	if len(commands.calls) == 0 {
		t.Fatal("kubectl calls = 0, want readiness polling")
	}
	for _, call := range commands.calls {
		if got := call[5:]; !reflect.DeepEqual(got, []string{"get", "--raw=/readyz"}) {
			t.Fatalf("kubectl call = %#v, want only stable endpoint probes before failure", call)
		}
	}
}

func TestKubectlBootstrapRunnerPollsAndRedactsWaitFailures(t *testing.T) {
	secret := "abcdef.0123456789abcdef"
	commands := &fakeKubectlCommandRunner{
		defaultResult: &readiness.CommandResult{ExitStatus: 1, Stderr: "still missing token " + secret},
		results: []readiness.CommandResult{
			{ExitStatus: 1, Stderr: "missing token " + secret},
			{ExitStatus: 1, Stderr: "still missing token " + secret},
		},
	}
	_, err := (KubectlBootstrapRunner{
		CommandRunner: commands,
		TempDir:       t.TempDir(),
		Timeout:       2 * time.Millisecond,
		PollInterval:  time.Millisecond,
		ProbeTimeout:  time.Millisecond,
	}).RunUserBootstrap(context.Background(), BootstrapRequest{
		Server: "10.0.0.11:6443",
		Credentials: AdminCredentials{
			CertificateAuthorityData: testCA,
			ClientCertificateData:    testCert,
			ClientKeyData:            testKey,
		},
		Waits: []BootstrapWait{{Kind: BootstrapWaitResourceExists, Namespace: "kube-system", Name: "daemonset/cilium"}},
	})
	if err == nil {
		t.Fatal("RunUserBootstrap() error = nil, want wait timeout")
	}
	if strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "[REDACTED BOOTSTRAP TOKEN]") {
		t.Fatalf("error = %q, want redacted token", err.Error())
	}
	if len(commands.calls) < 2 {
		t.Fatalf("kubectl calls = %#v, want polling retry", commands.calls)
	}
}

func phaseNames(phases []Phase) []string {
	names := make([]string, len(phases))
	for i, phase := range phases {
		names[i] = phase.Name
	}
	return names
}

func manifestPaths(manifests []BootstrapManifest) []string {
	paths := make([]string, len(manifests))
	for i, manifest := range manifests {
		paths[i] = manifest.Path
	}
	return paths
}

func containsString(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func validBootstrapManifest(name string) string {
	return `apiVersion: v1
kind: ConfigMap
metadata:
  name: ` + name + `
`
}

func writeBootstrapManifest(t *testing.T, name, objectName string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(validBootstrapManifest(objectName)), 0o644); err != nil {
		t.Fatalf("write bootstrap manifest: %v", err)
	}
	return path
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func validInventory() inventory.Inventory {
	return inventory.Inventory{
		ControlPlaneEndpoint: "api.katl.test:6443",
		KubernetesVersion:    "v1.36.1",
		Nodes: []inventory.Node{
			{
				Name:              "cp-1",
				Address:           "10.0.0.11",
				SystemRole:        inventory.RoleControlPlane,
				Access:            inventory.Access{Method: "agent"},
				KubeadmConfig:     inventory.KubeadmConfig{Ref: "control-plane", Path: "/etc/katl/kubeadm/control-plane/config.yaml", Intent: inventory.IntentControlPlane},
				KubernetesVersion: "v1.36.1",
			},
			{
				Name:              "worker-1",
				Address:           "10.0.0.21",
				SystemRole:        inventory.RoleWorker,
				Access:            inventory.Access{Method: "agent"},
				KubeadmConfig:     inventory.KubeadmConfig{Ref: "worker", Path: "/etc/katl/kubeadm/worker/config.yaml", Intent: inventory.IntentWorker},
				KubernetesVersion: "v1.36.1",
			},
		},
	}
}

func validSingleNodeInventory() inventory.Inventory {
	inv := validInventory()
	inv.ControlPlaneEndpoint = ""
	inv.Nodes = inv.Nodes[:1]
	return inv
}

func validPlannedNode(name string, action inventory.BootstrapAction) inventory.PlannedNode {
	role := inventory.RoleControlPlane
	intent := inventory.IntentControlPlane
	configRef := "control-plane"
	if action == inventory.ActionWorkerJoin {
		role = inventory.RoleWorker
		intent = inventory.IntentWorker
		configRef = "worker"
	}
	return inventory.PlannedNode{
		Name:              name,
		Address:           "10.0.0.11",
		SystemRole:        role,
		Action:            action,
		Access:            inventory.Access{Method: "agent", CredentialRef: "agent/" + name},
		KubeadmConfig:     inventory.KubeadmConfig{Ref: configRef, Path: "/etc/katl/kubeadm/" + configRef + "/config.yaml", Intent: intent},
		KubernetesVersion: "v1.36.1",
	}
}

type readyChecker struct{}

func (readyChecker) CheckReadiness(_ context.Context, node inventory.PlannedNode) (inventory.ReadinessSnapshot, error) {
	return inventory.ReadinessSnapshot{
		KatlKubeadmReadyTarget: true,
		KubernetesSysextActive: true,
		KubeadmConfigExists:    true,
		ContainerdActive:       true,
		CRIResponsive:          true,
		KubeletInstalled:       true,
		EtcKubernetesWritable:  true,
		EtcKubernetesProjected: true,
		KubernetesVersion:      node.KubernetesVersion,
		SystemRole:             node.SystemRole,
		KubeadmConfigIntent:    node.KubeadmConfig.Intent,
	}, nil
}

type failingChecker struct {
	snapshot inventory.ReadinessSnapshot
}

func (c failingChecker) CheckReadiness(_ context.Context, _ inventory.PlannedNode) (inventory.ReadinessSnapshot, error) {
	return c.snapshot, nil
}

type fakeNodeRunner struct {
	calls                []string
	events               *[]string
	credentials          AdminCredentials
	join                 JoinMaterial
	controlPlaneJoin     JoinMaterial
	err                  error
	apiErr               error
	controlPlaneReadyErr error
	workerReadyErr       error
}

func (r *fakeNodeRunner) RunKubeadmInit(_ context.Context, node inventory.PlannedNode, _ string) (AdminCredentials, error) {
	r.record("init:" + node.Name)
	if r.err != nil {
		return AdminCredentials{}, r.err
	}
	return r.credentials, nil
}

func (r *fakeNodeRunner) CreateWorkerJoin(_ context.Context, initNode inventory.PlannedNode) (JoinMaterial, error) {
	r.record("join-material:" + initNode.Name)
	if r.err != nil {
		return JoinMaterial{}, r.err
	}
	return r.join, nil
}

func (r *fakeNodeRunner) CreateControlPlaneJoin(_ context.Context, initNode inventory.PlannedNode) (JoinMaterial, error) {
	r.record("join-control-plane-material:" + initNode.Name)
	if r.err != nil {
		return JoinMaterial{}, r.err
	}
	return r.controlPlaneJoin, nil
}

func (r *fakeNodeRunner) RunControlPlaneJoin(_ context.Context, node inventory.PlannedNode, _ JoinMaterial) error {
	r.record("join-control-plane:" + node.Name)
	return r.err
}

func (r *fakeNodeRunner) WaitControlPlaneJoinReady(_ context.Context, _ inventory.PlannedNode, node inventory.PlannedNode) error {
	r.record("ready-control-plane:" + node.Name)
	if r.controlPlaneReadyErr != nil {
		return r.controlPlaneReadyErr
	}
	return r.err
}

func (r *fakeNodeRunner) RunWorkerJoin(_ context.Context, node inventory.PlannedNode, _ JoinMaterial) error {
	r.record("join-worker:" + node.Name)
	return r.err
}

func (r *fakeNodeRunner) WaitWorkerJoinReady(_ context.Context, _ inventory.PlannedNode, node inventory.PlannedNode) error {
	r.record("ready-worker:" + node.Name)
	if r.workerReadyErr != nil {
		return r.workerReadyErr
	}
	return r.err
}

func (r *fakeNodeRunner) WaitAPIReady(_ context.Context, initNode inventory.PlannedNode) error {
	r.record("ready:" + initNode.Name)
	if r.apiErr != nil {
		return r.apiErr
	}
	return r.err
}

func (r *fakeNodeRunner) record(call string) {
	r.calls = append(r.calls, call)
	if r.events != nil {
		*r.events = append(*r.events, call)
	}
}

type fakeBootstrapRunner struct {
	events   *[]string
	requests []BootstrapRequest
	result   BootstrapResult
	err      error
}

func (r *fakeBootstrapRunner) RunUserBootstrap(_ context.Context, request BootstrapRequest) (BootstrapResult, error) {
	r.requests = append(r.requests, request)
	if r.events != nil {
		*r.events = append(*r.events, "bootstrap")
	}
	if r.err != nil {
		return BootstrapResult{}, r.err
	}
	return r.result, nil
}

type fakeKubectlCommandRunner struct {
	calls         [][]string
	results       []readiness.CommandResult
	defaultResult *readiness.CommandResult
}

func (r *fakeKubectlCommandRunner) Run(_ context.Context, argv []string) (readiness.CommandResult, error) {
	r.calls = append(r.calls, append([]string(nil), argv...))
	if len(r.results) > 0 {
		result := r.results[0]
		r.results = r.results[1:]
		return result, nil
	}
	if r.defaultResult != nil {
		return *r.defaultResult, nil
	}
	return readiness.CommandResult{ExitStatus: 0}, nil
}

type fakeTransport struct {
	commands       map[string]readiness.CommandResult
	commandResults map[string][]readiness.CommandResult
	commandCalls   map[string]int
	files          map[string][]byte
	writes         map[string][]byte
}

func newFakeTransport() *fakeTransport {
	return &fakeTransport{
		commands:       map[string]readiness.CommandResult{},
		commandResults: map[string][]readiness.CommandResult{},
		commandCalls:   map[string]int{},
		files:          map[string][]byte{},
		writes:         map[string][]byte{},
	}
}

func (t *fakeTransport) RunCommand(_ context.Context, _ inventory.PlannedNode, req readiness.CommandRequest) (readiness.CommandResult, error) {
	key := commandKey(req.Argv...)
	t.commandCalls[key]++
	if results := t.commandResults[key]; len(results) > 0 {
		result := results[0]
		t.commandResults[key] = results[1:]
		return result, nil
	}
	result, ok := t.commands[key]
	if !ok {
		return readiness.CommandResult{ExitStatus: 127, Stderr: "unexpected command"}, nil
	}
	return result, nil
}

func (t *fakeTransport) ReadFile(_ context.Context, _ inventory.PlannedNode, req readiness.FileRequest) (readiness.FileResult, error) {
	data, ok := t.files[req.Path]
	if !ok {
		return readiness.FileResult{}, os.ErrNotExist
	}
	return readiness.FileResult{Content: data, Redaction: "sensitive"}, nil
}

func (t *fakeTransport) WriteFile(_ context.Context, _ inventory.PlannedNode, req readiness.WriteFileRequest) (readiness.WriteFileResult, error) {
	t.writes[req.Path] = req.Content
	t.files[req.Path] = req.Content
	return readiness.WriteFileResult{SizeBytes: uint32(len(req.Content)), Redaction: "sensitive"}, nil
}

func (t *fakeTransport) commandCount(key string) int {
	return t.commandCalls[key]
}

func commandKey(argv ...string) string {
	return strings.Join(argv, "\x00")
}

func testWorkerJoinConfig() string {
	return `apiVersion: kubeadm.k8s.io/v1beta4
kind: JoinConfiguration
nodeRegistration:
  criSocket: unix:///run/containerd/containerd.sock
`
}

func testControlPlaneJoinConfig() string {
	return `apiVersion: kubeadm.k8s.io/v1beta4
kind: JoinConfiguration
controlPlane: {}
nodeRegistration:
  criSocket: unix:///run/containerd/containerd.sock
`
}

func testControlPlaneInitConfig() string {
	return `apiVersion: kubeadm.k8s.io/v1beta4
kind: InitConfiguration
nodeRegistration:
  name: cp-2
  criSocket: unix:///run/containerd/containerd.sock
---
apiVersion: kubeadm.k8s.io/v1beta4
kind: ClusterConfiguration
clusterName: katl-smoke
---
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
cgroupDriver: systemd
`
}

func TestRenderControlPlaneJoinCarriesOperatorPatchDirectory(t *testing.T) {
	base := []byte(`apiVersion: kubeadm.k8s.io/v1beta4
kind: InitConfiguration
nodeRegistration:
  criSocket: unix:///run/containerd/containerd.sock
patches:
  directory: /etc/katl/kubeadm/control-plane/patches
`)
	material := JoinMaterial{Argv: []string{
		"kubeadm", "join", "api.katl.test:6443",
		"--token", "abcdef.0123456789abcdef",
		"--discovery-token-ca-cert-hash", testDiscoveryHash,
		"--control-plane",
		"--certificate-key", strings.Repeat("a", 64),
	}}
	rendered, err := RenderControlPlaneJoinConfig(base, material)
	if err != nil {
		t.Fatalf("RenderControlPlaneJoinConfig() error = %v", err)
	}
	if !strings.Contains(string(rendered), "directory: /etc/katl/kubeadm/control-plane/patches") {
		t.Fatalf("rendered join config did not retain patches directory:\n%s", rendered)
	}
}

func adminKubeconfig() string {
	return `apiVersion: v1
kind: Config
clusters:
- name: katl
  cluster:
    certificate-authority-data: ` + testCA + `
users:
- name: katl-admin
  user:
    client-certificate-data: ` + testCert + `
    client-key-data: ` + testKey + `
contexts:
- name: katl
  context:
    cluster: katl
    user: katl-admin
current-context: katl
`
}
