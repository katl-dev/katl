package installer

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestApplyInput(t *testing.T) {
	root := t.TempDir()
	preseed := filepath.Join(root, "seed")
	runDir := filepath.Join(root, "run")
	etcDir := filepath.Join(root, "etc")
	writeTestFile(t, filepath.Join(preseed, "install-input.json"), `{"manifestPath":"/run/katl/install-manifest.json"}`)
	writeTestFile(t, filepath.Join(preseed, "etc/katl/install-manifest.json"), `{"kind":"InstallManifest"}`)

	var stdout bytes.Buffer
	if err := ApplyInput(InputApplyRequest{
		PreseedDirs: []string{preseed},
		RunDir:      runDir,
		EtcDir:      etcDir,
		Stdout:      &stdout,
	}); err != nil {
		t.Fatalf("ApplyInput() error = %v", err)
	}

	assertFile(t, filepath.Join(runDir, "install-input.json"), `{"manifestPath":"/run/katl/install-manifest.json"}`)
	assertFile(t, filepath.Join(etcDir, "install-manifest.json"), `{"kind":"InstallManifest"}`)
	if got := stdout.String(); !strings.Contains(got, "copied") {
		t.Fatalf("stdout = %q, want copied log", got)
	}
}

func TestApplyInputCopiesManifestLocalRef(t *testing.T) {
	root := t.TempDir()
	preseed := filepath.Join(root, "seed")
	runDir := filepath.Join(root, "run")
	writeTestFile(t, filepath.Join(preseed, "install-manifest.json"), `{"katlosImage":{"localRef":"payloads/katlos-install.squashfs"}}`)
	writeTestFile(t, filepath.Join(preseed, "payloads/katlos-install.squashfs"), "katlos payload")

	var stdout bytes.Buffer
	if err := ApplyInput(InputApplyRequest{
		PreseedDirs: []string{preseed},
		RunDir:      runDir,
		Stdout:      &stdout,
	}); err != nil {
		t.Fatalf("ApplyInput() error = %v", err)
	}

	assertFile(t, filepath.Join(runDir, "install-manifest.json"), `{"katlosImage":{"localRef":"payloads/katlos-install.squashfs"}}`)
	assertFile(t, filepath.Join(runDir, "payloads/katlos-install.squashfs"), "katlos payload")
	if got := stdout.String(); !strings.Contains(got, "payloads/katlos-install.squashfs") {
		t.Fatalf("stdout = %q, want localRef copy log", got)
	}
}

func TestApplyInputKeepsLocalRefOnSelectedSeedManifest(t *testing.T) {
	root := t.TempDir()
	preseed := filepath.Join(root, "seed")
	runDir := filepath.Join(root, "run")
	manifestPath := filepath.Join(preseed, "install-manifest.json")
	writeTestFile(t, filepath.Join(preseed, "install-input.json"), `{"manifestPath":`+quoteJSON(manifestPath)+`}`)
	writeTestFile(t, manifestPath, `{"katlosImage":{"localRef":"payloads/katlos-install.squashfs"}}`)
	writeTestFile(t, filepath.Join(preseed, "payloads/katlos-install.squashfs"), "katlos payload")

	var stdout bytes.Buffer
	if err := ApplyInput(InputApplyRequest{
		PreseedDirs: []string{preseed},
		RunDir:      runDir,
		Stdout:      &stdout,
	}); err != nil {
		t.Fatalf("ApplyInput() error = %v", err)
	}

	assertFile(t, filepath.Join(runDir, "install-input.json"), `{"manifestPath":`+quoteJSON(manifestPath)+`}`)
	assertFile(t, filepath.Join(runDir, "install-manifest.json"), `{"katlosImage":{"localRef":"payloads/katlos-install.squashfs"}}`)
	assertNoFile(t, filepath.Join(runDir, "payloads/katlos-install.squashfs"))
	if got := stdout.String(); strings.Contains(got, "payloads/katlos-install.squashfs") {
		t.Fatalf("stdout = %q, did not expect localRef copy log", got)
	}
}

func TestApplyInputCopiesYAMLManifestLocalRef(t *testing.T) {
	root := t.TempDir()
	preseed := filepath.Join(root, "seed")
	runDir := filepath.Join(root, "run")
	writeTestFile(t, filepath.Join(preseed, "install-manifest.yaml"), `katlosImage:
  localRef: payloads/katlos-install.squashfs
`)
	writeTestFile(t, filepath.Join(preseed, "payloads/katlos-install.squashfs"), "katlos payload")

	var stdout bytes.Buffer
	if err := ApplyInput(InputApplyRequest{
		PreseedDirs: []string{preseed},
		RunDir:      runDir,
		Stdout:      &stdout,
	}); err != nil {
		t.Fatalf("ApplyInput() error = %v", err)
	}

	assertFile(t, filepath.Join(runDir, "install-manifest.yaml"), `katlosImage:
  localRef: payloads/katlos-install.squashfs
`)
	assertFile(t, filepath.Join(runDir, "payloads/katlos-install.squashfs"), "katlos payload")
	if got := stdout.String(); !strings.Contains(got, "payloads/katlos-install.squashfs") {
		t.Fatalf("stdout = %q, want localRef copy log", got)
	}
}

func TestApplyInputCopiesManifestKubeadmDirs(t *testing.T) {
	root := t.TempDir()
	preseed := filepath.Join(root, "seed")
	runDir := filepath.Join(root, "run")
	writeTestFile(t, filepath.Join(preseed, "install-manifest.json"), `{"kind":"InstallManifest"}`)
	writeTestFile(t, filepath.Join(preseed, KubeadmConfigObjectsDir, "control-plane.yaml"), "object")
	writeTestFile(t, filepath.Join(preseed, KubeadmConfigFilesDir, "control-plane.yaml"), "config")

	var stdout bytes.Buffer
	if err := ApplyInput(InputApplyRequest{
		PreseedDirs: []string{preseed},
		RunDir:      runDir,
		Stdout:      &stdout,
	}); err != nil {
		t.Fatalf("ApplyInput() error = %v", err)
	}

	assertFile(t, filepath.Join(runDir, KubeadmConfigObjectsDir, "control-plane.yaml"), "object")
	assertFile(t, filepath.Join(runDir, KubeadmConfigFilesDir, "control-plane.yaml"), "config")
	if got := stdout.String(); !strings.Contains(got, KubeadmConfigObjectsDir) || !strings.Contains(got, KubeadmConfigFilesDir) {
		t.Fatalf("stdout = %q, want kubeadm dir copy logs", got)
	}
}

func TestApplyInputMergesManifestKubeadmDirs(t *testing.T) {
	root := t.TempDir()
	preseed := filepath.Join(root, "seed")
	runDir := filepath.Join(root, "run")
	writeTestFile(t, filepath.Join(preseed, "install-manifest.json"), `{"kind":"InstallManifest"}`)
	writeTestFile(t, filepath.Join(preseed, KubeadmConfigObjectsDir, "existing.yaml"), "preseed object")
	writeTestFile(t, filepath.Join(preseed, KubeadmConfigObjectsDir, "new.yaml"), "new object")
	writeTestFile(t, filepath.Join(runDir, KubeadmConfigObjectsDir, "existing.yaml"), "existing object")

	var stdout bytes.Buffer
	if err := ApplyInput(InputApplyRequest{
		PreseedDirs: []string{preseed},
		RunDir:      runDir,
		Stdout:      &stdout,
	}); err != nil {
		t.Fatalf("ApplyInput() error = %v", err)
	}

	assertFile(t, filepath.Join(runDir, KubeadmConfigObjectsDir, "existing.yaml"), "existing object")
	assertFile(t, filepath.Join(runDir, KubeadmConfigObjectsDir, "new.yaml"), "new object")
	if got := stdout.String(); !strings.Contains(got, KubeadmConfigObjectsDir) {
		t.Fatalf("stdout = %q, want kubeadm dir copy log", got)
	}
}

func TestApplyInputCopiesInitrdNetworkd(t *testing.T) {
	root := t.TempDir()
	preseed := filepath.Join(root, "seed")
	networkDir := filepath.Join(root, "network")
	content := "[Match]\nName=*\n\n[Network]\nDHCP=yes\n"
	writeTestFile(t, filepath.Join(preseed, "etc/systemd/network/80-katl-vmtest-dhcp.network"), content)
	writeTestFile(t, filepath.Join(networkDir, "10-existing.network"), "[Match]\nName=lo\n")

	var stdout bytes.Buffer
	if err := ApplyInput(InputApplyRequest{
		PreseedDirs: []string{preseed},
		NetworkDir:  networkDir,
		Stdout:      &stdout,
	}); err != nil {
		t.Fatalf("ApplyInput() error = %v", err)
	}

	assertFile(t, filepath.Join(networkDir, "80-katl-vmtest-dhcp.network"), content)
	assertFile(t, filepath.Join(networkDir, "10-existing.network"), "[Match]\nName=lo\n")
	if got := stdout.String(); !strings.Contains(got, "etc/systemd/network") {
		t.Fatalf("stdout = %q, want networkd copy log", got)
	}
}

func TestApplyInputRejectsUnsafeManifestLocalRef(t *testing.T) {
	root := t.TempDir()
	preseed := filepath.Join(root, "seed")
	writeTestFile(t, filepath.Join(preseed, "install-manifest.json"), `{"katlosImage":{"localRef":"../katlos-install.squashfs"}}`)

	err := ApplyInput(InputApplyRequest{PreseedDirs: []string{preseed}, RunDir: filepath.Join(root, "run")})
	if err == nil || !strings.Contains(err.Error(), "dot segments") {
		t.Fatalf("ApplyInput() error = %v, want dot segments error", err)
	}
}

func TestApplyInputMountsSeedDevice(t *testing.T) {
	root := t.TempDir()
	device := filepath.Join(root, "seed-device")
	preseed := filepath.Join(root, "mounted-seed")
	runDir := filepath.Join(root, "run")
	writeTestFile(t, device, "")
	writeTestFile(t, filepath.Join(preseed, "install-input.json"), `{"manifestPath":"/run/katl/install-manifest.json"}`)

	commands := &NoopCommandRunner{}
	var stdout bytes.Buffer
	if err := ApplyInput(InputApplyRequest{
		PreseedDirs: []string{preseed},
		SeedDevices: []string{filepath.Join(root, "missing-seed-device"), device},
		SeedMount:   preseed,
		Commands:    commands,
		RunDir:      runDir,
		Stdout:      &stdout,
	}); err != nil {
		t.Fatalf("ApplyInput() error = %v", err)
	}

	assertFile(t, filepath.Join(runDir, "install-input.json"), `{"manifestPath":"/run/katl/install-manifest.json"}`)
	if len(commands.Calls) != 1 || commands.Calls[0].Name != "mount" {
		t.Fatalf("commands = %#v, want mount", commands.Calls)
	}
	if got := strings.Join(commands.Calls[0].Args, " "); !strings.Contains(got, device) || !strings.Contains(got, preseed) {
		t.Fatalf("mount args = %#v", commands.Calls[0].Args)
	}
	if got := stdout.String(); !strings.Contains(got, "mounted seed device") || !strings.Contains(got, "copied") {
		t.Fatalf("stdout = %q", got)
	}
}

func TestApplyInputMountsInstallMediaReadOnly(t *testing.T) {
	root := t.TempDir()
	device := filepath.Join(root, "installer-media")
	mountPoint := filepath.Join(root, "media")
	writeTestFile(t, device, "iso")

	commands := &NoopCommandRunner{}
	var stdout bytes.Buffer
	if err := ApplyInput(InputApplyRequest{
		PreseedDirs:  []string{filepath.Join(root, "missing-preseed")},
		MediaDevices: []string{filepath.Join(root, "missing-media"), device},
		MediaMount:   mountPoint,
		Commands:     commands,
		Stdout:       &stdout,
	}); err != nil {
		t.Fatalf("ApplyInput() error = %v", err)
	}
	if len(commands.Calls) != 1 || commands.Calls[0].Name != "mount" {
		t.Fatalf("commands = %#v, want one media mount", commands.Calls)
	}
	if got := strings.Join(commands.Calls[0].Args, " "); got != "-o ro "+device+" "+mountPoint {
		t.Fatalf("mount args = %q", got)
	}
	if got := stdout.String(); !strings.Contains(got, "mounted install media") {
		t.Fatalf("stdout = %q", got)
	}
}

func TestApplyInputSkipsMissingSeedDevice(t *testing.T) {
	root := t.TempDir()
	preseed := filepath.Join(root, "seed")
	runDir := filepath.Join(root, "run")
	writeTestFile(t, filepath.Join(preseed, "install-input.json"), `{"waitForConfig":true}`)

	commands := &NoopCommandRunner{}
	var stdout bytes.Buffer
	if err := ApplyInput(InputApplyRequest{
		PreseedDirs: []string{preseed},
		SeedDevices: []string{filepath.Join(root, "missing-seed-device")},
		SeedMount:   filepath.Join(root, "missing-mount"),
		Commands:    commands,
		RunDir:      runDir,
		Stdout:      &stdout,
	}); err != nil {
		t.Fatalf("ApplyInput() error = %v", err)
	}
	assertFile(t, filepath.Join(runDir, "install-input.json"), `{"waitForConfig":true}`)
	if len(commands.Calls) != 0 {
		t.Fatalf("commands = %#v, want no seed mount", commands.Calls)
	}
	if got := stdout.String(); !strings.Contains(got, "seed device not found") || !strings.Contains(got, "missing-seed-device") || !strings.Contains(got, "seed device directory "+root+" exists=true") {
		t.Fatalf("stdout = %q", got)
	}
}

func TestApplyInputWaitsForSeedDevice(t *testing.T) {
	root := t.TempDir()
	device := filepath.Join(root, "seed-device")
	preseed := filepath.Join(root, "mounted-seed")
	runDir := filepath.Join(root, "run")
	writeTestFile(t, filepath.Join(preseed, "install-input.json"), `{"manifestPath":"/run/katl/install-manifest.json"}`)
	go func() {
		time.Sleep(50 * time.Millisecond)
		writeTestFile(t, device, "")
	}()

	commands := &NoopCommandRunner{}
	if err := ApplyInput(InputApplyRequest{
		PreseedDirs: []string{preseed},
		SeedDevices: []string{device},
		SeedMount:   preseed,
		SeedWait:    time.Second,
		Commands:    commands,
		RunDir:      runDir,
	}); err != nil {
		t.Fatalf("ApplyInput() error = %v", err)
	}
	assertFile(t, filepath.Join(runDir, "install-input.json"), `{"manifestPath":"/run/katl/install-manifest.json"}`)
	if len(commands.Calls) != 1 || commands.Calls[0].Name != "mount" {
		t.Fatalf("commands = %#v, want mount", commands.Calls)
	}
}

func TestApplyInputNone(t *testing.T) {
	var stdout bytes.Buffer
	if err := ApplyInput(InputApplyRequest{
		PreseedDirs: []string{filepath.Join(t.TempDir(), "missing")},
		Stdout:      &stdout,
	}); err != nil {
		t.Fatalf("ApplyInput() error = %v", err)
	}
	if got, want := stdout.String(), "katl input: no preseed files found\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestApplyInputJSON(t *testing.T) {
	root := t.TempDir()
	preseed := filepath.Join(root, "seed")
	writeTestFile(t, filepath.Join(preseed, "install-input.json"), `{`)

	err := ApplyInput(InputApplyRequest{PreseedDirs: []string{preseed}, RunDir: filepath.Join(root, "run")})
	if err == nil || !strings.Contains(err.Error(), "not valid JSON") {
		t.Fatalf("ApplyInput() error = %v, want JSON error", err)
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

func quoteJSON(value string) string {
	return strconv.Quote(value)
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

func assertNoFile(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("%s exists, want missing", path)
	} else if !os.IsNotExist(err) {
		t.Fatalf("Stat(%s) error = %v", path, err)
	}
}
