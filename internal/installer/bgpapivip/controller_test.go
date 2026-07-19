package bgpapivip

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestControllerStartsWithdrawnThenAdvertisesAfterHealthy(t *testing.T) {
	bird := &fakeBird{status: readyBirdStatus()}
	controller := testController(bird, fakeHealth{result: HealthResult{Healthy: true, StatusCode: 200}})
	status, err := controller.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if status.AdvertisementState != AdvertisementWithdrawn || status.WithdrawReason != "waiting-for-health-threshold" {
		t.Fatalf("first status = %#v", status)
	}
	status, err = controller.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("second RunOnce() error = %v", err)
	}
	if got, want := bird.advertisements, []bool{false, true}; !reflect.DeepEqual(got, want) {
		t.Fatalf("advertisements = %#v, want %#v", got, want)
	}
	if status.AdvertisementState != AdvertisementAdvertised || status.Withdrawn {
		t.Fatalf("status advertisement = %#v", status)
	}
	if status.HealthState != HealthHealthy || status.HealthTarget != "https://10.40.0.10:6443/readyz" {
		t.Fatalf("status health = %#v", status)
	}
}

func TestControllerWithdrawsAfterHealthFailure(t *testing.T) {
	bird := &fakeBird{status: readyBirdStatus()}
	health := &sequenceHealth{results: []HealthResult{
		{Healthy: true, StatusCode: 200},
		{Healthy: true, StatusCode: 200},
		{Healthy: false, StatusCode: 503, Error: "readyz failed: Bearer secret-token"},
		{Healthy: false, StatusCode: 503, Error: "readyz failed: Bearer secret-token"},
		{Healthy: false, StatusCode: 503, Error: "readyz failed: Bearer secret-token"},
	}}
	controller := testController(bird, health)
	if _, err := controller.RunOnce(context.Background()); err != nil {
		t.Fatalf("first healthy RunOnce() error = %v", err)
	}
	if _, err := controller.RunOnce(context.Background()); err != nil {
		t.Fatalf("second healthy RunOnce() error = %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := controller.RunOnce(context.Background()); err != nil {
			t.Fatalf("unhealthy RunOnce(%d) error = %v", i, err)
		}
	}
	status, err := controller.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("unhealthy RunOnce() error = %v", err)
	}
	if got, want := bird.advertisements, []bool{false, true, false}; !reflect.DeepEqual(got, want) {
		t.Fatalf("advertisements = %#v, want %#v", got, want)
	}
	if status.AdvertisementState != AdvertisementWithdrawn || !status.Withdrawn || status.WithdrawReason != "local-health-failed" {
		t.Fatalf("status advertisement = %#v", status)
	}
	if strings.Contains(status.HealthFailure, "secret-token") || !strings.Contains(status.HealthFailure, "[REDACTED]") {
		t.Fatalf("status leaked health failure: %#v", status.HealthFailure)
	}
}

func TestControllerReportsUnavailableAPIAsHealthStateNotRuntimeFailure(t *testing.T) {
	bird := &fakeBird{status: readyBirdStatus()}
	controller := testController(bird, fakeHealth{result: HealthResult{Error: "waiting for kubeadm API CA"}})
	status, err := controller.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if status.HealthState != HealthUnhealthy || status.HealthFailure != "waiting for kubeadm API CA" {
		t.Fatalf("health status = %#v", status)
	}
	if status.RecoveryRequired || status.FailureReason != "" {
		t.Fatalf("API readiness was reported as a controller failure: %#v", status)
	}
}

func TestControllerRestartStartsWithdrawnBeforeAdvertising(t *testing.T) {
	bird := &fakeBird{status: readyBirdStatus()}
	controller := testController(bird, fakeHealth{result: HealthResult{Healthy: true, StatusCode: 200}})
	controller.started = true
	controller.advertised = true

	restarted := testController(bird, fakeHealth{result: HealthResult{Healthy: true, StatusCode: 200}})
	if _, err := restarted.RunOnce(context.Background()); err != nil {
		t.Fatalf("first RunOnce() error = %v", err)
	}
	if _, err := restarted.RunOnce(context.Background()); err != nil {
		t.Fatalf("second RunOnce() error = %v", err)
	}
	if got, want := bird.advertisements, []bool{false, true}; !reflect.DeepEqual(got, want) {
		t.Fatalf("restart advertisements = %#v, want %#v", got, want)
	}
}

func TestControllerStopWithdrawsAndWritesDurableStatus(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "run", "status.json")
	operation := filepath.Join(dir, "var", "operation-status.json")
	bird := &fakeBird{status: readyBirdStatus()}
	controller := testController(bird, fakeHealth{result: HealthResult{Healthy: true, StatusCode: 200}})
	controller.Writer = FileStatusWriter{LivePath: live, OperationPath: operation}
	if _, err := controller.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	status, err := controller.Stop(context.Background())
	if err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if status.AdvertisementState != AdvertisementWithdrawn || status.WithdrawReason != "service-stop" {
		t.Fatalf("stop status = %#v", status)
	}
	for _, path := range []string{live, operation} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read status %s: %v", path, err)
		}
		var written Status
		if err := json.Unmarshal(data, &written); err != nil {
			t.Fatalf("decode status %s: %v\n%s", path, err, data)
		}
		if written.AdvertisementState != AdvertisementWithdrawn || written.SelectedGeneration != "2026.06.19-001" {
			t.Fatalf("written status %s = %#v", path, written)
		}
	}
	if got := bird.advertisements[len(bird.advertisements)-1]; got {
		t.Fatalf("last advertisement = true, want withdrawal")
	}
}

func TestControllerStatusRedactsBirdFailuresAndPeerState(t *testing.T) {
	bird := &fakeBird{
		status: BirdRuntimeStatus{
			ProcessActive:      true,
			ControlSocketReady: true,
			ReadinessState:     "ready",
			FailureReason:      "bird failed with Bearer top-secret",
			Peers: []PeerRuntimeStatus{{
				Name:            "router-a",
				Kind:            "fabric",
				AddressFamily:   "ipv4",
				SessionState:    "idle",
				AdminState:      "start",
				AuthConfigured:  true,
				FailureCategory: "auth Bearer peer-secret failed",
			}},
		},
	}
	controller := testController(bird, fakeHealth{result: HealthResult{Healthy: true, StatusCode: 200}})
	controller.Config.Health.SuccessThreshold = 1
	status, err := controller.RunOnce(context.Background())
	if err == nil || strings.Contains(err.Error(), "top-secret") {
		t.Fatalf("RunOnce() error = %v, want redacted bird failure", err)
	}
	if strings.Contains(status.FailureReason, "top-secret") || !strings.Contains(status.FailureReason, "[REDACTED]") {
		t.Fatalf("status leaked failure: %#v", status.FailureReason)
	}
	if len(status.PeerSummary) != 1 || strings.Contains(status.PeerSummary[0].FailureCategory, "peer-secret") {
		t.Fatalf("peer summary leaked secret: %#v", status.PeerSummary)
	}
}

func TestControllerReturnsAdvertisementFailureAndKeepsWithdrawn(t *testing.T) {
	bird := &fakeBird{status: readyBirdStatus(), setErr: errors.New("birdc configure failed")}
	controller := testController(bird, fakeHealth{result: HealthResult{Healthy: true, StatusCode: 200}})
	controller.Config.Health.SuccessThreshold = 1
	status, err := controller.RunOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "birdc configure failed") {
		t.Fatalf("RunOnce() error = %v, want bird failure", err)
	}
	if status.AdvertisementState != AdvertisementWithdrawn || !status.RecoveryRequired {
		t.Fatalf("status = %#v", status)
	}
}

func TestControllerWithdrawsWhenDependencyNotReady(t *testing.T) {
	bird := &fakeBird{status: readyBirdStatus()}
	controller := testController(bird, fakeHealth{result: HealthResult{Healthy: true, StatusCode: 200}})
	controller.Config.Health.SuccessThreshold = 1
	if _, err := controller.RunOnce(context.Background()); err != nil {
		t.Fatalf("healthy RunOnce() error = %v", err)
	}
	controller.Interface = fakeInterface{ready: false}
	status, err := controller.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("dependency RunOnce() error = %v", err)
	}
	if got, want := bird.advertisements, []bool{false, true, false}; !reflect.DeepEqual(got, want) {
		t.Fatalf("advertisements = %#v, want %#v", got, want)
	}
	if status.VIPInterfaceReady || status.WithdrawReason != "dependency-not-ready" {
		t.Fatalf("status = %#v", status)
	}
}

func testController(bird *fakeBird, health HealthChecker) *Controller {
	return &Controller{
		Config:            minimalConfig(),
		GenerationID:      "2026.06.19-001",
		AppPayloadVersion: "bgp-api-vip-v0.1.0",
		Bird:              bird,
		Health:            health,
		Clock: func() time.Time {
			return time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
		},
	}
}

func readyBirdStatus() BirdRuntimeStatus {
	return BirdRuntimeStatus{
		ProcessActive:      true,
		ControlSocketReady: true,
		ControlSocketPath:  "/run/katl/apps/bird/bird.ctl",
		ReadinessState:     "ready",
		Peers: []PeerRuntimeStatus{{
			Name:           "router-a",
			Kind:           "fabric",
			AddressFamily:  "ipv4",
			SessionState:   "established",
			AdminState:     "up",
			LocalAddress:   "10.0.0.11",
			LastTransition: "2026-06-19T12:00:00Z",
		}},
	}
}

type fakeBird struct {
	status         BirdRuntimeStatus
	statusErr      error
	setErr         error
	advertisements []bool
}

func (b *fakeBird) Status(context.Context) (BirdRuntimeStatus, error) {
	return b.status, b.statusErr
}

func (b *fakeBird) SetAdvertisement(_ context.Context, enabled bool) error {
	b.advertisements = append(b.advertisements, enabled)
	return b.setErr
}

type fakeHealth struct {
	result HealthResult
}

func (h fakeHealth) Check(context.Context, Health) HealthResult {
	return h.result
}

type sequenceHealth struct {
	results []HealthResult
	next    int
}

func (h *sequenceHealth) Check(context.Context, Health) HealthResult {
	if h.next >= len(h.results) {
		return h.results[len(h.results)-1]
	}
	result := h.results[h.next]
	h.next++
	return result
}

type fakeInterface struct {
	ready bool
	err   error
}

func (i fakeInterface) Ready(context.Context, Config) (bool, error) {
	return i.ready, i.err
}
