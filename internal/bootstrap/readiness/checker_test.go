package readiness

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/zariel/katl/internal/bootstrap/inventory"
)

func TestCheckerReportsReadyNode(t *testing.T) {
	node := readyNode()
	checker := Checker{Agent: readyTransport(node)}

	snapshot, err := checker.CheckReadiness(context.Background(), node)
	if err != nil {
		t.Fatalf("CheckReadiness() error = %v", err)
	}
	if !snapshot.KatlKubeadmReadyTarget ||
		!snapshot.KubernetesSysextActive ||
		!snapshot.KubeadmConfigExists ||
		!snapshot.ContainerdActive ||
		!snapshot.CRIResponsive ||
		!snapshot.KubeletInstalled ||
		!snapshot.EtcKubernetesWritable ||
		!snapshot.EtcKubernetesProjected {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if snapshot.KubernetesVersion != "v1.36.1" {
		t.Fatalf("KubernetesVersion = %q", snapshot.KubernetesVersion)
	}
	if snapshot.SystemRole != inventory.RoleControlPlane || snapshot.KubeadmConfigIntent != inventory.IntentControlPlane {
		t.Fatalf("role/intent = %s/%s", snapshot.SystemRole, snapshot.KubeadmConfigIntent)
	}
	if !checker.Agent.(*fakeTransport).seen[key("test", "-e", "/run/extensions/selected-kubernetes.raw")] {
		t.Fatalf("selected Kubernetes sysext path was not probed: %#v", checker.Agent.(*fakeTransport).seen)
	}

	report, err := inventory.VerifyReadiness(context.Background(), inventory.Plan{Nodes: []inventory.PlannedNode{node}}, checker)
	if err != nil {
		t.Fatalf("VerifyReadiness() error = %v", err)
	}
	if !report.Ready {
		t.Fatalf("report = %#v", report)
	}
}

func TestCheckerDefaultsKubeadmConfigPathFromRef(t *testing.T) {
	node := readyNode()
	node.KubeadmConfig.Path = ""
	transport := readyTransport(node)

	snapshot, err := Checker{Agent: transport}.CheckReadiness(context.Background(), node)
	if err != nil {
		t.Fatalf("CheckReadiness() error = %v", err)
	}
	if !snapshot.KubeadmConfigExists || snapshot.KubeadmConfigIntent != inventory.IntentControlPlane {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if !transport.seen[key("test", "-f", "/etc/katl/kubeadm/control-plane/config.yaml")] {
		t.Fatalf("derived config path was not probed: %#v", transport.seen)
	}
}

func TestCheckerReportsNotReadyPrerequisites(t *testing.T) {
	node := readyNode()
	transport := readyTransport(node)
	transport.commands[key("systemctl", "is-active", "--quiet", "katl-kubeadm-ready.target")] = CommandResult{ExitStatus: 3, Stderr: "inactive"}
	transport.commands[key("crictl", "info")] = CommandResult{ExitStatus: 1, Stderr: "runtime not ready"}
	transport.commands[key("findmnt", "--noheadings", "--target", "/etc/kubernetes", "--output", "SOURCE")] = CommandResult{ExitStatus: 0, Stdout: "/etc/kubernetes\n"}

	report, err := inventory.VerifyReadiness(context.Background(), inventory.Plan{Nodes: []inventory.PlannedNode{node}}, Checker{Agent: transport})
	if err != nil {
		t.Fatalf("VerifyReadiness() error = %v", err)
	}
	if report.Ready {
		t.Fatalf("report.Ready = true, want false")
	}
	err = inventory.Error(report)
	if err == nil {
		t.Fatal("Error() = nil, want readiness error")
	}
	text := err.Error()
	for _, want := range []string{"katl-kubeadm-ready.target", "CRI socket", "/etc/kubernetes is backed by"} {
		if !strings.Contains(text, want) {
			t.Fatalf("readiness error %q missing %q", text, want)
		}
	}
}

func TestCheckerReportsInactiveKubernetesSysext(t *testing.T) {
	node := readyNode()
	transport := readyTransport(node)
	transport.commands[key("test", "-e", "/run/extensions/selected-kubernetes.raw")] = CommandResult{ExitStatus: 1, Stderr: "missing sysext"}

	report, err := inventory.VerifyReadiness(context.Background(), inventory.Plan{Nodes: []inventory.PlannedNode{node}}, Checker{Agent: transport})
	if err != nil {
		t.Fatalf("VerifyReadiness() error = %v", err)
	}
	if report.Ready {
		t.Fatalf("report.Ready = true, want false")
	}
	text := inventory.Error(report).Error()
	if !strings.Contains(text, "selected Kubernetes sysext is not active") || !strings.Contains(text, "/run/extensions/selected-kubernetes.raw") {
		t.Fatalf("readiness error = %q", text)
	}
}

func TestCheckerReportsAccessFailure(t *testing.T) {
	node := readyNode()
	transport := readyTransport(node)
	transport.errs[key("systemctl", "is-active", "--quiet", "katl-kubeadm-ready.target")] = errors.New("agent failed with Bearer secret-token")

	report, err := inventory.VerifyReadiness(context.Background(), inventory.Plan{Nodes: []inventory.PlannedNode{node}}, Checker{Agent: transport})
	if err != nil {
		t.Fatalf("VerifyReadiness() error = %v", err)
	}
	if report.Ready || len(report.Nodes) != 1 {
		t.Fatalf("report = %#v", report)
	}
	got := report.Nodes[0].Diagnostics[0].Message
	if strings.Contains(got, "secret-token") || !strings.Contains(got, "Bearer [REDACTED]") {
		t.Fatalf("access diagnostic was not redacted: %q", got)
	}
}

func TestCheckerReportsIncompleteNodeMetadata(t *testing.T) {
	node := readyNode()
	transport := readyTransport(node)
	transport.files[NodeMetadataPath] = []byte(`{"apiVersion":"katl.dev/v1alpha1","kind":"NodeMetadata"}`)

	report, err := inventory.VerifyReadiness(context.Background(), inventory.Plan{Nodes: []inventory.PlannedNode{node}}, Checker{Agent: transport})
	if err != nil {
		t.Fatalf("VerifyReadiness() error = %v", err)
	}
	if report.Ready {
		t.Fatalf("report.Ready = true, want false")
	}
	text := inventory.Error(report).Error()
	for _, want := range []string{
		"metadata systemRole is required",
		"metadata kubernetes.payloadVersion is required",
		"metadata kubernetes.activationPath is required",
		"metadata kubeadm.configRef is required",
		"metadata kubeadm.configPath is required",
		"metadata kubeadm.intent is required",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("readiness error %q missing %q", text, want)
		}
	}
}

func TestCheckerReportsVersionRoleAndIntentMismatch(t *testing.T) {
	node := readyNode()
	transport := readyTransport(node)
	transport.commands[key("kubeadm", "version", "-o", "short")] = CommandResult{ExitStatus: 0, Stdout: "v1.35.9\n"}
	transport.files[node.KubeadmConfig.Path] = []byte("apiVersion: kubeadm.k8s.io/v1beta4\nkind: JoinConfiguration\n")
	transport.files[NodeMetadataPath] = []byte(`{"apiVersion":"katl.dev/v1alpha1","kind":"NodeMetadata","systemRole":"worker","kubeadm":{"configRef":"worker","configPath":"/etc/katl/kubeadm/worker/config.yaml","intent":"worker"},"kubernetes":{"payloadVersion":"v1.35.9","activationPath":"/run/extensions/selected-kubernetes.raw"}}`)

	report, err := inventory.VerifyReadiness(context.Background(), inventory.Plan{Nodes: []inventory.PlannedNode{node}}, Checker{Agent: transport})
	if err != nil {
		t.Fatalf("VerifyReadiness() error = %v", err)
	}
	if report.Ready {
		t.Fatalf("report.Ready = true, want false")
	}
	text := inventory.Error(report).Error()
	for _, want := range []string{"kubernetesVersion", "systemRole", "kubeadm-config", "node-metadata"} {
		if !strings.Contains(text, want) {
			t.Fatalf("readiness error %q missing %q", text, want)
		}
	}
}

func TestCheckerRejectsUnsafeKubernetesActivationPath(t *testing.T) {
	for _, activationPath := range []string{"/etc/passwd", "/run/extensions/../passwd"} {
		t.Run(activationPath, func(t *testing.T) {
			node := readyNode()
			transport := readyTransport(node)
			transport.files[NodeMetadataPath] = []byte(`{"apiVersion":"katl.dev/v1alpha1","kind":"NodeMetadata","systemRole":"control-plane","kubeadm":{"configRef":"control-plane","configPath":"/etc/katl/kubeadm/control-plane/config.yaml","intent":"control-plane"},"kubernetes":{"payloadVersion":"v1.36.1","activationPath":"` + activationPath + `"}}`)

			report, err := inventory.VerifyReadiness(context.Background(), inventory.Plan{Nodes: []inventory.PlannedNode{node}}, Checker{Agent: transport})
			if err != nil {
				t.Fatalf("VerifyReadiness() error = %v", err)
			}
			if report.Ready {
				t.Fatalf("report.Ready = true, want false")
			}
			text := inventory.Error(report).Error()
			if !strings.Contains(text, "metadata kubernetes.activationPath") || transport.seen[key("test", "-e", activationPath)] {
				t.Fatalf("readiness error = %q, seen = %#v", text, transport.seen)
			}
		})
	}
}

func TestCheckerRedactsCommandDiagnostics(t *testing.T) {
	node := readyNode()
	transport := readyTransport(node)
	transport.commands[key("crictl", "info")] = CommandResult{
		ExitStatus: 1,
		Stderr:     "Authorization: Bearer token-secret discovery-token-ca-cert-hash sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}

	report, err := inventory.VerifyReadiness(context.Background(), inventory.Plan{Nodes: []inventory.PlannedNode{node}}, Checker{Agent: transport})
	if err != nil {
		t.Fatalf("VerifyReadiness() error = %v", err)
	}
	text := inventory.Error(report).Error()
	for _, leaked := range []string{"token-secret", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"} {
		if strings.Contains(text, leaked) {
			t.Fatalf("diagnostic leaked %q in %q", leaked, text)
		}
	}
	for _, want := range []string{"Bearer [REDACTED]", "[REDACTED DISCOVERY TOKEN HASH]"} {
		if !strings.Contains(text, want) {
			t.Fatalf("diagnostic %q missing %q", text, want)
		}
	}
}

func readyNode() inventory.PlannedNode {
	return inventory.PlannedNode{
		Name:              "cp-1",
		Address:           "10.0.0.11",
		SystemRole:        inventory.RoleControlPlane,
		Access:            inventory.Access{Method: "agent", CredentialRef: "vmtest/cp-1"},
		KubeadmConfig:     inventory.KubeadmConfig{Ref: "control-plane", Path: "/etc/katl/kubeadm/control-plane/config.yaml", Intent: inventory.IntentControlPlane},
		KubernetesVersion: "v1.36.1",
	}
}

func readyTransport(node inventory.PlannedNode) *fakeTransport {
	configPath := kubeadmConfigPath(node)
	transport := &fakeTransport{
		commands: map[string]CommandResult{
			key("systemctl", "is-active", "--quiet", "katl-kubeadm-ready.target"):               {ExitStatus: 0},
			key("test", "-e", "/run/extensions/selected-kubernetes.raw"):                        {ExitStatus: 0},
			key("kubeadm", "version", "-o", "short"):                                            {ExitStatus: 0, Stdout: node.KubernetesVersion + "\n"},
			key("test", "-f", configPath):                                                       {ExitStatus: 0},
			key("systemctl", "is-active", "--quiet", "containerd.service"):                      {ExitStatus: 0},
			key("crictl", "info"):                                                               {ExitStatus: 0, Stdout: "{}\n"},
			key("test", "-x", "/usr/bin/kubelet"):                                               {ExitStatus: 0},
			key("systemctl", "cat", "kubelet.service"):                                          {ExitStatus: 0, Stdout: "[Service]\n"},
			key("test", "-w", "/etc/kubernetes"):                                                {ExitStatus: 0},
			key("findmnt", "--noheadings", "--target", "/etc/kubernetes", "--output", "SOURCE"): {ExitStatus: 0, Stdout: "/dev/vdb4[/lib/katl/kubernetes/etc-kubernetes]\n"},
			key("test", "-f", NodeMetadataPath):                                                 {ExitStatus: 0},
		},
		errs: map[string]error{},
		seen: map[string]bool{},
		files: map[string][]byte{
			configPath:       []byte("apiVersion: kubeadm.k8s.io/v1beta4\nkind: InitConfiguration\n"),
			NodeMetadataPath: []byte(`{"apiVersion":"katl.dev/v1alpha1","kind":"NodeMetadata","identity":{"hostname":"cp-1"},"systemRole":"control-plane","kubeadm":{"configRef":"control-plane","configPath":"` + configPath + `","intent":"control-plane"},"kubernetes":{"payloadVersion":"v1.36.1","activationPath":"/run/extensions/selected-kubernetes.raw"}}`),
		},
	}
	return transport
}

type fakeTransport struct {
	commands map[string]CommandResult
	errs     map[string]error
	seen     map[string]bool
	files    map[string][]byte
}

func (t *fakeTransport) RunCommand(_ context.Context, _ inventory.PlannedNode, req CommandRequest) (CommandResult, error) {
	commandKey := key(req.Argv...)
	t.seen[commandKey] = true
	if err := t.errs[commandKey]; err != nil {
		return CommandResult{}, err
	}
	result, ok := t.commands[commandKey]
	if !ok {
		return CommandResult{ExitStatus: 127, Stderr: "unexpected command: " + strings.Join(req.Argv, " ")}, nil
	}
	return result, nil
}

func (t *fakeTransport) ReadFile(_ context.Context, _ inventory.PlannedNode, req FileRequest) (FileResult, error) {
	content, ok := t.files[req.Path]
	if !ok {
		return FileResult{}, errors.New("missing file " + req.Path)
	}
	return FileResult{Content: content}, nil
}

func (t *fakeTransport) WriteFile(_ context.Context, _ inventory.PlannedNode, req WriteFileRequest) (WriteFileResult, error) {
	t.files[req.Path] = req.Content
	return WriteFileResult{SizeBytes: uint32(len(req.Content))}, nil
}

func key(argv ...string) string {
	return strings.Join(argv, "\x00")
}
