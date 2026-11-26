package cluster

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
)

// The workstation reaches the init node only at its management host, which
// NAT translates or a second NIC holds, and which is in no serving
// certificate. The operator kubeconfig must dial that host's API proxy and
// verify a name the certificate carries, whatever port the agent uses.
func TestOperatorKubeconfigReachesTranslatedControlPlane(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		management           string
		kubernetes           string
		controlPlaneEndpoint string
		server, serverName   string
	}{
		{name: "NAT with an agent port", management: "198.18.0.11:19443", kubernetes: "10.0.0.11", server: "https://198.18.0.11:7445", serverName: "10.0.0.11"},
		{name: "multihomed with an endpoint name", management: "192.168.1.11:19443", kubernetes: "10.0.0.11", controlPlaneEndpoint: "api.katl.test:6443", server: "https://192.168.1.11:7445", serverName: "api.katl.test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &fakeAgentClient{
				status: readyAgentStatus("machine-cp-1"),
				accepted: &agentapi.OperationAccepted{
					OperationId: "bootstrap-init-1", RequestDigest: "digest-1",
					InitialStatus: &agentapi.OperationStatus{OperationId: "bootstrap-init-1"},
				},
				events: []*agentapi.OperationEvent{{
					OperationId: "bootstrap-init-1", JournalSeq: 1, Terminal: true,
					Status: &agentapi.OperationStatus{OperationId: "bootstrap-init-1", Terminal: true, Result: "succeeded"},
				}},
				getStatus: &agentapi.OperationStatus{OperationId: "bootstrap-init-1", Terminal: true, Result: "succeeded", AdminKubeconfig: adminKubeconfig()},
			}
			inv := validSingleNodeInventory(t)
			inv.Nodes[0].Address = mustManagementAddress(t, tc.management)
			inv.Nodes[0].KubernetesAddress = mustKubernetesAddr(t, tc.kubernetes)
			if tc.controlPlaneEndpoint != "" {
				inv.ControlPlaneEndpoint = mustHostPort(t, tc.controlPlaneEndpoint)
			}
			out := filepath.Join(t.TempDir(), "operator.conf")
			bootstrapRunner := &fakeBootstrapRunner{}

			result, err := RunAgentBootstrap(context.Background(), Request{Inventory: inv, KubeconfigOut: out, OverwriteKubeconfig: true}, AgentBootstrapDependencies{
				Connector:       newFakeAgentConnector(map[string]*fakeAgentClient{"cp-1": client}),
				BootstrapRunner: bootstrapRunner,
			})
			if err != nil {
				t.Fatal(err)
			}

			if len(bootstrapRunner.requests) == 0 || bootstrapRunner.requests[0].Server != strings.TrimPrefix(tc.server, "https://") || bootstrapRunner.requests[0].TLSServerName != tc.serverName {
				t.Fatalf("API proxy check = %#v, want %s verified as %s", bootstrapRunner.requests, tc.server, tc.serverName)
			}
			written, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"server: " + tc.server, "tls-server-name: " + tc.serverName} {
				if !strings.Contains(string(written), want) {
					t.Errorf("kubeconfig lacks %q:\n%s", want, written)
				}
			}
			if result.Kubeconfig.Access != "node-proxy" {
				t.Errorf("kubeconfig access = %q, want node-proxy", result.Kubeconfig.Access)
			}
		})
	}
}
