package configapply

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/manifest"
)

func TestKubernetesMetadataChange(t *testing.T) {
	current := manifestWithMetadata(
		map[string]string{"example.com/pool": "a", "example.com/old": "x"},
		[]manifest.NodeTaint{{Key: "dedicated", Value: "storage", Effect: "NoSchedule"}},
	)
	desiredLabels := map[string]string{"example.com/pool": "b", "example.com/new": ""}
	desiredTaints := []manifest.NodeTaint{{Key: "gpu", Effect: "NoExecute"}}

	tests := []struct {
		name         string
		registered   bool
		labels       map[string]string
		taints       []manifest.NodeTaint
		wantWarnings []string
	}{
		{
			name:   "before join",
			labels: desiredLabels,
			taints: desiredTaints,
		},
		{
			name:       "after join",
			registered: true,
			labels:     desiredLabels,
			taints:     desiredTaints,
			wantWarnings: []string{"Kubernetes node labels and taints apply when a node joins; cp-1 has joined, so Katl recorded the change without updating its Node. To update the Node, run: " +
				"kubectl label node cp-1 --overwrite example.com/new= example.com/pool=b example.com/old-; " +
				"kubectl taint node cp-1 --overwrite gpu:NoExecute dedicated:NoSchedule-"},
		},
		{
			name:       "cleared after join",
			registered: true,
			wantWarnings: []string{"Kubernetes node labels and taints apply when a node joins; cp-1 has joined, so Katl recorded the change without updating its Node. To update the Node, run: " +
				"kubectl label node cp-1 --overwrite example.com/old- example.com/pool-; " +
				"kubectl taint node cp-1 --overwrite dedicated:NoSchedule-"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			merged, changes := mergeRenderedMetadata(t, current, tt.registered, tt.labels, tt.taints)

			if !maps.Equal(merged.Node.Bootstrap.Labels, tt.labels) || !slices.Equal(merged.Node.Bootstrap.Taints, tt.taints) {
				t.Fatalf("recorded metadata = %v %v, want %v %v", merged.Node.Bootstrap.Labels, merged.Node.Bootstrap.Taints, tt.labels, tt.taints)
			}
			if merged.Node.Bootstrap.ClusterName != "lab" {
				t.Fatalf("bootstrap intent lost its other fields: %#v", merged.Node.Bootstrap)
			}

			if len(changes) != 1 || changes[0].Domain != DomainKubernetesNodeMetadata {
				t.Fatalf("changes = %#v, want only %s", changes, DomainKubernetesNodeMetadata)
			}
			if !slices.Equal(changes[0].Warnings, tt.wantWarnings) {
				t.Fatalf("warnings = %q, want %q", changes[0].Warnings, tt.wantWarnings)
			}
			decision, err := Plan(generation.ApplyModeAuto, changes)
			if err != nil || decision.AcceptedMode != generation.ApplyModeLive {
				t.Fatalf("Plan() = %#v, %v; want a live apply", decision, err)
			}
		})
	}
}

func TestKubernetesMetadataUnchanged(t *testing.T) {
	labels := map[string]string{"example.com/pool": "a"}
	taints := []manifest.NodeTaint{{Key: "dedicated", Effect: "NoSchedule"}}
	request := renderedMetadataRequest(t, manifestWithMetadata(labels, taints), true, labels, taints)

	if _, _, _, err := mergeRuntimeConfig(request); !errors.Is(err, ErrNoChanges) {
		t.Fatalf("mergeRuntimeConfig() error = %v, want ErrNoChanges", err)
	}
}

func mergeRenderedMetadata(t *testing.T, current manifest.Manifest, registered bool, labels map[string]string, taints []manifest.NodeTaint) (manifest.Manifest, []Change) {
	t.Helper()
	merged, changes, _, err := mergeRuntimeConfig(renderedMetadataRequest(t, current, registered, labels, taints))
	if err != nil {
		t.Fatalf("mergeRuntimeConfig() error = %v", err)
	}
	return merged, changes
}

// renderedMetadataRequest carries desired metadata through the workstation
// rendering and node decoding of a NodeConfigurationChange.
func renderedMetadataRequest(t *testing.T, current manifest.Manifest, registered bool, labels map[string]string, taints []manifest.NodeTaint) TrustedBundleRequest {
	t.Helper()
	desired := manifestWithMetadata(labels, taints)
	data, err := RenderNodeConfigurationChange(RenderNodeRequest{NodeName: "cp-1", Manifest: desired, SourceID: "lab", DesiredVersion: "2"})
	if err != nil {
		t.Fatal(err)
	}
	if hasMetadata := strings.Contains(string(data), "kubernetesMetadata:"); hasMetadata != (len(labels) > 0 || len(taints) > 0) {
		t.Fatalf("rendered kubernetesMetadata presence = %t for labels %v taints %v:\n%s", hasMetadata, labels, taints, data)
	}
	base := trustedBundleRequest(t.TempDir(), TrustedBundleRequest{CurrentManifest: current, KubeletRegistered: registered})
	request, err := DecodeNodeConfigurationChange(strings.NewReader(string(data)), base)
	if err != nil {
		t.Fatalf("DecodeNodeConfigurationChange() error = %v\n%s", err, data)
	}
	return request
}

func manifestWithMetadata(labels map[string]string, taints []manifest.NodeTaint) manifest.Manifest {
	out := baseManifest()
	out.Node.Bootstrap = &manifest.BootstrapIntent{ClusterName: "lab", Labels: labels, Taints: taints}
	return out
}
