package configdomain

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zariel/katl/internal/installer/kubeadmconfig"
	"github.com/zariel/katl/internal/installer/manifest"
)

func TestNativeEtcFilesRendersKnownDomains(t *testing.T) {
	installManifest, err := manifest.Decode(strings.NewReader(manifestJSON(`,
			"networkd": {
				"files": [
					{"name": "10-lan.network", "content": "[Match]\nName=enp1s0\n"}
				]
			},
			"sysctl": {
				"settings": {
					"net.ipv4.ip_forward": "1"
				}
			},
			"kubernetes": {
				"kubeadm": {"configRef": "control-plane"}
			}`)))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	files, err := NativeEtcFiles(RenderRequest{
		Manifest:                 installManifest,
		KubernetesVersion:        "v1.36.1",
		KubernetesActivationPath: "/run/extensions/katl-kubernetes.raw",
		KubeadmConfigs: map[string]kubeadmconfig.Plan{
			"control-plane": controlPlanePlan("v1.36.1"),
		},
	})
	if err != nil {
		t.Fatalf("NativeEtcFiles() error = %v", err)
	}
	want := []string{
		"/etc/katl/kubeadm/control-plane/config.yaml",
		"/etc/katl/node.json",
		"/etc/sysctl.d/90-katl.conf",
		"/etc/systemd/network/10-lan.network",
	}
	if len(files) != len(want) {
		t.Fatalf("len(files) = %d, want %d: %#v", len(files), len(want), files)
	}
	for i, path := range want {
		if files[i].Path != path {
			t.Fatalf("files[%d].Path = %q, want %q", i, files[i].Path, path)
		}
		if files[i].Mode != 0o644 || files[i].UID != 0 || files[i].GID != 0 {
			t.Fatalf("files[%d] mode/owner = %04o %d:%d", i, files[i].Mode, files[i].UID, files[i].GID)
		}
	}
	var metadata map[string]any
	if err := json.Unmarshal([]byte(files[1].Content), &metadata); err != nil {
		t.Fatalf("decode node metadata: %v\n%s", err, files[1].Content)
	}
	if metadata["apiVersion"] != "katl.dev/v1alpha1" || metadata["kind"] != "NodeMetadata" || metadata["systemRole"] != "control-plane" {
		t.Fatalf("metadata = %#v", metadata)
	}
	kubeadm := metadata["kubeadm"].(map[string]any)
	if kubeadm["configRef"] != "control-plane" || kubeadm["configPath"] != "/etc/katl/kubeadm/control-plane/config.yaml" || kubeadm["intent"] != "control-plane" {
		t.Fatalf("metadata kubeadm = %#v", kubeadm)
	}
	kubernetes := metadata["kubernetes"].(map[string]any)
	if kubernetes["payloadVersion"] != "v1.36.1" || kubernetes["activationPath"] != "/run/extensions/katl-kubernetes.raw" {
		t.Fatalf("metadata kubernetes = %#v", kubernetes)
	}
	if !strings.Contains(files[2].Content, "net.ipv4.ip_forward = 1") {
		t.Fatalf("sysctl content = %q", files[2].Content)
	}
}

func TestNativeEtcFilesRendersWorkerNodeMetadata(t *testing.T) {
	installManifest, err := manifest.Decode(strings.NewReader(strings.Replace(manifestJSON(`,
			"kubernetes": {
				"kubeadm": {"configRef": "worker"}
			}`), `"systemRole": "control-plane"`, `"systemRole": "worker"`, 1)))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	files, err := NativeEtcFiles(RenderRequest{
		Manifest:                 installManifest,
		KubernetesVersion:        "v1.36.1",
		KubernetesActivationPath: "/run/extensions/katl-kubernetes.raw",
		KubeadmConfigs: map[string]kubeadmconfig.Plan{
			"worker": workerPlan(),
		},
	})
	if err != nil {
		t.Fatalf("NativeEtcFiles() error = %v", err)
	}
	var metadata map[string]any
	for _, file := range files {
		if file.Path == "/etc/katl/node.json" {
			if err := json.Unmarshal([]byte(file.Content), &metadata); err != nil {
				t.Fatalf("decode node metadata: %v", err)
			}
		}
	}
	if metadata["systemRole"] != "worker" {
		t.Fatalf("metadata = %#v", metadata)
	}
	kubeadm := metadata["kubeadm"].(map[string]any)
	if kubeadm["intent"] != "worker" || kubeadm["configRef"] != "worker" {
		t.Fatalf("metadata kubeadm = %#v", kubeadm)
	}
}

func TestNativeEtcFilesAcceptsControlPlaneJoinIntent(t *testing.T) {
	installManifest, err := manifest.Decode(strings.NewReader(manifestJSON(`,
			"kubernetes": {
				"kubeadm": {"configRef": "control-plane-join"}
			}`)))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	files, err := NativeEtcFiles(RenderRequest{
		Manifest:                 installManifest,
		KubernetesVersion:        "v1.36.1",
		KubernetesActivationPath: "/run/extensions/katl-kubernetes.raw",
		KubeadmConfigs: map[string]kubeadmconfig.Plan{
			"control-plane-join": controlPlaneJoinPlan(),
		},
	})
	if err != nil {
		t.Fatalf("NativeEtcFiles() error = %v", err)
	}
	var metadata map[string]any
	for _, file := range files {
		if file.Path == "/etc/katl/node.json" {
			if err := json.Unmarshal([]byte(file.Content), &metadata); err != nil {
				t.Fatalf("decode node metadata: %v", err)
			}
		}
	}
	kubeadm := metadata["kubeadm"].(map[string]any)
	if kubeadm["intent"] != "control-plane" || kubeadm["configRef"] != "control-plane-join" {
		t.Fatalf("metadata kubeadm = %#v", kubeadm)
	}
}

func TestNativeEtcFilesRejectsUnresolvedKubeadmRef(t *testing.T) {
	installManifest, err := manifest.Decode(strings.NewReader(manifestJSON(`,
			"kubernetes": {
				"kubeadm": {"configRef": "control-plane"}
			}`)))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	_, err = NativeEtcFiles(RenderRequest{Manifest: installManifest})
	if err == nil || !strings.Contains(err.Error(), "was not resolved") {
		t.Fatalf("NativeEtcFiles() error = %v, want unresolved ref", err)
	}
}

func TestNativeEtcFilesRejectsMismatchedKubeadmPlan(t *testing.T) {
	installManifest, err := manifest.Decode(strings.NewReader(manifestJSON(`,
			"kubernetes": {
				"kubeadm": {"configRef": "control-plane"}
			}`)))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	_, err = NativeEtcFiles(RenderRequest{
		Manifest:                 installManifest,
		KubernetesVersion:        "v1.36.1",
		KubernetesActivationPath: "/run/extensions/katl-kubernetes.raw",
		KubeadmConfigs: map[string]kubeadmconfig.Plan{
			"control-plane": {
				Name: "worker",
				Config: kubeadmconfig.File{
					RenderPath: "/etc/katl/kubeadm/worker/config.yaml",
					Content:    []byte("apiVersion: kubeadm.k8s.io/v1beta4\nkind: JoinConfiguration\n"),
					Mode:       0o644,
				},
				Documents: []kubeadmconfig.Document{{APIVersion: "kubeadm.k8s.io/v1beta4", Kind: "JoinConfiguration"}},
			},
		},
	})
	if err == nil || !strings.Contains(err.Error(), `resolved to KubeadmConfig "worker"`) {
		t.Fatalf("NativeEtcFiles() error = %v, want mismatched plan", err)
	}
}

func TestNativeEtcFilesRejectsUnsafeRenderedPaths(t *testing.T) {
	installManifest, err := manifest.Decode(strings.NewReader(manifestJSON(`,
			"kubernetes": {
				"kubeadm": {"configRef": "control-plane"}
			}`)))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	_, err = NativeEtcFiles(RenderRequest{
		Manifest:                 installManifest,
		KubernetesVersion:        "v1.36.1",
		KubernetesActivationPath: "/run/extensions/katl-kubernetes.raw",
		KubeadmConfigs: map[string]kubeadmconfig.Plan{
			"control-plane": {
				Name: "control-plane",
				Config: kubeadmconfig.File{
					RenderPath: "/etc/kubernetes/admin.conf",
					Content:    []byte("unsafe"),
					Mode:       0o644,
				},
				Documents: []kubeadmconfig.Document{{APIVersion: "kubeadm.k8s.io/v1beta4", Kind: "InitConfiguration"}},
			},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "cannot own kubeadm-managed") {
		t.Fatalf("NativeEtcFiles() error = %v, want unsafe rendered path", err)
	}
}

func TestNativeEtcFilesRejectsRoleConfigIntentMismatch(t *testing.T) {
	installManifest, err := manifest.Decode(strings.NewReader(manifestJSON(`,
			"kubernetes": {
				"kubeadm": {"configRef": "worker"}
			}`)))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	_, err = NativeEtcFiles(RenderRequest{
		Manifest: installManifest,
		KubeadmConfigs: map[string]kubeadmconfig.Plan{
			"worker": workerPlan(),
		},
	})
	if err == nil || !strings.Contains(err.Error(), "requires kubeadm intent") {
		t.Fatalf("NativeEtcFiles() error = %v, want intent mismatch", err)
	}
}

func TestNativeEtcFilesRejectsKubernetesVersionMismatch(t *testing.T) {
	installManifest, err := manifest.Decode(strings.NewReader(manifestJSON(`,
			"kubernetes": {
				"kubeadm": {"configRef": "control-plane"}
			}`)))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	_, err = NativeEtcFiles(RenderRequest{
		Manifest:                 installManifest,
		KubernetesVersion:        "v1.36.1",
		KubernetesActivationPath: "/run/extensions/katl-kubernetes.raw",
		KubeadmConfigs: map[string]kubeadmconfig.Plan{
			"control-plane": controlPlanePlan("v1.35.9"),
		},
	})
	if err == nil || !strings.Contains(err.Error(), "does not match selected Kubernetes payload version") {
		t.Fatalf("NativeEtcFiles() error = %v, want version mismatch", err)
	}
}

func TestNativeEtcFilesRejectsMissingKubernetesActivationPathForKubeadmMetadata(t *testing.T) {
	installManifest, err := manifest.Decode(strings.NewReader(manifestJSON(`,
			"kubernetes": {
				"kubeadm": {"configRef": "control-plane"}
			}`)))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	_, err = NativeEtcFiles(RenderRequest{
		Manifest:          installManifest,
		KubernetesVersion: "v1.36.1",
		KubeadmConfigs: map[string]kubeadmconfig.Plan{
			"control-plane": controlPlanePlan("v1.36.1"),
		},
	})
	if err == nil || !strings.Contains(err.Error(), "requires selected Kubernetes activation path") {
		t.Fatalf("NativeEtcFiles() error = %v, want missing selected activation path", err)
	}
}

func TestNativeEtcFilesRejectsMissingKubernetesVersionForKubeadmMetadata(t *testing.T) {
	installManifest, err := manifest.Decode(strings.NewReader(manifestJSON(`,
			"kubernetes": {
				"kubeadm": {"configRef": "control-plane"}
			}`)))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	_, err = NativeEtcFiles(RenderRequest{
		Manifest: installManifest,
		KubeadmConfigs: map[string]kubeadmconfig.Plan{
			"control-plane": controlPlanePlan("v1.36.1"),
		},
	})
	if err == nil || !strings.Contains(err.Error(), "requires selected Kubernetes payload version") {
		t.Fatalf("NativeEtcFiles() error = %v, want missing selected version", err)
	}
}

func manifestJSON(nodeExtra string) string {
	return `{
		"apiVersion": "install.katl.dev/v1alpha1",
		"kind": "InstallManifest",
		"node": {
			"identity": {
				"hostname": "lab-node-01",
				"ssh": {
					"authorizedKeys": [
						"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDAxMjM0NTY3ODlhYmNkZWYwMTIzNDU2Nzg5YWJjZGVm katl@example"
					]
				}
			},
			"systemRole": "control-plane"` + nodeExtra + `
		},
		"install": {
    "wipeTarget": true,
			"targetDisk": {"byID": "/dev/disk/by-id/ata-root", "minSizeMiB": 32768}
		},
		"katlosImage": {
			"url": "https://example.invalid/katlos-install.squashfs",
			"sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"sizeBytes": 1073741824,
			"version": "2026.06.04",
			"architecture": "x86_64",
			"runtimeInterface": "katl-runtime-1",
			"role": "install"
		}
	}`
}

func controlPlanePlan(version string) kubeadmconfig.Plan {
	return kubeadmconfig.Plan{
		Name: "control-plane",
		Config: kubeadmconfig.File{
			RenderPath: "/etc/katl/kubeadm/control-plane/config.yaml",
			Content:    []byte("apiVersion: kubeadm.k8s.io/v1beta4\nkind: InitConfiguration\n"),
			Mode:       0o644,
		},
		Documents: []kubeadmconfig.Document{
			{APIVersion: "kubeadm.k8s.io/v1beta4", Kind: "InitConfiguration"},
			{APIVersion: "kubeadm.k8s.io/v1beta4", Kind: "ClusterConfiguration", KubernetesVersion: version},
		},
	}
}

func workerPlan() kubeadmconfig.Plan {
	return kubeadmconfig.Plan{
		Name: "worker",
		Config: kubeadmconfig.File{
			RenderPath: "/etc/katl/kubeadm/worker/config.yaml",
			Content:    []byte("apiVersion: kubeadm.k8s.io/v1beta4\nkind: JoinConfiguration\n"),
			Mode:       0o644,
		},
		Documents: []kubeadmconfig.Document{{APIVersion: "kubeadm.k8s.io/v1beta4", Kind: "JoinConfiguration"}},
	}
}

func controlPlaneJoinPlan() kubeadmconfig.Plan {
	return kubeadmconfig.Plan{
		Name: "control-plane-join",
		Config: kubeadmconfig.File{
			RenderPath: "/etc/katl/kubeadm/control-plane-join/config.yaml",
			Content:    []byte("apiVersion: kubeadm.k8s.io/v1beta4\nkind: JoinConfiguration\ncontrolPlane: {}\n"),
			Mode:       0o644,
		},
		Documents: []kubeadmconfig.Document{{APIVersion: "kubeadm.k8s.io/v1beta4", Kind: "JoinConfiguration", ControlPlane: true}},
	}
}
