package scriptstest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/gittest"
	"github.com/katl-dev/katl/internal/workstationtest"
)

func TestMkosiDirectRejectsRuntimePackaging(t *testing.T) {
	repo := repoRoot(t)
	cmd := exec.Command(filepath.Join(repo, "scripts", "mkosi"), "build-runtime")
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "KATL_CONTAINER_RUNTIME=direct")
	output, err := runInGroup(t, cmd, scriptTimeout)
	if err == nil {
		t.Fatalf("scripts/mkosi direct build-runtime unexpectedly passed:\n%s", output)
	}
	if !strings.Contains(string(output), "direct currently supports installer-image builds only") {
		t.Fatalf("output missing direct-mode rejection:\n%s", output)
	}
}

func TestMkosiRuntimeCacheUsesIncludedBinaryIdentity(t *testing.T) {
	const (
		unrelated = "./internal/vmtest/testcmd/net-client"
		included  = "./cmd/katl-host-config-activate"
		guest     = "./cmd/katl-vmtest-agent"
	)
	repo := scriptRepoFixture(t)
	tmp := t.TempDir()
	buildDir := filepath.Join(tmp, "mkosi-build")
	bin := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", bin, err)
	}
	builderCalled := filepath.Join(tmp, "builder-called")
	writeFakeExecutable(t, bin, "podman", `if [[ "${1:-}" == "image" && "${2:-}" == "exists" ]]; then
  exit 0
fi
if [[ "${1:-}" == "image" && "${2:-}" == "inspect" ]]; then
  printf 'fake-builder-image-id\n'
  exit 0
fi
[[ "$1" == run ]]
touch "$KATL_FAKE_BUILDER_CALLED"
`)
	stubMkosiGoBuilds(t, bin, included, guest)
	seedRuntimeCacheOutputs(t, buildDir)
	env := append(
		os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"KATL_CONTAINER_RUNTIME=podman",
		"KATL_MKOSI_BUILD_DIR="+buildDir,
		"KATL_FAKE_BUILDER_CALLED="+builderCalled,
		"KATL_BUILD_COMMIT=cache-test",
		"KATL_VERSION=0.0.0-cache-test",
		"TMPDIR="+tmp,
	)
	env = append(env, activeGoCacheEnv(t)...)

	build := func(t *testing.T, imageSupport string, wantHit bool) {
		t.Helper()
		cmd := exec.Command(filepath.Join(repo, "scripts", "mkosi"), "build-runtime")
		cmd.Dir = repo
		cmd.Env = append(env, "KATL_VMTEST_IMAGE_SUPPORT="+imageSupport)
		output, err := runInGroup(t, cmd, scriptTimeout)
		if err != nil {
			t.Fatalf("scripts/mkosi build-runtime failed: %v\n%s", err, output)
		}
		if hit := strings.Contains(string(output), "mkosi cache hit: runtime artifacts match the current repo"); hit != wantHit {
			t.Fatalf("runtime cache hit = %t, want %t:\n%s", hit, wantHit, output)
		}
		// Each step observes only its own builder invocation.
		err = os.Remove(builderCalled)
		if wantHit && !os.IsNotExist(err) || !wantHit && err != nil {
			t.Fatalf("builder call log on cache hit=%t: %v", wantHit, err)
		}
	}

	build(t, "0", false)
	t.Run("unrelated source", func(t *testing.T) {
		writeTemporaryFile(t, filepath.Join(repo, unrelated, "cache_identity_probe.go"), "package main\n\nfunc init() { println(\"unrelated change\") }\n")
		build(t, "0", true)
	})

	for _, artifact := range []string{"katl-runtime.efi", "katl-kernel-inputs.tar"} {
		t.Run("corrupt "+artifact, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(buildDir, artifact), []byte("corrupt"), 0o644); err != nil {
				t.Fatalf("corrupt runtime artifact: %v", err)
			}
			build(t, "0", false)
		})
		seedRuntimeCacheOutputs(t, buildDir)
	}

	writeTemporaryFile(t, filepath.Join(repo, included, "cache_identity_probe.go"), "package main\n\nfunc init() { println(\"included binary change\") }\n")
	t.Run("included source", func(t *testing.T) {
		build(t, "0", false)
	})

	t.Run("instrumented guest agent", func(t *testing.T) {
		build(t, "1", false)
		build(t, "1", true)
		writeTemporaryFile(t, filepath.Join(repo, guest, "cache_identity_probe.go"), "package main\n\nfunc init() { println(\"changed instrumented guest agent\") }\n")
		build(t, "1", false)
	})
}

func TestMkosiRuntimeCacheMissesWhenBinaryIdentityUnavailable(t *testing.T) {
	repo := scriptRepoFixture(t)
	tmp := t.TempDir()
	buildDir := filepath.Join(tmp, "mkosi-build")
	bin := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", bin, err)
	}
	podmanArgs := filepath.Join(tmp, "podman-args.txt")
	writeFakeExecutable(t, bin, "podman", `if [[ "${1:-}" == "image" && "${2:-}" == "exists" ]]; then
  exit 0
fi
if [[ "${1:-}" == "image" && "${2:-}" == "inspect" ]]; then
  printf 'fake-builder-image-id\n'
  exit 0
fi
printf '%s\n' "$*" >> "$KATL_FAKE_PODMAN_ARGS"
`)
	stubMkosiGoBuilds(t, bin)
	seedRuntimeCacheOutputs(t, buildDir)
	env := append(
		os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"KATL_CONTAINER_RUNTIME=podman",
		"KATL_MKOSI_BUILD_DIR="+buildDir,
		"KATL_FAKE_PODMAN_ARGS="+podmanArgs,
		"KATL_BUILD_COMMIT=cache-test",
		"KATL_VERSION=0.0.0-cache-test",
		"TMPDIR="+tmp,
	)
	env = append(env, activeGoCacheEnv(t)...)

	build := func(overrides ...string) string {
		t.Helper()
		cmd := exec.Command(filepath.Join(repo, "scripts", "mkosi"), "build-runtime")
		cmd.Dir = repo
		cmd.Env = append(env, overrides...)
		output, err := runInGroup(t, cmd, scriptTimeout)
		if err != nil {
			t.Fatalf("scripts/mkosi build-runtime failed: %v\n%s", err, output)
		}
		return string(output)
	}

	build()
	stamp := filepath.Join(buildDir, ".katl-stamps", "runtime.sha256")
	before := mustReadFile(t, stamp)
	if err := os.Remove(podmanArgs); err != nil {
		t.Fatalf("remove initial builder log: %v", err)
	}

	if output := build("KATL_FAKE_GO_BUILD_FAIL=1"); strings.Contains(output, "mkosi cache hit: runtime") {
		t.Fatalf("binary identity failure still hit cache:\n%s", output)
	}
	if got := readLinesForScripts(t, podmanArgs); len(got) == 0 {
		t.Fatalf("binary identity failure did not invoke fake podman")
	}
	if after := mustReadFile(t, stamp); !bytes.Equal(after, before) {
		t.Fatalf("binary identity failure changed cache stamp from %q to %q", before, after)
	}
}

func TestMkosiImageCacheWithExternalExtensions(t *testing.T) {
	repo := repoRoot(t)
	tmp := t.TempDir()
	fixture := scriptRepoFixture(t)
	bin := filepath.Join(tmp, "bin")
	scripts := filepath.Join(fixture, "scripts")
	buildDir := filepath.Join(fixture, "_build", "mkosi")
	for _, dir := range []string{bin, scripts} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	seedRuntimeCacheOutputs(t, buildDir)
	if err := os.WriteFile(filepath.Join(fixture, "mkosi.conf"), []byte("[Distribution]\nRelease=44\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	baseRelease, err := os.ReadFile(filepath.Join(repo, "scripts", "fedora-release"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scripts, "fedora-release"), baseRelease, 0o755); err != nil {
		t.Fatal(err)
	}
	writeReleaseArtifact(t, buildDir, "katlos-install-0.0.0-dev-x86_64.squashfs")
	writeReleaseArtifact(t, buildDir, "katl-endpoint-advertiser.raw")
	writeFakeExecutable(t, scripts, "mkosi", "exit 0\n")
	writeFakeExecutable(t, scripts, "build-endpoint-advertiser-sysext", "exit 0\n")
	writeFakeExecutable(t, scripts, "build-katlos-install-image", `printf 'assembled\n' >> "$KATL_ASSEMBLY_LOG"
`)
	writeFakeExecutable(t, bin, "podman", `if [[ "${2:-}" == inspect ]]; then printf 'builder-id\n'; fi
`)
	stubMkosiGoBuilds(t, bin)
	log := filepath.Join(tmp, "assembly.log")
	env := append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"KATL_REPO_ROOT="+fixture,
		"KATL_MKOSI_BUILD_DIR="+buildDir,
		"KATL_CONTAINER_RUNTIME=podman",
		"KATL_VERSION=0.0.0-dev",
		"KATL_ARCHITECTURE=x86_64",
		"KATL_ASSEMBLY_LOG="+log,
	)
	env = append(env, activeGoCacheEnv(t)...)
	// A repeated ordinary build is cached. An external selection must always
	// reach assembly, including when only its blob contents have changed.
	for _, step := range []struct {
		selection string
		builds    int
	}{
		{
			builds: 1,
		},
		{
			builds: 1,
		},
		{
			selection: filepath.Join(tmp, "release.json"),
			builds:    2,
		},
		{
			selection: filepath.Join(tmp, "release.json"),
			builds:    3,
		},
		{
			builds: 4,
		},
		{
			builds: 4,
		},
	} {
		cmd := exec.Command(filepath.Join(repo, "scripts", "mkosi"), "build-katlos-install-image")
		cmd.Env = append(env, "KATL_EXTENSION_RELEASE="+step.selection)
		if output, err := runInGroup(t, cmd, scriptTimeout); err != nil {
			t.Fatalf("build: %v\n%s", err, output)
		}
		if got := len(readLinesForScripts(t, log)); got != step.builds {
			t.Fatalf("selection %q: assembly count = %d, want %d", step.selection, got, step.builds)
		}
	}
}

// stubMkosiGoBuilds replaces incidental Go compilation in scripts/mkosi and
// its build hooks. Cache tests need deterministic binary bytes to hash, so
// packages whose sources they change are compiled for real. The hook's kernel
// input export needs a real builder root and is skipped; every other go run,
// including scripts/mkosi kernel preparation, stays intact.
func stubMkosiGoBuilds(t *testing.T, bin string, realPackages ...string) {
	t.Helper()
	goPath := workstationtest.Tool(t, "go")
	if err := os.Symlink(goPath, filepath.Join(bin, "real-go")); err != nil {
		t.Fatal(err)
	}
	writeFakeExecutable(t, bin, "go", `
if [[ "$1" == run && "${3:-}" == export-kernel-inputs ]]; then
  exit 0
fi
package="${@: -1}"
if [[ "$1" != build || " `+strings.Join(realPackages, " ")+` " == *" $package "* ]]; then
  exec "${0%/*}/real-go" "$@"
fi
if [[ "${KATL_FAKE_GO_BUILD_FAIL:-0}" == 1 ]]; then
  exit 42
fi
while [[ $# -gt 1 ]]; do
  if [[ "$1" == -o ]]; then
    mkdir -p "$(dirname "$2")"
    printf '%s\n' "$package" > "$2"
    exit 0
  fi
  shift
done
echo 'mkosi fixture expected go build with -o' >&2
exit 2
`)
}

func writeFakeExecutable(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	content := "#!/usr/bin/env bash\nset -euo pipefail\n" + body
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
	return path
}

func readLinesForScripts(t *testing.T, path string) []string {
	t.Helper()
	data := mustReadFile(t, path)
	text := strings.TrimSpace(string(data))
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func seedRuntimeCacheOutputs(t *testing.T, buildDir string) {
	t.Helper()
	paths := []string{
		filepath.Join(buildDir, "artifacts.json"),
		filepath.Join(buildDir, "katl-runtime-root"),
		filepath.Join(buildDir, "katl-runtime.packages.tsv"),
		filepath.Join(buildDir, "katl-runtime-root.initrd"),
		filepath.Join(buildDir, "katl-runtime-root.vmlinuz"),
	}
	for _, path := range paths {
		if strings.HasSuffix(path, "katl-runtime-root") {
			if err := os.MkdirAll(path, 0o755); err != nil {
				t.Fatalf("MkdirAll(%s) error = %v", path, err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s) error = %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte("seed"), 0o644); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", path, err)
		}
	}
	for _, name := range []string{"katl-runtime-root.squashfs", "katl-runtime.efi", "katl-kernel-inputs.tar"} {
		writeReleaseArtifact(t, buildDir, name)
	}
	seedKernelMetadata(t, buildDir, scriptKernelSet("standard"))
}

func scriptRepoFixture(t *testing.T) string {
	t.Helper()
	repo, fixture := repoRoot(t), t.TempDir()
	cmd := exec.Command("bash", "-euo", "pipefail", "-c", `tar -C "$1" --exclude=./.git --exclude=./.jj --exclude=./.beads --exclude=./_build --exclude=./build -cf - . | tar -C "$2" -xf -`, "bash", repo, fixture)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("copy script test repository: %v\n%s", err, output)
	}
	seedKernelLocks(t, fixture)
	if err := os.MkdirAll(filepath.Join(fixture, "mkosi.conf.d"), 0755); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func writeTemporaryFile(t *testing.T, path string, content string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("temporary test file already exists: %s", path)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat temporary test file %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
	t.Cleanup(func() {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			t.Fatalf("remove temporary test file %s: %v", path, err)
		}
	})
}

func activeGoCacheEnv(t *testing.T) []string {
	t.Helper()
	values := make([]string, 0, 2)
	for _, item := range []struct {
		name string
		env  string
	}{
		{"GOMODCACHE", "KATL_GO_MOD_CACHE"},
		{"GOCACHE", "KATL_GO_BUILD_CACHE"},
	} {
		cmd := exec.Command("go", "env", item.name)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("go env %s failed: %v\n%s", item.name, err, output)
		}
		value := strings.TrimSpace(string(output))
		if value == "" {
			t.Fatalf("go env %s returned an empty path", item.name)
		}
		values = append(values, item.env+"="+value)
	}
	return values
}

func TestMkosiFlavourInvalidatesArtifacts(t *testing.T) {
	repo, tmp := scriptRepoFixture(t), t.TempDir()
	buildDir, bin := filepath.Join(tmp, "build"), filepath.Join(tmp, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	stubMkosiGoBuilds(t, bin)
	calls := filepath.Join(tmp, "calls")
	writeFakeExecutable(t, bin, "podman", `if [[ "$1" == image ]]; then
  [[ "$2" != inspect ]] || echo fake-image
  exit 0
fi
printf 'CALL\n' >> "$KATL_FAKE_PODMAN_ARGS"
printf '%s\n' "$@" >> "$KATL_FAKE_PODMAN_ARGS.detail"
`)
	seedRuntimeCacheOutputs(t, buildDir)
	env := append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "KATL_CONTAINER_RUNTIME=podman", "KATL_MKOSI_BUILD_DIR="+buildDir, "KATL_FAKE_PODMAN_ARGS="+calls, "KATL_BUILD_COMMIT=flavour-test", "KATL_VERSION=2026.9.0-dev.1")
	env = append(env, activeGoCacheEnv(t)...)
	for _, step := range []struct {
		flavour string
		builds  int
	}{{"standard", 1}, {"standard", 1}, {"lts", 2}, {"lts", 2}, {"standard", 3}} {
		seedKernelMetadata(t, buildDir, scriptKernelSet(step.flavour))
		cmd := exec.Command(filepath.Join(repo, "scripts/mkosi"), "build-runtime")
		cmd.Dir, cmd.Env = repo, append(append([]string(nil), env...), "KATL_FLAVOUR="+step.flavour)
		if out, err := runInGroup(t, cmd, scriptTimeout); err != nil {
			t.Fatalf("%s: %v\n%s", step.flavour, err, out)
		}
		if got := len(readLinesForScripts(t, calls)); got != step.builds {
			t.Fatalf("%s: builder runs=%d, want %d", step.flavour, got, step.builds)
		}
	}
	writeTemporaryFile(t, filepath.Join(repo, "mkosi.conf.d", "cache-probe.conf"), "[Build]\nEnvironment=CACHE_PROBE=changed\n")
	cmd := exec.Command(filepath.Join(repo, "scripts", "mkosi"), "build-runtime")
	cmd.Dir, cmd.Env = repo, append(append([]string(nil), env...), "KATL_FLAVOUR=standard")
	if out, err := runInGroup(t, cmd, scriptTimeout); err != nil {
		t.Fatalf("changed mkosi input: %v\n%s", err, out)
	}
	if got := len(readLinesForScripts(t, calls)); got != 4 {
		t.Fatalf("workspace configuration change did not rebuild: calls=%d", got)
	}
	invocations := string(mustReadFile(t, calls+".detail"))
	if !strings.Contains(invocations, "KATL_FLAVOUR=lts") || !strings.Contains(invocations, "cache-lts:/mkosi-cache") {
		t.Fatalf("LTS kernel selection/cache was not passed to builder: %s", invocations)
	}
}

// Linked worktrees share the main checkout's mkosi image cache, which mkosi
// replaces in place without locking. The fake builder rewrites that shared
// cache slowly, as mkosi does, and copies it back as the image root.
func TestMkosiWorktreeBuildsShareCacheSafely(t *testing.T) {
	root, tmp := t.TempDir(), t.TempDir()
	checkout := filepath.Join(root, "main")
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, checkout, "init", "--quiet")
	runGit(t, checkout, "commit", "--quiet", "--allow-empty", "-m", "fixture")
	worktrees := []string{filepath.Join(root, "first"), filepath.Join(root, "second")}
	for _, worktree := range worktrees {
		runGit(t, checkout, "worktree", "add", "--quiet", "--detach", worktree)
		copyRepo := exec.Command("bash", "-euo", "pipefail", "-c", `tar -C "$1" --exclude=./.git --exclude=./.jj --exclude=./.beads --exclude=./_build --exclude=./build -cf - . | tar -C "$2" -xf -`, "bash", repoRoot(t), worktree)
		if output, err := copyRepo.CombinedOutput(); err != nil {
			t.Fatalf("copy script test repository: %v\n%s", err, output)
		}
		seedKernelLocks(t, worktree)
	}

	bin := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	stubMkosiGoBuilds(t, bin)
	writeFakeExecutable(t, bin, "podman", `if [[ "$1" == image ]]; then
  [[ "$2" != inspect ]] || echo fake-image
  exit 0
fi
cache="" build="" previous=""
for arg in "$@"; do
  if [[ "$previous" == --volume ]]; then
    case "$arg" in
      *:/mkosi-cache) cache="${arg%:/mkosi-cache}" ;;
      *:/mkosi-build) build="${arg%:/mkosi-build}" ;;
    esac
  fi
  previous="$arg"
done
tree="$cache/fake.cache"
rm -rf "$tree"
mkdir -p "$tree"
for package in $KATL_FAKE_PACKAGES; do
  printf '%s\n' "$package" >"$tree/$package"
  sleep 0.2
done
rm -rf "$build/katl-runtime-root"
cp -a "$tree" "$build/katl-runtime-root"
`)
	packageSets := [][]string{{}, {}}
	for i := 1; i <= 10; i++ {
		packageSets[0] = append(packageSets[0], fmt.Sprintf("alpha-%d", i))
		packageSets[1] = append(packageSets[1], fmt.Sprintf("beta-%d", i))
	}

	errs := make([]error, len(worktrees))
	outputs := make([][]byte, len(worktrees))
	var builds sync.WaitGroup
	for i, worktree := range worktrees {
		env := gittest.Env(os.Environ(), worktree)
		env = append(env, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "KATL_CONTAINER_RUNTIME=podman", "KATL_MKOSI_BUILD_DIR=", "KATL_MKOSI_CACHE_DIR=", "KATL_MKOSI_LOCK_FD=", "KATL_FAKE_PACKAGES="+strings.Join(packageSets[i], " "), "TMPDIR="+tmp)
		env = append(env, activeGoCacheEnv(t)...)
		cmd := exec.Command(filepath.Join(worktree, "scripts", "mkosi"), "runtime")
		cmd.Dir, cmd.Env = worktree, env
		builds.Add(1)
		go func() {
			defer builds.Done()
			outputs[i], errs[i] = runInGroup(t, cmd, scriptTimeout)
		}()
	}
	builds.Wait()

	for i, worktree := range worktrees {
		if errs[i] != nil {
			t.Fatalf("scripts/mkosi runtime in %s failed: %v\n%s", worktree, errs[i], outputs[i])
		}
		entries, err := os.ReadDir(filepath.Join(worktree, "_build", "mkosi", "katl-runtime-root"))
		if err != nil {
			t.Fatalf("read %s image root: %v", worktree, err)
		}
		var got []string
		for _, entry := range entries {
			got = append(got, entry.Name())
		}
		want := append([]string(nil), packageSets[i]...)
		sort.Strings(want)
		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Fatalf("%s image root holds %v, want its own package set %v", worktree, got, want)
		}
	}
}

func TestMkosiBuildRequiresEveryArtifact(t *testing.T) {
	repo := scriptRepoFixture(t)
	for _, missing := range []string{"katl-runtime-root.vmlinuz", "katl-runtime-root"} {
		t.Run(missing, func(t *testing.T) {
			tmp := t.TempDir()
			buildDir, bin := filepath.Join(tmp, "build"), filepath.Join(tmp, "bin")
			if err := os.MkdirAll(bin, 0o755); err != nil {
				t.Fatal(err)
			}
			stubMkosiGoBuilds(t, bin)
			// The builder reports success without restoring the artifact.
			writeFakeExecutable(t, bin, "podman", `if [[ "$1" == image ]]; then
  [[ "$2" != inspect ]] || echo fake-image
fi
exit 0
`)
			seedRuntimeCacheOutputs(t, buildDir)
			if err := os.RemoveAll(filepath.Join(buildDir, missing)); err != nil {
				t.Fatal(err)
			}
			env := append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "KATL_CONTAINER_RUNTIME=podman", "KATL_MKOSI_BUILD_DIR="+buildDir, "KATL_MKOSI_LOCK_FD=", "KATL_BUILD_COMMIT=artifact-test", "KATL_VERSION=0.0.0-artifact-test", "TMPDIR="+tmp)
			env = append(env, activeGoCacheEnv(t)...)

			for attempt := 1; attempt <= 2; attempt++ {
				cmd := exec.Command(filepath.Join(repo, "scripts", "mkosi"), "build-runtime")
				cmd.Dir, cmd.Env = repo, env
				output, err := runInGroup(t, cmd, scriptTimeout)
				if err == nil {
					t.Fatalf("attempt %d: build-runtime passed without %s:\n%s", attempt, missing, output)
				}
				if want := "build finished without expected artifact: " + filepath.Join(buildDir, missing); !strings.Contains(string(output), want) {
					t.Fatalf("attempt %d: error does not name the missing artifact %q:\n%s", attempt, want, output)
				}
			}
		})
	}
}

// The install image build runs the runtime and sysext builds through nested
// scripts/mkosi invocations, which must reuse the lock their parent holds.
func TestMkosiNestedBuildsReuseLock(t *testing.T) {
	repo, tmp := scriptRepoFixture(t), t.TempDir()
	buildDir, bin := filepath.Join(tmp, "build"), filepath.Join(tmp, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	stubMkosiGoBuilds(t, bin)
	writeFakeExecutable(t, bin, "podman", `if [[ "$1" == image ]]; then
  [[ "$2" != inspect ]] || echo fake-image
fi
exit 0
`)
	writeFakeExecutable(t, filepath.Join(repo, "scripts"), "build-endpoint-advertiser-sysext", `exec "$(dirname "$0")/mkosi" --profile endpoint-advertiser-sysext -f build
`)
	writeFakeExecutable(t, filepath.Join(repo, "scripts"), "build-katlos-install-image", "exit 0\n")
	seedRuntimeCacheOutputs(t, buildDir)
	writeReleaseArtifact(t, buildDir, "katlos-install-0.0.0-dev-x86_64.squashfs")
	writeReleaseArtifact(t, buildDir, "katl-endpoint-advertiser.raw")
	env := append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "KATL_CONTAINER_RUNTIME=podman", "KATL_MKOSI_BUILD_DIR="+buildDir, "KATL_MKOSI_LOCK_FD=", "KATL_VERSION=0.0.0-dev", "KATL_ARCHITECTURE=x86_64", "TMPDIR="+tmp)
	env = append(env, activeGoCacheEnv(t)...)

	cmd := exec.Command(filepath.Join(repo, "scripts", "mkosi"), "build-katlos-install-image")
	cmd.Dir, cmd.Env = repo, env
	output, err := runInGroup(t, cmd, 2*time.Minute)
	if errors.Is(err, errScriptTimeout) {
		t.Fatalf("nested scripts/mkosi waited on its parent's lock:\n%s", output)
	}
	if err != nil {
		t.Fatalf("build-katlos-install-image failed: %v\n%s", err, output)
	}
	if strings.Contains(string(output), "mkosi build lock: waiting") {
		t.Fatalf("nested scripts/mkosi waited for the lock:\n%s", output)
	}
}

const scriptTimeout = 10 * time.Minute

var errScriptTimeout = errors.New("script timed out")

// runInGroup runs cmd in its own process group and returns its combined
// output. The whole group is killed on timeout and when the test ends, so no
// descendant of cmd, such as a flock waiter, outlives the test.
func runInGroup(t *testing.T, cmd *exec.Cmd, timeout time.Duration) ([]byte, error) {
	t.Helper()
	return runInGroupContext(t, t.Context(), cmd, timeout, io.Discard)
}

func runInGroupContext(t *testing.T, ctx context.Context, cmd *exec.Cmd, timeout time.Duration, output io.Writer) ([]byte, error) {
	t.Helper()
	var out bytes.Buffer
	combined := io.MultiWriter(&out, output)
	cmd.Stdout, cmd.Stderr = combined, combined
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	pgid := cmd.Process.Pid
	killGroup := func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) }
	t.Cleanup(killGroup)

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return out.Bytes(), err
	case <-ctx.Done():
		killGroup()
		<-done
		return out.Bytes(), errScriptTimeout
	}
}

// The builder must not receive host devices: --privileged and --device make
// the runtime enumerate /dev at create time, and a device that disappears
// before start fails the build.
func TestMkosiBuilderGetsNoHostDevices(t *testing.T) {
	for _, runtime := range []string{"podman", "docker"} {
		t.Run(runtime, func(t *testing.T) {
			repo, tmp := scriptRepoFixture(t), t.TempDir()
			bin := filepath.Join(tmp, "bin")
			if err := os.MkdirAll(bin, 0o755); err != nil {
				t.Fatal(err)
			}
			args := filepath.Join(tmp, "args")
			writeFakeExecutable(t, bin, runtime, `if [[ "$1" == image ]]; then
  exit 0
fi
printf '%s\n' "$@" > "$KATL_FAKE_RUNTIME_ARGS"
`)
			cmd := exec.Command(filepath.Join(repo, "scripts/mkosi"), "--version")
			cmd.Dir = repo
			cmd.Env = append(os.Environ(),
				"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
				"KATL_CONTAINER_RUNTIME="+runtime,
				"KATL_MKOSI_BUILD_DIR="+filepath.Join(tmp, "build"),
				"KATL_FAKE_RUNTIME_ARGS="+args,
			)
			cmd.Env = append(cmd.Env, activeGoCacheEnv(t)...)
			if out, err := runInGroup(t, cmd, scriptTimeout); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}

			got := readLinesForScripts(t, args)
			for _, arg := range got {
				if arg == "--privileged" || arg == "--device" || strings.HasPrefix(arg, "--device=") {
					t.Fatalf("builder run passes host devices via %s: %v", arg, got)
				}
			}
			if !containsString(got, "SYS_ADMIN") {
				t.Fatalf("builder run lacks CAP_SYS_ADMIN needed by mkosi-sandbox: %v", got)
			}
		})
	}
}
