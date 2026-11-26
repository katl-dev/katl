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
	"github.com/katl-dev/katl/internal/installer/kubeadmconfig"
	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/installer/operation"
)

const registeredClusterSource = `apiVersion: config.katl.dev/v1alpha1
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

// A generation stores a node's kubeadm input with the node's registration
// (address, labels, taints) applied, or, after an apply by an earlier katlctl,
// without it. Registration belongs to the node's identity and metadata, so
// planning the same config again finds no kubeadm change.
func TestClusterApplyAfterBootstrapKeepsKubeadmConfig(t *testing.T) {
	for _, test := range []struct {
		name string
		node string
		role string
		// edit rewrites the source config before the apply.
		edit [2]string
		// shared stores the input an earlier katlctl applied, without registration.
		shared bool
		// drift changes the stored kubeadm configuration itself.
		drift bool
		want  bool
	}{
		{name: "init", node: "cp-1", role: "control-plane"},
		{name: "worker join", node: "worker-1", role: "worker"},
		{name: "label edit", node: "cp-1", role: "control-plane", edit: [2]string{"tier: control", "tier: edited"}},
		{name: "taint value edit", node: "cp-1", role: "control-plane", edit: [2]string{"            value: control\n", ""}},
		{name: "labels cleared", node: "worker-1", role: "worker", edit: [2]string{"        labels:\n          example.test/tier: worker\n", ""}},
		{name: "taints cleared", node: "cp-1", role: "control-plane", edit: [2]string{"        taints:\n          - key: dedicated\n            value: control\n            effect: NoSchedule\n", ""}},
		{name: "stored shared input", node: "worker-1", role: "worker", shared: true},
		{name: "kubeadm drift", node: "cp-1", role: "control-plane", drift: true, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			sourcePath := filepath.Join(t.TempDir(), "cluster.yaml")
			if err := os.WriteFile(sourcePath, []byte(registeredClusterSource), 0o600); err != nil {
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
			ref := selected.InstallManifest.Node.Kubernetes.Kubeadm.ConfigRef
			installed, err := kubeadmconfig.ForNode(selected.KubeadmConfigs[ref], selected.InstallManifest.Node)
			if err != nil {
				t.Fatal(err)
			}
			if err := configapply.WriteGenerationConfiguration(root, "0", configapply.GenerationConfiguration{Manifest: selected.InstallManifest, Kubeadm: installed}); err != nil {
				t.Fatal(err)
			}

			// Bootstrap preserves its selected input as the candidate's configuration.
			selection, err := bootstrapplan.SelectInput(root, "0", installer.ClusterIntent{
				GenerationID: "0",
				SystemRole:   test.role,
				Inventory:    installer.ClusterIntentInventory{NodeName: test.node},
			}, operation.BootstrapRequest{InventoryNodeName: test.node, SystemRole: test.role, BootstrapProfileRef: ref})
			if err != nil {
				t.Fatal(err)
			}
			stored := selection.Configuration
			if err := configapply.WriteGenerationConfiguration(root, "1", stored); err != nil {
				t.Fatal(err)
			}
			if test.shared {
				writeStoredInput(t, root, "1", sharedKubeadmInput(t, test.node, ref))
			}
			if test.drift {
				plan := stored.Kubeadm.Plan()
				drifted := strings.Replace(string(plan.Config.Content), "10.244.0.0/16", "10.245.0.0/16", 1)
				if drifted == string(plan.Config.Content) {
					t.Fatal("drift did not change the stored input")
				}
				plan.Config.Content = []byte(drifted)
				writeStoredInput(t, root, "1", plan)
			}
			current, err := configapply.ReadLocalGenerationConfiguration(root, "1")
			if err != nil {
				t.Fatal(err)
			}

			if test.edit[0] != "" {
				edited := strings.Replace(registeredClusterSource, test.edit[0], test.edit[1], 1)
				if edited == registeredClusterSource {
					t.Fatalf("edit %q did not apply", test.edit[0])
				}
				if err := os.WriteFile(sourcePath, []byte(edited), 0o600); err != nil {
					t.Fatal(err)
				}
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
				KubeadmConfigs:  map[string]kubeadmconfig.Plan{ref: current.Kubeadm.Plan()},
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := request.NodeOverrides[test.node].KubeadmChanged; got != test.want {
				t.Fatalf("kubeadm change = %t, want %t:\nstored:\n%s\napplied:\n%s", got, test.want, current.Kubeadm.Plan().Config.Content, request.KubeadmConfigs[ref].Config.Content)
			}
		})
	}
}

// kubeadmInput returns the input of a selected bundle node as the node holds it.
func kubeadmInput(t *testing.T, selected configbundle.SelectedNodeMaterial) kubeadmconfig.NodeInput {
	t.Helper()
	node := selected.InstallManifest.Node
	input, err := kubeadmconfig.ForNode(selected.KubeadmConfigs[node.Kubernetes.Kubeadm.ConfigRef], node)
	if err != nil {
		t.Fatal(err)
	}
	return input
}

// sharedKubeadmInput returns the node's input without registration, as an
// unregistered node of the same role installs it.
func sharedKubeadmInput(t *testing.T, node, ref string) kubeadmconfig.Plan {
	t.Helper()
	source := registeredClusterSource
	for _, registration := range []string{"        address: 10.1.0.11\n", "        address: 10.1.0.13\n", "        labels:\n          example.test/tier: control\n", "        labels:\n          example.test/tier: worker\n", "        taints:\n          - key: dedicated\n            value: control\n            effect: NoSchedule\n"} {
		source = strings.Replace(source, registration, "", 1)
	}
	source = strings.ReplaceAll(source, "      kubernetes:\n      install:", "      install:")
	path := filepath.Join(t.TempDir(), "cluster.yaml")
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	archive, _, err := configbundle.BuildArchive(configbundle.BuildRequest{SourcePath: path})
	if err != nil {
		t.Fatal(err)
	}
	selected, err := configbundle.ReadSelectedNode(bytes.NewReader(archive), configbundle.ReadOptions{NodeName: node, AllowMissingKatlosImage: true})
	if err != nil {
		t.Fatal(err)
	}
	plan, ok := selected.KubeadmConfigs[ref]
	if !ok || strings.Contains(string(plan.Config.Content), "node-ip") {
		t.Fatalf("shared input %q = %s", ref, plan.Config.Content)
	}
	return plan
}

// writeStoredInput replaces the stored kubeadm input of a generation with plan
// as is, in whatever form an earlier release stored it.
func writeStoredInput(t *testing.T, root, generationID string, plan kubeadmconfig.Plan) {
	t.Helper()
	dir := filepath.Join(root, "var/lib/katl/generations", generationID, "kubeadm-input", plan.Name)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	files, err := plan.InputFiles()
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		path := filepath.Join(dir, strings.TrimPrefix(file.RenderPath, kubeadmconfig.InputDir(plan.Name)+"/"))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, file.Content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
