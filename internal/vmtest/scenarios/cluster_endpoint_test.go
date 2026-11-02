package scenarios

import (
	"testing"

	"github.com/katl-dev/katl/internal/installer/configbundle"
	"github.com/katl-dev/katl/internal/installer/controlplaneendpoint"
)

func TestConfigurationRetainsClusterEndpoint(t *testing.T) {
	for _, tc := range []struct {
		endpoint, host string
		port           int
	}{
		{"192.0.2.10:6443", "192.0.2.10", 6443},
		{"api.unpublished.katl.test:7443", "api.unpublished.katl.test", 7443},
		{"[2001:db8::1]:6443", "2001:db8::1", 6443},
	} {
		t.Run(tc.endpoint, func(t *testing.T) {
			var source configbundle.SourceConfig
			source.Spec.ControlPlaneEndpoint = &controlplaneendpoint.Config{Host: "fixture.example", Port: 443}

			if err := retainClusterEndpoint(&source, []byte("controlPlaneEndpoint: '"+tc.endpoint+"'\n")); err != nil {
				t.Fatal(err)
			}

			got := source.Spec.ControlPlaneEndpoint
			if got.Host != tc.host || got.Port != tc.port {
				t.Fatalf("retained endpoint = %+v, want %s:%d", got, tc.host, tc.port)
			}
		})
	}
}
