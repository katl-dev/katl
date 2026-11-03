package agent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer"
	"github.com/katl-dev/katl/internal/installer/configapply"
	"github.com/katl-dev/katl/internal/installer/kubeadmconfig"
	"github.com/katl-dev/katl/internal/installer/kubeadmplan"
	"github.com/katl-dev/katl/internal/installer/operation"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/managementidentity"
)

func externalWorkerRequest(t *testing.T, server *testServer, executor *Executor) *agentapi.SubmitOperationRequest {
	t.Helper()
	seedBootstrapRuntimeRootForRole(t, server.Root, "worker")
	configuration, err := configapply.ReadGenerationConfiguration(server.Root, "0")
	if err != nil {
		t.Fatal(err)
	}
	configuration.Manifest.Node.Kubernetes.External = true
	input, err := kubeadmconfig.PlanFromRenderedFiles("default", []kubeadmconfig.File{{RenderPath: "/etc/katl/kubeadm/default/config.yaml", Mode: 0o644, Content: []byte("apiVersion: kubeadm.k8s.io/v1beta4\nkind: JoinConfiguration\n---\napiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\nclusterDNS: [10.43.0.10]\nclusterDomain: external.internal\n")}})
	if err != nil {
		t.Fatal(err)
	}
	configuration.KubeadmConfigs["default"] = input
	if err := configapply.WriteGenerationConfiguration(server.Root, "0", configuration); err != nil {
		t.Fatal(err)
	}
	if _, err := installer.WriteClusterIntent(installer.ClusterIntentRequest{TargetRoot: server.Root, Manifest: configuration.Manifest, KubeadmConfigs: configuration.KubeadmConfigs, KubernetesVersion: "v1.35.0", GenerationID: "0"}); err != nil {
		t.Fatal(err)
	}
	source, ref := configureExecutorBundle(t, executor, "v1.35.0", "external worker runtime")
	identity, err := managementidentity.Generate(managementidentity.GenerateOptions{ClusterName: "external-test"})
	if err != nil {
		t.Fatal(err)
	}
	bootstrap := fmt.Sprintf("apiVersion: v1\nkind: Config\nclusters:\n- name: external\n  cluster:\n    server: https://node-a.example.test:6443\n    certificate-authority-data: %s\nusers:\n- name: bootstrap\n  user:\n    token: secret-external-bootstrap-material\ncontexts:\n- name: bootstrap\n  context:\n    cluster: external\n    user: bootstrap\ncurrent-context: bootstrap\n", base64.StdEncoding.EncodeToString([]byte(identity.CertificateAuthority.Certificate)))
	req := submitRequest("external-worker")
	req.OperationKind = "bootstrap-join-worker"
	req.Bootstrap.SystemRole = "worker"
	req.Bootstrap.ExternalWorker = true
	req.Bootstrap.ApiProxyConfig = ""
	req.Bootstrap.ControlPlaneEndpoint = "node-a.example.test:6443"
	req.Bootstrap.BootstrapKubeconfig = []byte(bootstrap)
	setSubmitRequestBundle(req, source, ref)
	return req
}

func TestExternalWorkerEnrollment(t *testing.T) {
	server := newTestServer(t)
	executor := NewExecutor(server.Root, server.Store, "external-test")
	executor.Async = false
	executor.Now = server.Now
	req := externalWorkerRequest(t, server, executor)
	executor.RunReadiness = func(context.Context, []string, func(int)) ToolResult { return ToolResult{} }
	starts := 0
	executor.RunTool = func(_ context.Context, argv []string, _ func(int)) ToolResult {
		if argv[0] == "/usr/bin/kubectl" {
			return ToolResult{Stdout: []byte("True")}
		}
		if len(argv) > 1 && argv[1] == "is-active" {
			return ToolResult{}
		}
		if strings.Join(argv, " ") != "/usr/bin/systemctl start --no-block kubelet.service" {
			t.Fatalf("external join invoked unexpected command: %v", argv)
		}
		starts++
		assertFileContains(t, filepath.Join(server.Root, "var/lib/kubelet/config.yaml"), "external.internal")
		assertFileContains(t, filepath.Join(server.Root, "etc/kubernetes/bootstrap-kubelet.conf"), "secret-external-bootstrap-material")
		writeExternalWorkerCertificate(t, server.Root, executor.clock())
		return ToolResult{}
	}
	executor.RunPostHealth = func(context.Context, []string, func(int)) ToolResult { return ToolResult{} }
	executor.ConfigureLocalAPI = func(context.Context, string, operation.BootstrapRequest, ToolRunner) error {
		t.Fatal("external join configured managed API access")
		return nil
	}
	server.Dispatcher = executor
	accepted, err := server.SubmitOperation(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	record, err := server.Store.Read(accepted.OperationId)
	if err != nil {
		t.Fatal(err)
	}
	if !record.Terminal || record.Result != operation.ResultSucceeded {
		t.Fatalf("external join did not finish: %+v", record)
	}
	assertBootstrapGenerationLive(t, server.Root, record.CandidateGenerationID, record.OperationID)
	for _, path := range []string{"etc/kubernetes/bootstrap-kubelet.conf", "run/katl/bootstrap-join/" + record.OperationID + "/config.yaml", "etc/kubernetes/admin.conf"} {
		if _, err := os.Stat(filepath.Join(server.Root, path)); !os.IsNotExist(err) {
			t.Fatalf("unexpected retained %s: %v", path, err)
		}
	}
	err = filepath.WalkDir(server.Store.Root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if bytes.Contains(data, []byte("secret-external-bootstrap-material")) {
				t.Errorf("bootstrap credential persisted in operation artifact %s", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if starts != 1 {
		t.Fatalf("worker starts=%d", starts)
	}
}

func TestExternalWorkerContinuation(t *testing.T) {
	server := newTestServer(t)
	executor := NewExecutor(server.Root, server.Store, "external-test")
	executor.Async = false
	executor.Now = server.Now
	req := externalWorkerRequest(t, server, executor)
	executor.RunReadiness = func(context.Context, []string, func(int)) ToolResult { return ToolResult{} }
	executor.RunTool = func(_ context.Context, argv []string, _ func(int)) ToolResult {
		if argv[0] == "/usr/bin/kubectl" {
			return ToolResult{Stdout: []byte("True")}
		}
		writeExternalWorkerCertificate(t, server.Root, executor.clock())
		return ToolResult{}
	}
	executor.RunPostHealth = func(context.Context, []string, func(int)) ToolResult { return ToolResult{ExitStatus: 1} }
	server.Dispatcher = executor
	accepted, err := server.SubmitOperation(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	previous, err := server.Store.Read(accepted.OperationId)
	if err != nil {
		t.Fatal(err)
	}
	if !bootstrapContinuationEligible(previous) {
		t.Fatalf("native startup not resumable: %+v", previous)
	}
	for i := range previous.Invocations {
		previous.Invocations[i].BootID = "previous-boot"
	}
	if err := server.validateBootstrapContinuation(previous, req); err != nil {
		t.Fatalf("durable enrollment did not survive reboot: %v", err)
	}

	if err := os.Remove(filepath.Join(server.Root, "var/lib/kubelet/pki/kubelet-client-current.pem")); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(server.Root, "etc/kubernetes/bootstrap-kubelet.conf"), string(req.Bootstrap.BootstrapKubeconfig))
	if err := server.validateBootstrapContinuation(previous, req); err != nil {
		t.Fatalf("pending certificate did not resume with original bootstrap identity: %v", err)
	}
	writeTestFile(t, filepath.Join(server.Root, "etc/kubernetes/bootstrap-kubelet.conf"), "replaced bootstrap identity\n")
	if err := server.validateBootstrapContinuation(previous, req); err == nil {
		t.Fatal("early kubelet.conf bypassed bootstrap identity validation")
	}
	writeTestFile(t, filepath.Join(server.Root, "etc/kubernetes/bootstrap-kubelet.conf"), string(req.Bootstrap.BootstrapKubeconfig))
	kubeletConfig, err := os.ReadFile(filepath.Join(server.Root, "var/lib/kubelet/config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(server.Root, "var/lib/kubelet/config.yaml"), "changed configuration\n")
	if err := server.validateBootstrapContinuation(previous, req); err == nil {
		t.Fatal("continued enrollment with changed durable configuration")
	}
	writeTestFile(t, filepath.Join(server.Root, "var/lib/kubelet/config.yaml"), string(kubeletConfig))
	if err := os.Remove(filepath.Join(server.Root, "etc/kubernetes/kubelet.conf")); err != nil {
		t.Fatal(err)
	}
	req.ClientRequestId = "external-worker-refresh"
	req.RequestDigest = ""
	req.Bootstrap.ResumeOperationId = previous.OperationID
	req.Bootstrap.BootstrapKubeconfig = bytes.ReplaceAll(req.Bootstrap.BootstrapKubeconfig, []byte("secret-external-bootstrap-material"), []byte("replacement-bootstrap-token"))
	installedCA, err := os.ReadFile(filepath.Join(server.Root, "etc/kubernetes/pki/ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	otherIdentity, err := managementidentity.Generate(managementidentity.GenerateOptions{ClusterName: "other-cluster"})
	if err != nil {
		t.Fatal(err)
	}
	replacement := req.Bootstrap.BootstrapKubeconfig
	req.Bootstrap.BootstrapKubeconfig = bytes.ReplaceAll(replacement, []byte(base64.StdEncoding.EncodeToString(installedCA)), []byte(base64.StdEncoding.EncodeToString([]byte(otherIdentity.CertificateAuthority.Certificate))))
	if err := server.validateBootstrapContinuation(previous, req); err == nil {
		t.Fatal("credential refresh changed the external cluster CA")
	}
	req.Bootstrap.BootstrapKubeconfig = replacement
	executor.RunTool = func(_ context.Context, argv []string, _ func(int)) ToolResult {
		if argv[0] == "/usr/bin/kubectl" {
			return ToolResult{Stdout: []byte("True")}
		}
		if strings.Join(argv, " ") == "/usr/bin/systemctl restart --no-block kubelet.service" {
			assertFileContains(t, filepath.Join(server.Root, "etc/kubernetes/bootstrap-kubelet.conf"), "replacement-bootstrap-token")
			assertFileContains(t, filepath.Join(server.Root, "var/lib/kubelet/config.yaml"), "external.internal")
			writeExternalWorkerCertificate(t, server.Root, executor.clock())
		}
		return ToolResult{}
	}
	executor.RunPostHealth = func(context.Context, []string, func(int)) ToolResult { return ToolResult{} }
	executor.RunReadiness = func(context.Context, []string, func(int)) ToolResult { return ToolResult{ExitStatus: 1} }
	accepted, err = server.SubmitOperation(context.Background(), req)
	if err != nil {
		t.Fatalf("refresh pending enrollment with new credentials: %v", err)
	}
	interrupted, err := server.Store.Read(accepted.OperationId)
	if err != nil || !bootstrapContinuationEligible(interrupted) {
		t.Fatalf("interrupted refresh is not resumable: record=%+v err=%v", interrupted, err)
	}
	assertFileContains(t, filepath.Join(server.Root, "etc/kubernetes/bootstrap-kubelet.conf"), "secret-external-bootstrap-material")
	if err := os.Remove(rootedRuntimePath(server.Root, interrupted.BootstrapRequest.TemporaryJoinConfigPath)); err != nil {
		t.Fatal(err)
	}
	req.ClientRequestId = "external-worker-refresh-after-interruption"
	req.RequestDigest = ""
	req.Bootstrap.ResumeOperationId = interrupted.OperationID
	executor.RunReadiness = func(context.Context, []string, func(int)) ToolResult { return ToolResult{} }
	accepted, err = server.SubmitOperation(context.Background(), req)
	if err != nil {
		t.Fatalf("retry interrupted credential refresh: %v", err)
	}
	refreshed, err := server.Store.Read(accepted.OperationId)
	if err != nil || refreshed.Result != operation.ResultSucceeded || refreshed.CandidateGenerationID != previous.CandidateGenerationID {
		t.Fatalf("refresh did not complete existing enrollment: record=%+v err=%v", refreshed, err)
	}
}

func writeExternalWorkerCertificate(t *testing.T, root string, now time.Time) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "system:node:node-a", Organization: []string{"system:nodes"}}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	encodedKey, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pair := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encodedKey})...)
	writeTestFile(t, filepath.Join(root, "var/lib/kubelet/pki/kubelet-client-current.pem"), string(pair))
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if installed, err := os.ReadFile(filepath.Join(root, "etc/kubernetes/pki/ca.crt")); err == nil {
		certPEM = installed
	} else {
		writeTestFile(t, filepath.Join(root, "etc/kubernetes/pki/ca.crt"), string(certPEM))
	}
	writeTestFile(t, filepath.Join(root, "etc/kubernetes/kubelet.conf"), "apiVersion: v1\nkind: Config\nclusters:\n- cluster:\n    server: https://node-a.example.test:6443\n    certificate-authority-data: "+base64.StdEncoding.EncodeToString(certPEM)+"\n")
	writeTestFile(t, filepath.Join(root, "etc/hostname"), "node-a\n")
}

func TestExternalWorkerWaitsForCertificate(t *testing.T) {
	server := newTestServer(t)
	executor := NewExecutor(server.Root, server.Store, "external-test")
	req := externalWorkerRequest(t, server, executor)
	server.Dispatcher = dispatchFunc(func(context.Context, operation.OperationRecord) error { return nil })
	accepted, err := server.SubmitOperation(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	record, err := server.Store.Read(accepted.OperationId)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(server.Root, "etc/kubernetes/kubelet.conf"), "bootstrap token kubeconfig written before CSR approval\n")
	writeTestFile(t, filepath.Join(server.Root, "etc/kubernetes/bootstrap-kubelet.conf"), string(req.Bootstrap.BootstrapKubeconfig))
	writeTestFile(t, filepath.Join(server.Root, "etc/hostname"), "node-a\n")
	executor.RunTool = func(context.Context, []string, func(int)) ToolResult { return ToolResult{Stdout: []byte("True")} }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := executor.waitExternalWorkerCertificate(ctx, record); err == nil {
		t.Fatal("accepted a bootstrap kubeconfig without a rotating client certificate")
	}
	if _, err := os.Stat(filepath.Join(server.Root, "etc/kubernetes/bootstrap-kubelet.conf")); err != nil {
		t.Fatalf("pending bootstrap credentials were removed: %v", err)
	}
}

func TestExternalWorkerUpgrade(t *testing.T) {
	root, store, record, now := kubeadmUpgradeFixture(t, "external-worker")
	executor := newUpgradeExecutor(root, store)
	executor.Async = false
	executor.Now = func() time.Time { return now.Add(time.Minute) }
	executor.SetBootDefault = func(context.Context, string, string) error { return nil }
	executor.SetBootOneshot = func(context.Context, string, string) error { return nil }
	executor.RunTool = func(_ context.Context, argv []string, _ func(int)) ToolResult {
		joined := strings.Join(argv, " ")
		if filepath.Base(argv[0]) == "kubeadm" || strings.Contains(joined, "admin.conf") || slices.Contains(argv, "configmap") {
			t.Fatalf("external worker required cluster management: %v", argv)
		}
		if slices.Contains(argv, "--raw=/version") {
			return ToolResult{Stdout: []byte(`{"gitVersion":"v1.36.1"}`)}
		}
		if slices.Contains(argv, "--version") {
			return ToolResult{Stdout: []byte("Kubernetes v1.36.2\n")}
		}
		return ToolResult{}
	}
	if err := executor.Execute(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	completed, err := store.Read(record.OperationID)
	if err != nil || completed.Result != operation.ResultSucceeded {
		t.Fatalf("worker upgrade failed: record=%+v err=%v", completed, err)
	}
	selection, err := generation.ReadBootSelection(root)
	if err != nil || selection.DefaultGenerationID != "gen1" || selection.ActiveGenerationID != "gen1" || selection.PendingHealthValidation {
		t.Fatalf("upgraded runtime is not persistent: selection=%+v err=%v", selection, err)
	}
	spec, status, err := generation.ReadGeneration(root, "gen1")
	if err != nil || status.CommitState != generation.CommitStateCommitted || status.HealthState != generation.HealthStateHealthy {
		t.Fatalf("upgraded generation is not healthy: status=%+v err=%v", status, err)
	}
	ref, ok := kubernetesRef(spec.Sysexts)
	if !ok || ref.PayloadVersion != "v1.36.2" {
		t.Fatalf("target Kubernetes version not selected: %+v", ref)
	}
}

func TestExternalWorkerKubeletConfig(t *testing.T) {
	root := t.TempDir()
	writeKubeletStartup(t, root)
	writeTestFile(t, filepath.Join(root, "etc/hostname"), "worker-1\n")
	writeTestFile(t, filepath.Join(root, "etc/kubernetes/kubelet.conf"), "preserved worker identity\n")
	store, err := operation.NewStore(filepath.Join(root, "var/lib/katl/operations"))
	if err != nil {
		t.Fatal(err)
	}
	desired := "apiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\nclusterDNS: [10.96.0.10]\nclusterDomain: cluster.local\nmaxPods: 87\n"
	writeTestFile(t, filepath.Join(root, "etc/katl/kubeadm/worker/config.yaml"), desired)
	writeTestFile(t, filepath.Join(root, "var/lib/kubelet/config.yaml"), "apiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\nmaxPods: 110\n")
	body := controlPlaneConfigFromProto(validControlPlaneConfigRequest())
	body.ExternalWorker, body.CoordinatorUpload = true, false
	body.Component, body.CoordinatorNode, body.NodeName = "kubelet", "", "worker-1"
	body.ConfigName, body.ConfigPath = "worker", "/etc/katl/kubeadm/worker/config.yaml"
	body.KubernetesPayloadVersion, body.KubernetesPayloadSHA256 = "v1.36.1", strings.Repeat("c", 64)
	body.DesiredConfigSHA256, err = kubeadmplan.CanonicalKubeletConfigurationSHA256([]byte(desired))
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.Create(operation.OperationRecord{OperationID: "external-kubelet", OperationKind: OperationKindKubeadmControlPlaneConfig, Scope: "kubeadm-state", RequestDigest: strings.Repeat("f", 64), Phase: "accepted", KubeadmControlPlaneConfig: &body}, "accepted", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	executor := NewExecutor(root, store, "test")
	executor.Async = false
	restarts := 0
	executor.RunTool = func(_ context.Context, argv []string, _ func(int)) ToolResult {
		if result, ok := readyKubeletHealthProbe(argv); ok {
			return result
		}
		if slices.Contains(argv, "restart") && slices.Contains(argv, "kubelet.service") {
			restarts++
			return ToolResult{}
		}
		t.Fatalf("unexpected external kubelet command: %v", argv)
		return ToolResult{}
	}
	if err := executor.Execute(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	assertFileContains(t, filepath.Join(root, "var/lib/kubelet/config.yaml"), "maxPods: 87")
	assertFileContains(t, filepath.Join(root, "etc/kubernetes/kubelet.conf"), "preserved worker identity")
	repeated, err := store.Create(operation.OperationRecord{OperationID: "external-kubelet-repeat", OperationKind: OperationKindKubeadmControlPlaneConfig, Scope: "kubeadm-state", RequestDigest: strings.Repeat("f", 64), Phase: "accepted", KubeadmControlPlaneConfig: &body}, "accepted", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := executor.Execute(context.Background(), repeated); err != nil {
		t.Fatal(err)
	}
	if restarts != 1 {
		t.Fatalf("unchanged kubelet configuration restarted: restarts=%d", restarts)
	}
}

func TestExternalWorkerVersionSkew(t *testing.T) {
	for _, test := range []struct {
		target string
		valid  bool
	}{
		{"v1.36.2", true},
		{"v1.33.9", true},
		{"v1.37.0", false},
		{"v1.32.9", false},
	} {
		t.Run(test.target, func(t *testing.T) {
			run := func(context.Context, []string, func(int)) ToolResult {
				return ToolResult{Stdout: []byte(`{"gitVersion":"v1.36.1+vendor.1"}`)}
			}
			_, err := externalAPIServerVersion(context.Background(), t.TempDir(), run, test.target)
			if (err == nil) != test.valid {
				t.Fatalf("version skew accepted=%t: %v", err == nil, err)
			}
		})
	}
}

func TestLifecycleRequiresOwnership(t *testing.T) {
	for _, malformed := range []bool{false, true} {
		t.Run(fmt.Sprintf("malformed=%t", malformed), func(t *testing.T) {
			server := newTestServer(t)
			if malformed {
				writeTestFile(t, filepath.Join(server.Root, "var/lib/katl/generations/gen0/manifest.json"), "{malformed")
			}
			req := &agentapi.SubmitOperationRequest{OperationKind: OperationKindKubeadmUpgrade, ExpectedCurrentGenerationId: "gen0", KubernetesSysextUpdate: &agentapi.KubernetesSysextUpdateOperationRequest{UpgradeRole: "worker", TargetPayloadVersion: "v1.36.2"}}
			if _, err := server.planKubernetesSysextUpdateOperation(req, strings.Repeat("a", 64), "upgrade", nil, server.Now()); err == nil || !strings.Contains(err.Error(), "ownership") {
				t.Fatalf("upgrade selected a strategy without ownership: %v", err)
			}
			req.KubeadmControlPlaneConfig = validControlPlaneConfigRequest()
			if _, err := server.validateKubeadmControlPlaneConfigState(req); err == nil || !strings.Contains(err.Error(), "ownership") {
				t.Fatalf("configuration selected a strategy without ownership: %v", err)
			}
		})
	}
}
