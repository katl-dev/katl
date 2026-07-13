package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/installer/handoff"
	installstatus "github.com/katl-dev/katl/internal/installer/status"
)

func TestInstallDiscoverFindsWaitingInstallerAndDisks(t *testing.T) {
	oldAddrs := installerInterfaceAddrs
	installerInterfaceAddrs = func() ([]net.Addr, error) {
		return []net.Addr{&net.IPNet{IP: net.ParseIP("192.0.2.5"), Mask: net.CIDRMask(24, 32)}}, nil
	}
	t.Cleanup(func() { installerInterfaceAddrs = oldAddrs })
	oldProbe := installerDiscoveryProbe
	installerDiscoveryProbe = func(_ context.Context, endpoint string, _ time.Duration) (handoff.HandoffStatus, error) {
		if endpoint != "http://192.0.2.42:8080" {
			return handoff.HandoffStatus{}, fmt.Errorf("not an installer")
		}
		return handoff.HandoffStatus{State: handoff.HandoffWaiting, InstallStatus: installstatus.New(installstatus.StateWaitingForConfig, time.Now()), Disks: []handoff.HandoffDisk{{Path: "/dev/vda", ByID: []string{"/dev/disk/by-id/virtio-root"}, Selectable: true}}}, nil
	}
	t.Cleanup(func() { installerDiscoveryProbe = oldProbe })

	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"install", "discover"}, &stdout, &stderr); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	var report installDiscoveryReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Installers) != 1 || report.Installers[0].Endpoint != "http://192.0.2.42:8080" || !report.Installers[0].Status.Disks[0].Selectable {
		t.Fatalf("report = %#v", report)
	}
	endpoint, err := resolveInstallerEndpoint(context.Background(), "", time.Second)
	if err != nil || endpoint != "http://192.0.2.42:8080" {
		t.Fatalf("resolveInstallerEndpoint() = %q, %v", endpoint, err)
	}
}

func TestInstallStatusReportsWaitingInstaller(t *testing.T) {
	server := handoff.NewHandoffServer(nil)
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{"install", "status", "--endpoint", ts.URL}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run() error = %v, stderr=%s", err, stderr.String())
	}
	var report installHandoffReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode report: %v\n%s", err, stdout.String())
	}
	if report.Kind != "InstallHandoffReport" || report.Endpoint != ts.URL || report.Handoff.State != handoff.HandoffWaiting || report.Handoff.InstallStatus.State != installstatus.StateWaitingForConfig {
		t.Fatalf("report = %#v", report)
	}
}

func TestInstallApplyCompilesAndSubmitsSource(t *testing.T) {
	sourcePath := writeClusterConfig(t)
	server := handoff.NewHandoffServer(nil)
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{
		"install", "apply", sourcePath,
		"--endpoint", ts.URL,
		"--node", "cp-1",
		"--no-wait",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run() error = %v, stderr=%s", err, stderr.String())
	}
	var report installHandoffReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode report: %v\n%s", err, stdout.String())
	}
	if report.SelectedNode != "cp-1" || report.Handoff.State != handoff.HandoffAccepted || !report.Handoff.BundleAccepted || strings.Contains(stdout.String(), "bundleDigest") {
		t.Fatalf("report = %#v", report)
	}
	payload := server.Bundle()
	if payload.NodeName != "cp-1" || len(payload.Data) == 0 {
		t.Fatalf("server bundle = node=%q bytes=%d", payload.NodeName, len(payload.Data))
	}
}

func TestInstallApplyHelpKeepsBundleInternal(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{"install", "apply", "--help"}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run() error = %v, stderr=%s", err, stderr.String())
	}
	help := stdout.String()
	if !strings.Contains(help, "katlctl install apply SOURCE") {
		t.Fatalf("help does not advertise source input:\n%s", help)
	}
	for _, hiddenDetail := range []string{"--config-bundle", "--token", "--token-file"} {
		if strings.Contains(help, hiddenDetail) {
			t.Fatalf("help exposes %q:\n%s", hiddenDetail, help)
		}
	}
}

func TestInstallApplyWaitsForRebootReady(t *testing.T) {
	sourcePath := writeClusterConfig(t)
	var statusRequests atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, _ *http.Request) {
		request := statusRequests.Add(1)
		status := installTestStatus(handoff.HandoffWaiting, installstatus.StateWaitingForConfig, "")
		if request > 1 {
			status = installTestStatus(handoff.HandoffAccepted, installstatus.StateRebootRequested, "Reboot")
			status.BundleAccepted = true
			status.SelectedNode = "cp-1"
		}
		_ = json.NewEncoder(w).Encode(status)
	})
	mux.HandleFunc("POST /v1/config-bundle", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.URL.Query().Get("node") != "cp-1" || !strings.HasPrefix(r.URL.Query().Get("digest"), "sha256:") {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		status := installTestStatus(handoff.HandoffAccepted, installstatus.StateRunning, "WaitForLocalConfig")
		status.BundleAccepted = true
		status.SelectedNode = "cp-1"
		_ = json.NewEncoder(w).Encode(status)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{
		"install", "apply", sourcePath,
		"--endpoint", ts.URL,
		"--node", "cp-1",
		"--timeout", "5s",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run() error = %v, stderr=%s", err, stderr.String())
	}
	var report installHandoffReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Handoff.InstallStatus.State != installstatus.StateRebootRequested || !strings.Contains(stderr.String(), "state=reboot-requested") {
		t.Fatalf("report=%#v stderr=%q", report, stderr.String())
	}
}

func TestInstallApplyReportsClassifiedFailure(t *testing.T) {
	sourcePath := writeClusterConfig(t)
	var statusRequests atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, _ *http.Request) {
		state := installTestStatus(handoff.HandoffWaiting, installstatus.StateWaitingForConfig, "")
		if statusRequests.Add(1) > 1 {
			state = installTestStatus(handoff.HandoffAccepted, installstatus.StateFailedBeforeMutation, "Validate")
			state.InstallStatus.LastError = "target disk was not found"
		}
		_ = json.NewEncoder(w).Encode(state)
	})
	mux.HandleFunc("POST /v1/config-bundle", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(installTestStatus(handoff.HandoffAccepted, installstatus.StateRunning, "Validate"))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{
		"install", "apply", sourcePath, "--endpoint", ts.URL,
		"--node", "cp-1", "--timeout", "5s",
	}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "failed-before-mutation") || !strings.Contains(err.Error(), "target disk was not found") {
		t.Fatalf("run() error = %v", err)
	}
	if stdout.Len() == 0 {
		t.Fatal("classified failure did not print final report")
	}
}

func TestInstallApplyValidatesLocallyBeforeNetwork(t *testing.T) {
	sourcePath := writeClusterConfig(t)
	var requests atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	defer ts.Close()

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{
		"install", "apply", sourcePath, "--endpoint", ts.URL,
		"--node", "missing-node", "--no-wait",
	}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "missing-node") {
		t.Fatalf("run() error = %v", err)
	}
	if requests.Load() != 0 {
		t.Fatalf("network requests = %d, want 0", requests.Load())
	}
}

func TestInstallApplySendsNoAuthorization(t *testing.T) {
	sourcePath := writeClusterConfig(t)
	var requests atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(installTestStatus(handoff.HandoffWaiting, installstatus.StateWaitingForConfig, ""))
			return
		}
		requests.Add(1)
		if r.Header.Get("Authorization") != "" || r.Header.Get("X-Katl-Install-Token") != "" {
			http.Error(w, "unexpected authorization", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(installTestStatus(handoff.HandoffAccepted, installstatus.StateRunning, "Validate"))
	}))
	defer ts.Close()

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{
		"install", "apply", sourcePath, "--endpoint", ts.URL,
		"--node", "cp-1", "--no-wait",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if requests.Load() != 1 {
		t.Fatalf("POST requests = %d, want 1", requests.Load())
	}
}

func TestInstallApplyRejectsInvalidEndpoint(t *testing.T) {
	for _, endpoint := range []string{"installer.test:8080", "ftp://installer.test", "http://user@installer.test", "http://installer.test/path", "http://installer.test?token=secret"} {
		var stdout, stderr bytes.Buffer
		err := run(context.Background(), []string{"install", "status", "--endpoint", endpoint}, &stdout, &stderr)
		if err == nil {
			t.Fatalf("endpoint %q succeeded", endpoint)
		}
	}

}

func installTestStatus(handoffState handoff.HandoffState, state, step string) handoff.HandoffStatus {
	return handoff.HandoffStatus{
		State: handoffState,
		InstallStatus: installstatus.Record{
			APIVersion:  installstatus.APIVersion,
			Kind:        installstatus.Kind,
			State:       state,
			CurrentStep: step,
			UpdatedAt:   time.Now().UTC(),
		},
	}
}

func TestInstallApplyRejectsUnavailableStatusAfterSubmission(t *testing.T) {
	sourcePath := writeClusterConfig(t)
	var submitted atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			submitted.Store(true)
			_ = json.NewEncoder(w).Encode(installTestStatus(handoff.HandoffAccepted, installstatus.StateRunning, "Partition"))
			return
		}
		if submitted.Load() {
			http.Error(w, "gone", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(installTestStatus(handoff.HandoffWaiting, installstatus.StateWaitingForConfig, ""))
	}))
	defer ts.Close()

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{
		"install", "apply", sourcePath, "--endpoint", ts.URL,
		"--node", "cp-1", "--timeout", "3s",
	}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "became unavailable") || !strings.Contains(err.Error(), "Partition") {
		t.Fatalf("run() error = %v", err)
	}
}

func TestInstallRequestRejectsOversizedResponse(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, strings.Repeat("x", maxInstallResponseSize+1))
	}))
	defer ts.Close()
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{"install", "status", "--endpoint", ts.URL}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "response exceeds") {
		t.Fatalf("run() error = %v", err)
	}
}
