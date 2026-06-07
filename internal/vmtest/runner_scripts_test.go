package vmtest

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestVMTestRunInjectsWorld(t *testing.T) {
	repo := scriptTestRepoRoot(t)
	tmp := t.TempDir()
	fakeGo, fakeChild := writeFakeGoTools(t, tmp)
	host := writeFakeHostTools(t, tmp, true)
	runDir := filepath.Join(tmp, "run")
	goArgsPath := filepath.Join(tmp, "go-args.txt")
	childArgsPath := filepath.Join(tmp, "child-args.txt")
	childEnvPath := filepath.Join(tmp, "child-env.txt")

	cmd := exec.Command(filepath.Join(repo, "scripts", "vmtest-run"),
		"./internal/vmtest/scenarios",
		"-run", "^TestTwoNode$",
		"-count=99",
		"-timeout", "2m",
	)
	cmd.Dir = repo
	cmd.Env = appendHostEnv(os.Environ(), host,
		"KATL_VMTEST_GO="+fakeGo,
		"KATL_FAKE_GO_ARGS="+goArgsPath,
		"KATL_FAKE_CHILD="+fakeChild,
		"KATL_FAKE_CHILD_ARGS="+childArgsPath,
		"KATL_FAKE_CHILD_ENV="+childEnvPath,
		"KATL_VMTEST_RUN_ID=run-1",
		"KATL_VMTEST_RUN_DIR="+runDir,
		"TMPDIR="+tmp,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("vmtest-run failed: %v\n%s", err, output)
	}
	if strings.Contains(string(output), `{"Action":"run"`) {
		t.Fatalf("vmtest-run emitted JSON without caller -json:\n%s", output)
	}

	world, err := LoadWorld(filepath.Join(runDir, "world.json"))
	if err != nil {
		t.Fatalf("LoadWorld() error = %v", err)
	}
	if world.RunID != "run-1" || world.RunDir != runDir {
		t.Fatalf("world = %#v", world)
	}
	if world.RunIndex != filepath.Join(runDir, "run.json") {
		t.Fatalf("world run index = %q", world.RunIndex)
	}
	if world.Network.Backend != NetworkBridge || world.Network.LeaseFile != filepath.Join(runDir, "network", "leases.json") {
		t.Fatalf("world network = %#v", world.Network)
	}
	for _, capability := range []string{"qemu", "qemu-img", "ovmf", "kvm", "vsock", "bridge", "mtools", "sfdisk", "sha256sum", "awk", "realpath", "kubectl", "systemd-nspawn"} {
		if world.Capabilities[capability] != WorldStatusPassed {
			t.Fatalf("capability %s = %q", capability, world.Capabilities[capability])
		}
	}

	goArgs := readLines(t, goArgsPath)
	wantGoArgs := []string{
		"test",
		"-exec",
		filepath.Join(repo, "scripts", "vmtest-exec"),
		"./internal/vmtest/scenarios",
		"-run",
		"^TestTwoNode$",
		"-count=99",
		"-timeout",
		"2m",
	}
	if !reflect.DeepEqual(goArgs, wantGoArgs) {
		t.Fatalf("go args = %#v, want %#v", goArgs, wantGoArgs)
	}

	childArgs := readLines(t, childArgsPath)
	wantChildArgs := []string{"-test.run=^Forwarded$", "-test.v", "child-extra"}
	if !reflect.DeepEqual(childArgs, wantChildArgs) {
		t.Fatalf("child args = %#v, want %#v", childArgs, wantChildArgs)
	}

	childEnv := readKeyValues(t, childEnvPath)
	if childEnv["KATL_VMTEST_WORLD_MANIFEST"] != filepath.Join(runDir, "world.json") {
		t.Fatalf("child manifest env = %q", childEnv["KATL_VMTEST_WORLD_MANIFEST"])
	}
	if childEnv["KATL_VMTEST_BRIDGE"] != "katl-vmtest0" {
		t.Fatalf("child bridge env = %q", childEnv["KATL_VMTEST_BRIDGE"])
	}
	if childEnv["KATL_VMTEST_RUN"] != "1" || childEnv["KATL_VMTEST_WORLD_STRICT"] != "1" {
		t.Fatalf("child strict env = %#v", childEnv)
	}

	runIndex := readRunIndex(t, filepath.Join(runDir, "run.json"))
	if runIndex.Kind != "VMTestRun" || runIndex.Status != "go-test" {
		t.Fatalf("run index = %#v", runIndex)
	}
	if runIndex.RunID != "run-1" || runIndex.WorldManifest != filepath.Join(runDir, "world.json") || runIndex.HostCapabilities != filepath.Join(runDir, "host-capabilities.json") {
		t.Fatalf("run index paths = %#v", runIndex)
	}
	if !reflect.DeepEqual(runIndex.GoTestArgs, []string{"./internal/vmtest/scenarios", "-run", "^TestTwoNode$", "-count=99", "-timeout", "2m"}) {
		t.Fatalf("run index go test args = %#v", runIndex.GoTestArgs)
	}

	if _, err := os.Stat(filepath.Join(runDir, "summary.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("summary.json exists unexpectedly: %v", err)
	}
	if _, err := os.Stat(filepath.Join(runDir, "go-test.log")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("go-test.log exists unexpectedly: %v", err)
	}
	assertJSONEmptyObject(t, filepath.Join(runDir, "network", "leases.json"))
}

func TestVMTestRunForwardsJSONFlag(t *testing.T) {
	repo := scriptTestRepoRoot(t)
	tmp := t.TempDir()
	fakeGo, fakeChild := writeFakeGoTools(t, tmp)
	host := writeFakeHostTools(t, tmp, true)
	runDir := filepath.Join(tmp, "run")
	goArgsPath := filepath.Join(tmp, "go-args.txt")

	cmd := exec.Command(filepath.Join(repo, "scripts", "vmtest-run"),
		"-json",
		"./internal/vmtest/scenarios",
		"-run", "^TestTwoNode$",
	)
	cmd.Dir = repo
	cmd.Env = appendHostEnv(os.Environ(), host,
		"KATL_VMTEST_GO="+fakeGo,
		"KATL_FAKE_GO_ARGS="+goArgsPath,
		"KATL_FAKE_CHILD="+fakeChild,
		"KATL_FAKE_CHILD_ARGS="+filepath.Join(tmp, "child-args.txt"),
		"KATL_FAKE_CHILD_ENV="+filepath.Join(tmp, "child-env.txt"),
		"KATL_VMTEST_RUN_ID=run-json",
		"KATL_VMTEST_RUN_DIR="+runDir,
		"TMPDIR="+tmp,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("vmtest-run -json failed: %v\n%s", err, output)
	}

	goArgs := readLines(t, goArgsPath)
	if !contains(goArgs, "-json") {
		t.Fatalf("go args missing caller -json: %#v", goArgs)
	}
	if !strings.Contains(string(output), `{"Action":"run","Package":"fake/vmtest"}`) {
		t.Fatalf("output missing JSON events:\n%s", output)
	}
	if _, err := os.Stat(filepath.Join(runDir, "summary.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("summary.json exists unexpectedly: %v", err)
	}
}

func TestVMTestRunPreservesArbitraryGoTestArgs(t *testing.T) {
	repo := scriptTestRepoRoot(t)
	tmp := t.TempDir()
	fakeGo, fakeChild := writeFakeGoTools(t, tmp)
	host := writeFakeHostTools(t, tmp, true)
	runDir := filepath.Join(tmp, "run")
	goArgsPath := filepath.Join(tmp, "go-args.txt")
	childArgsPath := filepath.Join(tmp, "child-args.txt")

	cmd := exec.Command(filepath.Join(repo, "scripts", "vmtest-run"),
		"-json",
		"-count=99",
		"-run", "^TestTwoNode$",
		"-args",
		"-test.v",
		"literal",
		"./internal/vmtest/scenarios",
	)
	cmd.Dir = repo
	cmd.Env = appendHostEnv(os.Environ(), host,
		"KATL_VMTEST_GO="+fakeGo,
		"KATL_FAKE_GO_ARGS="+goArgsPath,
		"KATL_FAKE_CHILD="+fakeChild,
		"KATL_FAKE_CHILD_ARGS="+childArgsPath,
		"KATL_FAKE_CHILD_ENV="+filepath.Join(tmp, "child-env.txt"),
		"KATL_VMTEST_RUN_ID=run-preserve-args",
		"KATL_VMTEST_RUN_DIR="+runDir,
		"TMPDIR="+tmp,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("vmtest-run failed: %v\n%s", err, output)
	}

	goArgs := readLines(t, goArgsPath)
	wantGoArgs := []string{
		"test",
		"-exec",
		filepath.Join(repo, "scripts", "vmtest-exec"),
		"-timeout",
		"90m",
		"-json",
		"-count=99",
		"-run",
		"^TestTwoNode$",
		"-args",
		"-test.v",
		"literal",
		"./internal/vmtest/scenarios",
	}
	if !reflect.DeepEqual(goArgs, wantGoArgs) {
		t.Fatalf("go args = %#v, want %#v", goArgs, wantGoArgs)
	}
	if _, err := os.Stat(filepath.Join(runDir, "summary.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("summary.json exists unexpectedly: %v", err)
	}
	if _, err := os.Stat(filepath.Join(runDir, "go-test.log")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("go-test.log exists unexpectedly: %v", err)
	}
}

func TestVMTestExecRequiresManifest(t *testing.T) {
	repo := scriptTestRepoRoot(t)
	cmd := exec.Command(filepath.Join(repo, "scripts", "vmtest-exec"), filepath.Join(repo, "scripts", "vmtest-run"))
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "KATL_VMTEST_WORLD_MANIFEST=", "KATL_VMTEST_RUN_DIR=")

	output, err := cmd.CombinedOutput()
	if exitCode(err) != 2 {
		t.Fatalf("vmtest-exec exit = %v, want 2\n%s", err, output)
	}
	if !strings.Contains(string(output), "scripts/vmtest-run") {
		t.Fatalf("output missing runner hint:\n%s", output)
	}
}

func TestVMTestRunRecordsHostGapsAndExecsGo(t *testing.T) {
	repo := scriptTestRepoRoot(t)
	tmp := t.TempDir()
	fakeGo, fakeChild := writeFakeGoTools(t, tmp)
	host := writeFakeHostTools(t, tmp, true)
	runDir := filepath.Join(tmp, "run")
	goArgsPath := filepath.Join(tmp, "go-args.txt")

	cmd := exec.Command(filepath.Join(repo, "scripts", "vmtest-run"), "./internal/vmtest", "-run", "NeedsQEMU")
	cmd.Dir = repo
	cmd.Env = appendHostEnv(os.Environ(), host,
		"KATL_VMTEST_GO="+fakeGo,
		"KATL_FAKE_GO_ARGS="+goArgsPath,
		"KATL_FAKE_CHILD="+fakeChild,
		"KATL_FAKE_CHILD_ARGS="+filepath.Join(tmp, "child-args.txt"),
		"KATL_FAKE_CHILD_ENV="+filepath.Join(tmp, "child-env.txt"),
		"KATL_VMTEST_QEMU="+filepath.Join(tmp, "missing-qemu"),
		"KATL_VMTEST_RUN_ID=run-host-gap",
		"KATL_VMTEST_RUN_DIR="+runDir,
		"TMPDIR="+tmp,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("vmtest-run failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "vmtest world has missing host capabilities") {
		t.Fatalf("output missing host gap heading:\n%s", output)
	}
	if !strings.Contains(string(output), "  - qemu:") {
		t.Fatalf("output missing qemu diagnostic:\n%s", output)
	}
	goArgs := readLines(t, goArgsPath)
	if !reflect.DeepEqual(goArgs, []string{
		"test",
		"-exec",
		filepath.Join(repo, "scripts", "vmtest-exec"),
		"-timeout",
		"90m",
		"./internal/vmtest",
		"-run",
		"NeedsQEMU",
	}) {
		t.Fatalf("go args = %#v", goArgs)
	}
	world, err := LoadWorld(filepath.Join(runDir, "world.json"))
	if err != nil {
		t.Fatalf("LoadWorld() error = %v", err)
	}
	if world.Capabilities["qemu"] != WorldStatusFailed {
		t.Fatalf("qemu capability = %q", world.Capabilities["qemu"])
	}
	caps := readCapabilities(t, filepath.Join(runDir, "host-capabilities.json"))
	if !contains(caps.Missing, "qemu") {
		t.Fatalf("missing capabilities = %#v", caps.Missing)
	}
	runIndex := readRunIndex(t, filepath.Join(runDir, "run.json"))
	if !contains(runIndex.MissingCapabilities, "qemu") || runIndex.Status != "go-test" {
		t.Fatalf("run index = %#v", runIndex)
	}
	if _, err := os.Stat(filepath.Join(runDir, "summary.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("summary.json exists unexpectedly: %v", err)
	}
}

func TestVMTestRunDoesNotDefaultFlagOnlyArgs(t *testing.T) {
	repo := scriptTestRepoRoot(t)
	tmp := t.TempDir()
	fakeGo, fakeChild := writeFakeGoTools(t, tmp)
	host := writeFakeHostTools(t, tmp, true)
	runDir := filepath.Join(tmp, "run")
	goArgsPath := filepath.Join(tmp, "go-args.txt")

	cmd := exec.Command(filepath.Join(repo, "scripts", "vmtest-run"), "-run", "^TestDoesNotNeedQEMU$", "-timeout", "2m")
	cmd.Dir = repo
	cmd.Env = appendHostEnv(os.Environ(), host,
		"KATL_VMTEST_GO="+fakeGo,
		"KATL_FAKE_GO_ARGS="+goArgsPath,
		"KATL_FAKE_CHILD="+fakeChild,
		"KATL_FAKE_CHILD_ARGS="+filepath.Join(tmp, "child-args.txt"),
		"KATL_FAKE_CHILD_ENV="+filepath.Join(tmp, "child-env.txt"),
		"KATL_VMTEST_RUN_ID=run-flag-only",
		"KATL_VMTEST_RUN_DIR="+runDir,
		"TMPDIR="+tmp,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("vmtest-run failed: %v\n%s", err, output)
	}
	goArgs := readLines(t, goArgsPath)
	if !reflect.DeepEqual(goArgs, []string{
		"test",
		"-exec",
		filepath.Join(repo, "scripts", "vmtest-exec"),
		"-run",
		"^TestDoesNotNeedQEMU$",
		"-timeout",
		"2m",
	}) {
		t.Fatalf("go args = %#v", goArgs)
	}
}

func TestVMTestRunFinalProcessIsGoTest(t *testing.T) {
	repo := scriptTestRepoRoot(t)
	tmp := t.TempDir()
	fakeGo, fakeChild := writeFakeGoTools(t, tmp)
	host := writeFakeHostTools(t, tmp, true)
	runDir := filepath.Join(tmp, "run")
	pidPath := filepath.Join(tmp, "go-pid.txt")

	cmd := exec.Command(filepath.Join(repo, "scripts", "vmtest-run"), "./internal/vmtest")
	cmd.Dir = repo
	cmd.Env = appendHostEnv(os.Environ(), host,
		"KATL_VMTEST_GO="+fakeGo,
		"KATL_FAKE_GO_ARGS="+filepath.Join(tmp, "go-args.txt"),
		"KATL_FAKE_CHILD="+fakeChild,
		"KATL_FAKE_CHILD_ARGS="+filepath.Join(tmp, "child-args.txt"),
		"KATL_FAKE_CHILD_ENV="+filepath.Join(tmp, "child-env.txt"),
		"KATL_FAKE_GO_PID="+pidPath,
		"KATL_VMTEST_RUN_ID=run-final-process",
		"KATL_VMTEST_RUN_DIR="+runDir,
		"TMPDIR="+tmp,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("vmtest-run failed: %v\n%s", err, output)
	}
	pid := strings.TrimSpace(readScriptFile(t, pidPath))
	if pid != strconv.Itoa(cmd.ProcessState.Pid()) {
		t.Fatalf("fake go pid = %q, want wrapper process pid %d", pid, cmd.ProcessState.Pid())
	}
}

func TestVMTestRunBridgeACLAllowsFinalLineWithoutNewline(t *testing.T) {
	repo := scriptTestRepoRoot(t)
	tmp := t.TempDir()
	fakeGo, fakeChild := writeFakeGoTools(t, tmp)
	host := writeFakeHostTools(t, tmp, true)
	runDir := filepath.Join(tmp, "run")
	if err := os.WriteFile(host.bridgeConf, []byte("allow katl-vmtest0"), 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", host.bridgeConf, err)
	}

	cmd := exec.Command(filepath.Join(repo, "scripts", "vmtest-run"))
	cmd.Dir = repo
	cmd.Env = appendHostEnv(os.Environ(), host,
		"KATL_VMTEST_GO="+fakeGo,
		"KATL_FAKE_GO_ARGS="+filepath.Join(tmp, "go-args.txt"),
		"KATL_FAKE_CHILD="+fakeChild,
		"KATL_FAKE_CHILD_ARGS="+filepath.Join(tmp, "child-args.txt"),
		"KATL_FAKE_CHILD_ENV="+filepath.Join(tmp, "child-env.txt"),
		"KATL_VMTEST_RUN_ID=run-bridge-acl-no-newline",
		"KATL_VMTEST_RUN_DIR="+runDir,
		"TMPDIR="+tmp,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("vmtest-run failed: %v\n%s", err, output)
	}
	caps := readCapabilities(t, filepath.Join(runDir, "host-capabilities.json"))
	if contains(caps.Missing, "qemu-bridge-acl") {
		t.Fatalf("missing capabilities = %#v", caps.Missing)
	}
}

func TestVMTestRunBridgeCreateFailureRecordsCapabilityAndExecsGo(t *testing.T) {
	repo := scriptTestRepoRoot(t)
	tmp := t.TempDir()
	fakeGo, fakeChild := writeFakeGoTools(t, tmp)
	host := writeFakeHostTools(t, tmp, false)
	runDir := filepath.Join(tmp, "run")
	goArgsPath := filepath.Join(tmp, "go-args.txt")

	cmd := exec.Command(filepath.Join(repo, "scripts", "vmtest-run"))
	cmd.Dir = repo
	cmd.Env = appendHostEnv(os.Environ(), host,
		"KATL_VMTEST_GO="+fakeGo,
		"KATL_FAKE_GO_ARGS="+goArgsPath,
		"KATL_FAKE_CHILD="+fakeChild,
		"KATL_FAKE_CHILD_ARGS="+filepath.Join(tmp, "child-args.txt"),
		"KATL_FAKE_CHILD_ENV="+filepath.Join(tmp, "child-env.txt"),
		"KATL_FAKE_IP_FAIL_ADD=1",
		"KATL_VMTEST_RUN_ID=run-bridge-create-failure",
		"KATL_VMTEST_RUN_DIR="+runDir,
		"TMPDIR="+tmp,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("vmtest-run failed: %v\n%s", err, output)
	}
	if strings.Contains(string(output), "synthetic netlink failure") {
		t.Fatalf("vmtest-run leaked raw ip stderr:\n%s", output)
	}
	_ = readLines(t, goArgsPath)
	caps := readCapabilities(t, filepath.Join(runDir, "host-capabilities.json"))
	if !contains(caps.Missing, "bridge") {
		t.Fatalf("missing capabilities = %#v", caps.Missing)
	}
}

func TestVMTestRunInvalidCIDRSetupFailed(t *testing.T) {
	repo := scriptTestRepoRoot(t)
	tmp := t.TempDir()
	fakeGo, fakeChild := writeFakeGoTools(t, tmp)
	host := writeFakeHostTools(t, tmp, true)
	runDir := filepath.Join(tmp, "run")
	goArgsPath := filepath.Join(tmp, "go-args.txt")

	cmd := exec.Command(filepath.Join(repo, "scripts", "vmtest-run"), "./internal/vmtest")
	cmd.Dir = repo
	cmd.Env = appendHostEnv(os.Environ(), host,
		"KATL_VMTEST_GO="+fakeGo,
		"KATL_FAKE_GO_ARGS="+goArgsPath,
		"KATL_FAKE_CHILD="+fakeChild,
		"KATL_FAKE_CHILD_ARGS="+filepath.Join(tmp, "child-args.txt"),
		"KATL_FAKE_CHILD_ENV="+filepath.Join(tmp, "child-env.txt"),
		"KATL_VMTEST_CIDR=10.77.0.0/33",
		"KATL_VMTEST_RUN_ID=run-invalid-cidr",
		"KATL_VMTEST_RUN_DIR="+runDir,
		"TMPDIR="+tmp,
	)
	output, err := cmd.CombinedOutput()
	if exitCode(err) != 2 {
		t.Fatalf("vmtest-run exit = %v, want 2\n%s", err, output)
	}
	if _, err := os.Stat(goArgsPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("go test ran for setup-failed world, stat err = %v", err)
	}
	runIndex := readRunIndex(t, filepath.Join(runDir, "run.json"))
	if runIndex.Status != "setup-failed" {
		t.Fatalf("run index status = %q, want setup-failed", runIndex.Status)
	}
	if !reflect.DeepEqual(runIndex.GoTestArgs, []string{"./internal/vmtest"}) {
		t.Fatalf("run index go test args = %#v", runIndex.GoTestArgs)
	}
	if len(runIndex.SetupFailures) != 1 || !strings.Contains(runIndex.SetupFailures[0], "invalid CIDR prefix") {
		t.Fatalf("run index setup failures = %#v", runIndex.SetupFailures)
	}
	if len(runIndex.MissingCapabilities) != 0 {
		t.Fatalf("run index missing capabilities = %#v", runIndex.MissingCapabilities)
	}
	if _, err := os.Stat(filepath.Join(runDir, "summary.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("summary.json exists unexpectedly: %v", err)
	}
}

func writeFakeGoTools(t *testing.T, dir string) (string, string) {
	t.Helper()
	fakeGo := filepath.Join(dir, "go")
	fakeChild := filepath.Join(dir, "fake-test-binary")
	writeExecutable(t, fakeGo, `#!/usr/bin/env bash
set -euo pipefail
if [[ -n "${KATL_FAKE_GO_PID:-}" ]]; then
    printf '%s\n' "$$" > "$KATL_FAKE_GO_PID"
fi
printf '%s\n' "$@" > "$KATL_FAKE_GO_ARGS"
args=("$@")
if [[ "${args[0]:-}" != "test" ]]; then
    echo "unexpected go command: $*" >&2
    exit 91
fi
exec_wrapper=""
for ((i = 0; i < ${#args[@]}; i++)); do
    if [[ "${args[$i]}" == "-exec" ]]; then
        exec_wrapper="${args[$((i + 1))]:-}"
    fi
done
if [[ -z "$exec_wrapper" ]]; then
    echo "missing -exec" >&2
    exit 92
fi
json_output=0
for arg in "${args[@]}"; do
    if [[ "$arg" == "-json" ]]; then
        json_output=1
    fi
done
if [[ "$json_output" == 1 ]]; then
    printf '{"Action":"run","Package":"fake/vmtest"}\n'
else
    printf '=== RUN   TestForwarded\n'
fi
set +e
"$exec_wrapper" "$KATL_FAKE_CHILD" "-test.run=^Forwarded$" "-test.v" "child-extra"
exit_code=$?
set -e
if [[ "$json_output" == 1 ]]; then
    if [[ "${KATL_FAKE_GO_JSON_MALFORMED:-0}" == "1" ]]; then
        printf '{not json\n'
        exit "$exit_code"
    fi
    if [[ "${KATL_FAKE_GO_JSON_NO_TERMINAL:-0}" == "1" ]]; then
        exit "$exit_code"
    fi
    json_action="${KATL_FAKE_GO_JSON_ACTION:-pass}"
    printf '{"Action":"%s","Package":"fake/vmtest"}\n' "$json_action"
    if [[ "${KATL_FAKE_GO_JSON_EXTRA_SKIP:-0}" == "1" ]]; then
        printf '{"Action":"skip","Package":"fake/vmtest","Test":"TestUnexpectedSkip"}\n'
    fi
else
    printf -- '--- PASS: TestForwarded (0.00s)\n'
    printf 'ok  \tfake/vmtest\t0.001s\n'
fi
exit "$exit_code"
`)
	writeExecutable(t, fakeChild, `#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$@" > "$KATL_FAKE_CHILD_ARGS"
{
    printf 'KATL_VMTEST_WORLD_MANIFEST=%s\n' "${KATL_VMTEST_WORLD_MANIFEST:-}"
    printf 'KATL_VMTEST_BRIDGE=%s\n' "${KATL_VMTEST_BRIDGE:-}"
    printf 'KATL_VMTEST_RUN=%s\n' "${KATL_VMTEST_RUN:-}"
    printf 'KATL_VMTEST_WORLD_STRICT=%s\n' "${KATL_VMTEST_WORLD_STRICT:-}"
} > "$KATL_FAKE_CHILD_ENV"
[[ -f "${KATL_VMTEST_WORLD_MANIFEST:-}" ]] || exit 41
if [[ -n "${KATL_FAKE_CHILD_WORLD_SCENARIO:-}" ]]; then
    scenario_name="$KATL_FAKE_CHILD_WORLD_SCENARIO"
    scenario_id="${KATL_FAKE_CHILD_WORLD_SCENARIO_ID:-fake-scenario}"
    scenario_dir="$(jq -r '.scenarioDir' "$KATL_VMTEST_WORLD_MANIFEST")/$scenario_id"
    run_id="$(jq -r '.runID' "$KATL_VMTEST_WORLD_MANIFEST")"
    mkdir -p "$scenario_dir"
    result_path="$scenario_dir/result.json"
    if [[ "${KATL_FAKE_CHILD_WORLD_MANIFEST:-}" == "malformed" ]]; then
        printf '{' > "$scenario_dir/scenario.json"
        exit "${KATL_FAKE_CHILD_EXIT:-0}"
    fi
    jq -n \
        --arg name "$scenario_name" \
        --arg id "$scenario_id" \
        --arg dir "$scenario_dir" \
        --arg runID "$run_id" \
        --arg resultPath "$result_path" \
        '{
          apiVersion: "katl.dev/v1alpha1",
          kind: "VMTestScenario",
          worldRunID: $runID,
          name: $name,
          id: $id,
          dir: $dir,
          resultPath: $resultPath
        }' > "$scenario_dir/scenario.json"
    case "${KATL_FAKE_CHILD_WORLD_RESULT:-passed}" in
        missing)
            ;;
        malformed)
            printf '{' > "$result_path"
            ;;
        *)
            result_run_id="${KATL_FAKE_CHILD_WORLD_RESULT_RUN_ID:-$run_id}"
            result_name="${KATL_FAKE_CHILD_WORLD_RESULT_SCENARIO:-$scenario_name}"
            result_status="${KATL_FAKE_CHILD_WORLD_RESULT:-passed}"
            if [[ -n "${KATL_FAKE_CHILD_WORLD_MISSING:-}" ]]; then
                missing_filter='| .missing = [{name: $missing, detail: "synthetic missing capability"}]'
            else
                missing_filter=''
            fi
            jq -n \
                --arg scenarioName "$result_name" \
                --arg status "$result_status" \
                --arg runID "$result_run_id" \
                --arg runDir "$scenario_dir" \
                --arg manifestPath "$scenario_dir/scenario.json" \
                --arg missing "${KATL_FAKE_CHILD_WORLD_MISSING:-}" \
                --arg resultPath "$result_path" \
                '{
                  apiVersion: "katl.dev/v1alpha1",
                  kind: "VMTestScenarioResult",
                  scenarioName: $scenarioName,
                  status: $status,
                  runId: $runID,
                  runDir: $runDir,
                  manifestPath: $manifestPath,
                  resultPath: $resultPath
                } '"$missing_filter" > "$result_path"
            ;;
    esac
fi
exit "${KATL_FAKE_CHILD_EXIT:-0}"
`)
	return fakeGo, fakeChild
}

type fakeHostTools struct {
	qemu         string
	qemuImg      string
	ip           string
	ipLog        string
	ovmfCode     string
	ovmfVars     string
	kvm          string
	vsock        string
	tun          string
	bridgeHelper string
	bridgeConf   string
	kubectl      string
}

func writeFakeHostTools(t *testing.T, dir string, bridgeExists bool) fakeHostTools {
	t.Helper()
	tools := fakeHostTools{
		qemu:         filepath.Join(dir, "qemu-system-x86_64"),
		qemuImg:      filepath.Join(dir, "qemu-img"),
		ip:           filepath.Join(dir, "ip"),
		ipLog:        filepath.Join(dir, "ip.log"),
		ovmfCode:     filepath.Join(dir, "OVMF_CODE.fd"),
		ovmfVars:     filepath.Join(dir, "OVMF_VARS.fd"),
		kvm:          filepath.Join(dir, "kvm"),
		vsock:        filepath.Join(dir, "vhost-vsock"),
		tun:          filepath.Join(dir, "tun"),
		bridgeHelper: filepath.Join(dir, "qemu-bridge-helper"),
		bridgeConf:   filepath.Join(dir, "bridge.conf"),
		kubectl:      filepath.Join(dir, "kubectl"),
	}
	writeExecutable(t, tools.qemu, "#!/usr/bin/env bash\nexit 0\n")
	writeExecutable(t, tools.qemuImg, "#!/usr/bin/env bash\nexit 0\n")
	writeExecutable(t, filepath.Join(dir, "mformat"), "#!/usr/bin/env bash\nexit 0\n")
	writeExecutable(t, filepath.Join(dir, "mcopy"), "#!/usr/bin/env bash\nexit 0\n")
	writeExecutable(t, filepath.Join(dir, "truncate"), "#!/usr/bin/env bash\nexit 0\n")
	writeExecutable(t, filepath.Join(dir, "sfdisk"), "#!/usr/bin/env bash\nexit 0\n")
	writeExecutable(t, filepath.Join(dir, "sha256sum"), "#!/usr/bin/env bash\nprintf 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa  %s\\n' \"${1:-}\"\n")
	writeExecutable(t, filepath.Join(dir, "awk"), "#!/usr/bin/env bash\nexit 0\n")
	writeExecutable(t, filepath.Join(dir, "realpath"), "#!/usr/bin/env bash\nprintf '%s\\n' \"${@: -1}\"\n")
	writeExecutable(t, filepath.Join(dir, "systemd-nspawn"), "#!/usr/bin/env bash\nexit 0\n")
	writeExecutable(t, tools.kubectl, "#!/usr/bin/env bash\nexit 0\n")
	if err := os.WriteFile(tools.ovmfCode, []byte("code"), 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", tools.ovmfCode, err)
	}
	if err := os.WriteFile(tools.ovmfVars, []byte("vars"), 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", tools.ovmfVars, err)
	}
	if err := os.WriteFile(tools.kvm, []byte{}, 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", tools.kvm, err)
	}
	if err := os.WriteFile(tools.vsock, []byte{}, 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", tools.vsock, err)
	}
	if err := os.WriteFile(tools.tun, []byte{}, 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", tools.tun, err)
	}
	writeExecutable(t, tools.bridgeHelper, "#!/usr/bin/env bash\nexit 0\n")
	if err := os.WriteFile(tools.bridgeConf, []byte("allow katl-vmtest0\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", tools.bridgeConf, err)
	}
	existing := "0"
	if bridgeExists {
		existing = "1"
	}
	writeExecutable(t, tools.ip, `#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$KATL_FAKE_IP_LOG"
case "$*" in
    "link show katl-vmtest0")
        if [[ "${KATL_FAKE_BRIDGE_EXISTS:-0}" == "1" ]]; then
            exit 0
        fi
        exit 1
        ;;
    "link add name katl-vmtest0 type bridge")
        if [[ "${KATL_FAKE_IP_FAIL_ADD:-0}" == "1" ]]; then
            echo "synthetic netlink failure" >&2
            exit 1
        fi
        exit 0
        ;;
    "addr add 10.77.0.1/24 dev katl-vmtest0"|"link set katl-vmtest0 up"|"link del katl-vmtest0")
        exit 0
        ;;
    *)
        echo "unexpected ip args: $*" >&2
        exit 40
        ;;
esac
`)
	if err := os.WriteFile(tools.ipLog, nil, 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", tools.ipLog, err)
	}
	t.Setenv("KATL_FAKE_BRIDGE_EXISTS", existing)
	t.Setenv("KATL_FAKE_IP_LOG", tools.ipLog)
	return tools
}

func appendHostEnv(env []string, tools fakeHostTools, extra ...string) []string {
	env = append(env,
		"KATL_VMTEST_QEMU="+tools.qemu,
		"KATL_VMTEST_QEMU_IMG="+tools.qemuImg,
		"KATL_VMTEST_IP="+tools.ip,
		"KATL_OVMF_CODE="+tools.ovmfCode,
		"KATL_OVMF_VARS="+tools.ovmfVars,
		"KATL_VMTEST_KVM_DEVICE="+tools.kvm,
		"KATL_VMTEST_VSOCK_DEVICE="+tools.vsock,
		"KATL_VMTEST_TUN_DEVICE="+tools.tun,
		"KATL_QEMU_BRIDGE_HELPER="+tools.bridgeHelper,
		"KATL_QEMU_BRIDGE_CONF="+tools.bridgeConf,
		"KATL_VMTEST_BRIDGE=katl-vmtest0",
		"KATL_VMTEST_KUBECTL="+tools.kubectl,
		"KATL_NSPAWN_ALLOW_UNPRIVILEGED=1",
		"PATH="+filepath.Dir(tools.qemu)+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	return append(env, extra...)
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
}

func scriptTestRepoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("Abs(repo root) error = %v", err)
	}
	return root
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, err)
	}
	text := strings.TrimSuffix(string(data), "\n")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

func readKeyValues(t *testing.T, path string) map[string]string {
	t.Helper()
	values := make(map[string]string)
	for _, line := range readLines(t, path) {
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("malformed env line %q", line)
		}
		values[name] = value
	}
	return values
}

type vmtestHostCapabilities struct {
	Missing []string `json:"missing"`
}

type vmtestRunIndex struct {
	Kind                string   `json:"kind"`
	RunID               string   `json:"runID"`
	WorldManifest       string   `json:"worldManifest"`
	HostCapabilities    string   `json:"hostCapabilities"`
	Status              string   `json:"status"`
	MissingCapabilities []string `json:"missingCapabilities"`
	GoTestArgs          []string `json:"goTestArgs"`
	SetupFailures       []string `json:"setupFailures"`
}

func readRunIndex(t *testing.T, path string) vmtestRunIndex {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, err)
	}
	var index vmtestRunIndex
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatalf("Unmarshal(%s) error = %v", path, err)
	}
	return index
}

func readCapabilities(t *testing.T, path string) vmtestHostCapabilities {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, err)
	}
	var capabilities vmtestHostCapabilities
	if err := json.Unmarshal(data, &capabilities); err != nil {
		t.Fatalf("Unmarshal(%s) error = %v", path, err)
	}
	return capabilities
}

func assertJSONEmptyObject(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, err)
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatalf("Unmarshal(%s) error = %v", path, err)
	}
	if len(value) != 0 {
		t.Fatalf("%s = %#v, want empty JSON object", path, value)
	}
}

func readScriptFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, err)
	}
	return string(data)
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}
