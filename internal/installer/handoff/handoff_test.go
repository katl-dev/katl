package handoff

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
	if !strings.Contains(announcement, "http://192.0.2.10:8080/v1/install") || !strings.Contains(announcement, "token=test-token") {
		t.Fatalf("announcement = %q", announcement)
	}
}

func TestHandoffServerRequiresTokenAndAcceptsOneManifest(t *testing.T) {
	server := newTestHandoffServer(t)
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	manifest := validManifestJSON()
	resp := postManifest(t, ts.URL, "", manifest)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("POST without token status = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()

	resp = postManifest(t, ts.URL, "test-token", manifest)
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

	resp = postManifest(t, ts.URL, "test-token", manifest)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("second POST status = %d, want 409", resp.StatusCode)
	}
}

func TestHandoffServerValidatesBeforeAccepting(t *testing.T) {
	server := newTestHandoffServer(t)
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	resp := postManifest(t, ts.URL, "test-token", []byte(`{"apiVersion":"wrong","kind":"InstallManifest"}`))
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
			"allowDestructiveInstall": true,
			"destructiveInstallAcknowledgement": "I understand this will erase KatlOS, Kubernetes, kubelet, etcd, CNI, operation, and generation state on the selected nodes and bootstrap a new cluster identity.",
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
	server, err := NewHandoffServer("test-token", nil)
	if err != nil {
		t.Fatalf("NewHandoffServer() error = %v", err)
	}
	return server
}

func postManifest(t *testing.T, baseURL, token string, manifest []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/install", bytes.NewReader(manifest))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/install error = %v", err)
	}
	return resp
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
			"allowDestructiveInstall": true,
			"destructiveInstallAcknowledgement": "I understand this will erase KatlOS, Kubernetes, kubelet, etcd, CNI, operation, and generation state on the selected nodes and bootstrap a new cluster identity.",
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
