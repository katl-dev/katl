package apivip

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestControllerStartsReleasedThenAcquiresHealthyVIP(t *testing.T) {
	controller := testController(fakeHealth{result: HealthResult{Healthy: true, StatusCode: 200}})
	status, err := controller.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.OwnershipState != OwnershipReleased || status.ReleaseReason != "waiting-for-health-threshold" {
		t.Fatalf("first status = %#v", status)
	}
	status, err = controller.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	owner := controller.Owner.(*fakeVIPOwner)
	if !status.LocalVIPOwned || status.OwnershipState != OwnershipOwned || !reflect.DeepEqual(owner.changes, []bool{false, true}) {
		t.Fatalf("status = %#v, changes = %#v", status, owner.changes)
	}
}

func TestControllerReleasesVIPAfterHealthFailure(t *testing.T) {
	health := &sequenceHealth{results: []HealthResult{
		{Healthy: true}, {Healthy: true},
		{Error: "readyz Bearer secret-token"}, {Error: "readyz Bearer secret-token"}, {Error: "readyz Bearer secret-token"},
	}}
	controller := testController(health)
	var status Status
	for range health.results {
		var err error
		status, err = controller.RunOnce(context.Background())
		if err != nil {
			t.Fatal(err)
		}
	}
	if status.LocalVIPOwned || status.ReleaseReason != "local-health-failed" {
		t.Fatalf("status = %#v", status)
	}
	if strings.Contains(status.HealthFailure, "secret-token") || !strings.Contains(status.HealthFailure, "[REDACTED]") {
		t.Fatalf("health failure was not redacted: %q", status.HealthFailure)
	}
}

func TestControllerReportsUnavailableAPIAsHealthState(t *testing.T) {
	controller := testController(fakeHealth{result: HealthResult{Error: "waiting for kubeadm API CA"}})
	status, err := controller.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.HealthState != HealthUnhealthy || status.RecoveryRequired {
		t.Fatalf("status = %#v", status)
	}
}

func TestControllerReturnsVIPAcquisitionFailure(t *testing.T) {
	controller := testController(fakeHealth{result: HealthResult{Healthy: true}})
	controller.Config.Health.SuccessThreshold = 1
	controller.Owner.(*fakeVIPOwner).acquireErr = errors.New("netlink acquire failed")
	status, err := controller.RunOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "netlink acquire failed") || !status.RecoveryRequired {
		t.Fatalf("status = %#v, error = %v", status, err)
	}
}

func TestControllerStopReleasesVIPAndWritesStatus(t *testing.T) {
	dir := t.TempDir()
	controller := testController(fakeHealth{result: HealthResult{Healthy: true}})
	controller.Config.Health.SuccessThreshold = 1
	controller.Writer = FileStatusWriter{
		LivePath:      filepath.Join(dir, "run/status.json"),
		OperationPath: filepath.Join(dir, "var/status.json"),
	}
	if _, err := controller.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	status, err := controller.Stop(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.LocalVIPOwned || status.ReleaseReason != "service-stop" {
		t.Fatalf("status = %#v", status)
	}
	for _, path := range []string{controller.Writer.(FileStatusWriter).LivePath, controller.Writer.(FileStatusWriter).OperationPath} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var written Status
		if err := json.Unmarshal(data, &written); err != nil {
			t.Fatal(err)
		}
		if written.OwnershipState != OwnershipReleased {
			t.Fatalf("written status = %#v", written)
		}
	}
}

func TestHTTPHealthCheckerUsesConfiguredAddress(t *testing.T) {
	checker := HTTPHealthChecker{Client: &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != "https://127.0.0.1:6443/readyz" {
			t.Fatalf("URL = %s", request.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
	})}}
	if result := checker.Check(context.Background(), Health{Scheme: "https", Host: "127.0.0.1", Port: 6443, Path: "/readyz"}); !result.Healthy {
		t.Fatalf("result = %#v", result)
	}
}

func testController(health HealthChecker) *Controller {
	return &Controller{
		Config:            minimalConfig(),
		GenerationID:      "2026.08.31-001",
		AppPayloadVersion: "api-vip-v0.1.0",
		Health:            health,
		Interface:         fakeInterface{ready: true},
		Owner:             &fakeVIPOwner{},
		Clock:             func() time.Time { return time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC) },
	}
}

type fakeHealth struct{ result HealthResult }

func (h fakeHealth) Check(context.Context, Health) HealthResult { return h.result }

type sequenceHealth struct {
	results []HealthResult
	next    int
}

func (h *sequenceHealth) Check(context.Context, Health) HealthResult {
	result := h.results[min(h.next, len(h.results)-1)]
	h.next++
	return result
}

type fakeInterface struct {
	ready bool
	err   error
}

func (f fakeInterface) Ready(context.Context, Config) (bool, error) { return f.ready, f.err }

type fakeVIPOwner struct {
	owned      bool
	acquireErr error
	releaseErr error
	changes    []bool
}

func (o *fakeVIPOwner) Owned(context.Context, Config) (bool, error) { return o.owned, nil }

func (o *fakeVIPOwner) SetOwned(_ context.Context, _ Config, owned bool) error {
	o.changes = append(o.changes, owned)
	if owned && o.acquireErr != nil {
		return o.acquireErr
	}
	if !owned && o.releaseErr != nil {
		return o.releaseErr
	}
	o.owned = owned
	return nil
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}
