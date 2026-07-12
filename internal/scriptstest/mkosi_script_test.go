package scriptstest

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestMkosiDirectInstallerUsesDevShellTools(t *testing.T) {
	repo := repoRoot(t)
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", bin, err)
	}
	preserveFile(t, filepath.Join(repo, "_build", "mkosi", "katl-installer.packages.tsv"))
	mkosiArgs := filepath.Join(tmp, "mkosi-args.txt")
	mkosiEnv := filepath.Join(tmp, "mkosi-env.txt")
	goArgs := filepath.Join(tmp, "go-args.txt")
	goEnv := filepath.Join(tmp, "go-env.txt")
	writeFakeExecutable(t, bin, "mkosi", `printf '%s\n' "$@" > "$KATL_FAKE_MKOSI_ARGS"
{
  printf 'MKOSI_DNF=%s\n' "${MKOSI_DNF:-}"
  printf 'TMPDIR=%s\n' "${TMPDIR:-}"
  printf 'GOMODCACHE=%s\n' "${GOMODCACHE:-}"
} > "$KATL_FAKE_MKOSI_ENV"
`)
	writeFakeExecutable(t, bin, "go", `printf '%s\n' "$@" > "$KATL_FAKE_GO_ARGS"
{
  printf 'GOCACHE=%s\n' "${GOCACHE:-}"
  printf 'GOMODCACHE=%s\n' "${GOMODCACHE:-}"
} > "$KATL_FAKE_GO_ENV"
`)
	writeFakeExecutable(t, bin, "rpm", "printf 'systemd\\t0:259.6-1.fc44.x86_64\\n'\n")
	for _, tool := range []string{"dnf5", "ukify", "xargs"} {
		writeFakeExecutable(t, bin, tool, "exit 0\n")
	}
	for _, name := range []string{"katl-installer.iso", "katl-installer.iso.json", "katl-installer.iso.sha256"} {
		preserveFile(t, filepath.Join(repo, "_build", "mkosi", name))
	}
	seedInstallerRPMCache(t, repo)

	cmd := exec.Command(filepath.Join(repo, "scripts", "mkosi"), "build-installer")
	cmd.Dir = repo
	cmd.Env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"KATL_CONTAINER_RUNTIME=direct",
		"KATL_FAKE_MKOSI_ARGS="+mkosiArgs,
		"KATL_FAKE_MKOSI_ENV="+mkosiEnv,
		"KATL_FAKE_GO_ARGS="+goArgs,
		"KATL_FAKE_GO_ENV="+goEnv,
		"GOCACHE="+filepath.Join(tmp, "go-cache"),
		"TMPDIR="+tmp,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("scripts/mkosi direct failed: %v\n%s", err, output)
	}

	args := readLinesForScripts(t, mkosiArgs)
	if len(args) < 2 || args[0] != "--extra-search-path" {
		t.Fatalf("mkosi args missing extra search path: %#v", args)
	}
	if !strings.Contains(args[1], bin) {
		t.Fatalf("extra search path %q does not include fake tool dir %q", args[1], bin)
	}
	installerPackageSet := "KATL_INSTALLER_PACKAGE_SET=" + filepath.Join(repo, "_build", "mkosi", "katl-installer.packages.tsv")
	for _, want := range []string{"--profile", "installer-image", "-f", "build", "--environment", installerPackageSet} {
		if !containsString(args, want) {
			t.Fatalf("mkosi args missing %q: %#v", want, args)
		}
	}
	env := readKeyValuesForScripts(t, mkosiEnv)
	if env["MKOSI_DNF"] != "dnf5" || env["TMPDIR"] != tmp || env["GOMODCACHE"] != filepath.Join(repo, "_build", "go-mod") {
		t.Fatalf("mkosi env = %#v", env)
	}
	assertDirsExist(t, repo,
		"_build/go-cache",
		"_build/go-mod",
		"_build/mkosi/builddir",
		"_build/mkosi/cache",
		"_build/mkosi/package-cache",
		"_build/mkosi/workspace",
		"_build/mkosi/workspace/installer",
		"_build/mkosi/workspace/runtime",
	)
	if got := readLinesForScripts(t, goArgs); !reflect.DeepEqual(got, []string{"run", "./cmd/katl-mkosi-artifacts", "write"}) {
		t.Fatalf("go args = %#v", got)
	}
	if got := strings.TrimSpace(string(mustReadFile(t, filepath.Join(repo, "_build", "mkosi", "katl-installer.packages.tsv")))); got != "systemd\t0:259.6-1.fc44.x86_64" {
		t.Fatalf("installer package set = %q", got)
	}
}

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

func TestMkosiPodmanSkipsRecursiveBuildChown(t *testing.T) {
	repo := repoRoot(t)
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", bin, err)
	}
	preserveFile(t, filepath.Join(repo, "_build", "mkosi", "katl-installer.packages.tsv"))
	podmanArgs := filepath.Join(tmp, "podman-args.txt")
	writeFakeExecutable(t, bin, "podman", `if [[ "${1:-}" == "image" && "${2:-}" == "exists" ]]; then
  exit 0
fi
printf '%s\n' "$@" > "$KATL_FAKE_PODMAN_ARGS"
`)
	writeFakeExecutable(t, bin, "rpm", "printf 'systemd\\t0:259.6-1.fc44.x86_64\\n'\n")
	seedInstallerRPMCache(t, repo)

	cmd := exec.Command(filepath.Join(repo, "scripts", "mkosi"), "build-installer", "--debug")
	cmd.Dir = repo
	cmd.Env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"KATL_CONTAINER_RUNTIME=podman",
		"KATL_VERSION=2026.7.0-dev.0",
		"KATL_FAKE_PODMAN_ARGS="+podmanArgs,
		"GOCACHE="+filepath.Join(tmp, "go-cache"),
		"TMPDIR="+tmp,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("scripts/mkosi podman failed: %v\n%s", err, output)
	}

	args := readLinesForScripts(t, podmanArgs)
	if !containsString(args, "--userns=keep-id") || !containsString(args, "--user") || !containsString(args, "root") {
		t.Fatalf("podman args missing keep-id root mode: %#v", args)
	}
	if !containsString(args, "KATL_CHOWN_BUILD=0") {
		t.Fatalf("podman args missing KATL_CHOWN_BUILD=0: %#v", args)
	}
	if !containsString(args, "KATL_VERSION=2026.7.0-dev.0") {
		t.Fatalf("podman args missing release version: %#v", args)
	}
	if !containsString(args, "KATL_INSTALLER_PACKAGE_SET=/work/_build/mkosi/katl-installer.packages.tsv") {
		t.Fatalf("podman args missing container installer package path: %#v", args)
	}
}

func TestMkosiCacheInputsExcludeResourcePackageLock(t *testing.T) {
	repo := repoRoot(t)
	data := mustReadFile(t, filepath.Join(repo, "scripts", "mkosi"))
	if strings.Contains(string(data), "resource-package-lock.json") {
		t.Fatalf("scripts/mkosi cache inputs include generated resource package lock")
	}
}

func TestMkosiDefaultBuildIdentityIsStable(t *testing.T) {
	repo := repoRoot(t)
	data := mustReadFile(t, filepath.Join(repo, "scripts", "mkosi"))
	if !strings.Contains(string(data), `build_commit="${KATL_BUILD_COMMIT:-${KATL_VERSION:-0.0.0-dev}}"`) {
		t.Fatalf("scripts/mkosi default build identity is not stable")
	}
	for _, want := range []string{
		`go_mod_cache="${KATL_GO_MOD_CACHE:-$repo_root/_build/go-mod}"`,
		`go_build_cache="${KATL_GO_BUILD_CACHE:-$repo_root/_build/go-cache}"`,
	} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("scripts/mkosi cache roots missing %q", want)
		}
	}
}

func TestKatlosImageCheckSelectsRequestedRole(t *testing.T) {
	repo := repoRoot(t)
	data := mustReadFile(t, filepath.Join(repo, "scripts", "check-katlos-install-image"))
	for _, want := range []string{
		`image_role="${KATL_KATLOS_IMAGE_ROLE:-install}"`,
		`katlos-${image_role}-${version}-${architecture}.squashfs`,
		`.imageRole == $role`,
	} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("check-katlos-install-image missing %q", want)
		}
	}
}

func TestMkosiRuntimeCacheUsesIncludedBinaryIdentity(t *testing.T) {
	repo := repoRoot(t)
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
	env := append(os.Environ(),
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

	if err := os.WriteFile(filepath.Join(buildDir, "katl-runtime.efi"), []byte("corrupt"), 0o644); err != nil {
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
	env := append(os.Environ(),
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

func writeFakeExecutable(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	content := "#!/usr/bin/env bash\nset -euo pipefail\n" + body
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
	return path
}

func seedInstallerRPMCache(t *testing.T, repo string) {
	t.Helper()
	path := filepath.Join(repo, "_build", "mkosi", "cache", "fedora~44~x86-64~main.cache", "usr", "lib", "sysimage", "rpm")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", path, err)
	}
}

func preserveFile(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	exists := err == nil
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("ReadFile(%s) error = %v", path, err)
	}
	t.Cleanup(func() {
		if exists {
			if err := os.WriteFile(path, data, 0o644); err != nil {
				t.Fatalf("restore %s: %v", path, err)
			}
			return
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			t.Fatalf("remove %s: %v", path, err)
		}
	})
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

func readKeyValuesForScripts(t *testing.T, path string) map[string]string {
	t.Helper()
	out := make(map[string]string)
	for _, line := range readLinesForScripts(t, path) {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("line %q is not key=value", line)
		}
		out[key] = value
	}
	return out
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func assertDirsExist(t *testing.T, root string, paths ...string) {
	t.Helper()
	for _, path := range paths {
		full := filepath.Join(root, path)
		info, err := os.Stat(full)
		if err != nil {
			t.Fatalf("Stat(%s) error = %v", full, err)
		}
		if !info.IsDir() {
			t.Fatalf("%s is not a directory", full)
		}
	}
}

func seedRuntimeCacheOutputs(t *testing.T, buildDir string) {
	t.Helper()
	paths := []string{
		filepath.Join(buildDir, "artifacts.json"),
		filepath.Join(buildDir, "katl-runtime-root"),
		filepath.Join(buildDir, "katl-runtime.packages.tsv"),
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
	for _, name := range []string{"katl-runtime-root.squashfs", "katl-runtime.efi"} {
		writeReleaseArtifact(t, buildDir, name)
	}
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
