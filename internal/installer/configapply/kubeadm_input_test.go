package configapply

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/kubeadmconfig"
	"github.com/katl-dev/katl/internal/installer/manifest"
)

const (
	sharedInitInput = "apiVersion: kubeadm.k8s.io/v1beta4\nkind: InitConfiguration\n---\napiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\nkubernetesVersion: v1.36.1\n"

	registeredInitInput = `apiVersion: kubeadm.k8s.io/v1beta4
kind: InitConfiguration
localAPIEndpoint:
  advertiseAddress: 10.0.0.21
nodeRegistration:
  kubeletExtraArgs:
    - name: node-ip
      value: 10.0.0.21
    - name: node-labels
      value: example.test/tier=control
---
apiVersion: kubeadm.k8s.io/v1beta4
kind: ClusterConfiguration
kubernetesVersion: v1.36.1
`

	staleInitInput = `apiVersion: kubeadm.k8s.io/v1beta4
kind: InitConfiguration
localAPIEndpoint:
  advertiseAddress: 10.0.0.5
nodeRegistration:
  kubeletExtraArgs:
    - name: node-ip
      value: 10.0.0.5
    - name: node-labels
      value: example.test/tier=old
---
apiVersion: kubeadm.k8s.io/v1beta4
kind: ClusterConfiguration
kubernetesVersion: v1.36.1
`
)

func registeredManifest() manifest.Manifest {
	value := manifestWithKubeadm()
	value.Node.Kubernetes.Address = "10.0.0.21"
	value.Node.Bootstrap = &manifest.BootstrapIntent{Labels: map[string]string{"example.test/tier": "control"}}
	return value
}

func writeStoredKubeadmFile(t *testing.T, root, generationID, content string) {
	t.Helper()
	writeConfigurationFile(t, filepath.Join(root, "var/lib/katl/generations", generationID, "kubeadm-input/control-plane/config.yaml"), content)
}

func TestGenerationConfigurationReadsEveryStoredFormAsOneInput(t *testing.T) {
	expected, err := kubeadmconfig.PlanFromRenderedFiles("control-plane", []kubeadmconfig.File{{
		RenderPath: "/etc/katl/kubeadm/control-plane/config.yaml", Content: []byte(registeredInitInput),
	}})
	if err != nil {
		t.Fatal(err)
	}
	wantDigest, err := expected.InputDigest()
	if err != nil {
		t.Fatal(err)
	}

	for name, stored := range map[string]string{
		"shared, as v2026.11.0 and v2026.11.1 applies stored it": sharedInitInput,
		"registered, as bootstrap and bundles stored it":         registeredInitInput,
		"registered with stale address and labels":               staleInitInput,
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if err := WriteGenerationManifest(root, "selected", registeredManifest()); err != nil {
				t.Fatal(err)
			}
			writeStoredKubeadmFile(t, root, "selected", stored)

			got, err := ReadLocalGenerationConfiguration(root, "selected")
			if err != nil {
				t.Fatal(err)
			}
			if content := string(got.Kubeadm.Plan().Config.Content); content != registeredInitInput {
				t.Fatalf("input:\n%s\nwant:\n%s", content, registeredInitInput)
			}
			if digest, err := got.Kubeadm.InputDigest(); err != nil || digest != wantDigest {
				t.Fatalf("digest = %s, %v; want %s", digest, err, wantDigest)
			}
		})
	}
}

func TestGenerationConfigurationWritesNodeInputOnly(t *testing.T) {
	root := t.TempDir()
	value := GenerationConfiguration{Manifest: manifestWithKubeadm()}
	if _, err := value.SizeBytes(); err == nil {
		t.Fatal("configuration without its input was sized")
	}
	if err := WriteGenerationConfiguration(root, "selected", value); err == nil {
		t.Fatal("configuration without its input was written")
	}

	value.Kubeadm = nodeInput(t, kubeadmPlan("other"))
	if err := WriteGenerationConfiguration(root, "selected", value); err == nil || !strings.Contains(err.Error(), `holds input "other"`) {
		t.Fatalf("configuration holding another input: %v", err)
	}
}

func TestApplyStoresNodeInputForSharedPayload(t *testing.T) {
	root := t.TempDir()
	current := manifestWithKubeadm()
	current.Node.Kubernetes.Address = "10.0.0.21"
	shared := kubeadmPlan("control-plane")

	result, err := ApplyTrustedBundle(context.Background(), trustedBundleRequest(root, TrustedBundleRequest{
		ApplyMode:       generation.ApplyModeNextBoot,
		GenerationID:    "2026.06.05-002",
		ClusterDefaults: NodeOverlay{KubeadmChanged: true},
		CurrentManifest: current,
		KubeadmConfigs:  map[string]kubeadmconfig.Plan{"control-plane": shared},
		Executor:        &Executor{Runner: &fakeCommandRunner{}, Activator: &fakeActivator{}, Now: fixedNow},
	}))
	if err != nil {
		t.Fatalf("ApplyTrustedBundle() error = %v", err)
	}

	const want = `apiVersion: kubeadm.k8s.io/v1beta4
kind: InitConfiguration
localAPIEndpoint:
  advertiseAddress: 10.0.0.21
nodeRegistration:
  kubeletExtraArgs:
    - name: node-ip
      value: 10.0.0.21
---
apiVersion: kubeadm.k8s.io/v1beta4
kind: ClusterConfiguration
kubernetesVersion: v1.36.1
`
	stored, err := os.ReadFile(filepath.Join(root, "var/lib/katl/generations/2026.06.05-002/kubeadm-input/control-plane/config.yaml"))
	if err != nil || string(stored) != want {
		t.Fatalf("stored input = %q, %v; want the registered form:\n%s", stored, err, want)
	}
	rendered, err := os.ReadFile(filepath.Join(result.Tree.ConfextDir, "etc/katl/kubeadm/control-plane/config.yaml"))
	if err != nil || string(rendered) != want {
		t.Fatalf("confext input = %q, %v; want the stored form", rendered, err)
	}
}

func TestApplyStoresInlineReservedFieldsAsRegistered(t *testing.T) {
	root := t.TempDir()
	current := manifestWithKubeadm()
	current.Node.Kubernetes.Address = "10.0.0.21"
	conflicting := kubeadmPlan("control-plane")
	conflicting.Config.Content = []byte(staleInitInput)

	if _, err := ApplyTrustedBundle(context.Background(), trustedBundleRequest(root, TrustedBundleRequest{
		ApplyMode:       generation.ApplyModeNextBoot,
		GenerationID:    "2026.06.05-002",
		ClusterDefaults: NodeOverlay{KubeadmChanged: true},
		CurrentManifest: current,
		KubeadmConfigs:  map[string]kubeadmconfig.Plan{"control-plane": conflicting},
		Executor:        &Executor{Runner: &fakeCommandRunner{}, Activator: &fakeActivator{}, Now: fixedNow},
	})); err != nil {
		t.Fatalf("ApplyTrustedBundle() error = %v", err)
	}
	stored, err := os.ReadFile(filepath.Join(root, "var/lib/katl/generations/2026.06.05-002/kubeadm-input/control-plane/config.yaml"))
	if err != nil || strings.Contains(string(stored), "10.0.0.5") || !strings.Contains(string(stored), "10.0.0.21") {
		t.Fatalf("stored input keeps the inline registration: %q, %v", stored, err)
	}
}

func TestEditedRegistrationPlansNoKubeadmChange(t *testing.T) {
	const document = `
apiVersion: katl.dev/v1alpha1
kind: NodeConfigurationChange
metadata:
  sourceID: operator
  desiredVersion: "2"
apply:
  mode: next-boot
spec:
  kubeadmConfigs:
    control-plane:
      config: |
        apiVersion: kubeadm.k8s.io/v1beta4
        kind: InitConfiguration
        ---
        apiVersion: kubeadm.k8s.io/v1beta4
        kind: ClusterConfiguration
        kubernetesVersion: v1.36.1
  clusterDefaults:
    kubernetes:
      kubeadm:
        configRef: control-plane
`
	for name, tt := range map[string]struct {
		installed string
		want      bool
	}{
		"registered": {installed: registeredInitInput},
		"stale":      {installed: staleInitInput},
		"shared":     {installed: sharedInitInput},
		"drifted":    {installed: strings.Replace(registeredInitInput, "v1.36.1", "v1.36.2", 1), want: true},
	} {
		t.Run(name, func(t *testing.T) {
			installed, err := kubeadmconfig.PlanFromRenderedFiles("control-plane", []kubeadmconfig.File{{
				RenderPath: "/etc/katl/kubeadm/control-plane/config.yaml", Content: []byte(tt.installed),
			}})
			if err != nil {
				t.Fatal(err)
			}
			request, err := DecodeNodeConfigurationChange(strings.NewReader(document), TrustedBundleRequest{
				KubeadmConfigs: map[string]kubeadmconfig.Plan{"control-plane": installed},
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := request.ClusterDefaults.KubeadmChanged; got != tt.want {
				t.Fatalf("kubeadm change = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestBootstrapConfextDoesNotOwnKubeadmInput(t *testing.T) {
	root := t.TempDir()
	if err := WriteGenerationManifest(root, "bootstrap", manifestWithKubeadm()); err != nil {
		t.Fatal(err)
	}
	writeKubeadmStateGeneration(t, root, "parent", "")
	writeKubeadmStateGeneration(t, root, "bootstrap", "parent")
	if err := WriteGenerationKubeadmConfig(root, "parent", nodeInput(t, configurationKubeadm(t, "parent"))); err != nil {
		t.Fatal(err)
	}
	// A bootstrap generation of an earlier release rendered its init config at
	// the input path of its confext and stored no input of its own.
	confext := filepath.Join(root, "var/lib/katl/generations/bootstrap/confext/etc/katl")
	writeConfigurationFile(t, filepath.Join(confext, "bootstrap-runtime.json"), "{}")
	writeConfigurationFile(t, filepath.Join(confext, "kubeadm/control-plane/config.yaml"), "apiVersion: kubeadm.k8s.io/v1beta4\nkind: InitConfiguration\n---\napiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\ncontrolPlaneEndpoint: api.katl.test:6443\n")

	got, source, err := ReadEffectiveGenerationKubeadmConfig(root, "bootstrap", "control-plane", manifestWithKubeadm().Node)
	if err != nil {
		t.Fatal(err)
	}
	if source != "parent" || !strings.Contains(string(got.Plan().Config.Content), "name: parent\n") || strings.Contains(string(got.Plan().Config.Content), "controlPlaneEndpoint") {
		t.Fatalf("input = source %q:\n%s", source, got.Plan().Config.Content)
	}

	// With no ancestor input, the rendered config is not an input either.
	orphan := t.TempDir()
	if err := WriteGenerationManifest(orphan, "bootstrap", manifestWithKubeadm()); err != nil {
		t.Fatal(err)
	}
	writeKubeadmStateGeneration(t, orphan, "bootstrap", "")
	writeConfigurationFile(t, filepath.Join(orphan, "var/lib/katl/generations/bootstrap/confext/etc/katl/bootstrap-runtime.json"), "{}")
	writeConfigurationFile(t, filepath.Join(orphan, "var/lib/katl/generations/bootstrap/confext/etc/katl/kubeadm/control-plane/config.yaml"), "kind: ClusterConfiguration\n")
	if _, _, err := ReadEffectiveGenerationKubeadmConfig(orphan, "bootstrap", "control-plane", manifestWithKubeadm().Node); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rendered init config read as input: %v", err)
	}
}

func TestPlanWarnsOfReplacedInlineRegistration(t *testing.T) {
	const inline = `
apiVersion: katl.dev/v1alpha1
kind: NodeConfigurationChange
metadata:
  sourceID: operator
  desiredVersion: "2"
apply:
  mode: auto
spec:
  kubeadmConfigs:
    control-plane:
      config: |
        apiVersion: kubeadm.k8s.io/v1beta4
        kind: InitConfiguration
        localAPIEndpoint:
          advertiseAddress: 10.0.0.21
        nodeRegistration:
          kubeletExtraArgs:
            - name: node-ip
              value: %s
        ---
        apiVersion: kubeadm.k8s.io/v1beta4
        kind: ClusterConfiguration
        kubernetesVersion: v1.36.1
        %s
  clusterDefaults:
    kubernetes:
      address: 10.0.0.21
      kubeadm:
        configRef: control-plane
`
	const labelEdit = `
apiVersion: katl.dev/v1alpha1
kind: NodeConfigurationChange
metadata:
  sourceID: operator
  desiredVersion: "2"
apply:
  mode: next-boot
spec:
  nodeOverrides:
    cp-1:
      kubernetesMetadata:
        labels:
          example.test/tier: edge
`
	current := manifestWithKubeadm()
	current.Node.Kubernetes.Address = "10.0.0.21"
	installed, err := kubeadmconfig.PlanFromRenderedFiles("control-plane", []kubeadmconfig.File{{
		RenderPath: "/etc/katl/kubeadm/control-plane/config.yaml", Content: []byte(registeredInitInput),
	}})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name          string
		document      string
		wantNoChanges bool
		wantWarning   string
	}{
		{name: "registration-only edit", document: fmt.Sprintf(inline, "10.9.9.9", ""), wantNoChanges: true, wantWarning: "node-ip"},
		{name: "inline registration Katl would render", document: fmt.Sprintf(inline, "10.0.0.21", ""), wantNoChanges: true},
		{name: "registration beside a kubeadm change", document: fmt.Sprintf(inline, "10.9.9.9", "clusterName: other"), wantWarning: "node-ip"},
		{name: "rendered registration beside a kubeadm change", document: fmt.Sprintf(inline, "10.0.0.21", "clusterName: other")},
		{name: "label edit without inline config", document: labelEdit},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := trustedBundleRequest(t.TempDir(), TrustedBundleRequest{
				CurrentManifest:   current,
				KubeletRegistered: true,
				KubeadmConfigs:    map[string]kubeadmconfig.Plan{"control-plane": installed},
			})
			request, err := DecodeNodeConfigurationChange(strings.NewReader(tt.document), base)
			if err != nil {
				t.Fatal(err)
			}

			result, err := PlanTrustedBundle(request)
			if tt.wantNoChanges != errors.Is(err, ErrNoChanges) || (!tt.wantNoChanges && err != nil) {
				t.Fatalf("PlanTrustedBundle() error = %v, want no changes = %t", err, tt.wantNoChanges)
			}
			var warned []string
			for _, warning := range result.Plan.Decision.Warnings {
				if strings.Contains(warning, "kubeadm config") {
					warned = append(warned, warning)
				}
			}
			if tt.wantWarning == "" {
				if len(warned) != 0 {
					t.Fatalf("warnings = %q, want none about kubeadm config", warned)
				}
				return
			}
			if len(warned) != 1 || !strings.Contains(warned[0], tt.wantWarning) || !strings.Contains(warned[0], "control-plane") {
				t.Fatalf("warnings = %q, want one naming %s", warned, tt.wantWarning)
			}
		})
	}
}
