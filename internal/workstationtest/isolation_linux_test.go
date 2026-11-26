package workstationtest

import (
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// The default gate exercises known fallback paths. Run the full suite with
// KATL_TEST_FULL_WORKSTATION_ISOLATION=1 go test -count=1 -run '^TestWorkstationIsolation$' ./internal/workstationtest
func TestWorkstationIsolation(t *testing.T) {
	home := t.TempDir()
	sentinels := map[string]string{
		".config/katl/katlctl.yaml": `contexts:
- name: sentinel
  cluster: sentinel
clusters:
- name: sentinel
  nodes:
  - name: cp-1
    managementEndpoint: "192.0.2.1:9443"
    systemRole: control-plane
`,
		".gitconfig":   "[katl]\n\tsentinel = operator\n",
		".kube/config": "apiVersion: v1\nkind: Config\ncurrent-context: sentinel\n",
	}
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	watches := make(map[int32]string, len(sentinels))
	for name, contents := range sentinels {
		path := filepath.Join(home, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		wd, err := unix.InotifyAddWatch(fd, path, unix.IN_OPEN|unix.IN_MODIFY|unix.IN_MOVE_SELF|unix.IN_DELETE_SELF)
		if err != nil {
			t.Fatal(err)
		}
		watches[int32(wd)] = name
	}

	// Keep the Go build and module caches warm when changing HOME.
	output, err := exec.Command("go", "env", "GOCACHE", "GOMODCACHE").Output()
	if err != nil {
		t.Fatal(err)
	}
	cache, modules, _ := strings.Cut(strings.TrimSpace(string(output)), "\n")
	env := []string{"HOME=" + home, "XDG_CONFIG_HOME=" + filepath.Join(home, ".config"), "GOCACHE=" + cache, "GOMODCACHE=" + modules, "GOENV=off"}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "HOME", "XDG_CONFIG_HOME", "KATLCTL_CONFIG", "KATLCTL_CONFIG_DIR", "KUBECONFIG", "GOCACHE", "GOMODCACHE", "GOENV":
			continue
		}
		if !strings.HasPrefix(key, "GIT_") {
			env = append(env, entry)
		}
	}
	if err := disableGoTelemetry(env); err != nil {
		t.Fatal(err)
	}

	selected := []string{"TestNodeStatusShowsCertificateExpiry", "TestMkosiBuilderActionOutputIsIgnored", "TestGitHubLookupDecodesPullRequests"}
	args := []string{"test", "-buildvcs=false", "-count=1", "-v", "-run", "^(" + strings.Join(selected, "|") + ")$", "./cmd/katlctl", "./internal/scriptstest", "./internal/devclean"}
	full := os.Getenv("KATL_TEST_FULL_WORKSTATION_ISOLATION") == "1"
	if full {
		args = []string{"test", "-buildvcs=false", "-count=1", "-skip", "^TestWorkstationIsolation$", "./..."}
	}
	cmd := exec.Command("go", args...)
	cmd.Dir = "../.."
	cmd.Env = env
	output, childErr := cmd.CombinedOutput()

	var events [4096]byte
	for {
		n, err := unix.Read(fd, events[:])
		if errors.Is(err, unix.EAGAIN) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		for offset := 0; offset < n; {
			event := events[offset:]
			wd := int32(binary.NativeEndian.Uint32(event))
			if name, ok := watches[wd]; ok {
				t.Errorf("child tests accessed fake HOME sentinel %s", name)
				delete(watches, wd)
			} else if wd == -1 {
				t.Error("sentinel watch queue overflowed")
			}
			offset += unix.SizeofInotifyEvent + int(binary.NativeEndian.Uint32(event[12:]))
		}
	}
	if childErr != nil {
		t.Fatalf("child tests: %v\n%s", childErr, output)
	}
	if !full {
		for _, name := range selected {
			if !strings.Contains(string(output), "--- PASS: "+name+" (") {
				t.Errorf("child test %s did not run and pass", name)
			}
		}
	}
	t.Logf("child tests passed\n%s", output)
}
