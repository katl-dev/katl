package kubeadmplan

import (
	"reflect"
	"strings"
	"testing"
)

func TestCanonicalKubeletConfigurationSHA256SelectsKubeletDocument(t *testing.T) {
	multiDocument := []byte(`apiVersion: kubeadm.k8s.io/v1beta4
kind: InitConfiguration
---
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
maxPods: 120
`)
	kubeletOnly := []byte(`kind: KubeletConfiguration
maxPods: 120
apiVersion: kubelet.config.k8s.io/v1beta1
`)
	got, err := CanonicalKubeletConfigurationSHA256(multiDocument)
	if err != nil {
		t.Fatal(err)
	}
	want, err := CanonicalKubeletConfigurationSHA256(kubeletOnly)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("digest = %q, want %q", got, want)
	}
}

func TestKubeProxyConfigurationIdentityAndContainment(t *testing.T) {
	desired := []byte("apiVersion: kubeproxy.config.k8s.io/v1alpha1\nkind: KubeProxyConfiguration\nmode: nftables\n")
	actual := []byte("apiVersion: kubeproxy.config.k8s.io/v1alpha1\nkind: KubeProxyConfiguration\nmode: nftables\nbindAddress: 0.0.0.0\n")
	if _, err := CanonicalKubeProxyConfigurationSHA256(desired); err != nil {
		t.Fatalf("CanonicalKubeProxyConfigurationSHA256() error = %v", err)
	}
	if err := KubeProxyConfigurationContains(actual, desired); err != nil {
		t.Fatalf("KubeProxyConfigurationContains() error = %v", err)
	}
	if err := KubeProxyConfigurationContains([]byte("apiVersion: kubeproxy.config.k8s.io/v1alpha1\nkind: KubeProxyConfiguration\nmode: iptables\n"), desired); err == nil {
		t.Fatal("KubeProxyConfigurationContains() error = nil for mismatched mode")
	}
}

func TestKubeletConfigurationContainsAllowsDefaultedLiveFields(t *testing.T) {
	desired := []byte("apiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\ncontainerRuntimeEndpoint: \"\"\nmaxPods: 120\n")
	actual := []byte("apiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\ncontainerRuntimeEndpoint: unix:///run/containerd/containerd.sock\nmaxPods: 120\ncgroupDriver: systemd\n")
	if err := KubeletConfigurationContains(actual, desired); err != nil {
		t.Fatal(err)
	}
	actual = []byte("apiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\ncontainerRuntimeEndpoint: unix:///run/containerd/containerd.sock\nmaxPods: 110\ncgroupDriver: systemd\n")
	if err := KubeletConfigurationContains(actual, desired); err == nil {
		t.Fatal("KubeletConfigurationContains() error = nil, want mismatched desired value")
	}
	desired = []byte("apiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\ncontainerRuntimeEndpoint: unix:///run/crio/crio.sock\nmaxPods: 120\n")
	if err := KubeletConfigurationContains(actual, desired); err == nil {
		t.Fatal("KubeletConfigurationContains() error = nil, want non-empty endpoint mismatch")
	}
}

func TestKubeletConfigurationCanonicalizesDurations(t *testing.T) {
	desired := []byte("apiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\nshutdownGracePeriod: 60s\nshutdownGracePeriodCriticalPods: 20s\n")
	canonicalized := []byte("apiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\nshutdownGracePeriod: 1m0s\nshutdownGracePeriodCriticalPods: 20s\n")

	desiredDigest, err := CanonicalKubeletConfigurationSHA256(desired)
	if err != nil {
		t.Fatal(err)
	}
	canonicalDigest, err := CanonicalKubeletConfigurationSHA256(canonicalized)
	if err != nil {
		t.Fatal(err)
	}
	if desiredDigest != canonicalDigest {
		t.Fatalf("canonical kubelet digests differ: desired=%s live=%s", desiredDigest, canonicalDigest)
	}
	if err := KubeletConfigurationContains(canonicalized, desired); err != nil {
		t.Fatalf("KubeletConfigurationContains() rejected equivalent durations: %v", err)
	}

	different := []byte("apiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\nshutdownGracePeriod: 61s\nshutdownGracePeriodCriticalPods: 20s\n")
	if err := KubeletConfigurationContains(different, desired); err == nil {
		t.Fatal("KubeletConfigurationContains() accepted a different shutdown grace period")
	}
}

func TestSupportedControlPlaneProfilingDelta(t *testing.T) {
	live := []byte("apiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\nclusterName: katl\nkubernetesVersion: v1.36.1\n")
	desired := []byte("apiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\nclusterName: katl\nkubernetesVersion: v1.36.1\napiServer:\n  extraArgs:\n    - name: profiling\n      value: \"false\"\nscheduler:\n  extraArgs:\n    - name: profiling\n      value: \"false\"\n")
	got, err := SupportedControlPlaneProfilingDelta(desired, live)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ClusterConfiguration.apiServer.extraArgs.profiling=false", "ClusterConfiguration.scheduler.extraArgs.profiling=false"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("delta=%v want %v", got, want)
	}
}

func TestSupportedControlPlaneComponentDeltaAllowsManifestFields(t *testing.T) {
	live := []byte("apiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\nclusterName: katl\nkubernetesVersion: v1.36.1\n")
	desired := []byte("apiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\nclusterName: katl\nkubernetesVersion: v1.36.1\napiServer:\n  extraArgs:\n    - name: audit-log-maxage\n      value: '7'\nscheduler:\n  extraEnvs:\n    - name: KATL_TEST\n      value: enabled\n")
	got, err := SupportedControlPlaneComponentDelta(desired, live)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ClusterConfiguration.apiServer.extraArgs", "ClusterConfiguration.scheduler.extraEnvs"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("delta = %v, want %v", got, want)
	}
}

func TestEffectiveControlPlaneConfigurationPreservesLiveClusterState(t *testing.T) {
	live := []byte("apiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\nclusterName: katl\ncontrolPlaneEndpoint: api.katl.test:6443\nkubernetesVersion: v1.36.1\napiServer:\n  certSANs: [api.katl.test]\n  extraArgs:\n    - name: profiling\n      value: 'true'\n")
	desired := []byte("apiVersion: kubeadm.k8s.io/v1beta4\nkind: InitConfiguration\nnodeRegistration:\n  criSocket: unix:///run/containerd/containerd.sock\n---\napiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\nkubernetesVersion: v1.36.1\napiServer:\n  extraArgs:\n    - name: profiling\n      value: 'false'\n")
	effective, delta, err := EffectiveControlPlaneConfiguration(desired, live)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"ClusterConfiguration.apiServer.extraArgs"}; !reflect.DeepEqual(delta, want) {
		t.Fatalf("delta = %v, want %v", delta, want)
	}
	text := string(effective)
	for _, want := range []string{"kind: InitConfiguration", "controlPlaneEndpoint: api.katl.test:6443", "certSANs:", "value: \"false\""} {
		if !strings.Contains(text, want) {
			t.Fatalf("effective config missing %q:\n%s", want, text)
		}
	}
}

func TestSupportedControlPlaneComponentDeltaAllowsNoChange(t *testing.T) {
	live := []byte("apiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\nclusterName: katl\nkubernetesVersion: v1.36.1\n")
	desired := []byte("apiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\nkubernetesVersion: v1.36.1\n")
	delta, err := SupportedControlPlaneComponentDelta(desired, live)
	if err != nil {
		t.Fatal(err)
	}
	if len(delta) != 0 {
		t.Fatalf("delta = %v, want no changes", delta)
	}
}

func TestSupportedControlPlaneComponentDeltaRejectsCertificateAndClusterChanges(t *testing.T) {
	live := []byte("apiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\nkubernetesVersion: v1.36.1\n")
	for name, desired := range map[string][]byte{
		"certificate": []byte("apiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\nkubernetesVersion: v1.36.1\napiServer:\n  certSANs: [api.example.test]\n"),
		"version":     []byte("apiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\nkubernetesVersion: v1.36.2\n"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := SupportedControlPlaneComponentDelta(desired, live); err == nil || !strings.Contains(err.Error(), "outside control-plane") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestSupportedControlPlaneProfilingDeltaNormalizesEmptyLiveSections(t *testing.T) {
	live := []byte("apiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\napiServer: {}\ncontrollerManager: {}\nscheduler: {}\n")
	desired := []byte("apiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\napiServer:\n  extraArgs:\n    - name: profiling\n      value: \"false\"\ncontrollerManager:\n  extraArgs:\n    - name: profiling\n      value: \"false\"\nscheduler:\n  extraArgs:\n    - name: profiling\n      value: \"false\"\n")
	delta, err := SupportedControlPlaneProfilingDelta(desired, live)
	if err != nil {
		t.Fatal(err)
	}
	if len(delta) != 3 {
		t.Fatalf("delta = %v, want three supported fields", delta)
	}
}

func TestSupportedControlPlaneProfilingDeltaRejectsOtherChange(t *testing.T) {
	live := []byte("apiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\nkubernetesVersion: v1.36.1\n")
	desired := []byte("apiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\nkubernetesVersion: v1.36.2\napiServer:\n  extraArgs:\n    - name: profiling\n      value: \"false\"\n")
	_, err := SupportedControlPlaneProfilingDelta(desired, live)
	if err == nil || !strings.Contains(err.Error(), "outside profiling") {
		t.Fatalf("error=%v", err)
	}
}
