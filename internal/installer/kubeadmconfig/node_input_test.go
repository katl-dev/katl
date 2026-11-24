package kubeadmconfig

import (
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/installer/manifest"
)

const sharedControlPlane = `apiVersion: kubeadm.k8s.io/v1beta4
kind: InitConfiguration
nodeRegistration:
  criSocket: unix:///run/containerd/containerd.sock
---
apiVersion: kubeadm.k8s.io/v1beta4
kind: ClusterConfiguration
kubernetesVersion: v1.36.1
`

func testPlan(t *testing.T, config string) Plan {
	t.Helper()
	plan, err := PlanFromRenderedFiles("control-plane", []File{
		{RenderPath: "/etc/katl/kubeadm/control-plane/config.yaml", Content: []byte(config)},
		{RenderPath: "/etc/katl/kubeadm/control-plane/patches/kubeletconfiguration+merge.yaml", Content: []byte("maxPods: 120\n")},
	})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func testNode(address string, labels map[string]string, taints ...manifest.NodeTaint) manifest.NodeConfig {
	node := manifest.NodeConfig{Kubernetes: manifest.KubernetesConfig{Address: address}}
	if len(labels) > 0 || len(taints) > 0 {
		node.Bootstrap = &manifest.BootstrapIntent{Labels: labels, Taints: taints}
	}
	return node
}

func forNodeConfig(t *testing.T, plan Plan, node manifest.NodeConfig) string {
	t.Helper()
	input, err := ForNode(plan, node)
	if err != nil {
		t.Fatalf("ForNode() error = %v", err)
	}
	return string(input.Plan().Config.Content)
}

func TestForNodeRegistersSharedInput(t *testing.T) {
	node := testNode("10.0.0.21", map[string]string{"example.com/b": "", "example.com/a": "1"}, manifest.NodeTaint{Key: "dedicated", Value: "storage", Effect: "NoSchedule"})
	const want = `apiVersion: kubeadm.k8s.io/v1beta4
kind: InitConfiguration
localAPIEndpoint:
  advertiseAddress: 10.0.0.21
nodeRegistration:
  criSocket: unix:///run/containerd/containerd.sock
  kubeletExtraArgs:
    - name: node-ip
      value: 10.0.0.21
    - name: node-labels
      value: example.com/a=1,example.com/b=
  taints:
    - effect: NoSchedule
      key: dedicated
      value: storage
---
apiVersion: kubeadm.k8s.io/v1beta4
kind: ClusterConfiguration
kubernetesVersion: v1.36.1
`
	input, err := ForNode(testPlan(t, sharedControlPlane), node)
	if err != nil {
		t.Fatal(err)
	}
	plan := input.Plan()
	if got := string(plan.Config.Content); got != want {
		t.Fatalf("config:\n%s\nwant:\n%s", got, want)
	}
	if len(plan.Patches) != 1 || string(plan.Patches[0].Content) != "maxPods: 120\n" {
		t.Fatalf("patches = %#v", plan.Patches)
	}
}

func TestForNodeKeepsInputWithoutRegistration(t *testing.T) {
	// Hand-formatted input survives byte for byte when the node registers nothing.
	config := "# operator comment\napiVersion:   kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\n"
	if got := forNodeConfig(t, testPlan(t, config), testNode("", nil)); got != config {
		t.Fatalf("config changed:\n%s", got)
	}
}

func TestForNodeKeepsClearedTaints(t *testing.T) {
	// An empty taints list keeps a control plane schedulable; it is not registration.
	config := "apiVersion: kubeadm.k8s.io/v1beta4\nkind: InitConfiguration\nnodeRegistration:\n  taints: []\n"
	got := forNodeConfig(t, testPlan(t, config), testNode("10.0.0.21", nil))
	if !strings.Contains(got, "taints: []") {
		t.Fatalf("cleared taints lost:\n%s", got)
	}
}

func TestForNodeIsIdempotent(t *testing.T) {
	node := testNode("10.0.0.21", map[string]string{"example.com/a": "1"}, manifest.NodeTaint{Key: "dedicated", Effect: "NoSchedule"})
	first, err := ForNode(testPlan(t, sharedControlPlane), node)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ForNode(first.Plan(), node)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(second.Plan().Config.Content), string(first.Plan().Config.Content); got != want {
		t.Fatalf("second ForNode changed the input:\n%s\nwant:\n%s", got, want)
	}
	firstDigest, _ := first.InputDigest()
	secondDigest, _ := second.InputDigest()
	if firstDigest != secondDigest {
		t.Fatalf("digest changed: %s, %s", firstDigest, secondDigest)
	}
}

func TestForNodeNormalizesStaleRegistration(t *testing.T) {
	stale := testNode("10.0.0.21", map[string]string{"example.com/old": "1"}, manifest.NodeTaint{Key: "old", Effect: "NoSchedule"})
	registered, err := ForNode(testPlan(t, sharedControlPlane), stale)
	if err != nil {
		t.Fatal(err)
	}
	for name, node := range map[string]manifest.NodeConfig{
		"labels replaced":  testNode("10.0.0.21", map[string]string{"example.com/new": "2"}, manifest.NodeTaint{Key: "old", Effect: "NoSchedule"}),
		"taints replaced":  testNode("10.0.0.21", map[string]string{"example.com/old": "1"}, manifest.NodeTaint{Key: "new", Effect: "NoExecute"}),
		"address replaced": testNode("10.0.0.99", map[string]string{"example.com/old": "1"}, manifest.NodeTaint{Key: "old", Effect: "NoSchedule"}),
		"all cleared":      testNode("", nil),
		"all removed":      testNode("10.0.0.21", nil),
	} {
		t.Run(name, func(t *testing.T) {
			want := forNodeConfig(t, testPlan(t, sharedControlPlane), node)
			if got := forNodeConfig(t, registered.Plan(), node); got != want {
				t.Fatalf("stale registration survived:\n%s\nwant:\n%s", got, want)
			}
		})
	}
}

func TestForNodeExternalKeepsInput(t *testing.T) {
	node := testNode("10.0.0.21", map[string]string{"example.com/a": "1"})
	node.Kubernetes.External = true
	if got := forNodeConfig(t, testPlan(t, sharedControlPlane), node); got != sharedControlPlane {
		t.Fatalf("external node input changed:\n%s", got)
	}
}

func TestForNodeRefusesPathsOutsideInputTree(t *testing.T) {
	plan := testPlan(t, sharedControlPlane)
	plan.Config.RenderPath = "/etc/kubernetes/admin.conf"
	if _, err := ForNode(plan, testNode("10.0.0.21", nil)); err == nil {
		t.Fatal("ForNode() accepted a plan outside the kubeadm input tree")
	}
}

func TestNodeInputZero(t *testing.T) {
	if !(NodeInput{}).IsZero() {
		t.Fatal("zero NodeInput is not zero")
	}
	input, err := ForNode(testPlan(t, sharedControlPlane), testNode("", nil))
	if err != nil || input.IsZero() || input.Name() != "control-plane" {
		t.Fatalf("ForNode() = %+v, %v", input, err)
	}
}

func TestReadNodeInputNormalizesStoredForms(t *testing.T) {
	node := testNode("10.0.0.21", map[string]string{"example.com/a": "1"})
	want, err := ForNode(testPlan(t, sharedControlPlane), node)
	if err != nil {
		t.Fatal(err)
	}
	wantDigest, err := want.InputDigest()
	if err != nil {
		t.Fatal(err)
	}
	stale, err := ForNode(testPlan(t, sharedControlPlane), testNode("10.0.0.5", map[string]string{"example.com/gone": "x"}))
	if err != nil {
		t.Fatal(err)
	}
	for name, stored := range map[string]Plan{
		"shared":     testPlan(t, sharedControlPlane),
		"registered": want.Plan(),
		"stale":      stale.Plan(),
	} {
		t.Run(name, func(t *testing.T) {
			files, err := stored.InputFiles()
			if err != nil {
				t.Fatal(err)
			}
			got, err := ReadNodeInput("control-plane", files, node)
			if err != nil {
				t.Fatal(err)
			}
			if digest, err := got.InputDigest(); err != nil || digest != wantDigest {
				t.Fatalf("digest = %s, %v; want %s", digest, err, wantDigest)
			}
		})
	}
}

func TestSameKubeadmConfigIgnoresRegistration(t *testing.T) {
	shared := testPlan(t, sharedControlPlane)
	registered, err := ForNode(shared, testNode("10.0.0.21", map[string]string{"example.com/a": "1"}, manifest.NodeTaint{Key: "t", Effect: "NoSchedule"}))
	if err != nil {
		t.Fatal(err)
	}
	relabeled, err := ForNode(shared, testNode("10.0.0.22", map[string]string{"example.com/b": "2"}))
	if err != nil {
		t.Fatal(err)
	}
	if !registered.SameKubeadmConfig(relabeled) || !SameSharedConfig(shared, registered.Plan()) || !SameSharedConfig(registered.Plan(), shared) {
		t.Fatal("registration counted as a kubeadm change")
	}

	drifted := testPlan(t, strings.Replace(sharedControlPlane, "v1.36.1", "v1.36.2", 1))
	if SameSharedConfig(shared, drifted) || SameSharedConfig(registered.Plan(), drifted) {
		t.Fatal("kubeadm change went unnoticed")
	}
	patched := shared
	patched.Patches = nil
	if SameSharedConfig(shared, patched) {
		t.Fatal("patch removal went unnoticed")
	}
	owned := shared
	owned.NodeLocalKubelet = true
	if SameSharedConfig(shared, owned) {
		t.Fatal("kubelet ownership change went unnoticed")
	}
}

func TestSameSharedConfigIgnoresOptionalTerminalNewline(t *testing.T) {
	a := testPlan(t, sharedControlPlane)
	b := testPlan(t, strings.TrimSuffix(sharedControlPlane, "\n"))
	if !SameSharedConfig(a, b) {
		t.Fatal("terminal newline counted as a kubeadm change")
	}
}
