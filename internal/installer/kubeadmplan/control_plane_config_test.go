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

func TestKubeletConfigurationDifferences(t *testing.T) {
	actual := []byte("apiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\nmaxPods: 110\n")
	desired := []byte("apiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\nmaxPods: 150\nsystemReserved:\n  memory: 1Gi\n")
	err := KubeletConfigurationContains(actual, desired)
	if err == nil {
		t.Fatal("missing and unequal fields were accepted")
	}
	for _, difference := range []string{
		"maxPods: desired 150, actual 110",
		`systemReserved.memory: desired "1Gi", actual <missing>`,
	} {
		if !strings.Contains(err.Error(), difference) {
			t.Errorf("error %q does not identify %q", err, difference)
		}
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
	got, err := SupportedControlPlaneComponentDelta(desired, live, "")
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
	effective, delta, err := EffectiveControlPlaneConfiguration(desired, live, "")
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
	delta, err := SupportedControlPlaneComponentDelta(desired, live, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(delta) != 0 {
		t.Fatalf("delta = %v, want no changes", delta)
	}
}

func TestSupportedControlPlaneComponentDeltaRejectsCertificateAndClusterChanges(t *testing.T) {
	live := []byte("apiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\nkubernetesVersion: v1.36.1\n")
	for name, tt := range map[string]struct {
		desired []byte
		want    string
	}{
		"certificate": {[]byte("apiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\nkubernetesVersion: v1.36.1\napiServer:\n  certSANs: [api.example.test]\n"), "api.example.test"},
		"version":     {[]byte("apiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\nkubernetesVersion: v1.36.2\n"), "outside control-plane"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := SupportedControlPlaneComponentDelta(tt.desired, live, ""); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestSupportedControlPlaneComponentDeltaAccountsForBootstrapSANs(t *testing.T) {
	cluster := func(endpoint string, sans ...string) []byte {
		config := "apiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\nkubernetesVersion: v1.36.1\n"
		if endpoint != "" {
			config += "controlPlaneEndpoint: " + endpoint + "\n"
		}
		if len(sans) > 0 {
			config += "apiServer:\n  certSANs: [" + strings.Join(sans, ", ") + "]\n"
		}
		return []byte(config)
	}
	const endpoint = "api.katl.test:6443"
	bootstrapped := cluster(endpoint, "user-api.example", "api.katl.test", "127.0.0.1", "192.0.2.10")
	for _, tt := range []struct {
		name      string
		desired   []byte
		live      []byte
		vip       string
		wantError string
	}{
		{name: "operator SANs beside bootstrap SANs", desired: cluster("", "user-api.example"), live: bootstrapped, vip: "192.0.2.10"},
		{name: "endpoint in desired", desired: cluster(endpoint, "user-api.example"), live: bootstrapped, vip: "192.0.2.10"},
		{name: "operator lists the host", desired: cluster("", "user-api.example", "api.katl.test"), live: bootstrapped, vip: "192.0.2.10"},
		{name: "operator lists the VIP", desired: cluster("", "user-api.example", "192.0.2.10"), live: bootstrapped, vip: "192.0.2.10"},
		{name: "live lacks the VIP", desired: cluster("", "user-api.example", "192.0.2.10"), live: cluster(endpoint, "user-api.example", "api.katl.test", "127.0.0.1"), vip: "192.0.2.10", wantError: "[user-api.example 192.0.2.10] differs from the running cluster's [user-api.example]; config apply does not change API server certificate SANs, so set apiServer.certSANs"},
		{name: "VIP advertised but absent from both SAN lists", desired: cluster("", "user-api.example"), live: cluster(endpoint, "user-api.example", "api.katl.test", "127.0.0.1"), vip: "192.0.2.10"},
		{name: "no operator SANs", desired: cluster(""), live: cluster(endpoint, "api.katl.test", "127.0.0.1")},
		{name: "live endpoint without a port", desired: cluster("", "extra.example"), live: cluster("api.example", "api.example", "127.0.0.1", "extra.example")},
		{name: "live IPv6 endpoint without a port", desired: cluster("", "extra.example"), live: cluster("\"[2001:db8::1]\"", "2001:db8::1", "127.0.0.1", "extra.example")},
		{name: "legacy IPv6 bootstrap SAN", desired: cluster("", "extra.example"), live: cluster("'[2001:0db8::1]:6443'", "2001:0db8::1", "127.0.0.1", "extra.example")},
		{name: "SAN added", desired: cluster("", "user-api.example", "new.example"), live: bootstrapped, vip: "192.0.2.10", wantError: "[user-api.example new.example] differs from the running cluster's [user-api.example]"},
		{name: "operator SAN removed", desired: cluster(""), live: bootstrapped, vip: "192.0.2.10", wantError: "[] differs from the running cluster's [user-api.example]"},
		{name: "operator SANs replaced", desired: cluster("", "other.example"), live: bootstrapped, vip: "192.0.2.10", wantError: "[other.example] differs from the running cluster's [user-api.example]; config apply does not change API server certificate SANs, so set apiServer.certSANs"},
		{name: "VIP the desired config does not advertise", desired: cluster("", "user-api.example"), live: bootstrapped, wantError: "differs from the running cluster's [user-api.example 192.0.2.10]"},
		{name: "other endpoint", desired: cluster("other.test:6443", "user-api.example"), live: bootstrapped, vip: "192.0.2.10", wantError: "outside control-plane"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			delta, err := SupportedControlPlaneComponentDelta(tt.desired, tt.live, tt.vip)
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("error = %v, want %q", err, tt.wantError)
				}
				return
			}
			if err != nil || len(delta) != 0 {
				t.Fatalf("delta = %v, error = %v; want accepted without change", delta, err)
			}
		})
	}
}

func TestEffectiveControlPlaneConfigurationKeepsLiveSANs(t *testing.T) {
	live := []byte("apiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\ncontrolPlaneEndpoint: api.katl.test:6443\napiServer:\n  certSANs: [user-api.example, api.katl.test, 127.0.0.1]\n")
	desired := []byte("apiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\napiServer:\n  certSANs: [user-api.example]\n  extraArgs:\n    - name: profiling\n      value: \"false\"\n")
	effective, delta, err := EffectiveControlPlaneConfiguration(desired, live, "")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(delta, []string{"ClusterConfiguration.apiServer.extraArgs"}) {
		t.Fatalf("delta = %v", delta)
	}
	for _, want := range []string{"- api.katl.test", "- 127.0.0.1", "name: profiling"} {
		if !strings.Contains(string(effective), want) {
			t.Fatalf("effective config lacks %q:\n%s", want, effective)
		}
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
