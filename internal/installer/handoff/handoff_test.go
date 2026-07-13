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

	"github.com/katl-dev/katl/internal/installer/configbundle"
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
	return NewHandoffServer(nil)
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
  controlPlaneEndpoint: api.katl.test:6443
  kubernetes:
    version: v1.36.1
  katlosImage:
    url: https://example.invalid/katlos-install.squashfs
    sha256: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    sizeBytes: 1073741824
    version: 2026.06.04
    architecture: x86_64
    runtimeInterface: katl-runtime-1
    role: install
  defaults:
    install:
      wipeTarget: true
    identity:
      ssh:
        authorizedKeys:
          - ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDAxMjM0NTY3ODlhYmNkZWYwMTIzNDU2Nzg5YWJjZGVm katl@example
    bootstrap:
      access:
        method: agent
        credentialRef: vsock:1234:10240
  systemRoleDefaults:
    control-plane:
      kubernetes:
        kubeadm:
          configRef: control-plane
  kubeadmConfigs:
    control-plane:
      config: |
        apiVersion: kubeadm.k8s.io/v1beta4
        kind: InitConfiguration
        nodeRegistration:
          criSocket: unix:///run/containerd/containerd.sock
        ---
        apiVersion: kubeadm.k8s.io/v1beta4
        kind: ClusterConfiguration
        kubernetesVersion: v1.36.1
  nodes:
    - name: cp-1
      systemRole: control-plane
      overrides:
        install:
          targetDisk:
            byID: /dev/disk/by-id/ata-root
`
}
