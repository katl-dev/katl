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
	for _, tool := range []string{"dnf5", "ukify", "xargs"} {
		writeFakeExecutable(t, bin, tool, "exit 0\n")
	}

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
	for _, want := range []string{"--profile", "installer-image", "-f", "build", "--environment", "KATL_INSTALLER_PACKAGE_SET=_build/mkosi/katl-installer.packages.tsv"} {
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
	podmanArgs := filepath.Join(tmp, "podman-args.txt")
	writeFakeExecutable(t, bin, "podman", `if [[ "${1:-}" == "image" && "${2:-}" == "exists" ]]; then
  exit 0
fi
printf '%s\n' "$@" > "$KATL_FAKE_PODMAN_ARGS"
`)

	cmd := exec.Command(filepath.Join(repo, "scripts", "mkosi"), "build-installer", "--debug")
	cmd.Dir = repo
	cmd.Env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"KATL_CONTAINER_RUNTIME=podman",
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
}

func TestMkosiCacheInputsExcludeResourcePackageLock(t *testing.T) {
	repo := repoRoot(t)
	data := mustReadFile(t, filepath.Join(repo, "scripts", "mkosi"))
	if strings.Contains(string(data), "resource-package-lock.json") {
		t.Fatalf("scripts/mkosi cache inputs include generated resource package lock")
	}
}

func TestMkosiCacheInputsIncludeBuildCommit(t *testing.T) {
	repo := repoRoot(t)
	data := mustReadFile(t, filepath.Join(repo, "scripts", "mkosi"))
	if !strings.Contains(string(data), "KATL_BUILD_COMMIT=%s") || !strings.Contains(string(data), "$build_commit") {
		t.Fatalf("scripts/mkosi cache inputs do not include embedded build commit")
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
