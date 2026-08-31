package agent

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/apiproxy"
)

func TestNodeAPIProxyStatusDistinguishesAccessAndPublication(t *testing.T) {
	root := t.TempDir()
	config := testNodeAPIProxyConfig()
	content, err := apiproxy.Render(config)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(root, apiproxy.ConfigPath), content)
	now := time.Date(2026, 8, 31, 15, 0, 0, 0, time.UTC)
	live := apiproxy.Status{
		APIVersion: apiproxy.APIVersion, Kind: apiproxy.StatusKind, UpdatedAt: now,
		Listeners: config.Listeners,
		Backends: []apiproxy.BackendStatus{{
			Name: "cp-1", Address: "192.0.2.11:6443", Local: true,
			Eligible: true, Reason: "ready", LastChecked: now,
		}},
		Canonical: apiproxy.CanonicalEndpointStatus{
			Endpoint: "api.unpublished.katl.test:6443", State: "unreachable", Reason: "no such host", LastChecked: now,
		},
	}
	data, err := json.Marshal(live)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(root, apiproxy.StatusPath), string(data))

	status, err := nodeAPIProxyStatus(root)
	if err != nil {
		t.Fatal(err)
	}
	if status.GetState() != "ready" || !status.GetLocalApiEligible() || status.GetCanonicalState() != "unreachable" {
		t.Fatalf("API proxy status = %#v", status)
	}
	if len(status.GetListeners()) != 2 || len(status.GetBackends()) != 1 || !status.GetBackends()[0].GetEligible() {
		t.Fatalf("API proxy detail = %#v", status)
	}
}

func TestNodeAPIProxyStatusReportsConfiguredButUnavailable(t *testing.T) {
	root := t.TempDir()
	content, err := apiproxy.Render(testNodeAPIProxyConfig())
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(root, apiproxy.ConfigPath), content)
	status, err := nodeAPIProxyStatus(root)
	if err != nil {
		t.Fatal(err)
	}
	if status.GetState() != "unavailable" || status.GetCanonicalState() != "not-checked" || status.GetFailureReason() == "" {
		t.Fatalf("API proxy status = %#v", status)
	}
}

func testNodeAPIProxyConfig() apiproxy.Config {
	return apiproxy.Config{
		TLSName: "api.unpublished.katl.test",
		Listeners: []apiproxy.Listener{
			{Address: "127.0.0.1:7445", Exposure: apiproxy.ExposureNodeLocal},
			{Address: "192.0.2.11:7445", Exposure: apiproxy.ExposureWorkstation},
		},
		Backends: []apiproxy.Backend{{Name: "cp-1", Address: "192.0.2.11:6443", Local: true}},
	}
}
