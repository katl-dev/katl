package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/installer"
	"github.com/katl-dev/katl/internal/installer/discovery"
	"github.com/katl-dev/katl/internal/installer/disk"
	"github.com/katl-dev/katl/internal/installer/katlosimage"
	installstatus "github.com/katl-dev/katl/internal/installer/status"
)

func TestVersion(t *testing.T) {
	oldVersion, oldCommit, oldDate := version, commit, date
	version, commit, date = "dev", "abc123", "2026-06-01T00:00:00Z"
	t.Cleanup(func() {
		version, commit, date = oldVersion, oldCommit, oldDate
	})

	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"--version"}, &stdout, &stderr); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if got, want := stdout.String(), "katlos-install version=dev commit=abc123 date=2026-06-01T00:00:00Z\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestApplyInput(t *testing.T) {
	root := t.TempDir()
	preseed := filepath.Join(root, "seed")
	runDir := filepath.Join(root, "run")
	etcDir := filepath.Join(root, "etc")
	if err := os.MkdirAll(preseed, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(preseed, "install-input.json"), []byte(`{"waitForConfig":true}`), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{
		"--apply-input",
		"--preseed-dir", preseed,
		"--seed-wait", "0s",
		"--run-dir", runDir,
		"--etc-dir", etcDir,
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(runDir, "install-input.json")); err != nil {
		t.Fatalf("input file missing: %v", err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestBootInput(t *testing.T) {
	root := t.TempDir()
	runDir := filepath.Join(root, "run")
	etcDir := filepath.Join(root, "etc")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	inputPath := filepath.Join(runDir, "install-input.json")
	inputJSON := `{"manifestPath":"/run/katl/install-manifest.json","installMode":"auto"}`
	if err := os.WriteFile(inputPath, []byte(inputJSON), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	input, err := bootInput(runDir, etcDir)
	if err != nil {
		t.Fatalf("bootInput() error = %v", err)
	}
	if input.Action != installer.InstallActionRun || !input.CanMutateDisks() {
		t.Fatalf("action = %s canMutate = %t, want run", input.Action, input.CanMutateDisks())
	}
	if got := bootInputMode(input); got != installstatus.InputModeOfflineMedia {
		t.Fatalf("boot input mode = %q, want offline media", got)
	}
}

func TestBootInputDiscoversYAMLManifest(t *testing.T) {
	root := t.TempDir()
	runDir := filepath.Join(root, "run")
	etcDir := filepath.Join(root, "etc")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	manifestPath := filepath.Join(runDir, "install-manifest.yaml")
	if err := os.WriteFile(manifestPath, []byte(`node:
  identity:
    hostname: yaml-node
katlosImage:
  url: https://manifest.example/artifacts/katlos-install.squashfs
`), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	input, err := bootInput(runDir, etcDir)
	if err != nil {
		t.Fatalf("bootInput() error = %v", err)
	}
	if input.ManifestPath != manifestPath {
		t.Fatalf("manifest path = %q, want %q", input.ManifestPath, manifestPath)
	}
	if input.NodeName != "yaml-node" {
		t.Fatalf("node name = %q", input.NodeName)
	}
	if got := bootInputMode(input); got != installstatus.InputModeOfflineMedia {
		t.Fatalf("boot input mode = %q, want offline media", got)
	}
}

func TestBootInputDiscoversConfigBundle(t *testing.T) {
	root := t.TempDir()
	runDir := filepath.Join(root, "run")
	etcDir := filepath.Join(root, "etc")
	bundlePath := filepath.Join(runDir, "config.katlcfg")
	writeTestFile(t, bundlePath, "bundle")
	writeTestFile(t, filepath.Join(runDir, "install-input.json"), `{"nodeName":"cp-1","installMode":"auto"}`)

	input, err := bootInput(runDir, etcDir)
	if err != nil {
		t.Fatalf("bootInput() error = %v", err)
	}
	if input.BundlePath != bundlePath {
		t.Fatalf("bundle path = %q, want %q", input.BundlePath, bundlePath)
	}
	if input.NodeName != "cp-1" || input.Action != installer.InstallActionRun || !input.CanMutateDisks() {
		t.Fatalf("input = %#v, want runnable bundle input", input)
	}
	if got := bootInputMode(input); got != installstatus.InputModeOfflineMedia {
		t.Fatalf("boot input mode = %q, want offline media", got)
	}
}

func TestBootInputMode(t *testing.T) {
	tests := []struct {
		name  string
		input installer.BootInput
		want  string
	}{
		{
			name: "run path",
			input: installer.BootInput{SelectedSources: map[string]installer.InputSource{
				"manifestPath": installer.InputSourceRunKatl,
			}},
			want: installstatus.InputModeOfflineMedia,
		},
		{
			name: "etc path",
			input: installer.BootInput{SelectedSources: map[string]installer.InputSource{
				"manifestPath": installer.InputSourceEtcKatl,
			}},
			want: installstatus.InputModeOfflineMedia,
		},
		{
			name: "embedded path",
			input: installer.BootInput{SelectedSources: map[string]installer.InputSource{
				"manifestPath": installer.InputSourceEmbeddedMedia,
			}},
			want: installstatus.InputModeOfflineMedia,
		},
		{
			name: "kernel path",
			input: installer.BootInput{SelectedSources: map[string]installer.InputSource{
				"manifestPath": installer.InputSourceKernelCmdline,
			}},
			want: installstatus.InputModePXEPreseed,
		},
		{
			name:  "manifest URL",
			input: installer.BootInput{ManifestURL: "https://example.invalid/install.json"},
			want:  installstatus.InputModePXEPreseed,
		},
		{
			name: "bundle run path",
			input: installer.BootInput{SelectedSources: map[string]installer.InputSource{
				"bundlePath": installer.InputSourceRunKatl,
			}},
			want: installstatus.InputModeOfflineMedia,
		},
		{
			name:  "bundle URL",
			input: installer.BootInput{BundleURL: "https://example.invalid/config.katlcfg"},
			want:  installstatus.InputModePXEPreseed,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := bootInputMode(tt.input); got != tt.want {
				t.Fatalf("bootInputMode() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFetchBundleURL(t *testing.T) {
	bundle := []byte("bundle archive")
	digest := sha256.Sum256(bundle)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cluster.katlcfg" {
			t.Fatalf("request path = %s", r.URL.Path)
		}
		_, _ = w.Write(bundle)
	}))
	t.Cleanup(server.Close)

	runDir := t.TempDir()
	path, err := fetchBundleURL(context.Background(), server.URL+"/cluster.katlcfg", hex.EncodeToString(digest[:]), runDir)
	if err != nil {
		t.Fatalf("fetchBundleURL() error = %v", err)
	}
	if path != filepath.Join(runDir, "config.katlcfg") {
		t.Fatalf("bundle path = %q", path)
	}
	assertFile(t, path, "bundle archive")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("bundle mode = %o, want 0600", got)
	}
}

func TestFetchBundleURLDerivesDigestWhenNotSupplied(t *testing.T) {
	bundle := []byte("bundle archive")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bundle)
	}))
	t.Cleanup(server.Close)

	path, err := fetchBundleURL(context.Background(), server.URL+"/cluster.katlcfg", "", t.TempDir())
	if err != nil {
		t.Fatalf("fetchBundleURL() error = %v", err)
	}
	assertFile(t, path, "bundle archive")
}

func TestFetchBundleURLRejectsOptionalDigestMismatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("bundle archive"))
	}))
	t.Cleanup(server.Close)

	_, err := fetchBundleURL(context.Background(), server.URL+"/cluster.katlcfg", strings.Repeat("a", 64), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("fetchBundleURL() error = %v, want optional digest mismatch", err)
	}
}

func TestFetchManifestURL(t *testing.T) {
	manifest := []byte(`{"apiVersion":"install.katl.dev/v1alpha1","kind":"InstallManifest"}`)
	digest := sha256.Sum256(manifest)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cp-1.json" {
			t.Fatalf("request path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(manifest)
	}))
	t.Cleanup(server.Close)

	runDir := t.TempDir()
	path, err := fetchManifestURL(context.Background(), server.URL+"/cp-1.json", hex.EncodeToString(digest[:]), runDir)
	if err != nil {
		t.Fatalf("fetchManifestURL() error = %v", err)
	}
	if path != filepath.Join(runDir, "install-manifest.json") {
		t.Fatalf("manifest path = %q", path)
	}
	assertFile(t, path, `{"apiVersion":"install.katl.dev/v1alpha1","kind":"InstallManifest"}`)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("manifest mode = %o, want 0600", got)
	}
}

func TestFetchManifestURLDoesNotFollowRedirects(t *testing.T) {
	manifest := []byte(`{"apiVersion":"install.katl.dev/v1alpha1","kind":"InstallManifest"}`)
	digest := sha256.Sum256(manifest)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "/cp-1.json")
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(server.Close)

	_, err := fetchManifestURL(context.Background(), server.URL+"/redirect?token=secret", hex.EncodeToString(digest[:]), t.TempDir())
	if err == nil {
		t.Fatal("fetchManifestURL() error = nil, want redirect rejection")
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("fetchManifestURL() leaked query token: %v", err)
	}
}

func TestBootFetchesManifestURL(t *testing.T) {
	manifest := []byte(`{"apiVersion":"install.katl.dev/v1alpha1","kind":"InstallManifest"}`)
	digest := sha256.Sum256(manifest)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(manifest)
	}))
	t.Cleanup(server.Close)

	root := t.TempDir()
	runDir := filepath.Join(root, "run")
	writeTestFile(t, filepath.Join(runDir, "install-input.json"), `{"manifestURL":"`+server.URL+`/cp-1.json?token=secret","manifestSHA256":"`+hex.EncodeToString(digest[:])+`","installMode":"auto"}`)
	writeTestFile(t, filepath.Join(runDir, "install-manifest.json"), `{"apiVersion":"stale"}`)

	var stdout bytes.Buffer
	err := runBoot(context.Background(), runDir, filepath.Join(root, "etc"), "127.0.0.1:0", &stdout)
	if err == nil {
		t.Fatal("runBoot() error = nil, want manifest validation failure")
	}
	if strings.Contains(err.Error(), "not implemented") {
		t.Fatalf("runBoot() returned old URL handoff error: %v", err)
	}
	assertFile(t, filepath.Join(runDir, "install-manifest.json"), `{"apiVersion":"install.katl.dev/v1alpha1","kind":"InstallManifest"}`)
	if !strings.Contains(stdout.String(), "downloaded manifest") {
		t.Fatalf("stdout = %q, want downloaded manifest log", stdout.String())
	}
	if strings.Contains(stdout.String(), "secret") {
		t.Fatalf("stdout leaked manifest URL query token: %q", stdout.String())
	}
}

func TestFetchManifestURLDoesNotLeakMalformedURL(t *testing.T) {
	_, err := fetchManifestURL(context.Background(), "http://example.invalid/%zz?token=secret", strings.Repeat("a", 64), t.TempDir())
	if err == nil {
		t.Fatal("fetchManifestURL() error = nil, want malformed URL rejection")
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("fetchManifestURL() leaked query token: %v", err)
	}
}

func TestManifestRunnerContextConfiguresImageResolver(t *testing.T) {
	root := t.TempDir()
	manifestPath := filepath.Join(root, "media", "install-manifest.json")
	stateDir := filepath.Join(root, "state")

	install, err := manifestRunnerContext(manifestPath, stateDir, installstatus.InputModePXEPreseed, manifestPath)
	if err != nil {
		t.Fatalf("manifestRunnerContext() error = %v", err)
	}

	resolver, ok := install.KatlosResolver.(katlosimage.Resolver)
	if !ok {
		t.Fatalf("KatlosResolver = %T, want katlosimage.Resolver", install.KatlosResolver)
	}
	if resolver.MediaRoot != filepath.Dir(manifestPath) {
		t.Fatalf("MediaRoot = %q, want %q", resolver.MediaRoot, filepath.Dir(manifestPath))
	}
	if resolver.WorkDir != filepath.Join(stateDir, "katlos-image") {
		t.Fatalf("WorkDir = %q, want state-backed image workdir", resolver.WorkDir)
	}
	if resolver.Commands == nil || install.Commands == nil {
		t.Fatalf("command runners are not configured: resolver=%#v install=%#v", resolver.Commands, install.Commands)
	}
	if source, ok := install.Discovery.(discovery.CommandDiscoverySource); !ok || source.Commands == nil {
		t.Fatalf("Discovery = %#v, want command-backed discovery", install.Discovery)
	}
	if _, ok := install.RootSlotOpener.(disk.FileRootSlotDeviceOpener); !ok {
		t.Fatalf("RootSlotOpener = %T, want disk.FileRootSlotDeviceOpener", install.RootSlotOpener)
	}
	if install.IdentityRandom == nil {
		t.Fatal("IdentityRandom is nil")
	}
	if install.Chown == nil {
		t.Fatal("Chown is nil")
	}
}

func TestManifestRunnerContextLoadsKubeadmConfigs(t *testing.T) {
	root := t.TempDir()
	media := filepath.Join(root, "media")
	manifestPath := filepath.Join(media, "install-manifest.json")
	stateDir := filepath.Join(root, "state")
	writeTestFile(t, manifestPath, `{"kind":"InstallManifest"}`)
	writeTestFile(t, filepath.Join(media, installer.KubeadmConfigObjectsDir, "control-plane.yaml"), `apiVersion: config.katl.dev/v1alpha1
kind: KubeadmConfig
metadata:
  name: control-plane
spec:
  configFile: kubeadm/control-plane.yaml
`)
	writeTestFile(t, filepath.Join(media, installer.KubeadmConfigFilesDir, "control-plane.yaml"), `apiVersion: kubeadm.k8s.io/v1beta4
kind: InitConfiguration
`)

	install, err := manifestRunnerContext(manifestPath, stateDir, installstatus.InputModePXEPreseed, manifestPath)
	if err != nil {
		t.Fatalf("manifestRunnerContext() error = %v", err)
	}

	plan, ok := install.KubeadmConfigs["control-plane"]
	if !ok {
		t.Fatalf("KubeadmConfigs = %#v, want control-plane", install.KubeadmConfigs)
	}
	if plan.Config.RenderPath != "/etc/katl/kubeadm/control-plane/config.yaml" {
		t.Fatalf("RenderPath = %q", plan.Config.RenderPath)
	}
	if len(plan.Documents) != 1 || plan.Documents[0].Kind != "InitConfiguration" {
		t.Fatalf("Documents = %#v", plan.Documents)
	}
}

func TestBootWait(t *testing.T) {
	previousNotify := notifySystemd
	t.Cleanup(func() { notifySystemd = previousNotify })
	var notifications []string
	notifySystemd = func(payload string) error {
		notifications = append(notifications, payload)
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	runDir := filepath.Join(t.TempDir(), "run")
	etcDir := filepath.Join(t.TempDir(), "etc")
	var stdout bytes.Buffer
	err := runBootWithHandoff(ctx, runDir, etcDir, "127.0.0.1:0", &stdout, func(ctx context.Context, gotRunDir, gotAddr string, stdout io.Writer) error {
		if gotRunDir != runDir {
			t.Fatalf("run dir = %q, want %q", gotRunDir, runDir)
		}
		if gotAddr != "127.0.0.1:0" {
			t.Fatalf("handoff addr = %q", gotAddr)
		}
		fmt.Fprintln(stdout, "katlos-install waiting for config at http://127.0.0.1:0/v1/install")
		<-ctx.Done()
		return ctx.Err()
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("runBoot() error = %v, want deadline", err)
	}
	if got := stdout.String(); !strings.Contains(got, "waiting for config") {
		t.Fatalf("stdout = %q, want handoff announcement", got)
	}
	if len(notifications) < 2 || !strings.Contains(notifications[0], "STATUS=reading installer boot inputs") || !strings.Contains(notifications[1], "READY=1\nSTATUS=configuration handoff mode selected") {
		t.Fatalf("notifications = %#v", notifications)
	}
}

func TestHandoffAnnouncementBaseURLKeepsExplicitAddress(t *testing.T) {
	got, err := handoffAnnouncementBaseURLWithHost(&net.TCPAddr{
		IP:   net.ParseIP("192.0.2.44"),
		Port: 8080,
	}, func() (string, error) {
		t.Fatal("explicit listener address should not detect host")
		return "", nil
	})
	if err != nil {
		t.Fatalf("handoffAnnouncementBaseURL() error = %v", err)
	}
	if got != "http://192.0.2.44:8080" {
		t.Fatalf("base URL = %q", got)
	}
}

func TestHandoffAnnouncementBaseURLReplacesWildcardAddress(t *testing.T) {
	got, err := handoffAnnouncementBaseURLWithHost(&net.TCPAddr{
		IP:   net.ParseIP("0.0.0.0"),
		Port: 8080,
	}, func() (string, error) {
		return "192.0.2.44", nil
	})
	if err != nil {
		t.Fatalf("handoffAnnouncementBaseURL() error = %v", err)
	}
	if got != "http://192.0.2.44:8080" {
		t.Fatalf("base URL = %q", got)
	}
}

func TestWaitHandoffAnnouncementBaseURLRetriesUntilAddressExists(t *testing.T) {
	attempts := 0
	got, err := waitHandoffAnnouncementBaseURLWithHost(context.Background(), &net.TCPAddr{
		IP:   net.ParseIP("0.0.0.0"),
		Port: 8080,
	}, time.Second, time.Millisecond, func() (string, error) {
		attempts++
		if attempts < 3 {
			return "", errors.New("no non-loopback interface address found")
		}
		return "192.0.2.44", nil
	})
	if err != nil {
		t.Fatalf("waitHandoffAnnouncementBaseURL() error = %v", err)
	}
	if got != "http://192.0.2.44:8080" {
		t.Fatalf("base URL = %q", got)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
}

func TestHandoffAnnouncementIPRejectsWildcardAndLoopback(t *testing.T) {
	for _, ip := range []net.IP{
		net.ParseIP("0.0.0.0"),
		net.ParseIP("::"),
		net.ParseIP("127.0.0.1"),
		net.ParseIP("::1"),
		net.ParseIP("169.254.10.20"),
	} {
		if handoffAnnouncementIP(ip) {
			t.Fatalf("handoffAnnouncementIP(%s) = true", ip)
		}
	}
	if !handoffAnnouncementIP(net.ParseIP("192.0.2.44")) {
		t.Fatal("handoffAnnouncementIP(192.0.2.44) = false")
	}
}

func TestMaterializeHandoffPayloadsCopiesSeedMedia(t *testing.T) {
	root := t.TempDir()
	runDir := filepath.Join(root, "run")
	manifestPath := filepath.Join(runDir, "install-manifest.json")
	writeTestFile(t, manifestPath, `{"katlosImage":{"localRef":"katlos-install.squashfs"}}`)
	writeTestFile(t, filepath.Join(runDir, "preseed", "katlos-install.squashfs"), "katlos payload")
	writeTestFile(t, filepath.Join(runDir, "preseed", installer.KubeadmConfigObjectsDir, "control-plane.yaml"), "object")
	writeTestFile(t, filepath.Join(runDir, "preseed", installer.KubeadmConfigFilesDir, "control-plane.yaml"), "config")

	var stdout bytes.Buffer
	if err := materializeHandoffPayloads(manifestPath, runDir, &stdout); err != nil {
		t.Fatalf("materializeHandoffPayloads() error = %v", err)
	}

	assertFile(t, filepath.Join(runDir, "katlos-install.squashfs"), "katlos payload")
	assertFile(t, filepath.Join(runDir, installer.KubeadmConfigObjectsDir, "control-plane.yaml"), "object")
	assertFile(t, filepath.Join(runDir, installer.KubeadmConfigFilesDir, "control-plane.yaml"), "config")
	if got := stdout.String(); !strings.Contains(got, "katlos-install.squashfs") || !strings.Contains(got, installer.KubeadmConfigFilesDir) {
		t.Fatalf("stdout = %q, want copied payload logs", got)
	}
}

func TestBootHold(t *testing.T) {
	root := t.TempDir()
	runDir := filepath.Join(root, "run")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "install-input.json"), []byte(`{"holdForDebug":true}`), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	var stdout bytes.Buffer
	err := runBoot(ctx, runDir, filepath.Join(root, "etc"), "127.0.0.1:0", &stdout)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("runBoot() error = %v, want deadline", err)
	}
	if got := stdout.String(); !strings.Contains(got, "debug hold active") {
		t.Fatalf("stdout = %q, want debug hold log", got)
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
}

func assertFile(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(got) != want {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
}
