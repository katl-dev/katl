package platformendpoint

import (
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/installer/apivip"
)

func TestComposeHostManagedVIP(t *testing.T) {
	plan, err := Compose(Config{
		Mode:           ModeHostManagedVIP,
		Endpoint:       Endpoint{Host: "api.home.example", Port: 6443, Provenance: "platform-host"},
		APIEndpointVIP: ptr(minimalVIPConfig()),
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.ControlPlaneEndpoint != "api.home.example:6443" || plan.APIEndpointVIP == nil || plan.HelperStatus.AppID != apivip.AppID {
		t.Fatalf("plan = %#v", plan)
	}
	files := NativeEtcFiles(plan)
	for _, file := range files {
		if file.Path == apivip.ConfigPath && strings.Contains(file.Content, "kind: APIEndpointVIP") {
			return
		}
	}
	t.Fatalf("API VIP config missing from %#v", files)
}

func TestComposeExternalRejectsVIPConfig(t *testing.T) {
	_, err := Compose(Config{Mode: ModeExternal, Endpoint: Endpoint{Host: "api.example"}, APIEndpointVIP: ptr(minimalVIPConfig())})
	if err == nil || !strings.Contains(err.Error(), "must not set apiEndpointVIP") {
		t.Fatalf("Compose() error = %v", err)
	}
}

func minimalVIPConfig() apivip.Config {
	return apivip.Config{Endpoint: apivip.Endpoint{Host: "api.home.example", VIP: "10.40.0.10/32"}}
}

func ptr(config apivip.Config) *apivip.Config { return &config }
