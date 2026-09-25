package scriptstest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMkosiDirectRejectsRuntimePackaging(t *testing.T) {
	repo := repoRoot(t)
	cmd := exec.Command(filepath.Join(repo, "scripts", "mkosi"), "build-runtime")
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "KATL_CONTAINER_RUNTIME=direct")
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("scripts/mkosi direct build-runtime unexpectedly passed:\n%s", output)
	}
	if !strings.Contains(string(output), "direct currently supports installer-image builds only") {
		t.Fatalf("output missing direct-mode rejection:\n%s", output)
	}
}

func TestMkosiRuntimeCacheUsesIncludedBinaryIdentity(t *testing.T) {
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

	first := exec.Command(filepath.Join(repo, "scripts", "mkosi"), "build-runtime")
	first.Dir = repo
	first.Env = env
	output, err := first.CombinedOutput()
	if err != nil {
		t.Fatalf("initial scripts/mkosi build-runtime failed: %v\n%s", err, output)
	}
	if strings.Contains(string(output), "mkosi cache hit: runtime") {
		t.Fatalf("initial build unexpectedly hit cache:\n%s", output)
	}
	if got := readLinesForScripts(t, podmanArgs); len(got) == 0 {
		t.Fatalf("initial build did not invoke fake podman")
	}

	unrelatedSource := filepath.Join(repo, "internal", "vmtest", "testcmd", "net-client", "cache_identity_probe.go")
	writeTemporaryFile(t, unrelatedSource, "package main\n\nconst cacheIdentityProbe = \"unrelated\"\n")
	if err := os.Remove(podmanArgs); err != nil {
		t.Fatalf("remove podman args: %v", err)
	}
	second := exec.Command(filepath.Join(repo, "scripts", "mkosi"), "build-runtime")
	second.Dir = repo
	second.Env = env
	output, err = second.CombinedOutput()
	if err != nil {
		t.Fatalf("cached scripts/mkosi build-runtime failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "mkosi cache hit: runtime artifacts match the current repo") {
		t.Fatalf("unrelated Go source edit did not hit cache:\n%s", output)
	}
	if _, err := os.Stat(podmanArgs); !os.IsNotExist(err) {
		t.Fatalf("fake podman ran for unrelated Go source edit: %v", err)
	}

	for _, artifact := range []string{"katl-runtime.efi", "katl-kernel-inputs.tar"} {
		if err := os.WriteFile(filepath.Join(buildDir, artifact), []byte("corrupt"), 0o644); err != nil {
			t.Fatalf("corrupt runtime artifact: %v", err)
		}
		corrupt := exec.Command(filepath.Join(repo, "scripts", "mkosi"), "build-runtime")
		corrupt.Dir = repo
		corrupt.Env = env
		output, err = corrupt.CombinedOutput()
		if err != nil {
			t.Fatalf("scripts/mkosi with corrupt cached artifact failed: %v\n%s", err, output)
		}
		if strings.Contains(string(output), "mkosi cache hit: runtime") {
			t.Fatalf("corrupt cached artifact unexpectedly hit cache:\n%s", output)
		}
		if got := readLinesForScripts(t, podmanArgs); len(got) == 0 {
			t.Fatal("corrupt cached artifact did not invoke fake podman")
		}
		seedRuntimeCacheOutputs(t, buildDir)
	}

	includedSource := filepath.Join(repo, "cmd", "katl-runtime-status", "cache_identity_probe.go")
	writeTemporaryFile(t, includedSource, "package main\n\nvar cacheIdentityProbeRuntimeStatus = \"changed\"\n")
	third := exec.Command(filepath.Join(repo, "scripts", "mkosi"), "build-runtime")
	third.Dir = repo
	third.Env = env
	output, err = third.CombinedOutput()
	if err != nil {
		t.Fatalf("changed-binary scripts/mkosi build-runtime failed: %v\n%s", err, output)
	}
	if strings.Contains(string(output), "mkosi cache hit: runtime") {
		t.Fatalf("included binary edit unexpectedly hit cache:\n%s", output)
	}
	if got := readLinesForScripts(t, podmanArgs); len(got) == 0 {
		t.Fatalf("changed included binary did not invoke fake podman")
	}
}

func TestMkosiRuntimeCacheMissesWhenBinaryIdentityUnavailable(t *testing.T) {
	repo := repoRoot(t)
	tmp := t.TempDir()
	buildDir := filepath.Join(tmp, "mkosi-build")
	bin := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(filepath.Join(buildDir, ".katl-stamps"), 0o755); err != nil {
		t.Fatalf("MkdirAll(stamps) error = %v", err)
	}
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
	writeFakeExecutable(t, bin, "go", `exit 42
`)
	seedRuntimeCacheOutputs(t, buildDir)
	stamp := filepath.Join(buildDir, ".katl-stamps", "runtime.sha256")
	if err := os.WriteFile(stamp, []byte(strings.Repeat("a", 64)+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", stamp, err)
	}
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

	cmd := exec.Command(filepath.Join(repo, "scripts", "mkosi"), "build-runtime")
	cmd.Dir = repo
	cmd.Env = env
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("scripts/mkosi build-runtime with failing identity probe failed: %v\n%s", err, output)
	}
	if strings.Contains(string(output), "mkosi cache hit: runtime") {
		t.Fatalf("binary identity failure still hit cache:\n%s", output)
	}
	if got := readLinesForScripts(t, podmanArgs); len(got) == 0 {
		t.Fatalf("binary identity failure did not invoke fake podman")
	}
	stampData := strings.TrimSpace(string(mustReadFile(t, stamp)))
	if stampData != strings.Repeat("a", 64) {
		t.Fatalf("binary identity failure overwrote cache stamp with %q", stampData)
	}
}

func TestMkosiImageCacheWithExternalExtensions(t *testing.T) {
	repo := repoRoot(t)
	tmp := t.TempDir()
	fixture := filepath.Join(tmp, "repo")
	bin := filepath.Join(tmp, "bin")
	scripts := filepath.Join(fixture, "scripts")
	buildDir := filepath.Join(fixture, "_build", "mkosi")
	for _, dir := range []string{bin, scripts} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	seedRuntimeCacheOutputs(t, buildDir)
	if err := os.WriteFile(filepath.Join(fixture, "mkosi.conf"), []byte("[Distribution]\nRelease=99\n"), 0o644); err != nil {
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
	writeFakeExecutable(t, bin, "go", `while [[ $# -gt 0 ]]; do
  if [[ "$1" == -o ]]; then printf 'binary\n' > "$2"; exit 0; fi
  shift
done
exit 1
`)
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
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build: %v\n%s", err, output)
		}
		if got := len(readLinesForScripts(t, log)); got != step.builds {
			t.Fatalf("selection %q: assembly count = %d, want %d", step.selection, got, step.builds)
		}
	}
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
}

func scriptRepoFixture(t *testing.T) string {
	t.Helper()
	repo, fixture := repoRoot(t), t.TempDir()
	cmd := exec.Command("bash", "-euo", "pipefail", "-c", `tar -C "$1" --exclude=./.git --exclude=./.jj --exclude=./.beads --exclude=./_build --exclude=./build -cf - . | tar -C "$2" -xf -`, "bash", repo, fixture)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("copy script test repository: %v\n%s", err, output)
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
		cmd := exec.Command(filepath.Join(repo, "scripts/mkosi"), "build-runtime")
		cmd.Dir, cmd.Env = repo, append(append([]string(nil), env...), "KATL_FLAVOUR="+step.flavour)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", step.flavour, err, out)
		}
		if got := len(readLinesForScripts(t, calls)); got != step.builds {
			t.Fatalf("%s: builder runs=%d, want %d", step.flavour, got, step.builds)
		}
	}
	writeTemporaryFile(t, filepath.Join(repo, "mkosi.conf.d", "cache-probe.conf"), "[Build]\nEnvironment=CACHE_PROBE=changed\n")
	cmd := exec.Command(filepath.Join(repo, "scripts", "mkosi"), "build-runtime")
	cmd.Dir, cmd.Env = repo, append(append([]string(nil), env...), "KATL_FLAVOUR=standard")
	if out, err := cmd.CombinedOutput(); err != nil {
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
