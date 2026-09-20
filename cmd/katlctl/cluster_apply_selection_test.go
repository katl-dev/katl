package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/installer/configapply"
	"github.com/katl-dev/katl/internal/installer/operation"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/katlctl/workstation"
)

func TestClusterApplySelection(t *testing.T) {
	for _, test := range []struct {
		name      string
		args      []string
		want      []string
		wantError string
	}{
		{name: "default", want: []string{"cp-1", "cp-2", "worker-1"}},
		{name: "one", args: []string{"--node", "cp-1"}, want: []string{"cp-1"}},
		{name: "unselected coordinator", args: []string{"--node", "cp-1", "--coordinator", "cp-2"}, want: []string{"cp-1"}, wantError: "not selected"},
		{name: "repeated", args: []string{"--node", "cp-2", "--node", "cp-1"}, want: []string{"cp-1", "cp-2"}},
		{name: "duplicate", args: []string{"--node", "cp-1", "--node", "cp-1"}, want: []string{"cp-1"}},
		{name: "worker", args: []string{"--node", "worker-1"}, want: []string{"worker-1"}},
		{name: "unknown", args: []string{"--node", "missing"}, wantError: "missing"},
		{name: "empty", args: []string{"--node", ""}, wantError: "node"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := writeClusterConfig(t)
			writeTestEnrollmentContext(t, "lab",
				workstation.Node{Name: "cp-1", ManagementEndpoint: "10.0.0.11:9443", SystemRole: inventory.RoleControlPlane, EnrollmentID: "enrollment-cp-1", MachineID: "machine-cp-1"},
				workstation.Node{Name: "cp-2", ManagementEndpoint: "10.0.0.12:9443", SystemRole: inventory.RoleControlPlane, EnrollmentID: "enrollment-cp-2", MachineID: "machine-cp-2"},
				workstation.Node{Name: "worker-1", ManagementEndpoint: "10.0.0.13:9443", SystemRole: inventory.RoleWorker, EnrollmentID: "enrollment-worker-1", MachineID: "machine-worker-1"},
			)
			source := configBundleSource() + `    - name: cp-2
      controlPlane: true
      management:
        address: 10.0.0.12
      install:
        systemDisk:
          byID: /dev/disk/by-id/ata-cp-2-root
    - name: worker-1
      management:
        address: 10.0.0.13
      install:
        systemDisk:
          byID: /dev/disk/by-id/ata-worker-root
      kubernetes:
        kubelet:
          configFile: kubelet.yaml
`
			if err := os.WriteFile(path, []byte(source), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(filepath.Dir(path), "kubelet.yaml"), []byte("apiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\nmaxPods: 111\n"), 0600); err != nil {
				t.Fatal(err)
			}
			clients := map[string]*fakeKatlcAgentClient{}
			for _, name := range test.want {
				configName := "control-plane"
				if name == "worker-1" {
					configName = "node-worker-1"
				}
				clients[name] = &fakeKatlcAgentClient{
					nodeStatus:      &agentapi.NodeStatus{MachineId: "machine-" + name, CurrentGenerationId: "generation-1", Kubernetes: &agentapi.KubernetesStatus{State: "ready"}},
					validateResult:  &agentapi.ConfigValidationResult{Accepted: true, AcceptedApplyMode: "live", ChangedDomains: []string{configapply.DomainKubeadmConfig}},
					submitAccepted:  &agentapi.OperationAccepted{OperationId: "apply-" + name},
					operationStatus: &agentapi.OperationStatus{Terminal: true, Result: operation.ResultSucceeded},
					generation:      &agentapi.Generation{GenerationId: "generation-1", CommitState: "committed", HealthState: "healthy", ConfigApply: &agentapi.ConfigApplyStatus{SelectedKubeadmConfigName: configName}, Sysexts: []*agentapi.ExtensionRef{{Name: "kubernetes", PayloadVersion: "v1.36.1", Sha256: strings.Repeat("a", 64)}}},
				}
			}
			oldDial := dialKatlcAgent
			t.Cleanup(func() { dialKatlcAgent = oldDial })
			dialKatlcAgent = func(_ context.Context, endpoint string) (katlcAgentConnection, error) {
				name := map[string]string{"10.0.0.11:9443": "cp-1", "10.0.0.12:9443": "cp-2", "10.0.0.13:9443": "worker-1"}[endpoint]
				client := clients[name]
				if client == nil {
					t.Fatalf("contacted unselected node %q", name)
				}
				return katlcAgentConnection{Client: client, Close: func() error { return nil }}, nil
			}

			var stdout, stderr bytes.Buffer
			args := append([]string{"cluster", "apply", "--config", path}, test.args...)
			err := run(context.Background(), args, &stdout, &stderr)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("error = %v", err)
				}
				for name, client := range clients {
					if len(client.submitRequests) != 0 {
						t.Fatalf("rejected selection mutated %s", name)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("apply: %v\n%s", err, stderr.String())
			}
			var report struct {
				Nodes  int
				Result string
			}
			if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
				t.Fatal(err)
			}
			if report.Nodes != len(test.want) || report.Result != "succeeded" {
				t.Fatalf("report = %+v", report)
			}
			for name, client := range clients {
				if client.validateRequest == nil {
					t.Fatalf("%s not validated", name)
				}
				var applies int
				components := map[string]bool{}
				for _, request := range client.submitRequests {
					if request.DryRun {
						continue
					}
					if request.OperationKind == "generation-apply" {
						applies++
					}
					if body := request.KubeadmControlPlaneConfig; body != nil {
						for _, component := range body.SupportedFieldDelta {
							components[component] = true
						}
						if name == "worker-1" && (!body.NodeLocalKubelet || body.CoordinatorUpload) {
							t.Fatal("worker kubelet configuration was not node-local")
						}
						if len(test.want) == 1 && name != "worker-1" && !body.CoordinatorUpload {
							t.Fatal("selected control plane did not publish shared configuration")
						}
					}
				}
				if applies != 1 || components["component/control-plane"] != (name != "worker-1") || !components["component/kubelet"] {
					t.Fatalf("%s applies=%d components=%v", name, applies, components)
				}
			}
		})
	}
}

func TestClusterApplyHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"cluster", "apply", "--help"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	help := stdout.String()
	for _, want := range []string{"--node NAME", "--config", "next boot", "katlctl cluster apply --config cluster.yaml"} {
		if !strings.Contains(help, want) {
			t.Fatalf("missing %q in help: %s", want, help)
		}
	}
	for _, unwanted := range []string{"wipe", "--rebind-volume", "authorize"} {
		if strings.Contains(help, unwanted) {
			t.Fatalf("routine help includes %q", unwanted)
		}
	}
}
