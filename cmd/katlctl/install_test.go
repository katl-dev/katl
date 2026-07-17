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
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/installer/configbundle"
	"github.com/katl-dev/katl/internal/installer/handoff"
	"github.com/katl-dev/katl/internal/installer/manifest"
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

func TestInstallDiscoverWritesClusterConfig(t *testing.T) {
	oldAddrs := installerInterfaceAddrs
	installerInterfaceAddrs = func() ([]net.Addr, error) {
		return []net.Addr{&net.IPNet{IP: net.ParseIP("192.0.2.5"), Mask: net.CIDRMask(24, 32)}}, nil
	}
	t.Cleanup(func() { installerInterfaceAddrs = oldAddrs })
	oldProbe := installerDiscoveryProbe
	installerDiscoveryProbe = func(_ context.Context, endpoint string, _ time.Duration) (handoff.HandoffStatus, error) {
		status := handoff.HandoffStatus{State: handoff.HandoffWaiting}
		switch endpoint {
		case "http://192.0.2.11:8080":
			status.Disks = []handoff.HandoffDisk{{
				Path: "/dev/vda", ByID: []string{"/dev/disk/by-id/virtio-cp-root", "/dev/disk/by-id/ata-cp-root"}, Selectable: true,
			}}
		case "http://192.0.2.21:8080":
			status.Disks = []handoff.HandoffDisk{{Path: "/dev/sda", WWN: "worker-root-wwn", Selectable: true}}
		default:
			return handoff.HandoffStatus{}, fmt.Errorf("not an installer")
		}
		return status, nil
	}
	t.Cleanup(func() { installerDiscoveryProbe = oldProbe })

	dir := t.TempDir()
	oldAgent := sshAgentPublicKeys
	sshAgentPublicKeys = func() ([]byte, error) { return []byte(uxTestSSHKey + "\n"), nil }
	t.Cleanup(func() { sshAgentPublicKeys = oldAgent })
	t.Setenv("KATLCTL_CONFIG", filepath.Join(dir, "katlctl.yaml"))
	outputPath := filepath.Join(dir, "cluster.yaml")
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{
		"install", "discover", outputPath,
		"--name", "homelab",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run() error = %v\nstderr=%s", err, stderr.String())
	}
	data, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	source, err := configbundle.DecodeSource(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("DecodeSource() error = %v\n%s", err, data)
	}
	if source.Metadata.Name != "homelab" || source.Spec.ControlPlaneEndpoint != "" || source.Spec.Kubernetes.Version != configbundle.DefaultKubernetesVersion || len(source.Spec.Nodes) != 2 {
		t.Fatalf("generated source = %#v", source)
	}
	if keys := source.Spec.Defaults.Identity.SSH.AuthorizedKeys; len(keys) != 1 || keys[0] != uxTestSSHKey {
		t.Fatalf("authorized keys = %#v", keys)
	}
	cp, worker := source.Spec.Nodes[0], source.Spec.Nodes[1]
	if cp.Name != "cp-1" || cp.SystemRole != inventory.RoleControlPlane || cp.Bootstrap.Address != "192.0.2.11" || cp.Install.TargetDisk == nil || cp.Install.TargetDisk.ByID != "/dev/disk/by-id/ata-cp-root" {
		t.Fatalf("control-plane node = %#v", cp)
	}
	if worker.Name != "worker-1" || worker.SystemRole != inventory.RoleWorker || worker.Bootstrap.Address != "192.0.2.21" || worker.Install.TargetDisk == nil || worker.Install.TargetDisk.WWN != "worker-root-wwn" {
		t.Fatalf("worker node = %#v", worker)
	}
	if !strings.Contains(stdout.String(), "created "+outputPath) {
		t.Fatalf("stdout = %q", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	if err := run(context.Background(), []string{"config", "validate", outputPath}, &stdout, &stderr); err != nil {
		t.Fatalf("validate generated config: %v\nstderr=%s", err, stderr.String())
	}
}

func TestDiscoveredInitNodesRejectsUnsafeInference(t *testing.T) {
	tests := []struct {
		name       string
		installers []discoveredInstaller
		want       string
	}{
		{
			name:       "no waiting installer",
			installers: []discoveredInstaller{{Endpoint: "http://192.0.2.11:8080", Status: handoff.HandoffStatus{State: handoff.HandoffAccepted}}},
			want:       "no waiting KatlOS installer",
		},
		{
			name: "no selectable disk",
			installers: []discoveredInstaller{{
				Endpoint: "http://192.0.2.11:8080",
				Status:   handoff.HandoffStatus{State: handoff.HandoffWaiting, Disks: []handoff.HandoffDisk{{Path: "/dev/vda"}}},
			}},
			want: "0 selectable disks",
		},
		{
			name: "ambiguous disks",
			installers: []discoveredInstaller{{
				Endpoint: "http://192.0.2.11:8080",
				Status: handoff.HandoffStatus{State: handoff.HandoffWaiting, Disks: []handoff.HandoffDisk{
					{Path: "/dev/vda", ByID: []string{"/dev/disk/by-id/virtio-a"}, Selectable: true},
					{Path: "/dev/vdb", ByID: []string{"/dev/disk/by-id/virtio-b"}, Selectable: true},
				}},
			}},
			want: "2 selectable disks (/dev/vda, /dev/vdb)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := discoveredInitNodes(tt.installers)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("discoveredInitNodes() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestConfigInitFromInstallerAddress(t *testing.T) {
	status := installTestStatus(handoff.HandoffWaiting, installstatus.StateWaitingForConfig, "")
	status.Disks = []handoff.HandoffDisk{{
		Path: "/dev/vda", ByID: []string{"/dev/disk/by-id/virtio-root"}, Selectable: true,
	}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/status" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(status)
	}))
	defer server.Close()

	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("KATLCTL_CONFIG", filepath.Join(dir, "katlctl.yaml"))
	oldAgent := sshAgentPublicKeys
	sshAgentPublicKeys = func() ([]byte, error) { return []byte(uxTestSSHKey + "\n"), nil }
	t.Cleanup(func() { sshAgentPublicKeys = oldAgent })

	address := strings.TrimPrefix(server.URL, "http://")
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{
		"config", "init",
		"--name", "direct-lab",
		"--installer", address,
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run() error = %v\nstderr=%s", err, stderr.String())
	}
	source, err := configbundle.DecodeSource(bytes.NewReader(stdout.Bytes()))
	if err != nil {
		t.Fatalf("DecodeSource() error = %v\n%s", err, stdout.String())
	}
	if len(source.Spec.Nodes) != 1 || source.Spec.Nodes[0].Name != "cp-1" || source.Spec.Nodes[0].Install.TargetDisk == nil || source.Spec.Nodes[0].Install.TargetDisk.ByID != "/dev/disk/by-id/virtio-root" {
		t.Fatalf("generated source = %#v", source)
	}
	if got := source.Spec.Nodes[0].Bootstrap.Address; got != "127.0.0.1" {
		t.Fatalf("bootstrap address = %q", got)
	}
}

func TestConfigInitInstallerInputValidation(t *testing.T) {
	endpoint, err := normalizeInstallerAddress("192.0.2.11")
	if err != nil || endpoint != "http://192.0.2.11:8080" {
		t.Fatalf("normalizeInstallerAddress() = %q, %v", endpoint, err)
	}

	var stdout, stderr bytes.Buffer
	err = run(context.Background(), []string{
		"config", "init",
		"--node", "cp-1=control-plane,192.0.2.11,/dev/disk/by-id/ata-root",
		"--installer", "192.0.2.11",
	}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "--node and --installer cannot be used together") {
		t.Fatalf("run() error = %v", err)
	}
}

func TestInstallStatusReportsWaitingInstaller(t *testing.T) {
	server := handoff.NewHandoffServerWithDefaultImage(nil, manifest.KatlosImage{
		LocalRef:         "images/katlos-install-test-x86_64.squashfs",
		SHA256:           strings.Repeat("a", 64),
		SizeBytes:        1,
		Version:          "test",
		Architecture:     "x86_64",
		RuntimeInterface: "katl-runtime-1",
		Role:             "install",
	})
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
	server := handoff.NewHandoffServerWithDefaultImage(nil, manifest.KatlosImage{
		LocalRef:         "images/katlos-install-test-x86_64.squashfs",
		SHA256:           strings.Repeat("a", 64),
		SizeBytes:        1,
		Version:          "test",
		Architecture:     "x86_64",
		RuntimeInterface: "katl-runtime-1",
		Role:             "install",
	})
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{
		"install", "apply", "--config", sourcePath,
		"--endpoint", ts.URL,
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

func TestInstallApplyAcceptsGeneratedConfigByAddress(t *testing.T) {
	dir := t.TempDir()
	outputPath := filepath.Join(dir, "cluster.yaml")
	keyPath := filepath.Join(dir, "id_ed25519.pub")
	if err := os.WriteFile(keyPath, []byte(uxTestSSHKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{
		"config", "init", outputPath,
		"--ssh-authorized-key", keyPath,
		"--node", "cp-1=control-plane,192.0.2.11,/dev/disk/by-id/ata-cp-root",
	}, &stdout, &stderr); err != nil {
		t.Fatalf("config init error = %v, stderr=%s", err, stderr.String())
	}

	server := handoff.NewHandoffServerWithDefaultImage(nil, manifest.KatlosImage{
		LocalRef:         "katlos-install.raw",
		SHA256:           strings.Repeat("a", 64),
		SizeBytes:        1,
		Version:          "test",
		Architecture:     "x86_64",
		RuntimeInterface: "katl-runtime-1",
		Role:             "install",
	})
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	stdout.Reset()
	stderr.Reset()
	if err := run(context.Background(), []string{
		"install", "apply", "--config", outputPath,
		"--endpoint", ts.URL,
		"--node", "192.0.2.11",
		"--no-wait",
	}, &stdout, &stderr); err != nil {
		t.Fatalf("install apply error = %v, stderr=%s", err, stderr.String())
	}
	if payload := server.Bundle(); payload.NodeName != "cp-1" || len(payload.Data) == 0 {
		t.Fatalf("server bundle = node=%q bytes=%d", payload.NodeName, len(payload.Data))
	}
}

func TestSelectInstallNode(t *testing.T) {
	manifest := configbundle.BundleManifest{
		Nodes: []configbundle.NodeRecord{{Name: "cp-1"}, {Name: "worker-1"}},
		Cluster: configbundle.ClusterRecord{BootstrapInventory: inventory.Inventory{Nodes: []inventory.Node{
			{Name: "cp-1", Address: "192.0.2.11"},
			{Name: "worker-1", Address: "192.0.2.21"},
		}}},
	}
	for _, test := range []struct {
		name     string
		selector string
		want     string
		wantErr  string
	}{
		{name: "name", selector: "cp-1", want: "cp-1"},
		{name: "address", selector: "192.0.2.21", want: "worker-1"},
		{name: "required for multiple", wantErr: "contains 2 nodes"},
		{name: "unknown", selector: "192.0.2.99", wantErr: "cp-1 (192.0.2.11)"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := selectInstallNode(manifest, test.selector)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("selectInstallNode() error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("selectInstallNode() = %q, %v, want %q", got, err, test.want)
			}
		})
	}

	single := manifest
	single.Nodes = single.Nodes[:1]
	if got, err := selectInstallNode(single, ""); err != nil || got != "cp-1" {
		t.Fatalf("single-node selection = %q, %v", got, err)
	}
	manifest.Cluster.BootstrapInventory.Nodes[1].Address = "192.0.2.11"
	if _, err := selectInstallNode(manifest, "192.0.2.11"); err == nil || !strings.Contains(err.Error(), "matches multiple nodes") {
		t.Fatalf("ambiguous address error = %v", err)
	}
}

func TestInstallEndpointHint(t *testing.T) {
	for _, test := range []struct {
		name     string
		endpoint string
		selector string
		nodeName string
		want     string
	}{
		{name: "explicit endpoint", endpoint: "http://installer.test:8080", selector: "192.0.2.11", nodeName: "cp-1", want: "http://installer.test:8080"},
		{name: "IPv4 address selector", selector: "192.0.2.11", nodeName: "cp-1", want: "http://192.0.2.11:8080"},
		{name: "IPv6 address selector", selector: "2001:db8::11", nodeName: "cp-1", want: "http://[2001:db8::11]:8080"},
		{name: "hostname selector", selector: "installer.home.arpa", nodeName: "cp-1", want: "http://installer.home.arpa:8080"},
		{name: "address with port selector", selector: "192.0.2.11:9080", nodeName: "cp-1", want: "http://192.0.2.11:9080"},
		{name: "node name discovers", selector: "cp-1", nodeName: "cp-1"},
		{name: "inferred node discovers", nodeName: "cp-1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := installEndpointHint(test.endpoint, test.selector, test.nodeName)
			if err != nil || got != test.want {
				t.Fatalf("installEndpointHint() = %q, %v, want %q", got, err, test.want)
			}
		})
	}

	if _, err := installEndpointHint("installer.test:8080", "192.0.2.11", "cp-1"); err == nil || !strings.Contains(err.Error(), "scheme must be http or https") {
		t.Fatalf("bare explicit endpoint error = %v", err)
	}
}

func TestInstallApplyHelpKeepsBundleInternal(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{"install", "apply", "--help"}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run() error = %v, stderr=%s", err, stderr.String())
	}
	help := stdout.String()
	if !strings.Contains(help, "katlctl install apply --config cluster.yaml") || !strings.Contains(help, "--config string") {
		t.Fatalf("help does not advertise unified config input:\n%s", help)
	}
	for _, hiddenDetail := range []string{"--config-bundle", "--source", "--token", "--token-file"} {
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
		"install", "apply", "--config", sourcePath,
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
		"install", "apply", "--config", sourcePath, "--endpoint", ts.URL,
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
		"install", "apply", "--config", sourcePath, "--endpoint", ts.URL,
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
		"install", "apply", "--config", sourcePath, "--endpoint", ts.URL,
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
		"install", "apply", "--config", sourcePath, "--endpoint", ts.URL,
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
