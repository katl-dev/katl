package vmtest

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/systemextensionbundle"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const extensionRegistryCAPath = "/var/lib/katl/test-artifacts/extension-registry-ca.pem"

// serveGuestSystemExtension publishes a sysext containing files to a TLS
// registry on the guest's default gateway and returns a pinned bundle
// reference. The node resolves extension selections itself, so its management
// agent is restarted to trust the registry CA for the rest of this boot.
func serveGuestSystemExtension(t *testing.T, ctx context.Context, guest *GuestControl, name string, files map[string]string) string {
	t.Helper()
	level := guestOSReleaseValue(t, ctx, guest, "SYSEXT_LEVEL")
	tree := t.TempDir()
	files["usr/lib/extension-release.d/extension-release."+name] = "ID=_any\nSYSEXT_LEVEL=" + level + "\nARCHITECTURE=x86-64\n"
	for path, content := range files {
		full := filepath.Join(tree, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	image := filepath.Join(t.TempDir(), name+".raw")
	if output, err := exec.CommandContext(ctx, "mksquashfs", tree, image, "-noappend", "-all-root", "-no-xattrs", "-quiet").CombinedOutput(); err != nil {
		t.Fatalf("build %s sysext: %v\n%s", name, err, output)
	}
	layout := t.TempDir()
	packed, _, err := systemextensionbundle.Export(ctx, layout, systemextensionbundle.BuildRequest{
		Name: name, ArtifactVersion: "v1", PayloadVersion: "v1", Architecture: "x86_64",
		SupportedRuntimeInterfaces: []string{level}, CreatedAt: time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC),
		Payloads: []systemextensionbundle.Input{{Path: image, Role: systemextensionbundle.SysextRole, FileName: name + ".raw"}},
	}, nil)
	if err != nil {
		t.Fatalf("export %s bundle: %v", name, err)
	}

	gateway := guestDefaultGateway(t, ctx, guest)
	cert, caPEM := extensionRegistryCertificate(t, gateway)
	listener, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	repository := "katl-vmtest/" + name
	manifests, blobs := "/v2/"+repository+"/manifests/", "/v2/"+repository+"/blobs/"
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/":
			w.WriteHeader(http.StatusOK)
		case strings.HasPrefix(r.URL.Path, manifests):
			if ref := strings.TrimPrefix(r.URL.Path, manifests); ref != "v1" && ref != packed.ManifestDigest {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", ocispec.MediaTypeImageManifest)
			w.Header().Set("Docker-Content-Digest", packed.ManifestDigest)
			http.ServeContent(w, r, "manifest.json", time.Time{}, bytes.NewReader(packed.Manifest))
		case strings.HasPrefix(r.URL.Path, blobs):
			digest := strings.TrimPrefix(r.URL.Path, blobs)
			path := filepath.Join(layout, "blobs", "sha256", strings.TrimPrefix(digest, "sha256:"))
			if !strings.HasPrefix(digest, "sha256:") || strings.Contains(digest, "/") {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Docker-Content-Digest", digest)
			http.ServeFile(w, r, path)
		default:
			http.NotFound(w, r)
		}
	}))
	server.Listener = listener
	server.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	t.Cleanup(server.Close)

	writeGuestFile(t, ctx, guest, extensionRegistryCAPath, caPEM, 0o644)
	guestCommand(t, ctx, guest, "trust-extension-registry", "systemctl", "set-environment", "SSL_CERT_FILE="+extensionRegistryCAPath)
	guestCommand(t, ctx, guest, "restart-agent-for-extension-registry", "systemctl", "restart", "katlc-agent.service")
	guestCommand(t, ctx, guest, "agent-after-extension-registry", "systemctl", "is-active", "--quiet", "katlc-agent.service")
	port := listener.Addr().(*net.TCPAddr).Port
	return net.JoinHostPort(gateway, strconv.Itoa(port)) + "/" + repository + ":v1@" + packed.ManifestDigest
}

func guestOSReleaseValue(t *testing.T, ctx context.Context, guest *GuestControl, key string) string {
	t.Helper()
	for _, line := range strings.Split(readGuestFile(t, ctx, guest, "/usr/lib/os-release"), "\n") {
		if value, ok := strings.CutPrefix(line, key+"="); ok {
			return strings.Trim(value, `"`)
		}
	}
	t.Fatalf("guest os-release has no %s", key)
	return ""
}

func guestDefaultGateway(t *testing.T, ctx context.Context, guest *GuestControl) string {
	t.Helper()
	fields := strings.Fields(guestCommandOutput(t, ctx, guest, "default-route", "ip", "-4", "route", "show", "default"))
	for i := range len(fields) - 1 {
		if fields[i] == "via" && net.ParseIP(fields[i+1]) != nil {
			return fields[i+1]
		}
	}
	t.Fatalf("guest has no IPv4 default gateway: %q", fields)
	return ""
}

func extensionRegistryCertificate(t *testing.T, gateway string) (tls.Certificate, []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "katl vmtest extension registry"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true, IsCA: true,
		IPAddresses: []net.IP{net.ParseIP(gateway)},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return cert, certPEM
}

// runExtensionBootHealthSmoke stages a trial generation whose extension
// breaks configured-unit activation at boot, then requires that boot health
// rejects it and Katl returns to the fallback generation with management
// access.
func runExtensionBootHealthSmoke(t *testing.T, ctx context.Context, node *RunningInstalledRuntimeNode, guest *GuestControl, client *AgentClient, result Result, katlctl, endpoint, fallbackGeneration, scenario, version string, files map[string]string, units string, wantJournal ...string) (*GuestControl, *AgentClient) {
	t.Helper()
	ref := serveGuestSystemExtension(t, ctx, guest, scenario, files)
	// The restarted agent is active before it accepts management connections.
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(2 * time.Second) {
		if _, err := runKatlctlOutcome(t, ctx, result, katlctl, scenario+"-agent-ready", "node", "status", "cp-1"); err == nil {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("%s: management agent did not return after trusting the registry: %v", scenario, err)
		}
	}
	change := `apiVersion: katl.dev/v1alpha1
kind: NodeConfigurationChange
metadata:
  sourceID: operator
  desiredVersion: "` + version + `"
apply:
  mode: next-boot
spec:
  systemExtensionSelections:
    - bundle: ` + ref + `
` + units
	path := filepath.Join(t.TempDir(), scenario+".yaml")
	if err := os.WriteFile(path, []byte(change), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate := "2026.11.22-" + scenario
	submitKatlctlConfigApply(t, ctx, result, katlctl, endpoint, "config-apply-"+scenario, "next-boot", candidate, path, false)
	if selection := bootSelectionFromGuest(t, ctx, guest); selection.TrialGenerationID != candidate || selection.PreviousKnownGoodGenerationID != fallbackGeneration {
		t.Fatalf("%s selection = %#v", scenario, selection)
	}

	previousBootID := guestBootID(t, ctx, client)
	_, rebootErr := runKatlctlOutcome(t, ctx, result, katlctl, "host-reboot-"+scenario, "node", "reboot", "cp-1", "--timeout", "10m")
	if rebootErr == nil || !strings.Contains(rebootErr.Error(), "rejected generation "+candidate+" during boot health and returned on generation "+fallbackGeneration) {
		t.Fatalf("%s reboot error = %v, want actionable fallback report", scenario, rebootErr)
	}
	_ = client.Close()
	guest, client = reconnectGuestAfterGeneration(t, ctx, node, previousBootID, fallbackGeneration)

	if _, status := generationRecordsFromGuest(t, ctx, guest, candidate); status.BootState != generation.BootStateFailed || status.HealthState != generation.HealthStateUnhealthy {
		t.Fatalf("%s candidate status = %#v, want failed/unhealthy", scenario, status)
	}
	journal := guestCommandOutput(t, ctx, guest, scenario+"-journal", "journalctl", "-b", "-1", "--no-pager", "-o", "cat",
		"-u", "katl-system-extensions-activate.service", "-u", "katl-configured-units-check.service", "-u", "katl-boot-health.service")
	for _, want := range append(wantJournal, "Dependency failed for katl-boot-health.service") {
		if !strings.Contains(journal, want) {
			t.Fatalf("%s rejected boot journal lacks %q:\n%s", scenario, want, journal)
		}
	}
	// Recovery reboots the rejected trial at once; management must work again
	// on the fallback generation.
	if status := string(runKatlctl(t, ctx, result, katlctl, scenario+"-status", "node", "status", "cp-1")); !strings.Contains(status, fallbackGeneration) {
		t.Fatalf("%s katlctl status after fallback does not report %s:\n%s", scenario, fallbackGeneration, status)
	}
	return guest, client
}
