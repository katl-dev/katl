package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/installer"
	"github.com/katl-dev/katl/internal/installer/bootstrapplan"
	"github.com/katl-dev/katl/internal/installer/configapply"
	"github.com/katl-dev/katl/internal/installer/configbundle"
	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/installer/operation"
)

// The generation a bootstrap or join prepares stores the node's kubeadm
// input. Applying the unedited source config afterwards must find it unchanged.
func TestClusterApplyAfterBootstrapKeepsKubeadmConfig(t *testing.T) {
	for _, test := range []struct {
		name string
		node string
		role string
	}{
		{name: "init", node: "cp-1", role: "control-plane"},
		{name: "worker join", node: "worker-1", role: "worker"},
	} {
		t.Run(test.name, func(t *testing.T) {
			sourcePath := filepath.Join(t.TempDir(), "cluster.yaml")
			source := `apiVersion: config.katl.dev/v1alpha1
kind: ClusterConfig
metadata:
  name: lab
spec:
  managementAuthentication: mtls
  controlPlaneEndpoint:
    host: api.katl.test
    port: 6443
  kubernetes:
    version: v1.36.1
  defaults:
    install:
      systemDisk:
        minSizeMiB: 32768
    access:
      ssh:
        authorizedKeys:
          - ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDAxMjM0NTY3ODlhYmNkZWYwMTIzNDU2Nzg5YWJjZGVm katl@example
  nodes:
    - name: cp-1
      controlPlane: true
      management:
        address: 10.0.0.11
      kubernetes:
        address: 10.1.0.11
        labels:
          example.test/tier: control
        taints:
          - key: dedicated
            value: control
            effect: NoSchedule
      install:
        systemDisk:
          byID: /dev/disk/by-id/ata-cp-root
    - name: worker-1
      management:
        address: 10.0.0.13
      kubernetes:
        address: 10.1.0.13
        labels:
          example.test/tier: worker
      install:
        systemDisk:
          byID: /dev/disk/by-id/ata-worker-root
`
			if err := os.WriteFile(sourcePath, []byte(source), 0o600); err != nil {
				t.Fatal(err)
			}

			// Install stores the node's bundle input as generation 0.
			archive, _, err := configbundle.BuildArchive(configbundle.BuildRequest{SourcePath: sourcePath})
			if err != nil {
				t.Fatal(err)
			}
			selected, err := configbundle.ReadSelectedNode(bytes.NewReader(archive), configbundle.ReadOptions{NodeName: test.node, AllowMissingKatlosImage: true})
			if err != nil {
				t.Fatal(err)
			}
			selected.InstallManifest.KatlosImage = manifest.KatlosImage{LocalRef: "images/katlos.raw", SHA256: strings.Repeat("a", 64), SizeBytes: 1024, Version: "0.1.0", Architecture: "x86_64", Role: "install"}
			root := t.TempDir()
			if err := configapply.WriteGenerationConfiguration(root, "0", configapply.GenerationConfiguration{Manifest: selected.InstallManifest, KubeadmConfigs: selected.KubeadmConfigs}); err != nil {
				t.Fatal(err)
			}

			// Bootstrap preserves its selected input as the candidate's configuration.
			ref := selected.InstallManifest.Node.Kubernetes.Kubeadm.ConfigRef
			selection, err := bootstrapplan.SelectInput(root, "0", installer.ClusterIntent{
				GenerationID: "0",
				SystemRole:   test.role,
				Inventory:    installer.ClusterIntentInventory{NodeName: test.node},
			}, operation.BootstrapRequest{InventoryNodeName: test.node, SystemRole: test.role, BootstrapProfileRef: ref})
			if err != nil {
				t.Fatal(err)
			}
			if err := configapply.WriteGenerationConfiguration(root, "1", selection.Configuration); err != nil {
				t.Fatal(err)
			}
			current, err := configapply.ReadLocalGenerationConfiguration(root, "1")
			if err != nil {
				t.Fatal(err)
			}

			loaded, err := loadNodeConfiguration(context.Background(), sourcePath, configbundle.PlanningInputs{Nodes: []string{test.node}})
			if err != nil {
				t.Fatal(err)
			}
			render := loaded.Nodes[test.node]
			render.DesiredVersion = "2"
			change, err := configapply.RenderNodeConfigurationChange(render)
			if err != nil {
				t.Fatal(err)
			}
			request, err := configapply.DecodeNodeConfigurationChange(bytes.NewReader(change), configapply.TrustedBundleRequest{
				NodeName:        test.node,
				CurrentManifest: current.Manifest,
				KubeadmConfigs:  current.KubeadmConfigs,
			})
			if err != nil {
				t.Fatal(err)
			}
			if request.NodeOverrides[test.node].KubeadmChanged {
				t.Fatalf("unedited config changes kubeadm input %q:\nstored:\n%s\napplied:\n%s", ref, current.KubeadmConfigs[ref].Config.Content, request.KubeadmConfigs[ref].Config.Content)
			}
		})
	}
}
