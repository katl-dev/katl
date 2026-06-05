package inventory

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestPlanInventorySelectsInitAndClassifiesNodes(t *testing.T) {
	plan, err := PlanInventory(PlanRequest{
		Inventory:       validInventory(),
		InitNode:        "cp-1",
		AddressOverride: map[string]string{"worker-1": "10.0.0.22"},
	})
	if err != nil {
		t.Fatalf("PlanInventory() error = %v", err)
	}
	if plan.InitNode != "cp-1" {
		t.Fatalf("init node = %q", plan.InitNode)
	}
	if plan.ControlPlaneEndpoint != "api.katl.test:6443" {
		t.Fatalf("control-plane endpoint = %q", plan.ControlPlaneEndpoint)
	}
	if len(plan.Nodes) != 2 {
		t.Fatalf("nodes len = %d", len(plan.Nodes))
	}
	assertNode(t, plan.Nodes[0], "cp-1", "10.0.0.11", RoleControlPlane, ActionInit)
	assertNode(t, plan.Nodes[1], "worker-1", "10.0.0.22", RoleWorker, ActionWorkerJoin)
	if len(plan.AddressOverrides) != 1 || plan.AddressOverrides[0].Node != "worker-1" || plan.AddressOverrides[0].Before != "10.0.0.21" {
		t.Fatalf("address overrides = %#v", plan.AddressOverrides)
	}
}

func TestPlanInventoryDefaultsSingleControlPlaneInit(t *testing.T) {
	plan, err := PlanInventory(PlanRequest{Inventory: validInventory()})
	if err != nil {
		t.Fatalf("PlanInventory() error = %v", err)
	}
	if plan.InitNode != "cp-1" {
		t.Fatalf("init node = %q", plan.InitNode)
	}
}

func TestPlanInventoryClassifiesAdditionalControlPlaneJoin(t *testing.T) {
	inv := validInventory()
	inv.Nodes = append(inv.Nodes, Node{
		Name:              "cp-2",
		Address:           "10.0.0.12",
		SystemRole:        RoleControlPlane,
		Access:            Access{Method: "ssh", User: "core", CredentialRef: "ssh/cp-2"},
		KubeadmConfig:     KubeadmConfig{Ref: "control-plane", Intent: IntentControlPlane},
		KubernetesVersion: "v1.36.1",
	})
	plan, err := PlanInventory(PlanRequest{Inventory: inv, InitNode: "cp-1"})
	if err != nil {
		t.Fatalf("PlanInventory() error = %v", err)
	}
	assertNode(t, plan.Nodes[2], "cp-2", "10.0.0.12", RoleControlPlane, ActionControlPlaneJoin)
}

func TestPlanInventoryResolvesNodeKubernetesVersion(t *testing.T) {
	inv := validInventory()
	inv.KubernetesVersion = ""

	plan, err := PlanInventory(PlanRequest{Inventory: inv})
	if err != nil {
		t.Fatalf("PlanInventory() error = %v", err)
	}
	if plan.KubernetesVersion != "v1.36.1" {
		t.Fatalf("KubernetesVersion = %q", plan.KubernetesVersion)
	}
}

func TestPlanInventoryAppliesAddressOverrideBeforeValidation(t *testing.T) {
	inv := validInventory()
	inv.Nodes[1].Address = ""
	plan, err := PlanInventory(PlanRequest{
		Inventory:       inv,
		AddressOverride: map[string]string{"worker-1": "10.0.0.99"},
	})
	if err != nil {
		t.Fatalf("PlanInventory() error = %v", err)
	}
	if got := plan.Nodes[1].Address; got != "10.0.0.99" {
		t.Fatalf("worker address = %q", got)
	}
	if len(plan.AddressOverrides) != 1 || plan.AddressOverrides[0].Before != "" || plan.AddressOverrides[0].Address != "10.0.0.99" {
		t.Fatalf("address overrides = %#v", plan.AddressOverrides)
	}
}

func TestPlanInventoryRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name string
		mut  func(Inventory) PlanRequest
		want string
	}{
		{
			name: "duplicate names",
			mut: func(inv Inventory) PlanRequest {
				inv.Nodes[1].Name = "cp-1"
				return PlanRequest{Inventory: inv}
			},
			want: "duplicate node name",
		},
		{
			name: "missing address",
			mut: func(inv Inventory) PlanRequest {
				inv.Nodes[0].Address = ""
				return PlanRequest{Inventory: inv}
			},
			want: "address is required",
		},
		{
			name: "invalid node name",
			mut: func(inv Inventory) PlanRequest {
				inv.Nodes[0].Name = "CP_1"
				return PlanRequest{Inventory: inv}
			},
			want: "DNS-style label",
		},
		{
			name: "no control plane",
			mut: func(inv Inventory) PlanRequest {
				inv.Nodes = inv.Nodes[1:]
				return PlanRequest{Inventory: inv}
			},
			want: "at least one control-plane",
		},
		{
			name: "multiple implicit control planes",
			mut: func(inv Inventory) PlanRequest {
				inv.Nodes = append(inv.Nodes, Node{
					Name:              "cp-2",
					Address:           "10.0.0.12",
					SystemRole:        RoleControlPlane,
					Access:            Access{Method: "ssh", User: "core", CredentialRef: "ssh/cp-2"},
					KubeadmConfig:     KubeadmConfig{Ref: "control-plane", Intent: IntentControlPlane},
					KubernetesVersion: "v1.36.1",
				})
				return PlanRequest{Inventory: inv}
			},
			want: "multiple control-plane nodes",
		},
		{
			name: "init node worker",
			mut: func(inv Inventory) PlanRequest {
				return PlanRequest{Inventory: inv, InitNode: "worker-1"}
			},
			want: "must be a control-plane",
		},
		{
			name: "unsupported role",
			mut: func(inv Inventory) PlanRequest {
				inv.Nodes[0].SystemRole = "storage"
				return PlanRequest{Inventory: inv}
			},
			want: "unsupported",
		},
		{
			name: "missing endpoint for multi-node",
			mut: func(inv Inventory) PlanRequest {
				inv.ControlPlaneEndpoint = ""
				return PlanRequest{Inventory: inv}
			},
			want: "control-plane endpoint is required",
		},
		{
			name: "inconsistent Kubernetes version",
			mut: func(inv Inventory) PlanRequest {
				inv.Nodes[1].KubernetesVersion = "v1.35.9"
				return PlanRequest{Inventory: inv}
			},
			want: "does not match inventory version",
		},
		{
			name: "inconsistent node Kubernetes versions without inventory default",
			mut: func(inv Inventory) PlanRequest {
				inv.KubernetesVersion = ""
				inv.Nodes[1].KubernetesVersion = "v1.35.9"
				return PlanRequest{Inventory: inv}
			},
			want: "does not match inventory version",
		},
		{
			name: "role config mismatch",
			mut: func(inv Inventory) PlanRequest {
				inv.Nodes[1].KubeadmConfig.Intent = IntentControlPlane
				return PlanRequest{Inventory: inv}
			},
			want: "systemRole worker requires worker",
		},
		{
			name: "unsupported access method",
			mut: func(inv Inventory) PlanRequest {
				inv.Nodes[0].Access.Method = "http"
				return PlanRequest{Inventory: inv}
			},
			want: "access method",
		},
		{
			name: "inline access secret",
			mut: func(inv Inventory) PlanRequest {
				inv.Nodes[0].Access.CredentialRef = "abcdef.0123456789abcdef"
				return PlanRequest{Inventory: inv}
			},
			want: "inline secret material",
		},
		{
			name: "kubeadm config path outside katl",
			mut: func(inv Inventory) PlanRequest {
				inv.Nodes[0].KubeadmConfig.Path = "/etc/kubernetes/admin.conf"
				return PlanRequest{Inventory: inv}
			},
			want: "must be under /etc/katl/kubeadm",
		},
		{
			name: "endpoint URL rejected",
			mut: func(inv Inventory) PlanRequest {
				inv.ControlPlaneEndpoint = "https://api.katl.test:6443/path"
				return PlanRequest{Inventory: inv}
			},
			want: "host:port",
		},
		{
			name: "unknown override",
			mut: func(inv Inventory) PlanRequest {
				return PlanRequest{Inventory: inv, AddressOverride: map[string]string{"missing": "10.0.0.99"}}
			},
			want: "unknown node",
		},
		{
			name: "empty override",
			mut: func(inv Inventory) PlanRequest {
				return PlanRequest{Inventory: inv, AddressOverride: map[string]string{"worker-1": " "}}
			},
			want: "address override",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := PlanInventory(tt.mut(validInventory()))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("PlanInventory() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestVerifyReadinessAcceptsReadyNodes(t *testing.T) {
	plan, err := PlanInventory(PlanRequest{Inventory: validInventory()})
	if err != nil {
		t.Fatal(err)
	}
	report, err := VerifyReadiness(context.Background(), plan, fakeChecker{snapshots: readySnapshots(plan)})
	if err != nil {
		t.Fatalf("VerifyReadiness() error = %v", err)
	}
	if !report.Ready {
		t.Fatalf("report = %#v", report)
	}
	if err := Error(report); err != nil {
		t.Fatalf("Error() = %v", err)
	}
}

func TestVerifyReadinessReportsNotReadyNodes(t *testing.T) {
	plan, err := PlanInventory(PlanRequest{Inventory: validInventory()})
	if err != nil {
		t.Fatal(err)
	}
	snapshots := readySnapshots(plan)
	snapshot := snapshots["worker-1"]
	snapshot.KatlKubeadmReadyTarget = false
	snapshot.CRIResponsive = false
	snapshot.KubernetesVersion = "v1.35.9"
	snapshot.Diagnostics = []Diagnostic{{Field: "journal", Message: "token=abcdef.0123456789abcdef https://user:pass@example.invalid/path?secret=yes"}}
	snapshots["worker-1"] = snapshot

	report, err := VerifyReadiness(context.Background(), plan, fakeChecker{snapshots: snapshots})
	if err != nil {
		t.Fatalf("VerifyReadiness() error = %v", err)
	}
	if report.Ready {
		t.Fatalf("report.Ready = true, want false")
	}
	err = Error(report)
	if err == nil {
		t.Fatal("Error() = nil, want readiness error")
	}
	text := err.Error()
	for _, want := range []string{"katl-kubeadm-ready.target", "CRI socket", "v1.35.9", "[REDACTED BOOTSTRAP TOKEN]", "https://example.invalid/path"} {
		if !strings.Contains(text, want) {
			t.Fatalf("readiness error %q missing %q", text, want)
		}
	}
	if strings.Contains(text, "abcdef.0123456789abcdef") || strings.Contains(text, "user:pass") || strings.Contains(text, "secret=yes") {
		t.Fatalf("readiness error was not redacted: %q", text)
	}
}

func TestVerifyReadinessReportsAccessFailure(t *testing.T) {
	plan, err := PlanInventory(PlanRequest{Inventory: validInventory()})
	if err != nil {
		t.Fatal(err)
	}
	report, err := VerifyReadiness(context.Background(), plan, fakeChecker{err: errors.New("ssh failed with token abcdef.0123456789abcdef")})
	if err != nil {
		t.Fatalf("VerifyReadiness() error = %v", err)
	}
	if report.Ready || len(report.Nodes) != 2 {
		t.Fatalf("report = %#v", report)
	}
	if got := report.Nodes[0].Diagnostics[0].Message; strings.Contains(got, "abcdef.0123456789abcdef") || !strings.Contains(got, "[REDACTED BOOTSTRAP TOKEN]") {
		t.Fatalf("diagnostic was not redacted: %q", got)
	}
}

func TestRedactRemovesBootstrapSecretMaterial(t *testing.T) {
	input := strings.Join([]string{
		"token abcdef.0123456789abcdef",
		"discovery-token-ca-cert-hash sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"certificateKey=0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"Authorization: Bearer eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.secret",
		"client-certificate-data: LS0tCERT",
		"client-key-data: LS0tKEY",
		"-----BEGIN PRIVATE KEY-----\nsecret\n-----END PRIVATE KEY-----",
	}, "\n")

	got := Redact(input)
	for _, leaked := range []string{
		"abcdef.0123456789abcdef",
		"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.secret",
		"LS0tCERT",
		"LS0tKEY",
		"BEGIN PRIVATE KEY",
		"END PRIVATE KEY",
	} {
		if strings.Contains(got, leaked) {
			t.Fatalf("Redact() leaked %q in %q", leaked, got)
		}
	}
	for _, want := range []string{
		"[REDACTED BOOTSTRAP TOKEN]",
		"[REDACTED DISCOVERY TOKEN HASH]",
		"certificateKey=[REDACTED]",
		"Bearer [REDACTED]",
		"client-certificate-data: [REDACTED]",
		"client-key-data: [REDACTED]",
		"[REDACTED PRIVATE KEY]",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("Redact() = %q, missing %q", got, want)
		}
	}
}

func validInventory() Inventory {
	return Inventory{
		ControlPlaneEndpoint: "api.katl.test:6443",
		KubernetesVersion:    "v1.36.1",
		Nodes: []Node{
			{
				Name:              "cp-1",
				Address:           "10.0.0.11",
				SystemRole:        RoleControlPlane,
				Access:            Access{Method: "ssh", User: "core", CredentialRef: "ssh/cp-1"},
				KubeadmConfig:     KubeadmConfig{Ref: "control-plane", Path: "/etc/katl/kubeadm/control-plane/config.yaml", Intent: IntentControlPlane},
				KubernetesVersion: "v1.36.1",
			},
			{
				Name:              "worker-1",
				Address:           "10.0.0.21",
				SystemRole:        RoleWorker,
				Access:            Access{Method: "ssh", User: "core", CredentialRef: "ssh/worker-1"},
				KubeadmConfig:     KubeadmConfig{Ref: "worker", Path: "/etc/katl/kubeadm/worker/config.yaml", Intent: IntentWorker},
				KubernetesVersion: "v1.36.1",
			},
		},
	}
}

func assertNode(t *testing.T, node PlannedNode, name, address string, role SystemRole, action BootstrapAction) {
	t.Helper()
	if node.Name != name || node.Address != address || node.SystemRole != role || node.Action != action {
		t.Fatalf("node = %#v, want name=%s address=%s role=%s action=%s", node, name, address, role, action)
	}
}

func readySnapshots(plan Plan) map[string]ReadinessSnapshot {
	snapshots := make(map[string]ReadinessSnapshot, len(plan.Nodes))
	for _, node := range plan.Nodes {
		snapshots[node.Name] = ReadinessSnapshot{
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
		}
	}
	return snapshots
}

type fakeChecker struct {
	snapshots map[string]ReadinessSnapshot
	err       error
}

func (f fakeChecker) CheckReadiness(_ context.Context, node PlannedNode) (ReadinessSnapshot, error) {
	if f.err != nil {
		return ReadinessSnapshot{}, f.err
	}
	return f.snapshots[node.Name], nil
}
