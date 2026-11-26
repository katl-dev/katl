package agent

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/installer/configapply"
	"github.com/katl-dev/katl/internal/installer/controlplaneendpoint"
	"github.com/katl-dev/katl/internal/installer/kubeadmconfig"
	"github.com/katl-dev/katl/internal/installer/kubeadmplan"
	"github.com/katl-dev/katl/internal/installer/operation"
)

// After bootstrap, the live ClusterConfiguration carries the SANs bootstrap
// added to the operator's own. A control-plane config apply compares only the
// operator's.
func TestControlPlaneConfigWithOperatorCertSANsAfterBootstrap(t *testing.T) {
	const base = "apiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\nclusterName: katl\nkubernetesVersion: v1.36.1\n"
	for _, tt := range []struct {
		name, sans, wantError string
	}{
		{name: "operator SANs", sans: "apiServer:\n  certSANs: [user-api.example]\n"},
		{name: "operator lists the endpoint host", sans: "apiServer:\n  certSANs: [user-api.example, api.katl.test]\n"},
		{name: "no operator SANs"},
		{name: "added SAN", sans: "apiServer:\n  certSANs: [user-api.example, other.example]\n", wantError: "differs from the running cluster's"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			writeKubeletStartup(t, root)
			store, err := operation.NewStore(filepath.Join(root, "var/lib/katl/operations"))
			if err != nil {
				t.Fatal(err)
			}
			desiredConfig := base + tt.sans

			// The live config is what bootstrap uploads for the operator's input.
			operatorInput := base
			if tt.sans != "" {
				operatorInput += "apiServer:\n  certSANs: [user-api.example]\n"
			}
			live, err := kubeadmconfig.RenderInitConfig([]byte(operatorInput), "api.katl.test:6443", "192.0.2.10")
			if err != nil {
				t.Fatal(err)
			}

			writeKubernetesStatusFile(t, root, "etc/katl/kubeadm/control-plane/config.yaml", desiredConfig)
			writeKubeadmConfigFixture(t, root, "gen-2", "control-plane", desiredConfig, nil)
			configuration, err := configapply.ReadLocalGenerationConfiguration(root, "gen-2")
			if err != nil {
				t.Fatal(err)
			}
			configuration.Manifest.Node.SystemRole = "control-plane"
			configuration.Manifest.Node.ControlPlaneEndpoint = &controlplaneendpoint.Config{
				Host: "api.katl.test", Advertisement: &controlplaneendpoint.Advertisement{VIP: "192.0.2.10"},
			}
			if err := configapply.WriteGenerationConfiguration(root, "gen-2", configuration); err != nil {
				t.Fatal(err)
			}
			writeControlPlaneManifests(t, root)
			body := controlPlaneConfigFromProto(validControlPlaneConfigRequest())
			body.KubernetesPayloadVersion = "v1.36.1"
			body.KubernetesPayloadSHA256 = strings.Repeat("c", 64)
			body.DesiredConfigSHA256, _ = kubeadmplan.CanonicalClusterConfigurationSHA256([]byte(desiredConfig))
			record, err := store.Create(operation.OperationRecord{OperationID: "cp-config-sans", OperationKind: OperationKindKubeadmControlPlaneConfig, Scope: "kubeadm-state", RequestDigest: strings.Repeat("b", 64), Phase: "accepted", KubeadmControlPlaneConfig: &body}, "accepted", time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}

			var commands [][]string
			executor := NewExecutor(root, store, "agent-start")
			executor.Async = false
			executor.RunTool = func(_ context.Context, argv []string, _ func(int)) ToolResult {
				if result, ok := healthyControlPlaneProbe(argv); ok {
					return result
				}
				if result, ok := readyKubeletHealthProbe(argv); ok {
					return result
				}
				if previewControlPlaneTestManifests(t, root, argv, nil) {
					return ToolResult{}
				}
				commands = append(commands, slices.Clone(argv))
				return ToolResult{Stdout: live}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			err = executor.Execute(ctx, record)

			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("Execute() error = %v, want %q", err, tt.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			completed, err := store.Read(record.OperationID)
			if err != nil || !completed.Terminal || completed.Result != operation.ResultSucceeded || completed.MutatingToolRan || len(commands) != 1 {
				t.Fatalf("completed = %#v, commands = %v, err = %v; want a no-change success", completed, commands, err)
			}
		})
	}
}

func TestDesiredEndpointVIP(t *testing.T) {
	root := t.TempDir()
	writeKubeadmConfigFixture(t, root, "gen-2", "control-plane", "apiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\n", nil)
	executor := &Executor{Root: root}
	if vip, err := executor.desiredEndpointVIP("gen-2"); err != nil || vip != "" {
		t.Fatalf("vip = %q, %v; want none without an advertised endpoint", vip, err)
	}
	configuration, err := configapply.ReadLocalGenerationConfiguration(root, "gen-2")
	if err != nil {
		t.Fatal(err)
	}
	configuration.Manifest.Node.SystemRole = "control-plane"
	configuration.Manifest.Node.ControlPlaneEndpoint = &controlplaneendpoint.Config{Host: "api.katl.test", Advertisement: &controlplaneendpoint.Advertisement{VIP: "192.0.2.10"}}
	if err := configapply.WriteGenerationConfiguration(root, "gen-2", configuration); err != nil {
		t.Fatal(err)
	}
	if vip, err := executor.desiredEndpointVIP("gen-2"); err != nil || vip != "192.0.2.10" {
		t.Fatalf("vip = %q, %v", vip, err)
	}
	if _, err := executor.desiredEndpointVIP("missing"); err == nil {
		t.Fatal("unreadable desired generation was ignored")
	}
}
