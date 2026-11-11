package configbundle

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/installer/configapply"
)

func TestExternalWorkers(t *testing.T) {
	source := strings.Replace(validSourceConfig(), "  controlPlaneEndpoint:\n    host: api.katl.test\n    port: 6443\n", "", 1)
	source = strings.Replace(source, "    version: v1.36.1", "    version: v1.36.1\n    external:\n      endpoint: https://api.external.test:6443", 1)
	source = strings.Replace(source, "      controlPlane: true\n", "", 1)
	source = strings.Replace(source, "  defaults:\n", "  defaults:\n    kubernetes:\n      kubelet:\n        configFile: kubelet.yaml\n", 1)
	source = strings.Replace(source, "          katl.dev/pool: workers", "          katl.dev/pool: workers\n        address: 10.0.0.21", 1)
	path := writeSource(t, source)
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "kubelet.yaml"), []byte("apiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\nclusterDNS: [10.96.0.10]\nclusterDomain: cluster.local\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := newSourceSchemaValidator(t).Validate(sourceSchemaValue(t, source)); err != nil {
		t.Fatal(err)
	}
	archive, _, err := BuildArchive(BuildRequest{SourcePath: path})
	if err != nil {
		t.Fatal(err)
	}
	selected, err := ReadSelectedNode(bytes.NewReader(archive), ReadOptions{NodeName: "worker-1", AllowMissingKatlosImage: true})
	if err != nil {
		t.Fatal(err)
	}
	if !selected.InstallManifest.Node.Kubernetes.External {
		t.Fatal("bundle lost external control-plane ownership")
	}

	before, err := InspectSelectedNode(selected)
	if err != nil {
		t.Fatal(err)
	}
	if before.Cluster.External == nil || before.Cluster.External.Endpoint != "https://api.external.test:6443" {
		t.Fatalf("inspection lost external endpoint: %+v", before.Cluster)
	}
	selected.Source.Spec.Kubernetes.External = &SourceExternalKubernetes{Endpoint: "https://api.other.test:6443"}
	after, err := InspectSelectedNode(selected)
	if err != nil {
		t.Fatal(err)
	}
	diff, err := DiffNodeResolutions(before, after)
	if err != nil {
		t.Fatal(err)
	}
	if len(diff.Changes) != 1 || diff.Changes[0].Path != "spec.kubernetes.external" || diff.Changes[0].RequiredOperation != "wipe-reinstall" {
		t.Fatalf("endpoint diff=%+v", diff)
	}
	result, err := PlanConfiguration(BuildRequest{SourcePath: path})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Plan.BootstrapInventory.ExternalControlPlane || result.Plan.ControlPlaneEndpoint != "api.external.test:6443" {
		t.Fatalf("external intent lost: %+v", result.Plan)
	}
	for _, node := range result.Plan.Nodes {
		if !node.InstallManifest.Node.Kubernetes.External {
			t.Fatal("install material lost external ownership")
		}
		if !node.APIProxy.IsZero() {
			t.Fatal("external worker received managed API proxy")
		}
		if node.Name == selected.Node.Name {
			data, err := configapply.RenderNodeConfigurationChange(configapply.RenderNodeRequest{NodeName: node.Name, SourceID: "external-lab", DesiredVersion: "2", Manifest: node.InstallManifest, KubeadmConfigs: result.KubeadmConfigs})
			if err != nil {
				t.Fatal(err)
			}
			request, err := configapply.DecodeNodeConfigurationChange(bytes.NewReader(data), configapply.TrustedBundleRequest{NodeName: node.Name, CurrentManifest: selected.InstallManifest, KubeadmConfigs: selected.KubeadmConfigs})
			if err != nil {
				t.Fatal(err)
			}
			if request.NodeOverrides[node.Name].KubeadmChanged {
				t.Fatal("unchanged external source differs from installed bundle input")
			}
		}
	}
	for _, tt := range []struct{ name, source string }{
		{"control plane", strings.Replace(source, "    - name: cp-1", "    - name: cp-1\n      controlPlane: true", 1)},
		{"kubeadm", strings.Replace(source, "    external:", "    kubeadm:\n      configFile: kubeadm.yaml\n    external:", 1)},
		{"missing kubelet", strings.Replace(source, "    kubernetes:\n      kubelet:\n        configFile: kubelet.yaml\n", "", 1)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(tt.source), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := BuildArchive(BuildRequest{SourcePath: path}); err == nil {
				t.Fatal("accepted incompatible external worker configuration")
			}
		})
	}
}
