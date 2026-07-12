package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/installer/handoff"
	installstatus "github.com/katl-dev/katl/internal/installer/status"
)

func TestInstallStatusReportsWaitingInstaller(t *testing.T) {
	server, err := handoff.NewHandoffServer("install-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	var stdout, stderr bytes.Buffer
	err = run(context.Background(), []string{"install", "status", "--endpoint", ts.URL}, &stdout, &stderr)
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

func TestInstallApplyValidatesAndSubmitsBundle(t *testing.T) {
	bundlePath, bundleDigest := writeConfigBundle(t)
	server, err := handoff.NewHandoffServer("install-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	var stdout, stderr bytes.Buffer
	err = run(context.Background(), []string{
		"install", "apply",
		"--endpoint", ts.URL,
		"--token", "install-token",
		"--config-bundle", bundlePath,
		"--config-bundle-digest", bundleDigest,
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
	if report.SelectedNode != "cp-1" || report.BundleDigest != bundleDigest || report.Handoff.State != handoff.HandoffAccepted || !report.Handoff.BundleAccepted {
		t.Fatalf("report = %#v", report)
	}
	payload := server.Bundle()
	if payload.NodeName != "cp-1" || len(payload.Data) == 0 {
		t.Fatalf("server bundle = node=%q bytes=%d", payload.NodeName, len(payload.Data))
	}
}

func TestInstallApplyReadsProtectedTokenFile(t *testing.T) {
	bundlePath, bundleDigest := writeConfigBundle(t)
	server, err := handoff.NewHandoffServer("file-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	tokenPath := filepath.Join(t.TempDir(), "installer.token")
	if err := os.WriteFile(tokenPath, []byte("file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	err = run(context.Background(), []string{
		"install", "apply",
		"--endpoint", ts.URL,
		"--token-file", tokenPath,
		"--config-bundle", bundlePath,
		"--config-bundle-digest", bundleDigest,
		"--node", "cp-1",
		"--no-wait",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run() error = %v, stderr=%s", err, stderr.String())
	}
}

func TestInstallApplyWaitsForRebootReady(t *testing.T) {
	bundlePath, bundleDigest := writeConfigBundle(t)
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
		if r.Header.Get("Authorization") != "Bearer wait-token" || r.URL.Query().Get("node") != "cp-1" || r.URL.Query().Get("digest") != bundleDigest {
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
		"install", "apply",
		"--endpoint", ts.URL,
		"--token", "wait-token",
		"--config-bundle", bundlePath,
		"--config-bundle-digest", bundleDigest,
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
	bundlePath, bundleDigest := writeConfigBundle(t)
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
		"install", "apply", "--endpoint", ts.URL, "--token", "failure-token",
		"--config-bundle", bundlePath, "--config-bundle-digest", bundleDigest,
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
	bundlePath, _ := writeConfigBundle(t)
	var requests atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	defer ts.Close()

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{
		"install", "apply", "--endpoint", ts.URL, "--token", "local-token",
		"--config-bundle", bundlePath, "--config-bundle-digest", "sha256:" + strings.Repeat("0", 64),
		"--node", "cp-1", "--no-wait",
	}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("run() error = %v", err)
	}
	if requests.Load() != 0 {
		t.Fatalf("network requests = %d, want 0", requests.Load())
	}
}

func TestInstallApplyRedactsTokenFromHTTPFailure(t *testing.T) {
	bundlePath, bundleDigest := writeConfigBundle(t)
	const token = "secret-install-token"
	var requests atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(installTestStatus(handoff.HandoffWaiting, installstatus.StateWaitingForConfig, ""))
			return
		}
		requests.Add(1)
		http.Error(w, "rejected "+token, http.StatusUnauthorized)
	}))
	defer ts.Close()

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{
		"install", "apply", "--endpoint", ts.URL, "--token", token,
		"--config-bundle", bundlePath, "--config-bundle-digest", bundleDigest,
		"--node", "cp-1", "--no-wait",
	}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "<redacted>") || strings.Contains(err.Error(), token) {
		t.Fatalf("run() error = %v", err)
	}
	if requests.Load() != 1 {
		t.Fatalf("POST requests = %d, want 1", requests.Load())
	}
}

func TestInstallApplyRejectsInvalidEndpointAndOversizedBundle(t *testing.T) {
	for _, endpoint := range []string{"installer.test:8080", "ftp://installer.test", "http://user@installer.test", "http://installer.test/path", "http://installer.test?token=secret"} {
		var stdout, stderr bytes.Buffer
		err := run(context.Background(), []string{"install", "status", "--endpoint", endpoint}, &stdout, &stderr)
		if err == nil {
			t.Fatalf("endpoint %q succeeded", endpoint)
		}
	}

	path := filepath.Join(t.TempDir(), "large.katlcfg")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(maxInstallBundleSize + 1); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err = run(context.Background(), []string{
		"install", "apply", "--endpoint", "http://installer.test:8080", "--token", "token",
		"--config-bundle", path, "--config-bundle-digest", "sha256:" + strings.Repeat("0", 64), "--node", "cp-1",
	}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized bundle error = %v", err)
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
	bundlePath, bundleDigest := writeConfigBundle(t)
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
		"install", "apply", "--endpoint", ts.URL, "--token", "token",
		"--config-bundle", bundlePath, "--config-bundle-digest", bundleDigest,
		"--node", "cp-1", "--timeout", "3s",
	}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "became unavailable") || !strings.Contains(err.Error(), "Partition") {
		t.Fatalf("run() error = %v", err)
	}
}

func TestInstallTokenFlagsAreExclusive(t *testing.T) {
	bundlePath, bundleDigest := writeConfigBundle(t)
	for _, args := range [][]string{
		{"--token", "a", "--token-file", "token-file"},
		{},
	} {
		base := []string{"install", "apply", "--endpoint", "http://installer.test:8080", "--config-bundle", bundlePath, "--config-bundle-digest", bundleDigest, "--node", "cp-1", "--no-wait"}
		base = append(base, args...)
		var stdout, stderr bytes.Buffer
		err := run(context.Background(), base, &stdout, &stderr)
		if err == nil || !strings.Contains(err.Error(), "exactly one") {
			t.Fatalf("args=%v error=%v", args, err)
		}
	}
}

func TestInstallTokenFileMustBePrivate(t *testing.T) {
	bundlePath, bundleDigest := writeConfigBundle(t)
	tokenPath := filepath.Join(t.TempDir(), "installer.token")
	if err := os.WriteFile(tokenPath, []byte("token\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{
		"install", "apply", "--endpoint", "http://installer.test:8080", "--token-file", tokenPath,
		"--config-bundle", bundlePath, "--config-bundle-digest", bundleDigest, "--node", "cp-1", "--no-wait",
	}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "permissions") {
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
