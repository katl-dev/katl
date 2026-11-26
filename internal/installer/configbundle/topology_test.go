package configbundle

import (
	"strings"
	"testing"
)

func TestSourceTopologyExternalEndpoint(t *testing.T) {
	for _, tt := range []struct {
		name     string
		url      string
		endpoint string
	}{
		{name: "missing scheme", url: "api.example:6443"},
		{name: "http scheme", url: "http://api.example:6443"},
		{name: "missing port", url: "https://api.example"},
		{name: "https URL", url: "https://api.example:6443", endpoint: "api.example:6443"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var source SourceConfig
			source.Spec.Kubernetes.External = &SourceExternalKubernetes{Endpoint: tt.url}
			source.Spec.Nodes = []SourceNode{{Name: "worker-1"}}

			inv, err := SourceTopology(source)
			if tt.endpoint == "" {
				if err == nil || !strings.Contains(err.Error(), "spec.kubernetes.external.endpoint") || !strings.Contains(err.Error(), "https://api.example:6443") {
					t.Fatalf("SourceTopology(%q) = %v, want field and HTTPS endpoint guidance", tt.url, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := inv.ControlPlaneEndpoint.String(); !inv.ExternalControlPlane || got != tt.endpoint {
				t.Errorf("endpoint = %q, external = %t, want %q and external", got, inv.ExternalControlPlane, tt.endpoint)
			}
		})
	}
}
