package scriptstest

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/installer/payloadbundle"
	"github.com/katl-dev/katl/internal/kernelrelease"
)

func TestKernelInputBuilds(t *testing.T) {
	repo := scriptRepoFixture(t)
	tmp := t.TempDir()
	buildDir := filepath.Join(tmp, "build")
	seedRuntimeCacheOutputs(t, buildDir)
	bin := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(tmp, "container-calls")
	writeFakeExecutable(t, bin, "podman", `case "$1" in
image) printf 'fake-builder\n'; exit 0 ;;
run)
  selected=""
  for arg in "$@"; do
    case "$arg" in KATL_KERNEL_LOCK=*) selected="${arg#KATL_KERNEL_LOCK=/work/_build/mkosi/}" ;; esac
  done
  [[ "$selected" == kernel-rpms/records/*.json ]] || exit 91
  [[ "$(jq -r .reference "$KATL_MKOSI_BUILD_DIR/$selected")" == "$(jq -r .reference "$KATL_KERNEL_LOCK")" ]] || exit 92
  printf '%q ' "$@" >> "$KATL_FAKE_PODMAN_ARGS"; printf '\n' >> "$KATL_FAKE_PODMAN_ARGS" ;;
*) exit 1 ;;
esac
`)
	set := kernelrelease.Set{SchemaVersion: 1, FedoraRelease: "44", Architecture: "x86_64", Flavour: "standard", KernelRelease: "7.2.10-200.fc44.x86_64"}
	rpms := filepath.Join(tmp, "rpms")
	if err := os.Mkdir(rpms, 0o755); err != nil {
		t.Fatal(err)
	}
	for i, name := range []string{"kernel-core", "kernel-modules", "kernel-modules-core", "kernel-devel"} {
		file := name + "-7.2.10-200.fc44.x86_64.rpm"
		data := []byte("candidate " + name)
		role := "runtime"
		if i == 3 {
			role = "build"
		}
		blob := payloadbundle.DescribeBytes(data, role, kernelrelease.RPMMediaType, file)
		set.Packages = append(set.Packages, kernelrelease.Package{Name: name, NEVRA: name + "-0:7.2.10-200.fc44.x86_64", SourceURL: "https://removed.invalid/" + file, Descriptor: blob.Descriptor})
		if err := os.WriteFile(filepath.Join(rpms, file), data, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(filepath.Join(rpms, file), time.Unix(1, 0), time.Unix(1, 0)); err != nil {
			t.Fatal(err)
		}
	}
	layout := filepath.Join(tmp, "layout")
	lock, err := kernelrelease.Export(context.Background(), set, rpms, layout, "registry.example/kernel-inputs")
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(tmp, "lock.json")
	if err := kernelrelease.Write(lockPath, lock); err != nil {
		t.Fatal(err)
	}
	seedKernelMetadata(t, buildDir, lock.Set)
	env := append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "KATL_CONTAINER_RUNTIME=podman", "KATL_FLAVOUR=standard", "KATL_VERSION=0.0.0-kernel-test", "KATL_KERNEL_LOCK="+lockPath, "KATL_KERNEL_LAYOUT="+layout, "KATL_MKOSI_BUILD_DIR="+buildDir, "KATL_FAKE_PODMAN_ARGS="+calls, "KATL_BUILD_COMMIT=kernel-input-test")
	env = append(env, activeGoCacheEnv(t)...)
	run := func(args ...string) (string, error) {
		cmd := exec.Command(filepath.Join(repo, "scripts/mkosi"), args...)
		cmd.Dir, cmd.Env = repo, env
		output, err := cmd.CombinedOutput()
		return string(output), err
	}
	for _, args := range [][]string{{"build-runtime"}, {"--profile", "runtime", "-f", "build"}, {"--profile=runtime", "-f", "build"}, {"--profile", "installer-image", "-f", "build"}} {
		if output, err := run(args...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, output)
		}
	}
	for _, call := range readLinesForScripts(t, calls) {
		if !strings.Contains(call, "--include /work/_build/mkosi/kernel-") {
			t.Fatalf("explicit lock ignored: %s", call)
		}
	}
	before, err := os.ReadFile(filepath.Join(buildDir, "kernel-runtime.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(calls); err != nil {
		t.Fatal(err)
	}
	if output, err := run("build-runtime"); err != nil || !strings.Contains(output, "mkosi cache hit: runtime") {
		t.Fatalf("repeat build: %v\n%s", err, output)
	}
	if _, err := os.Stat(calls); !os.IsNotExist(err) {
		t.Fatalf("verified unchanged inputs missed image cache: %v", err)
	}

	identity, err := lock.Digest()
	if err != nil {
		t.Fatal(err)
	}
	pkg := &set.Packages[0]
	cached := filepath.Join(buildDir, "kernel-rpms", identity, pkg.FileName)
	if err := os.Chmod(cached, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cached, []byte("tampered! "+pkg.Name), 0o644); err != nil {
		t.Fatal(err)
	}
	if output, err := run("build-runtime"); err == nil || !strings.Contains(output, "failed SHA-256") {
		t.Fatalf("corrupt RPM accepted before image-cache reuse: %v\n%s", err, output)
	}
	if _, err := os.Stat(calls); !os.IsNotExist(err) {
		t.Fatalf("builder ran with corrupt RPMs: %v", err)
	}

	// Preserve the RPM filename, version and mtime while changing its bytes.
	// Both the wrapper artifact cache and mkosi's own key must distinguish it.
	path := filepath.Join(rpms, pkg.FileName)
	stamp := time.Unix(1, 0)
	data := []byte("changed " + pkg.Name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	pkg.Descriptor = payloadbundle.DescribeBytes(data, pkg.Role, kernelrelease.RPMMediaType, pkg.FileName).Descriptor
	lock, err = kernelrelease.Export(context.Background(), set, rpms, layout, "registry.example/kernel-inputs")
	if err != nil {
		t.Fatal(err)
	}
	if err := kernelrelease.Write(lockPath, lock); err != nil {
		t.Fatal(err)
	}
	if output, err := run("build-runtime"); err != nil || strings.Contains(output, "mkosi cache hit: runtime") {
		t.Fatalf("changed RPM bytes reused image: %v\n%s", err, output)
	}
	after, err := os.ReadFile(filepath.Join(buildDir, "kernel-runtime.conf"))
	if err != nil {
		t.Fatal(err)
	}
	cacheKey := func(config []byte) string {
		for _, line := range strings.Split(string(config), "\n") {
			if strings.HasPrefix(line, "CacheKey=") {
				return line
			}
		}
		return ""
	}
	if cacheKey(before) == "" || cacheKey(before) == cacheKey(after) {
		t.Fatal("same-NEVRA byte change reused native mkosi cache key")
	}
}

// Build fixtures use real retained OCI closures while keeping package downloads
// and container execution outside the wrapper tests' contract.
func scriptKernelSet(flavour string) kernelrelease.Set {
	set := kernelrelease.Set{SchemaVersion: 1, FedoraRelease: "44", Architecture: "x86_64", Flavour: flavour, KernelRelease: "7.2.10-200.fc44.x86_64"}
	names, _ := kernelrelease.Names(flavour)
	for i, name := range names {
		role := "runtime"
		if i == 3 {
			role = "build"
		}
		file := name + "-" + set.KernelRelease + ".rpm"
		blob := payloadbundle.DescribeBytes([]byte("retained "+name), role, kernelrelease.RPMMediaType, file)
		set.Packages = append(set.Packages, kernelrelease.Package{Name: name, NEVRA: name + "-0:" + set.KernelRelease, SourceURL: "https://removed.invalid/" + file, Descriptor: blob.Descriptor})
	}
	return set
}

func seedKernelMetadata(t *testing.T, buildDir string, set kernelrelease.Set) {
	t.Helper()
	identity, err := set.Digest()
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.ReadFile(filepath.Join(buildDir, "katl-runtime-root.squashfs.json"))
	if err != nil {
		t.Fatal(err)
	}
	var runtime map[string]any
	if err := json.Unmarshal(root, &runtime); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(buildDir, "katl-kernel-inputs.tar.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var headers map[string]any
	if err := json.Unmarshal(data, &headers); err != nil {
		t.Fatal(err)
	}
	headers["kernelSetSHA256"] = identity
	headers["target"] = map[string]any{"runtimeSHA256": runtime["sha256"]}
	data, err = json.Marshal(headers)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
}

func seedKernelLocks(t *testing.T, repo string) {
	t.Helper()
	layout := t.TempDir()
	for _, flavour := range []string{"standard", "lts"} {
		set := scriptKernelSet(flavour)
		rpms := t.TempDir()
		for _, pkg := range set.Packages {
			if err := os.WriteFile(filepath.Join(rpms, pkg.FileName), []byte("retained "+pkg.Name), 0644); err != nil {
				t.Fatal(err)
			}
		}
		lock, err := kernelrelease.Export(context.Background(), set, rpms, layout, "registry.example/kernel-inputs")
		if err != nil {
			t.Fatal(err)
		}
		if err := kernelrelease.Write(filepath.Join(repo, "kernels/fedora-44/x86_64", flavour+".json"), lock); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("KATL_KERNEL_LAYOUT", layout)
}

func TestCommittedKernelLocks(t *testing.T) {
	repo := repoRoot(t)
	release, err := exec.Command(filepath.Join(repo, "scripts/fedora-release")).Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, flavour := range []string{"standard", "lts"} {
		lock, err := kernelrelease.Read(filepath.Join(repo, "kernels", "fedora-"+strings.TrimSpace(string(release)), "x86_64", flavour+".json"))
		if err != nil {
			t.Fatal(err)
		}
		if lock.Flavour != flavour || lock.FedoraRelease != strings.TrimSpace(string(release)) || lock.Architecture != "x86_64" {
			t.Fatalf("committed kernel lock selects another build: %+v", lock.Set)
		}
	}
}
