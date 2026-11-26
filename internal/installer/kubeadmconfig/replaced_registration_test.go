package kubeadmconfig

import (
	"reflect"
	"testing"

	"github.com/katl-dev/katl/internal/installer/manifest"
)

func TestReplacedRegistration(t *testing.T) {
	const initConfig = "apiVersion: kubeadm.k8s.io/v1beta4\nkind: InitConfiguration\n"
	const joinConfig = "apiVersion: kubeadm.k8s.io/v1beta4\nkind: JoinConfiguration\ncontrolPlane:\n  localAPIEndpoint:\n    advertiseAddress: 10.0.0.9\n"
	taint := manifest.NodeTaint{Key: "dedicated", Value: "storage", Effect: "NoSchedule"}
	node := testNode("10.0.0.21", map[string]string{"b": "2", "a": "1"}, taint)

	registered, err := ForNode(testPlan(t, initConfig), node)
	if err != nil {
		t.Fatal(err)
	}
	external := node
	external.Kubernetes.External = true

	tests := []struct {
		name   string
		config string
		plan   *Plan
		node   manifest.NodeConfig
		want   []string
	}{
		{name: "own output", plan: ptr(registered.Plan()), node: node},
		{name: "no registration", config: initConfig, node: node},
		{
			name:   "other node-ip",
			config: initConfig + "nodeRegistration:\n  kubeletExtraArgs:\n    - name: node-ip\n      value: 10.9.9.9\n",
			node:   node,
			want:   []string{"InitConfiguration nodeRegistration.kubeletExtraArgs node-ip (nodes[].kubernetes.address)"},
		},
		{
			name:   "labels in another order",
			config: initConfig + "nodeRegistration:\n  kubeletExtraArgs:\n    - name: node-labels\n      value: b=2,a=1\n",
			node:   node,
		},
		{
			name:   "labels with spaces and empty items",
			config: initConfig + "nodeRegistration:\n  kubeletExtraArgs:\n    - name: node-labels\n      value: \"b=2, a=1,\"\n",
			node:   node,
		},
		{
			name:   "other init advertiseAddress",
			config: initConfig + "localAPIEndpoint:\n  advertiseAddress: 10.9.9.9\n",
			node:   node,
			want:   []string{"InitConfiguration localAPIEndpoint.advertiseAddress (nodes[].kubernetes.address)"},
		},
		{
			name:   "other join advertiseAddress",
			config: joinConfig,
			node:   node,
			want:   []string{"JoinConfiguration controlPlane.localAPIEndpoint.advertiseAddress (nodes[].kubernetes.address)"},
		},
		{
			name:   "other taints",
			config: initConfig + "nodeRegistration:\n  taints:\n    - key: other\n      effect: NoSchedule\n",
			node:   node,
			want:   []string{"InitConfiguration nodeRegistration.taints (nodes[].kubernetes.taints)"},
		},
		{
			name:   "cleared taints on a node without taints",
			config: initConfig + "nodeRegistration:\n  taints: []\n",
			node:   testNode("10.0.0.21", nil),
		},
		{
			name:   "external node",
			config: initConfig + "localAPIEndpoint:\n  advertiseAddress: 10.9.9.9\n",
			node:   external,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var plan Plan
			if tt.plan == nil {
				plan = testPlan(t, tt.config)
			} else {
				plan = *tt.plan
			}
			got, err := ReplacedRegistration(plan, tt.node)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("ReplacedRegistration() = %q, want %q", got, tt.want)
			}
		})
	}
}

func ptr[T any](value T) *T { return &value }
