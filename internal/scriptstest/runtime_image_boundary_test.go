package scriptstest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRuntimeBuildExcludesVMTestSupportByDefault(t *testing.T) {
	repo := repoRoot(t)
	bin := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatalf("mkdir fake bin: %v", err)
	}
	writeFakeExecutable(t, bin, "go", `
output=""
while [[ $# -gt 0 ]]; do
  if [[ "$1" == "-o" ]]; then
    output="$2"
    break
  fi
  shift
done
[[ -n "$output" ]] || exit 2
mkdir -p "$(dirname "$output")"
printf 'fake binary\n' > "$output"
`)

	production := filepath.Join(t.TempDir(), "production")
	runRuntimeBuild(t, repo, bin, production, "0")
	for _, path := range vmtestRuntimePaths(production) {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("production runtime contains VM-test path %s: %v", path, err)
		}
	}

	instrumented := filepath.Join(t.TempDir(), "instrumented")
	runRuntimeBuild(t, repo, bin, instrumented, "1")
	for _, path := range vmtestRuntimePaths(instrumented) {
		if _, err := os.Lstat(path); err != nil {
			t.Errorf("instrumented runtime missing VM-test path %s: %v", path, err)
		}
	}
}

func TestVMTestImageSupportIsExplicitAndCacheScoped(t *testing.T) {
	repo := repoRoot(t)
	mkosi := string(mustReadFile(t, filepath.Join(repo, "scripts", "mkosi")))
	assertTextContains(t, mkosi,
		`vmtest_image_support="${KATL_VMTEST_IMAGE_SUPPORT:-0}"`,
		`--environment "KATL_VMTEST_IMAGE_SUPPORT=$vmtest_image_support"`,
		`printf 'KATL_VMTEST_IMAGE_SUPPORT=%s\n' "$vmtest_image_support"`,
		`if [[ "$vmtest_image_support" == 1 ]]`,
	)
	runner := string(mustReadFile(t, filepath.Join(repo, "scripts", "vmtest-run")))
	if !strings.Contains(runner, `KATL_VMTEST_IMAGE_SUPPORT=1 "$repo_root/scripts/mkosi" "$target"`) {
		t.Fatal("vmtest runner does not explicitly request instrumented image builds")
	}
	runtimeCheck := string(mustReadFile(t, filepath.Join(repo, "scripts", "check-runtime-root")))
	installerCheck := string(mustReadFile(t, filepath.Join(repo, "scripts", "check-installer-image")))
	assertTextContains(t, runtimeCheck, "runtime image contains VM-test support")
	assertTextContains(t, installerCheck, "installer image contains VM-test support")
	releaseWorkflow := string(mustReadFile(t, filepath.Join(repo, ".github", "workflows", "release-artifacts.yml")))
	assertTextContains(t, releaseWorkflow, `KATL_VMTEST_IMAGE_SUPPORT: "0"`)
}

func TestReleaseRootPackagingIsCompressedAndPruned(t *testing.T) {
	repo := repoRoot(t)
	mkosi := string(mustReadFile(t, filepath.Join(repo, "scripts", "mkosi")))
	assertTextContains(t, mkosi,
		`runtime_packages=_build/mkosi/katl-runtime.packages.tsv`,
		`"$root/usr/lib/sysimage/rpm"`,
		`find "$root/usr/lib/modules" -type f \( -name System.map -o -name vmlinuz \) -delete`,
		`rm -f "$root/usr/bin/ctr"`,
		`-b 1M`,
		`-Xcompression-level 22`,
	)
	installImage := string(mustReadFile(t, filepath.Join(repo, "scripts", "build-katlos-install-image")))
	assertTextContains(t, installImage, `-b 1M`, `-Xcompression-level 22`)
	runtimeBuild := string(mustReadFile(t, filepath.Join(repo, "mkosi.profiles", "runtime", "mkosi.build")))
	installerBuild := string(mustReadFile(t, filepath.Join(repo, "mkosi.profiles", "installer-image", "mkosi.build")))
	assertTextContains(t, runtimeBuild, `-ldflags="-s -w`)
	assertTextContains(t, installerBuild, `-ldflags="-s -w`)
}

func runRuntimeBuild(t *testing.T, repo, bin, dest, support string) {
	t.Helper()
	cmd := exec.Command(filepath.Join(repo, "mkosi.profiles", "runtime", "mkosi.build"))
	cmd.Dir = repo
	cmd.Env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"BUILDDIR="+t.TempDir(),
		"DESTDIR="+dest,
		"SRCDIR="+repo,
		"KATL_BUILD_COMMIT=test",
		"KATL_VERSION=0.0.0-test",
		"KATL_VMTEST_IMAGE_SUPPORT="+support,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runtime build support=%s failed: %v\n%s", support, err, output)
	}
}

func vmtestRuntimePaths(root string) []string {
	return []string{
		filepath.Join(root, "usr", "lib", "katl", "vmtest"),
		filepath.Join(root, "usr", "lib", "katl", "vmtest", "katl-vmtest-agent"),
		filepath.Join(root, "usr", "lib", "systemd", "system", "katl-vmtest-agent.service"),
		filepath.Join(root, "usr", "lib", "systemd", "system", "katl-vmtest-debug-shell.service"),
		filepath.Join(root, "usr", "lib", "systemd", "system", "multi-user.target.wants", "katl-vmtest-agent.service"),
		filepath.Join(root, "usr", "lib", "systemd", "system", "multi-user.target.wants", "katl-vmtest-debug-shell.service"),
	}
}
