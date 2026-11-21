package kubeadmconfig

import (
	"reflect"
	"testing"

	"github.com/katl-dev/katl/internal/installer/manifest"
	"gopkg.in/yaml.v3"
)

func TestWithNodeRegistration(t *testing.T) {
	metadata := NodeRegistration{
		Labels: map[string]string{"example.com/b": "", "example.com/a": "1"},
		Taints: []manifest.NodeTaint{
			{Key: "dedicated", Value: "storage", Effect: "NoSchedule"},
			{Key: "gpu", Effect: "NoExecute"},
		},
	}
	const registeredMetadata = `
kubeletExtraArgs:
  - name: node-ip
    value: 10.0.0.21
  - name: node-labels
    value: example.com/a=1,example.com/b=
taints:
  - key: dedicated
    value: storage
    effect: NoSchedule
  - key: gpu
    effect: NoExecute
`
	tests := []struct {
		name         string
		config       string
		kind         string
		registration NodeRegistration
		want         string
	}{
		{
			name: "worker join",
			config: `apiVersion: kubeadm.k8s.io/v1beta4
kind: JoinConfiguration
nodeRegistration:
  criSocket: unix:///run/containerd/containerd.sock
`,
			kind:         "JoinConfiguration",
			registration: NodeRegistration{Address: "10.0.0.21", Labels: metadata.Labels, Taints: metadata.Taints},
			want:         "criSocket: unix:///run/containerd/containerd.sock" + registeredMetadata,
		},
		{
			name: "control-plane init replaces default taints",
			config: `apiVersion: kubeadm.k8s.io/v1beta4
kind: InitConfiguration
nodeRegistration:
  criSocket: unix:///run/containerd/containerd.sock
  taints: []
---
apiVersion: kubeadm.k8s.io/v1beta4
kind: ClusterConfiguration
`,
			kind:         "InitConfiguration",
			registration: NodeRegistration{Address: "10.0.0.21", Labels: metadata.Labels, Taints: metadata.Taints},
			want:         "criSocket: unix:///run/containerd/containerd.sock" + registeredMetadata,
		},
		{
			name: "no metadata keeps default taints",
			config: `apiVersion: kubeadm.k8s.io/v1beta4
kind: InitConfiguration
nodeRegistration:
  criSocket: unix:///run/containerd/containerd.sock
  taints: []
`,
			kind:         "InitConfiguration",
			registration: NodeRegistration{Address: "10.0.0.21"},
			want: `criSocket: unix:///run/containerd/containerd.sock
kubeletExtraArgs:
  - name: node-ip
    value: 10.0.0.21
taints: []
`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := PlanFromRenderedFiles("node", []File{{RenderPath: "/etc/katl/kubeadm/node/config.yaml", Content: []byte(tt.config)}})
			if err != nil {
				t.Fatal(err)
			}

			got, err := WithNodeRegistration(plan, tt.registration)
			if err != nil {
				t.Fatalf("WithNodeRegistration() error = %v", err)
			}
			var want any
			if err := yaml.Unmarshal([]byte(tt.want), &want); err != nil {
				t.Fatal(err)
			}
			if registration := nodeRegistrationOf(t, got, tt.kind); !reflect.DeepEqual(registration, want) {
				t.Fatalf("nodeRegistration = %#v, want %#v", registration, want)
			}

			// Stored node-specific input is specialized again when it is read back.
			again, err := WithNodeRegistration(got, tt.registration)
			if err != nil {
				t.Fatalf("WithNodeRegistration() on specialized input error = %v", err)
			}
			if string(again.Config.Content) != string(got.Config.Content) {
				t.Fatalf("respecialized input changed:\n%s\nwant:\n%s", again.Config.Content, got.Config.Content)
			}
		})
	}
}

func nodeRegistrationOf(t *testing.T, plan Plan, kind string) any {
	t.Helper()
	documents, err := decodeDocuments(plan.Config.Content)
	if err != nil {
		t.Fatal(err)
	}
	for _, document := range documents {
		if document["kind"] == kind {
			return document["nodeRegistration"]
		}
	}
	t.Fatalf("rendered input has no %s:\n%s", kind, plan.Config.Content)
	return nil
}
