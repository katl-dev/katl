package handoff

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/installer/configbundle"
	"github.com/katl-dev/katl/internal/installer/discovery"
	"github.com/katl-dev/katl/internal/installer/manifest"
	installstatus "github.com/katl-dev/katl/internal/installer/status"
)

func TestHandoffServerHealthStatusAndAnnouncement(t *testing.T) {
	server := newTestHandoffServer(t)
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz status = %d, want 200", resp.StatusCode)
	}

	resp, err = http.Get(ts.URL + "/v1/status")
	if err != nil {
		t.Fatalf("GET /v1/status error = %v", err)
	}
	defer resp.Body.Close()
	var status HandoffStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if status.State != HandoffWaiting || status.ManifestAccepted {
		t.Fatalf("status = %#v", status)
	}
	if status.InstallStatus.State != "waiting-for-config" || status.InstallStatus.InputMode != "local-handoff" || status.InstallStatus.InputSource != "local-handoff" {
		t.Fatalf("install status = %#v", status.InstallStatus)
	}

	announcement := server.Announcement("http://192.0.2.10:8080/")
	if announcement != "katlos-install waiting for config at http://192.0.2.10:8080/v1/config-bundle" {
		t.Fatalf("announcement = %q", announcement)
	}
}

func TestHandoffStatusReportsStableSelectableDisks(t *testing.T) {
	server := newTestHandoffServer(t)
	server.SetHardwareFacts(discovery.HardwareFacts{
		BlockDevices: []discovery.BlockDevice{
			{Path: "/dev/vda", Type: discovery.DeviceDisk, ByID: []string{"/dev/disk/by-id/virtio-root"}, Model: "Virtual Disk", SizeBytes: 64 << 30},
			{Path: "/dev/vdb", Type: discovery.DeviceDisk, Serial: "data", SizeBytes: 128 << 30, Partitions: []discovery.BlockDevice{{Path: "/dev/vdb1", Mountpoints: []string{"/mnt"}}}},
		},
	})
	status := server.Status()
	if len(status.Disks) != 2 || !status.Disks[0].Selectable || status.Disks[0].ByID[0] != "/dev/disk/by-id/virtio-root" {
		t.Fatalf("disks = %#v", status.Disks)
	}
	if status.Disks[1].Selectable || !status.Disks[1].Mounted {
		t.Fatalf("mounted disk = %#v", status.Disks[1])
	}
}

func TestHandoffStatusUsesDurableInstallerState(t *testing.T) {
	server := newTestHandoffServer(t)
	durable := server.Status().InstallStatus
	durable.State = "reboot-requested"
	durable.CurrentStep = "Reboot"
	durable.InstalledGeneration = "0"
	server.SetStatusReader(func() (installstatus.Record, error) {
		return durable, nil
	})

	status := server.Status()
	if status.InstallStatus.State != "reboot-requested" || status.InstallStatus.CurrentStep != "Reboot" || status.InstallStatus.InstalledGeneration != "0" {
		t.Fatalf("durable install status = %#v", status.InstallStatus)
	}

	server.SetStatusReader(func() (installstatus.Record, error) {
		return installstatus.Record{}, os.ErrNotExist
	})
	if fallback := server.Status().InstallStatus; fallback.State != "waiting-for-config" {
		t.Fatalf("fallback status = %#v", fallback)
	}
}

func TestHandoffStatusDoesNotRegressNewAttemptToOlderDurableState(t *testing.T) {
	server := newTestHandoffServer(t)
	durable := installstatus.New(installstatus.StateFailedBeforeMutation, time.Now().Add(-time.Hour))
	durable.LastError = "old failure"
	server.SetStatusReader(func() (installstatus.Record, error) {
		return durable, nil
	})
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	resp := postManifest(t, ts.URL, validManifestJSON())
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST status = %d, want 200", resp.StatusCode)
	}
	status := server.Status()
	if status.InstallStatus.State != installstatus.StateRunning || status.InstallStatus.LastError != "" {
		t.Fatalf("accepted status regressed to old durable state = %#v", status.InstallStatus)
	}
}

func TestHandoffServerAcceptsOneManifest(t *testing.T) {
	server := newTestHandoffServer(t)
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	manifest := validManifestJSON()
	resp := postManifest(t, ts.URL, manifest)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST valid manifest status = %d, want 200", resp.StatusCode)
	}
	if got := string(server.Manifest()); got != string(manifest) {
		t.Fatalf("stored manifest = %s", got)
	}
	status := server.Status()
	if status.InstallStatus.RequestDigest == "" || status.InstallStatus.InputSource != "local-handoff" || status.InstallStatus.KatlosImage.SHA256 == "" {
		t.Fatalf("accepted status missing digest/image: %#v", status.InstallStatus)
	}
	if status.InstallStatus.KatlosImage.URL != "https://example.invalid/katlos-install.squashfs" {
		t.Fatalf("status image URL = %q", status.InstallStatus.KatlosImage.URL)
	}

	resp = postManifest(t, ts.URL, manifest)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("second POST status = %d, want 409", resp.StatusCode)
	}
}

func TestHandoffServerAllowsRetryOnlyBeforeMutation(t *testing.T) {
	server := newTestHandoffServer(t)
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	resp := postManifest(t, ts.URL, validManifestJSON())
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first POST status = %d, want 200", resp.StatusCode)
	}

	failure := installstatus.New(installstatus.StateFailedBeforeMutation, time.Now().Add(time.Second))
	failure.CurrentStep = "PlanInstall"
	failure.LastError = "target disk selector matched no disks"
	if !server.PrepareRetry(failure) {
		t.Fatal("PrepareRetry() = false for failed-before-mutation")
	}
	status := server.Status()
	if status.State != HandoffWaiting || status.ManifestAccepted || status.BundleAccepted || status.SelectedNode != "" {
		t.Fatalf("retry status = %#v", status)
	}
	if status.InstallStatus.State != installstatus.StateFailedBeforeMutation || status.InstallStatus.LastError != failure.LastError {
		t.Fatalf("retry install status = %#v", status.InstallStatus)
	}

	resp = postManifest(t, ts.URL, validManifestJSON())
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("retry POST status = %d, want 200", resp.StatusCode)
	}

	refused := installstatus.New(installstatus.StateInstallRefused, time.Now().Add(2*time.Second))
	refused.RefusalReason = "existing signatures require explicit approval"
	if !server.PrepareRetry(refused) {
		t.Fatal("PrepareRetry() = false for install refusal before mutation")
	}
	resp = postManifest(t, ts.URL, validManifestJSON())
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post-refusal retry status = %d, want 200", resp.StatusCode)
	}

	unsafe := installstatus.New(installstatus.StateFailedAfterMutation, time.Now().Add(2*time.Second))
	unsafe.DestructiveMutation = true
	if server.PrepareRetry(unsafe) {
		t.Fatal("PrepareRetry() = true after destructive mutation")
	}
	if status := server.Status(); status.State != HandoffAccepted || !status.ManifestAccepted {
		t.Fatalf("unsafe retry changed accepted handoff = %#v", status)
	}
}

func TestHandoffServerAcceptsConfigBundleWithSelectedNode(t *testing.T) {
	server := newTestHandoffServer(t)
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	bundle, result := validConfigBundle(t)

	resp := postBundle(t, ts.URL, "cp-1", result.Digest, bundle)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST valid bundle status = %d, want 200", resp.StatusCode)
	}
	payload := server.Bundle()
	if !bytes.Equal(payload.Data, bundle) || payload.NodeName != "cp-1" {
		t.Fatalf("stored bundle = %d bytes node=%q", len(payload.Data), payload.NodeName)
	}
	status := server.Status()
	if !status.BundleAccepted || status.ManifestAccepted || status.SelectedNode != "cp-1" {
		t.Fatalf("handoff status = %#v", status)
	}
	if status.InstallStatus.BundleDigest != result.Digest || status.InstallStatus.SourceDigest == "" || status.InstallStatus.NodeMaterialDigest == "" {
		t.Fatalf("install status missing bundle digests: %#v", status.InstallStatus)
	}

	resp = postManifest(t, ts.URL, validManifestJSON())
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("manifest after bundle status = %d, want 409", resp.StatusCode)
	}
}

func TestHandoffServerRejectsConfigBundleWithoutSelectedNode(t *testing.T) {
	server := newTestHandoffServer(t)
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	bundle, result := validConfigBundle(t)

	resp := postBundle(t, ts.URL, "", result.Digest, bundle)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST bundle without node status = %d, want 400", resp.StatusCode)
	}
	if server.Status().State != HandoffWaiting {
		t.Fatalf("state = %s, want waiting after invalid bundle", server.Status().State)
	}
}

func TestHandoffServerValidatesBeforeAccepting(t *testing.T) {
	server := newTestHandoffServer(t)
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	resp := postManifest(t, ts.URL, []byte(`{"apiVersion":"wrong","kind":"InstallManifest"}`))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid manifest status = %d, want 400", resp.StatusCode)
	}
	if server.Status().State != HandoffWaiting {
		t.Fatalf("state = %s, want waiting after invalid manifest", server.Status().State)
	}
}

func TestValidateManifestEnvelope(t *testing.T) {
	if err := ValidateInstallManifestEnvelope(validManifestJSON()); err != nil {
		t.Fatalf("ValidateInstallManifestEnvelope(valid) error = %v", err)
	}

	namedManifest := bytes.Replace(validManifestJSON(), []byte(`"node": {`), []byte(`"metadata": {"name": "lab-node-01"}, "node": {`), 1)
	err := ValidateInstallManifestEnvelope(namedManifest)
	if err == nil {
		t.Fatal("ValidateInstallManifestEnvelope() error = nil, want metadata rejection")
	}
	if !strings.Contains(err.Error(), "unknown field") || !strings.Contains(err.Error(), "metadata") {
		t.Fatalf("error = %q, want metadata rejection", err)
	}

	unsafeManifest := []byte(`{
		"apiVersion": "install.katl.dev/v1alpha1",
		"kind": "InstallManifest",
		"node": {
			"identity": {
				"hostname": "lab-node-01",
				"ssh": {"authorizedKeys": ["ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDAxMjM0NTY3ODlhYmNkZWYwMTIzNDU2Nzg5YWJjZGVm katl@example"]}
			},
			"systemRole": "control-plane"
		},
		"install": {
    "wipeTarget": true,
			"targetDisk": {"byID": "/dev/disk/by-id/ata-root"}
		},
		"katlosImage": {
			"url": "https://example.invalid/katlos-install.squashfs",
			"sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"sizeBytes": 1073741824,
			"version": "2026.06.04",
			"architecture": "x86_64",
			"runtimeInterface": "katl-runtime-1",
			"role": "install"
		},
		"etc": {
			"files": {
				"/etc/kubernetes/admin.conf": "unsafe\n"
			}
		}
	}`)
	err = ValidateInstallManifestEnvelope(unsafeManifest)
	if err == nil {
		t.Fatal("ValidateInstallManifestEnvelope() error = nil, want top-level etc rejection")
	}
	if !strings.Contains(err.Error(), "unknown field") || !strings.Contains(err.Error(), "etc") {
		t.Fatalf("error = %q, want top-level etc rejection", err)
	}
}

func newTestHandoffServer(t *testing.T) *HandoffServer {
	t.Helper()
	return NewHandoffServerWithDefaultImage(nil, manifest.KatlosImage{
		LocalRef:         "images/katlos-install-test-x86_64.squashfs",
		SHA256:           strings.Repeat("a", 64),
		SizeBytes:        1,
		Version:          "test",
		Architecture:     "x86_64",
		RuntimeInterface: "katl-runtime-1",
		Role:             "install",
	})
}

func postManifest(t *testing.T, baseURL string, manifest []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/install", bytes.NewReader(manifest))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/install error = %v", err)
	}
	return resp
}

func postBundle(t *testing.T, baseURL, node, digest string, bundle []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/config-bundle?node="+node+"&digest="+digest, bytes.NewReader(bundle))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/config-bundle error = %v", err)
	}
	return resp
}

func validConfigBundle(t *testing.T) ([]byte, configbundle.Result) {
	t.Helper()
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "cluster.yaml")
	if err := os.WriteFile(sourcePath, []byte(validBundleSourceConfig()), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	archive, result, err := configbundle.BuildArchive(configbundle.BuildRequest{SourcePath: sourcePath})
	if err != nil {
		t.Fatalf("BuildArchive() error = %v", err)
	}
	return archive, result
}

func validManifestJSON() []byte {
	return []byte(`{
		"apiVersion": "install.katl.dev/v1alpha1",
		"kind": "InstallManifest",
		"node": {
			"identity": {
				"hostname": "lab-node-01",
				"ssh": {
					"authorizedKeys": [
						"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDAxMjM0NTY3ODlhYmNkZWYwMTIzNDU2Nzg5YWJjZGVm katl@example"
					]
				}
			},
			"systemRole": "control-plane"
		},
		"install": {
    "wipeTarget": true,
			"targetDisk": {"byID": "/dev/disk/by-id/ata-root"}
		},
		"katlosImage": {
			"url": "https://example.invalid/katlos-install.squashfs",
			"sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"sizeBytes": 1073741824,
			"version": "2026.06.04",
			"architecture": "x86_64",
			"runtimeInterface": "katl-runtime-1",
			"role": "install"
		}
	}`)
}

func validBundleSourceConfig() string {
	return `apiVersion: config.katl.dev/v1alpha1
kind: ClusterConfig
metadata:
  name: lab
spec:
  controlPlaneEndpoint:
    host: api.katl.test
    port: 6443
  kubernetes:
    version: v1.36.1
  defaults:
    install:
    identity:
      ssh:
        authorizedKeys:
          - ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDAxMjM0NTY3ODlhYmNkZWYwMTIzNDU2Nzg5YWJjZGVm katl@example
  nodes:
    - name: cp-1
      controlPlane: true
      install:
        targetDisk:
          byID: /dev/disk/by-id/ata-root
`
}
