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

	"github.com/zariel/katl/internal/bootstrap/inventory"
	"github.com/zariel/katl/internal/bootstrap/kubeconfig"
	"github.com/zariel/katl/internal/bootstrap/readiness"
)

const (
	testCA   = "Y2EtZGF0YQ=="
	testCert = "Y2VydC1kYXRh"
	testKey  = "a2V5LWRhdGE="
)

func TestRunBootstrapsInitWorkerAndKubeconfig(t *testing.T) {
	out := filepath.Join(t.TempDir(), "operator.conf")
	nodeRunner := &fakeNodeRunner{
		credentials: AdminCredentials{
			CertificateAuthorityData: testCA,
			ClientCertificateData:    testCert,
			ClientKeyData:            testKey,
		},
		join: JoinMaterial{Argv: []string{"kubeadm", "join", "api.katl.test:6443", "--token", "abcdef.0123456789abcdef", "--discovery-token-ca-cert-hash", "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}},
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
	wantPhases := []string{"plan", "readiness", "kubeadm-init", "api-ready", "join-material", "worker-join", "api-ready-after-join", "kubeconfig"}
	if got := phaseNames(result.Phases); !reflect.DeepEqual(got, wantPhases) {
		t.Fatalf("phases = %#v, want %#v", got, wantPhases)
	}
	if got, want := nodeRunner.calls, []string{
		"init:cp-1",
		"ready:cp-1",
		"join-material:cp-1",
		"join-worker:worker-1",
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
	if len(request.Waits) != 2 || request.Waits[1].Kind != BootstrapWaitStableEndpoint || request.Waits[1].Name != "api.stable.test:6443" {
		t.Fatalf("waits = %#v, want explicit wait plus stable endpoint", request.Waits)
	}
	if result.Kubeconfig.Server != "https://api.stable.test:6443" {
		t.Fatalf("kubeconfig server = %q, want stable endpoint", result.Kubeconfig.Server)
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

func TestRunRejectsAdditionalControlPlaneJoinBeforeReadiness(t *testing.T) {
	inv := validInventory()
	inv.Nodes = append(inv.Nodes, inventory.Node{
		Name:              "cp-2",
		Address:           "10.0.0.12",
		SystemRole:        inventory.RoleControlPlane,
		Access:            inventory.Access{Method: "agent", CredentialRef: "agent/cp-2"},
		KubeadmConfig:     inventory.KubeadmConfig{Ref: "control-plane", Path: "/etc/katl/kubeadm/control-plane/config.yaml", Intent: inventory.IntentControlPlane},
		KubernetesVersion: "v1.36.1",
	})
	_, err := Run(context.Background(), Request{Inventory: inv, InitNode: "cp-1"}, Dependencies{
		ReadinessChecker: readyChecker{},
		NodeRunner:       &fakeNodeRunner{},
	})
	if err == nil || !strings.Contains(err.Error(), "control-plane join") {
		t.Fatalf("Run() error = %v, want unsupported control-plane join", err)
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
	credentials, err := runner.RunKubeadmInit(context.Background(), validPlannedNode("cp-1", inventory.ActionInit))
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
	transport.commands[commandKey("kubeadm", "join", "api.katl.test:6443", "--token", secret, "--config", "/etc/katl/kubeadm/worker/config.yaml")] = readiness.CommandResult{
		ExitStatus: 1,
		Stderr:     "join failed",
	}
	err = (TransportRunner{Transport: transport}).RunWorkerJoin(context.Background(), validPlannedNode("worker-1", inventory.ActionWorkerJoin), JoinMaterial{
		Argv: []string{"kubeadm", "join", "api.katl.test:6443", "--token", secret},
	})
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "[REDACTED BOOTSTRAP TOKEN]") {
		t.Fatalf("RunWorkerJoin() error = %v, want redacted argv failure", err)
	}
}

func TestTransportRunnerContinuesWhenInitAlreadyCompleted(t *testing.T) {
	transport := newFakeTransport()
	transport.commands[commandKey("kubeadm", "init", "--config", "/etc/katl/kubeadm/control-plane/config.yaml")] = readiness.CommandResult{
		ExitStatus: 1,
		Stderr:     "this node is already initialized",
	}
	transport.files[adminKubeconfigPath] = []byte(adminKubeconfig())
	credentials, err := (TransportRunner{Transport: transport}).RunKubeadmInit(context.Background(), validPlannedNode("cp-1", inventory.ActionInit))
	if err != nil {
		t.Fatalf("RunKubeadmInit() error = %v", err)
	}
	if credentials.ClientCertificateData != testCert {
		t.Fatalf("credentials = %#v", credentials)
	}
}

func TestTransportRunnerSkipsAlreadyJoinedWorker(t *testing.T) {
	transport := newFakeTransport()
	transport.commands[commandKey("kubeadm", "join", "api.katl.test:6443", "--token", "abcdef.0123456789abcdef", "--config", "/etc/katl/kubeadm/worker/config.yaml")] = readiness.CommandResult{
		ExitStatus: 1,
		Stderr:     "node is already joined",
	}
	transport.commands[commandKey("test", "-f", "/etc/kubernetes/kubelet.conf")] = readiness.CommandResult{ExitStatus: 0}
	transport.commands[commandKey("systemctl", "is-active", "--quiet", "kubelet.service")] = readiness.CommandResult{ExitStatus: 0}
	err := (TransportRunner{Transport: transport}).RunWorkerJoin(context.Background(), validPlannedNode("worker-1", inventory.ActionWorkerJoin), JoinMaterial{
		Argv: []string{"kubeadm", "join", "api.katl.test:6443", "--token", "abcdef.0123456789abcdef"},
	})
	if err != nil {
		t.Fatalf("RunWorkerJoin() error = %v", err)
	}
}

func TestTransportRunnerRejectsAmbiguousJoinAlreadyExists(t *testing.T) {
	transport := newFakeTransport()
	transport.commands[commandKey("kubeadm", "join", "api.katl.test:6443", "--token", "abcdef.0123456789abcdef", "--config", "/etc/katl/kubeadm/worker/config.yaml")] = readiness.CommandResult{
		ExitStatus: 1,
		Stderr:     "/etc/kubernetes/kubelet.conf already exists",
	}
	err := (TransportRunner{Transport: transport}).RunWorkerJoin(context.Background(), validPlannedNode("worker-1", inventory.ActionWorkerJoin), JoinMaterial{
		Argv: []string{"kubeadm", "join", "api.katl.test:6443", "--token", "abcdef.0123456789abcdef"},
	})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("RunWorkerJoin() error = %v, want non-idempotent failure", err)
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
			{Kind: BootstrapWaitResourceExists, Namespace: "kube-system", Name: "daemonset/cilium"},
			{Kind: BootstrapWaitCondition, Namespace: "kube-system", Name: "deployment/cilium-operator", Condition: "Available"},
			{Kind: BootstrapWaitNodesReady},
			{Kind: BootstrapWaitStableEndpoint, Name: "api.stable.test:6443"},
		},
	})
	if err != nil {
		t.Fatalf("RunUserBootstrap() error = %v", err)
	}
	if len(result.AppliedManifests) != 1 || len(result.Waits) != 5 || !result.StableEndpointReady {
		t.Fatalf("result = %#v", result)
	}
	if len(commands.calls) != 6 {
		t.Fatalf("kubectl calls = %#v, want 6 calls", commands.calls)
	}
	if got := commands.calls[0][5:]; !reflect.DeepEqual(got, []string{"apply", "-f", filepath.Join(dir, "0000.yaml")}) {
		t.Fatalf("first kubectl args = %#v, want apply", commands.calls[0])
	}
	if got := commands.calls[1][5:]; !reflect.DeepEqual(got, []string{"get", "--raw=/readyz"}) {
		t.Fatalf("api ready args = %#v", commands.calls[1])
	}
	if got := commands.calls[2][5:]; !reflect.DeepEqual(got, []string{"-n", "kube-system", "get", "daemonset/cilium"}) {
		t.Fatalf("resource wait args = %#v", commands.calls[2])
	}
	if got := commands.calls[3][5:]; !reflect.DeepEqual(got, []string{"-n", "kube-system", "wait", "--for=condition=Available", "deployment/cilium-operator", "--timeout=5m"}) {
		t.Fatalf("condition wait args = %#v", commands.calls[3])
	}
	if got := commands.calls[4][5:]; !reflect.DeepEqual(got, []string{"wait", "--for=condition=Ready", "nodes", "--all", "--timeout=5m"}) {
		t.Fatalf("nodes wait args = %#v", commands.calls[4])
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
	stableKubeconfig := readFileString(t, commands.calls[5][2])
	if !strings.Contains(stableKubeconfig, "server: https://api.stable.test:6443") {
		t.Fatalf("stable bootstrap kubeconfig did not normalize server:\n%s", stableKubeconfig)
	}
}

func TestKubectlBootstrapRunnerPollsAndRedactsWaitFailures(t *testing.T) {
	secret := "abcdef.0123456789abcdef"
	commands := &fakeKubectlCommandRunner{
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
				Access:            inventory.Access{Method: "agent", CredentialRef: "agent/cp-1"},
				KubeadmConfig:     inventory.KubeadmConfig{Ref: "control-plane", Path: "/etc/katl/kubeadm/control-plane/config.yaml", Intent: inventory.IntentControlPlane},
				KubernetesVersion: "v1.36.1",
			},
			{
				Name:              "worker-1",
				Address:           "10.0.0.21",
				SystemRole:        inventory.RoleWorker,
				Access:            inventory.Access{Method: "agent", CredentialRef: "agent/worker-1"},
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
	calls       []string
	events      *[]string
	credentials AdminCredentials
	join        JoinMaterial
	err         error
	apiErr      error
}

func (r *fakeNodeRunner) RunKubeadmInit(_ context.Context, node inventory.PlannedNode) (AdminCredentials, error) {
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

func (r *fakeNodeRunner) RunWorkerJoin(_ context.Context, node inventory.PlannedNode, _ JoinMaterial) error {
	r.record("join-worker:" + node.Name)
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
	calls   [][]string
	results []readiness.CommandResult
}

func (r *fakeKubectlCommandRunner) Run(_ context.Context, argv []string) (readiness.CommandResult, error) {
	r.calls = append(r.calls, append([]string(nil), argv...))
	if len(r.results) > 0 {
		result := r.results[0]
		r.results = r.results[1:]
		return result, nil
	}
	return readiness.CommandResult{ExitStatus: 0}, nil
}

type fakeTransport struct {
	commands       map[string]readiness.CommandResult
	commandResults map[string][]readiness.CommandResult
	commandCalls   map[string]int
	files          map[string][]byte
}

func newFakeTransport() *fakeTransport {
	return &fakeTransport{
		commands:       map[string]readiness.CommandResult{},
		commandResults: map[string][]readiness.CommandResult{},
		commandCalls:   map[string]int{},
		files:          map[string][]byte{},
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

func (t *fakeTransport) commandCount(key string) int {
	return t.commandCalls[key]
}

func commandKey(argv ...string) string {
	return strings.Join(argv, "\x00")
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
