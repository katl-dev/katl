package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/katl-dev/katl/internal/vmtest"
)

// fakeGateGo stands in for go test under the real scripts/vmtest-run. Both
// flavours write sentinels into the directories their world binds, wait for
// each other, and check that the sentinels survived, so flavours that share a
// build or fixture cache directory fail. They meet in the gate's root, which
// holds both fixture caches and lies in the worktree the sandbox binds. Each run also leaves a malformed
// published fixture in its build directory, which a fixture cache walk that
// enters the build directory reports as an error.
const fakeGateGo = `#!/usr/bin/env bash
set -euo pipefail
package="" pattern=""
while [[ "$#" -gt 0 ]]; do
    case "$1" in
        -run) pattern="$2"; shift ;;
        ./*) package="$1" ;;
    esac
    shift
done
build="$(jq -er .mkosiBuildDir "$KATL_VMTEST_WORLD_MANIFEST")"
cache="$(jq -er .cacheDir "$KATL_VMTEST_WORLD_MANIFEST")"
sync="$(dirname "$(dirname "$cache")")/sync"
mkdir -p "$build/decoy" "$cache" "$sync"
printf '{' >"$build/decoy/published-first-install-runtime-fixture.json"
printf '%s\n' "$KATL_FLAVOUR" >"$build/owner"
printf '%s\n' "$KATL_FLAVOUR" >"$cache/owner"
touch "$sync/$KATL_FLAVOUR.$pattern"
for ((attempt=0; attempt<400; attempt++)); do
    if [[ -f "$sync/standard.$pattern" && -f "$sync/lts.$pattern" ]]; then break; fi
    sleep 0.05
done
[[ "$(<"$build/owner")" == "$KATL_FLAVOUR" && "$(<"$cache/owner")" == "$KATL_FLAVOUR" ]]
printf '%s\t%s\n' "$package" "$pattern" >>"$build/groups"
tests="${pattern#^}"
tests="${tests#(}"
tests="${tests%$}"
tests="${tests%)}"
status=0
IFS='|' read -ra names <<<"$tests"
for name in "${names[@]}"; do
    if [[ "$name" == TestExternalWorkerJoin && "$KATL_FLAVOUR" == lts ]]; then
        printf -- '--- FAIL: %s (0.00s)\n' "$name"
        status=1
    else
        printf -- '--- PASS: %s (0.00s)\n' "$name"
    fi
done
exit "$status"
`

const fakeGateVirsh = `#!/usr/bin/env bash
[[ "$1" == -c ]] || exit 2
shift 2
case "$1" in
    uri) printf 'qemu:///system\n' ;;
    net-info) printf 'Name: default\nActive: yes\n' ;;
    net-dumpxml) printf "<network><ip address='10.77.0.1' prefix='24'/></network>\n" ;;
    pool-info) printf 'Name: default\nState: running\n' ;;
    pool-dumpxml) printf '<pool><target><path>%s</path></target></pool>\n' "$TMPDIR" ;;
    *) exit 2 ;;
esac
`

// gateHarness returns the real scripts/vmtest-run with fake host tools, so the
// gate summary is the runner's own evidence.
func gateHarness(t *testing.T) string {
	t.Helper()
	katl, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, script := range map[string]string{
		"go":       fakeGateGo,
		"virsh":    fakeGateVirsh,
		"qemu-img": "#!/usr/bin/env bash\n",
		"kubectl":  "#!/usr/bin/env bash\n",
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("TMPDIR", tmp)
	t.Setenv("KATL_VMTEST_GO", filepath.Join(bin, "go"))
	t.Setenv("KATL_VMTEST_VIRSH", filepath.Join(bin, "virsh"))
	t.Setenv("KATL_VMTEST_IMAGE_TOOL", filepath.Join(bin, "qemu-img"))
	t.Setenv("KATL_VMTEST_KUBECTL", filepath.Join(bin, "kubectl"))
	t.Setenv("KATL_VMTEST_ARTIFACT_SET", "none")
	t.Setenv("KATL_VMTEST_KEEP", "always")
	t.Setenv("KATL_VMTEST_REQUIRE_SCENARIO_RESULT", "0")
	for _, name := range []string{"KATL_REPO_ROOT", "KATL_FLAVOUR", "KATL_MKOSI_LOCK_FD", "KATL_VMTEST_WORLD_MANIFEST", "CI"} {
		t.Setenv(name, "")
	}
	return filepath.Join(katl, "scripts", "vmtest-run")
}

func TestAffectedGate(t *testing.T) {
	repo := t.TempDir()
	runner := gateHarness(t)
	t.Chdir(repo)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GOWORK", "off")
	gitIn(t, repo, "init", "--quiet")
	put(t, repo, ".gitignore", "/_build/\n")
	put(t, repo, "go.mod", "module example.com/gate\n\ngo 1.26.0\n")
	for _, target := range append([]string{"./internal/vmtest", "./internal/vmtest/scenarios"}, hostedGoTargets...) {
		dir := strings.TrimSuffix(target, "/...")
		put(t, repo, filepath.Join(dir, "inputs.go"), "package fixture\n")
	}
	for _, fixture := range []string{configFixture, workloadFixture} {
		put(t, repo, fixture, "base\n")
	}
	gitIn(t, repo, "add", ".")
	gitIn(t, repo, "commit", "-qm", "base")
	gitIn(t, repo, "update-ref", "refs/remotes/origin/main", "HEAD")
	for _, fixture := range []string{configFixture, workloadFixture} {
		put(t, repo, fixture, "head\n")
	}
	gitIn(t, repo, "commit", "-qam", "change scenario fixtures")

	var summary bytes.Buffer
	err := runGate([]string{"--runner", runner, "--id", "fixture", "--affected"}, &summary, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "fixture-lts-cluster") || strings.Contains(err.Error(), "fixture-standard") {
		t.Fatalf("gate result = %v, want only the LTS cluster failure\n%s", err, &summary)
	}
	const wantGroups = "./internal/vmtest/scenarios\t^(TestWorkerReaddress)$\n" +
		"./internal/vmtest/scenarios\t^(TestInstalledRuntimeTwoNodeOperationBackedBootstrapSmoke)$\n" +
		"./internal/vmtest/scenarios\t^(TestExternalWorkerJoin)$\n" +
		"./internal/vmtest\t^(TestInstalledRuntimeRequiredLinkHealth|TestInstalledRuntimeConfigApplyModesSmoke|TestInstalledRuntimeServiceSecuritySmoke|TestInstalledRuntimeCarrierRecovery|TestInstalledRuntimeLinkRecovery)$\n"
	for _, kernel := range []string{"standard", "lts"} {
		build, _, err := gateDirs(kernel)
		if err != nil {
			t.Fatal(err)
		}
		output, err := os.ReadFile(filepath.Join(build, "groups"))
		if err != nil || string(output) != wantGroups {
			t.Errorf("%s ran %q, want %q (read error %v)", kernel, output, wantGroups, err)
		}
	}
	standard, lts, found := strings.Cut(summary.String(), "\nlts gate ")
	if !found || !strings.Contains(standard, "\nstandard gate ") {
		t.Fatalf("summary lacks flavour run IDs:\n%s", &summary)
	}
	for _, check := range []struct{ section, result string }{
		{standard, "--- PASS: TestExternalWorkerJoin (0.00s)"},
		{lts, "--- FAIL: TestExternalWorkerJoin (0.00s)"},
		{standard, "--- PASS: TestInstalledRuntimeCarrierRecovery (0.00s)"},
		{lts, "--- PASS: TestInstalledRuntimeCarrierRecovery (0.00s)"},
	} {
		if !strings.Contains(check.section, check.result) {
			t.Errorf("summary lacks %s in:\n%s", check.result, check.section)
		}
	}

	gitIn(t, repo, "update-ref", "refs/remotes/origin/main", "HEAD")
	summary.Reset()
	if err := runGate([]string{"--runner", "must-not-run", "--id", "empty", "--affected"}, &summary, io.Discard); err != nil || summary.String() != "vmtest gate: no affected groups\n" {
		t.Fatalf("empty committed diff: %v\n%s", err, &summary)
	}

	// The runner would test this edit, which the committed plan cannot see.
	put(t, repo, configFixture, "uncommitted\n")
	summary.Reset()
	err = runGate([]string{"--runner", "must-not-run", "--id", "dirty", "--affected"}, &summary, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "git stash -u") || summary.Len() != 0 {
		t.Fatalf("dirty tree: %v\n%s", err, &summary)
	}
}

func TestFocusedGate(t *testing.T) {
	repo := t.TempDir()
	runner := gateHarness(t)
	t.Chdir(repo)
	// The runner's go test sandbox runs inside a Git worktree.
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	gitIn(t, repo, "init", "--quiet")

	var summary bytes.Buffer
	args := []string{"--runner", runner, "--id", "fixture", "--flavour", "standard,lts", "--", "./internal/vmtest", "-run", "^TestExample$", "-count=1"}
	if err := runGate(args, &summary, io.Discard); err != nil {
		t.Fatalf("gate: %v\n%s", err, &summary)
	}
	for _, kernel := range []string{"standard", "lts"} {
		if !strings.Contains(summary.String(), fmt.Sprintf("\n%s gate fixture-%s\n", kernel, kernel)) {
			t.Errorf("summary missing %s run ID:\n%s", kernel, &summary)
		}
	}
	if strings.Count(summary.String(), "--- PASS: TestExample (0.00s)") != 2 {
		t.Fatalf("summary missing both passes:\n%s", &summary)
	}

	// Each run's own world records the directories it used. The default
	// fixture cache and every flavour's cache are walked for published
	// fixtures; none of the walks may reach a build directory.
	roots := []string{vmtest.DefaultVMTestCacheDir(repo)}
	var builds []string
	for _, kernel := range []string{"standard", "lts"} {
		world, err := vmtest.LoadWorld(filepath.Join(os.Getenv("TMPDIR"), "katl-vmtest", "fixture-"+kernel, "world.json"))
		if err != nil {
			t.Fatal(err)
		}
		roots = append(roots, world.CacheDir)
		builds = append(builds, world.MkosiBuildDir)
	}
	if builds[0] == builds[1] || roots[1] == roots[2] {
		t.Fatalf("flavours share directories: builds %q, caches %q", builds, roots[1:])
	}
	for _, build := range builds {
		if _, err := vmtest.FindPublishedFirstInstallRuntimeFixtureInBuildRoots([]string{build}, vmtest.NodeSpec{}); err == nil || strings.Contains(err.Error(), "run the first-install fixture contract") {
			t.Fatalf("walking %s = %v, want the decoy's decode error", build, err)
		}
	}
	for _, root := range roots {
		_, err := vmtest.FindPublishedFirstInstallRuntimeFixtureInBuildRoots([]string{root}, vmtest.NodeSpec{})
		if err == nil || !strings.Contains(err.Error(), "run the first-install fixture contract") {
			t.Errorf("walking fixture cache %s = %v, want no fixture found", root, err)
		}
	}
}

func TestGateHoldsBuildLock(t *testing.T) {
	repo := t.TempDir()
	runner := gateHarness(t)
	t.Chdir(repo)
	build, _, err := gateDirs("lts")
	if err != nil {
		t.Fatal(err)
	}
	// A plain run or katldev build of the same directory holds scripts/mkosi's
	// lock while it uses the outputs.
	if err := os.MkdirAll(build, 0o755); err != nil {
		t.Fatal(err)
	}
	mkosi := exec.Command(filepath.Join(filepath.Dir(runner), "mkosi"), "lock-path")
	mkosi.Env = append(os.Environ(), "KATL_FLAVOUR=lts", "KATL_MKOSI_BUILD_DIR="+build)
	lockPath, err := mkosi.Output()
	if err != nil {
		t.Fatalf("scripts/mkosi lock-path: %v", err)
	}
	holder, err := os.OpenFile(strings.TrimSpace(string(lockPath)), os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}

	var summary bytes.Buffer
	err = runGate([]string{"--runner", runner, "--id", "busy", "--flavour", "lts", "--", "./internal/vmtest", "-run", "^TestExample$"}, &summary, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "wait for it to finish") {
		t.Fatalf("gate with a held build lock = %v\n%s", err, &summary)
	}
}
