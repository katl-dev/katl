package vmtest

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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
		"-count=1",
		"-exec",
		filepath.Join(repo, "scripts", "vmtest-exec"),
		"./internal/vmtest/scenarios",
		"-run",
		"^TestTwoNode$",
		"-timeout",
		"2m",
	}
	if !reflect.DeepEqual(goArgs, wantGoArgs) {
		t.Fatalf("go args = %#v, want %#v", goArgs, wantGoArgs)
	}
	for _, arg := range goArgs {
		if arg == "-count=99" {
			t.Fatalf("vmtest-run did not force -count=1: %#v", goArgs)
		}
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

	summary := readSummary(t, filepath.Join(runDir, "summary.json"))
	if summary.Status != "passed" || summary.ExitCode != 0 {
		t.Fatalf("summary = %#v", summary)
	}
	if summary.GoTestLog != filepath.Join(runDir, "go-test.log") {
		t.Fatalf("summary go test log = %q", summary.GoTestLog)
	}
	if !reflect.DeepEqual(summary.Args, []string{"./internal/vmtest/scenarios", "-run", "^TestTwoNode$", "-timeout", "2m"}) {
		t.Fatalf("summary args = %#v", summary.Args)
	}
	if _, err := os.Stat(filepath.Join(runDir, "go-test.log")); err != nil {
		t.Fatalf("go-test.log missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(runDir, "go-test.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("go-test.json exists unexpectedly: %v", err)
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
}

func TestVMTestRunAggregatesScenarioResults(t *testing.T) {
	repo := scriptTestRepoRoot(t)
	tmp := t.TempDir()
	fakeGo, fakeChild := writeFakeGoTools(t, tmp)
	host := writeFakeHostTools(t, tmp, true)
	runDir := filepath.Join(tmp, "run")

	cmd := exec.Command(filepath.Join(repo, "scripts", "vmtest-run"), "./internal/vmtest", "-run", "^TestFakeScenario$")
	cmd.Dir = repo
	cmd.Env = appendHostEnv(os.Environ(), host,
		"KATL_VMTEST_GO="+fakeGo,
		"KATL_FAKE_GO_ARGS="+filepath.Join(tmp, "go-args.txt"),
		"KATL_FAKE_CHILD="+fakeChild,
		"KATL_FAKE_CHILD_ARGS="+filepath.Join(tmp, "child-args.txt"),
		"KATL_FAKE_CHILD_ENV="+filepath.Join(tmp, "child-env.txt"),
		"KATL_FAKE_CHILD_WORLD_SCENARIO=fake scenario",
		"KATL_VMTEST_RUN_ID=run-aggregate",
		"KATL_VMTEST_RUN_DIR="+runDir,
		"TMPDIR="+tmp,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("vmtest-run failed: %v\n%s", err, output)
	}

	summary := readSummary(t, filepath.Join(runDir, "summary.json"))
	if summary.Status != "passed" || summary.ExitCode != 0 || summary.ChildExitCode != 0 {
		t.Fatalf("summary = %#v", summary)
	}
	if !reflect.DeepEqual(summary.SelectedPackages, []string{"./internal/vmtest"}) {
		t.Fatalf("selected packages = %#v", summary.SelectedPackages)
	}
	if summary.Counts["passed"] != 1 {
		t.Fatalf("counts = %#v", summary.Counts)
	}
	if len(summary.Scenarios) != 1 || summary.Scenarios[0].Name != "fake scenario" || summary.Scenarios[0].Status != "passed" {
		t.Fatalf("scenarios = %#v", summary.Scenarios)
	}
	if summary.Scenarios[0].ManifestPath == "" || summary.Scenarios[0].ResultPath == "" {
		t.Fatalf("scenario paths missing: %#v", summary.Scenarios[0])
	}
}

func TestVMTestRunFailsMissingScenarioResult(t *testing.T) {
	repo := scriptTestRepoRoot(t)
	tmp := t.TempDir()
	fakeGo, fakeChild := writeFakeGoTools(t, tmp)
	host := writeFakeHostTools(t, tmp, true)
	runDir := filepath.Join(tmp, "run")

	cmd := exec.Command(filepath.Join(repo, "scripts", "vmtest-run"), "./internal/vmtest", "-run", "^TestFakeScenario$")
	cmd.Dir = repo
	cmd.Env = appendHostEnv(os.Environ(), host,
		"KATL_VMTEST_GO="+fakeGo,
		"KATL_FAKE_GO_ARGS="+filepath.Join(tmp, "go-args.txt"),
		"KATL_FAKE_CHILD="+fakeChild,
		"KATL_FAKE_CHILD_ARGS="+filepath.Join(tmp, "child-args.txt"),
		"KATL_FAKE_CHILD_ENV="+filepath.Join(tmp, "child-env.txt"),
		"KATL_FAKE_CHILD_WORLD_SCENARIO=missing result scenario",
		"KATL_FAKE_CHILD_WORLD_RESULT=missing",
		"KATL_VMTEST_RUN_ID=run-missing-scenario-result",
		"KATL_VMTEST_RUN_DIR="+runDir,
		"TMPDIR="+tmp,
	)
	output, err := cmd.CombinedOutput()
	if exitCode(err) != 1 {
		t.Fatalf("vmtest-run exit = %v, want 1\n%s", err, output)
	}
	summary := readSummary(t, filepath.Join(runDir, "summary.json"))
	if summary.Status != "failed" || summary.ExitCode != 1 || summary.ChildExitCode != 0 {
		t.Fatalf("summary = %#v", summary)
	}
	if len(summary.Scenarios) != 1 || summary.Scenarios[0].Status != "setup-failed" || !strings.Contains(summary.Scenarios[0].FailureSummary, "scenario result missing") {
		t.Fatalf("scenarios = %#v", summary.Scenarios)
	}
}

func TestVMTestRunFailsFailedScenarioResult(t *testing.T) {
	repo := scriptTestRepoRoot(t)
	tmp := t.TempDir()
	fakeGo, fakeChild := writeFakeGoTools(t, tmp)
	host := writeFakeHostTools(t, tmp, true)
	runDir := filepath.Join(tmp, "run")

	cmd := exec.Command(filepath.Join(repo, "scripts", "vmtest-run"), "./internal/vmtest", "-run", "^TestFakeScenario$")
	cmd.Dir = repo
	cmd.Env = appendHostEnv(os.Environ(), host,
		"KATL_VMTEST_GO="+fakeGo,
		"KATL_FAKE_GO_ARGS="+filepath.Join(tmp, "go-args.txt"),
		"KATL_FAKE_CHILD="+fakeChild,
		"KATL_FAKE_CHILD_ARGS="+filepath.Join(tmp, "child-args.txt"),
		"KATL_FAKE_CHILD_ENV="+filepath.Join(tmp, "child-env.txt"),
		"KATL_FAKE_CHILD_WORLD_SCENARIO=failed result scenario",
		"KATL_FAKE_CHILD_WORLD_RESULT=failed",
		"KATL_VMTEST_RUN_ID=run-failed-scenario-result",
		"KATL_VMTEST_RUN_DIR="+runDir,
		"TMPDIR="+tmp,
	)
	output, err := cmd.CombinedOutput()
	if exitCode(err) != 1 {
		t.Fatalf("vmtest-run exit = %v, want 1\n%s", err, output)
	}
	summary := readSummary(t, filepath.Join(runDir, "summary.json"))
	if summary.Status != "failed" || summary.ExitCode != 1 || summary.ChildExitCode != 0 {
		t.Fatalf("summary = %#v", summary)
	}
	if len(summary.Scenarios) != 1 || summary.Scenarios[0].Status != "failed" {
		t.Fatalf("scenarios = %#v", summary.Scenarios)
	}
}

func TestVMTestRunAllowsHostSkippedScenarioResult(t *testing.T) {
	repo := scriptTestRepoRoot(t)
	tmp := t.TempDir()
	fakeGo, fakeChild := writeFakeGoTools(t, tmp)
	host := writeFakeHostTools(t, tmp, true)
	runDir := filepath.Join(tmp, "run")

	cmd := exec.Command(filepath.Join(repo, "scripts", "vmtest-run"), "./internal/vmtest", "-run", "^TestFakeScenario$")
	cmd.Dir = repo
	cmd.Env = appendHostEnv(os.Environ(), host,
		"KATL_VMTEST_GO="+fakeGo,
		"KATL_FAKE_GO_ARGS="+filepath.Join(tmp, "go-args.txt"),
		"KATL_FAKE_CHILD="+fakeChild,
		"KATL_FAKE_CHILD_ARGS="+filepath.Join(tmp, "child-args.txt"),
		"KATL_FAKE_CHILD_ENV="+filepath.Join(tmp, "child-env.txt"),
		"KATL_FAKE_CHILD_WORLD_SCENARIO=host skipped scenario",
		"KATL_FAKE_CHILD_WORLD_RESULT=skipped",
		"KATL_FAKE_CHILD_WORLD_MISSING=systemd-nspawn",
		"KATL_VMTEST_RUN_ID=run-host-skipped-scenario-result",
		"KATL_VMTEST_RUN_DIR="+runDir,
		"TMPDIR="+tmp,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("vmtest-run failed: %v\n%s", err, output)
	}
	summary := readSummary(t, filepath.Join(runDir, "summary.json"))
	if summary.Status != "passed" || summary.ExitCode != 0 || summary.Counts["host-skipped"] != 1 {
		t.Fatalf("summary = %#v", summary)
	}
	if len(summary.Scenarios) != 1 || summary.Scenarios[0].Status != "host-skipped" {
		t.Fatalf("scenarios = %#v", summary.Scenarios)
	}
}

func TestVMTestRunFailsMalformedScenarioResult(t *testing.T) {
	repo := scriptTestRepoRoot(t)
	tmp := t.TempDir()
	fakeGo, fakeChild := writeFakeGoTools(t, tmp)
	host := writeFakeHostTools(t, tmp, true)
	runDir := filepath.Join(tmp, "run")

	cmd := exec.Command(filepath.Join(repo, "scripts", "vmtest-run"), "./internal/vmtest", "-run", "^TestFakeScenario$")
	cmd.Dir = repo
	cmd.Env = appendHostEnv(os.Environ(), host,
		"KATL_VMTEST_GO="+fakeGo,
		"KATL_FAKE_GO_ARGS="+filepath.Join(tmp, "go-args.txt"),
		"KATL_FAKE_CHILD="+fakeChild,
		"KATL_FAKE_CHILD_ARGS="+filepath.Join(tmp, "child-args.txt"),
		"KATL_FAKE_CHILD_ENV="+filepath.Join(tmp, "child-env.txt"),
		"KATL_FAKE_CHILD_WORLD_SCENARIO=malformed result scenario",
		"KATL_FAKE_CHILD_WORLD_RESULT=malformed",
		"KATL_VMTEST_RUN_ID=run-malformed-scenario-result",
		"KATL_VMTEST_RUN_DIR="+runDir,
		"TMPDIR="+tmp,
	)
	output, err := cmd.CombinedOutput()
	if exitCode(err) != 1 {
		t.Fatalf("vmtest-run exit = %v, want 1\n%s", err, output)
	}
	summary := readSummary(t, filepath.Join(runDir, "summary.json"))
	if len(summary.Scenarios) != 1 || summary.Scenarios[0].Status != "setup-failed" || !strings.Contains(summary.Scenarios[0].FailureSummary, "invalid JSON") {
		t.Fatalf("scenarios = %#v", summary.Scenarios)
	}
}

func TestVMTestRunFailsStaleScenarioResult(t *testing.T) {
	repo := scriptTestRepoRoot(t)
	tmp := t.TempDir()
	fakeGo, fakeChild := writeFakeGoTools(t, tmp)
	host := writeFakeHostTools(t, tmp, true)
	runDir := filepath.Join(tmp, "run")

	cmd := exec.Command(filepath.Join(repo, "scripts", "vmtest-run"), "./internal/vmtest", "-run", "^TestFakeScenario$")
	cmd.Dir = repo
	cmd.Env = appendHostEnv(os.Environ(), host,
		"KATL_VMTEST_GO="+fakeGo,
		"KATL_FAKE_GO_ARGS="+filepath.Join(tmp, "go-args.txt"),
		"KATL_FAKE_CHILD="+fakeChild,
		"KATL_FAKE_CHILD_ARGS="+filepath.Join(tmp, "child-args.txt"),
		"KATL_FAKE_CHILD_ENV="+filepath.Join(tmp, "child-env.txt"),
		"KATL_FAKE_CHILD_WORLD_SCENARIO=stale result scenario",
		"KATL_FAKE_CHILD_WORLD_RESULT_RUN_ID=previous-run",
		"KATL_VMTEST_RUN_ID=run-stale-scenario-result",
		"KATL_VMTEST_RUN_DIR="+runDir,
		"TMPDIR="+tmp,
	)
	output, err := cmd.CombinedOutput()
	if exitCode(err) != 1 {
		t.Fatalf("vmtest-run exit = %v, want 1\n%s", err, output)
	}
	summary := readSummary(t, filepath.Join(runDir, "summary.json"))
	if len(summary.Scenarios) != 1 || summary.Scenarios[0].Status != "setup-failed" || !strings.Contains(summary.Scenarios[0].FailureSummary, "another run") {
		t.Fatalf("scenarios = %#v", summary.Scenarios)
	}
}

func TestVMTestRunPropagatesChildExit(t *testing.T) {
	repo := scriptTestRepoRoot(t)
	tmp := t.TempDir()
	fakeGo, fakeChild := writeFakeGoTools(t, tmp)
	host := writeFakeHostTools(t, tmp, false)
	runDir := filepath.Join(tmp, "run")

	cmd := exec.Command(filepath.Join(repo, "scripts", "vmtest-run"), "./internal/vmtest", "-run", "Nspawn")
	cmd.Dir = repo
	cmd.Env = appendHostEnv(os.Environ(), host,
		"KATL_VMTEST_GO="+fakeGo,
		"KATL_FAKE_GO_ARGS="+filepath.Join(tmp, "go-args.txt"),
		"KATL_FAKE_CHILD="+fakeChild,
		"KATL_FAKE_CHILD_ARGS="+filepath.Join(tmp, "child-args.txt"),
		"KATL_FAKE_CHILD_ENV="+filepath.Join(tmp, "child-env.txt"),
		"KATL_FAKE_CHILD_EXIT=7",
		"KATL_VMTEST_RUN_ID=run-2",
		"KATL_VMTEST_RUN_DIR="+runDir,
		"KATL_VMTEST_KEEP=failed",
		"TMPDIR="+tmp,
	)
	output, err := cmd.CombinedOutput()
	if exitCode(err) != 7 {
		t.Fatalf("vmtest-run exit = %v, want 7\n%s", err, output)
	}
	if !strings.Contains(string(output), "vmtest run dir: "+runDir) {
		t.Fatalf("output missing run dir %q:\n%s", runDir, output)
	}

	summary := readSummary(t, filepath.Join(runDir, "summary.json"))
	if summary.Status != "failed" || summary.ExitCode != 7 {
		t.Fatalf("summary = %#v", summary)
	}
	if strings.Contains(readScriptFile(t, host.ipLog), "link del katl-vmtest0") {
		t.Fatalf("failed run cleaned bridge despite keep=failed:\n%s", readScriptFile(t, host.ipLog))
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

func TestVMTestRunHostSkipped(t *testing.T) {
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
		"KATL_VMTEST_HOST_POLICY=skip",
		"KATL_VMTEST_RUN_ID=run-skip",
		"KATL_VMTEST_RUN_DIR="+runDir,
		"TMPDIR="+tmp,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("vmtest-run host-skip failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "host-skipped") {
		t.Fatalf("output missing host-skipped:\n%s", output)
	}
	if _, err := os.Stat(goArgsPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("go test ran for host-skipped world, stat err = %v", err)
	}
	world, err := LoadWorld(filepath.Join(runDir, "world.json"))
	if err != nil {
		t.Fatalf("LoadWorld() error = %v", err)
	}
	if world.Capabilities["qemu"] != WorldStatusHostSkipped {
		t.Fatalf("qemu capability = %q", world.Capabilities["qemu"])
	}
	caps := readCapabilities(t, filepath.Join(runDir, "host-capabilities.json"))
	if !contains(caps.Missing, "qemu") {
		t.Fatalf("missing capabilities = %#v", caps.Missing)
	}
	summary := readSummary(t, filepath.Join(runDir, "summary.json"))
	if summary.Status != "host-skipped" || summary.ExitCode != 0 {
		t.Fatalf("summary = %#v", summary)
	}
}

func TestVMTestRunRequiredHostGapFails(t *testing.T) {
	repo := scriptTestRepoRoot(t)
	tmp := t.TempDir()
	fakeGo, fakeChild := writeFakeGoTools(t, tmp)
	host := writeFakeHostTools(t, tmp, true)
	runDir := filepath.Join(tmp, "run")

	cmd := exec.Command(filepath.Join(repo, "scripts", "vmtest-run"))
	cmd.Dir = repo
	cmd.Env = appendHostEnv(os.Environ(), host,
		"KATL_VMTEST_GO="+fakeGo,
		"KATL_FAKE_GO_ARGS="+filepath.Join(tmp, "go-args.txt"),
		"KATL_FAKE_CHILD="+fakeChild,
		"KATL_FAKE_CHILD_ARGS="+filepath.Join(tmp, "child-args.txt"),
		"KATL_FAKE_CHILD_ENV="+filepath.Join(tmp, "child-env.txt"),
		"KATL_VMTEST_QEMU="+filepath.Join(tmp, "missing-qemu"),
		"KATL_VMTEST_RUN_ID=run-required",
		"KATL_VMTEST_RUN_DIR="+runDir,
		"TMPDIR="+tmp,
	)
	output, err := cmd.CombinedOutput()
	if exitCode(err) != 1 {
		t.Fatalf("vmtest-run exit = %v, want 1\n%s", err, output)
	}
	world, loadErr := LoadWorld(filepath.Join(runDir, "world.json"))
	if loadErr != nil {
		t.Fatalf("LoadWorld() error = %v", loadErr)
	}
	if world.Capabilities["qemu"] != WorldStatusFailed {
		t.Fatalf("qemu capability = %q", world.Capabilities["qemu"])
	}
	summary := readSummary(t, filepath.Join(runDir, "summary.json"))
	if summary.Status != "setup-failed" || summary.ExitCode != 1 {
		t.Fatalf("summary = %#v", summary)
	}
}

func TestVMTestRunExplicitSelectionContinuesWithHostGap(t *testing.T) {
	repo := scriptTestRepoRoot(t)
	tmp := t.TempDir()
	fakeGo, fakeChild := writeFakeGoTools(t, tmp)
	host := writeFakeHostTools(t, tmp, true)
	runDir := filepath.Join(tmp, "run")
	goArgsPath := filepath.Join(tmp, "go-args.txt")

	cmd := exec.Command(filepath.Join(repo, "scripts", "vmtest-run"), "./internal/vmtest", "-run", "^TestDoesNotNeedQEMU$")
	cmd.Dir = repo
	cmd.Env = appendHostEnv(os.Environ(), host,
		"KATL_VMTEST_GO="+fakeGo,
		"KATL_FAKE_GO_ARGS="+goArgsPath,
		"KATL_FAKE_CHILD="+fakeChild,
		"KATL_FAKE_CHILD_ARGS="+filepath.Join(tmp, "child-args.txt"),
		"KATL_FAKE_CHILD_ENV="+filepath.Join(tmp, "child-env.txt"),
		"KATL_VMTEST_QEMU="+filepath.Join(tmp, "missing-qemu"),
		"KATL_VMTEST_RUN_ID=run-explicit-host-gap",
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
		"-count=1",
		"-exec",
		filepath.Join(repo, "scripts", "vmtest-exec"),
		"./internal/vmtest",
		"-run",
		"^TestDoesNotNeedQEMU$",
	}) {
		t.Fatalf("go args = %#v", goArgs)
	}
	caps := readCapabilities(t, filepath.Join(runDir, "host-capabilities.json"))
	if !contains(caps.Missing, "qemu") {
		t.Fatalf("missing capabilities = %#v", caps.Missing)
	}
	summary := readSummary(t, filepath.Join(runDir, "summary.json"))
	if summary.Status != "passed" || summary.ExitCode != 0 {
		t.Fatalf("summary = %#v", summary)
	}
}

func TestVMTestRunNspawnSelectionAvoidsVMHostSetup(t *testing.T) {
	repo := scriptTestRepoRoot(t)
	tmp := t.TempDir()
	fakeGo, fakeChild := writeFakeGoTools(t, tmp)
	host := writeFakeHostTools(t, tmp, false)
	runDir := filepath.Join(tmp, "run")
	goArgsPath := filepath.Join(tmp, "go-args.txt")

	cmd := exec.Command(filepath.Join(repo, "scripts", "vmtest-run"), "./internal/vmtest", "-run", "Nspawn", "-timeout", "2m")
	cmd.Dir = repo
	cmd.Env = appendHostEnv(os.Environ(), host,
		"KATL_VMTEST_GO="+fakeGo,
		"KATL_FAKE_GO_ARGS="+goArgsPath,
		"KATL_FAKE_CHILD="+fakeChild,
		"KATL_FAKE_CHILD_ARGS="+filepath.Join(tmp, "child-args.txt"),
		"KATL_FAKE_CHILD_ENV="+filepath.Join(tmp, "child-env.txt"),
		"KATL_VMTEST_QEMU="+filepath.Join(tmp, "missing-qemu"),
		"KATL_VMTEST_QEMU_IMG="+filepath.Join(tmp, "missing-qemu-img"),
		"KATL_OVMF_CODE="+filepath.Join(tmp, "missing-code.fd"),
		"KATL_OVMF_VARS="+filepath.Join(tmp, "missing-vars.fd"),
		"KATL_VMTEST_KVM_DEVICE="+filepath.Join(tmp, "missing-kvm"),
		"KATL_VMTEST_VSOCK_DEVICE="+filepath.Join(tmp, "missing-vsock"),
		"KATL_VMTEST_TUN_DEVICE="+filepath.Join(tmp, "missing-tun"),
		"KATL_QEMU_BRIDGE_CONF="+filepath.Join(tmp, "missing-bridge.conf"),
		"KATL_VMTEST_RUN_ID=run-nspawn-only",
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
		"-count=1",
		"-exec",
		filepath.Join(repo, "scripts", "vmtest-exec"),
		"./internal/vmtest",
		"-run",
		"Nspawn",
		"-timeout",
		"2m",
	}) {
		t.Fatalf("go args = %#v", goArgs)
	}
	if ipLog := strings.TrimSpace(readScriptFile(t, host.ipLog)); ipLog != "" {
		t.Fatalf("nspawn-only run touched bridge networking:\n%s", ipLog)
	}
	caps := readCapabilities(t, filepath.Join(runDir, "host-capabilities.json"))
	for _, unexpected := range []string{"qemu", "qemu-img", "ovmf", "kvm", "vsock", "bridge", "tun", "qemu-bridge-helper", "qemu-bridge-acl"} {
		if contains(caps.Missing, unexpected) {
			t.Fatalf("nspawn-only missing capabilities include %q: %#v", unexpected, caps.Missing)
		}
	}
	world, err := LoadWorld(filepath.Join(runDir, "world.json"))
	if err != nil {
		t.Fatalf("LoadWorld() error = %v", err)
	}
	if world.Capabilities["systemd-nspawn"] != WorldStatusPassed {
		t.Fatalf("systemd-nspawn capability = %q", world.Capabilities["systemd-nspawn"])
	}
	if world.Capabilities["qemu"] != WorldStatusDisabled || world.Capabilities["bridge"] != WorldStatusDisabled {
		t.Fatalf("VM capabilities were probed for nspawn-only run: %#v", world.Capabilities)
	}
	summary := readSummary(t, filepath.Join(runDir, "summary.json"))
	if summary.Status != "passed" || summary.ExitCode != 0 {
		t.Fatalf("summary = %#v", summary)
	}
}

func TestVMTestRunNspawnSelectionFailsOnNspawnHostGap(t *testing.T) {
	repo := scriptTestRepoRoot(t)
	tmp := t.TempDir()
	fakeGo, fakeChild := writeFakeGoTools(t, tmp)
	host := writeFakeHostTools(t, tmp, false)
	runDir := filepath.Join(tmp, "run")
	goArgsPath := filepath.Join(tmp, "go-args.txt")

	cmd := exec.Command(filepath.Join(repo, "scripts", "vmtest-run"), "./internal/vmtest", "-run", "Nspawn")
	cmd.Dir = repo
	cmd.Env = appendHostEnv(os.Environ(), host,
		"KATL_VMTEST_GO="+fakeGo,
		"KATL_FAKE_GO_ARGS="+goArgsPath,
		"KATL_FAKE_CHILD="+fakeChild,
		"KATL_FAKE_CHILD_ARGS="+filepath.Join(tmp, "child-args.txt"),
		"KATL_FAKE_CHILD_ENV="+filepath.Join(tmp, "child-env.txt"),
		"KATL_NSPAWN_ALLOW_UNPRIVILEGED=0",
		"KATL_VMTEST_RUN_ID=run-nspawn-host-gap",
		"KATL_VMTEST_RUN_DIR="+runDir,
		"TMPDIR="+tmp,
	)
	output, err := cmd.CombinedOutput()
	if exitCode(err) != 1 {
		t.Fatalf("vmtest-run exit = %v, want 1\n%s", err, output)
	}
	if _, err := os.Stat(goArgsPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("go test ran for nspawn setup failure, stat err = %v", err)
	}
	caps := readCapabilities(t, filepath.Join(runDir, "host-capabilities.json"))
	if !contains(caps.Missing, "nspawn-privileges") {
		t.Fatalf("missing capabilities = %#v", caps.Missing)
	}
	if contains(caps.Missing, "qemu") || contains(caps.Missing, "bridge") {
		t.Fatalf("nspawn host gap included VM capabilities: %#v", caps.Missing)
	}
	summary := readSummary(t, filepath.Join(runDir, "summary.json"))
	if summary.Status != "setup-failed" || summary.ExitCode != 1 {
		t.Fatalf("summary = %#v", summary)
	}
}

func TestVMTestRunFlagOnlySelectionDefaultsPackage(t *testing.T) {
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
		"KATL_VMTEST_QEMU="+filepath.Join(tmp, "missing-qemu"),
		"KATL_VMTEST_RUN_ID=run-flag-only-selection",
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
		"-count=1",
		"-exec",
		filepath.Join(repo, "scripts", "vmtest-exec"),
		"-run",
		"^TestDoesNotNeedQEMU$",
		"-timeout",
		"2m",
		"./...",
	}) {
		t.Fatalf("go args = %#v", goArgs)
	}
	summary := readSummary(t, filepath.Join(runDir, "summary.json"))
	if !reflect.DeepEqual(summary.Args, []string{"-run", "^TestDoesNotNeedQEMU$", "-timeout", "2m", "./..."}) {
		t.Fatalf("summary args = %#v", summary.Args)
	}
	if summary.Status != "passed" || summary.ExitCode != 0 {
		t.Fatalf("summary = %#v", summary)
	}
}

func TestVMTestRunNonSelectionFlagsKeepFullPreflight(t *testing.T) {
	repo := scriptTestRepoRoot(t)
	tmp := t.TempDir()
	fakeGo, fakeChild := writeFakeGoTools(t, tmp)
	host := writeFakeHostTools(t, tmp, true)
	runDir := filepath.Join(tmp, "run")
	goArgsPath := filepath.Join(tmp, "go-args.txt")

	cmd := exec.Command(filepath.Join(repo, "scripts", "vmtest-run"), "-timeout", "2m", "-json")
	cmd.Dir = repo
	cmd.Env = appendHostEnv(os.Environ(), host,
		"KATL_VMTEST_GO="+fakeGo,
		"KATL_FAKE_GO_ARGS="+goArgsPath,
		"KATL_FAKE_CHILD="+fakeChild,
		"KATL_FAKE_CHILD_ARGS="+filepath.Join(tmp, "child-args.txt"),
		"KATL_FAKE_CHILD_ENV="+filepath.Join(tmp, "child-env.txt"),
		"KATL_VMTEST_QEMU="+filepath.Join(tmp, "missing-qemu"),
		"KATL_VMTEST_RUN_ID=run-non-selection-preflight",
		"KATL_VMTEST_RUN_DIR="+runDir,
		"TMPDIR="+tmp,
	)
	output, err := cmd.CombinedOutput()
	if exitCode(err) != 1 {
		t.Fatalf("vmtest-run exit = %v, want 1\n%s", err, output)
	}
	if _, err := os.Stat(goArgsPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("go test ran for setup-failed world, stat err = %v", err)
	}
	summary := readSummary(t, filepath.Join(runDir, "summary.json"))
	if !reflect.DeepEqual(summary.Args, []string{"-timeout", "2m", "-json", "./..."}) {
		t.Fatalf("summary args = %#v", summary.Args)
	}
	if summary.Status != "setup-failed" || summary.ExitCode != 1 {
		t.Fatalf("summary = %#v", summary)
	}
}

func TestVMTestRunExplicitSuitePackageKeepsFullPreflight(t *testing.T) {
	repo := scriptTestRepoRoot(t)
	tmp := t.TempDir()
	fakeGo, fakeChild := writeFakeGoTools(t, tmp)
	host := writeFakeHostTools(t, tmp, true)
	runDir := filepath.Join(tmp, "run")
	goArgsPath := filepath.Join(tmp, "go-args.txt")

	cmd := exec.Command(filepath.Join(repo, "scripts", "vmtest-run"), "./...", "-timeout", "2m")
	cmd.Dir = repo
	cmd.Env = appendHostEnv(os.Environ(), host,
		"KATL_VMTEST_GO="+fakeGo,
		"KATL_FAKE_GO_ARGS="+goArgsPath,
		"KATL_FAKE_CHILD="+fakeChild,
		"KATL_FAKE_CHILD_ARGS="+filepath.Join(tmp, "child-args.txt"),
		"KATL_FAKE_CHILD_ENV="+filepath.Join(tmp, "child-env.txt"),
		"KATL_VMTEST_QEMU="+filepath.Join(tmp, "missing-qemu"),
		"KATL_VMTEST_RUN_ID=run-explicit-suite-preflight",
		"KATL_VMTEST_RUN_DIR="+runDir,
		"TMPDIR="+tmp,
	)
	output, err := cmd.CombinedOutput()
	if exitCode(err) != 1 {
		t.Fatalf("vmtest-run exit = %v, want 1\n%s", err, output)
	}
	if _, err := os.Stat(goArgsPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("go test ran for setup-failed world, stat err = %v", err)
	}
	summary := readSummary(t, filepath.Join(runDir, "summary.json"))
	if !reflect.DeepEqual(summary.Args, []string{"./...", "-timeout", "2m"}) {
		t.Fatalf("summary args = %#v", summary.Args)
	}
	if summary.Status != "setup-failed" || summary.ExitCode != 1 {
		t.Fatalf("summary = %#v", summary)
	}
}

func TestVMTestRunKubectlGapFails(t *testing.T) {
	repo := scriptTestRepoRoot(t)
	tmp := t.TempDir()
	fakeGo, fakeChild := writeFakeGoTools(t, tmp)
	host := writeFakeHostTools(t, tmp, true)
	if err := os.Remove(host.kubectl); err != nil {
		t.Fatalf("Remove(%s) error = %v", host.kubectl, err)
	}
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
		"KATL_VMTEST_KUBECTL="+filepath.Join(tmp, "missing-kubectl"),
		"KATL_VMTEST_RUN_ID=run-kubectl-gap",
		"KATL_VMTEST_RUN_DIR="+runDir,
		"TMPDIR="+tmp,
	)
	output, err := cmd.CombinedOutput()
	if exitCode(err) != 1 {
		t.Fatalf("vmtest-run exit = %v, want 1\n%s", err, output)
	}
	if _, err := os.Stat(goArgsPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("go test ran for setup-failed world, stat err = %v", err)
	}
	world, loadErr := LoadWorld(filepath.Join(runDir, "world.json"))
	if loadErr != nil {
		t.Fatalf("LoadWorld() error = %v", loadErr)
	}
	if world.Capabilities["kubectl"] != WorldStatusFailed {
		t.Fatalf("kubectl capability = %q", world.Capabilities["kubectl"])
	}
	caps := readCapabilities(t, filepath.Join(runDir, "host-capabilities.json"))
	if !contains(caps.Missing, "kubectl") {
		t.Fatalf("missing capabilities = %#v", caps.Missing)
	}
	summary := readSummary(t, filepath.Join(runDir, "summary.json"))
	if summary.Status != "setup-failed" || summary.ExitCode != 1 {
		t.Fatalf("summary = %#v", summary)
	}
}

func TestVMTestRunBridgePrereqGapFails(t *testing.T) {
	repo := scriptTestRepoRoot(t)
	tmp := t.TempDir()
	fakeGo, fakeChild := writeFakeGoTools(t, tmp)
	host := writeFakeHostTools(t, tmp, true)
	if err := os.WriteFile(host.bridgeConf, []byte("allow otherbr0\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", host.bridgeConf, err)
	}
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
		"KATL_VMTEST_RUN_ID=run-bridge-prereq",
		"KATL_VMTEST_RUN_DIR="+runDir,
		"TMPDIR="+tmp,
	)
	output, err := cmd.CombinedOutput()
	if exitCode(err) != 1 {
		t.Fatalf("vmtest-run exit = %v, want 1\n%s", err, output)
	}
	if _, err := os.Stat(goArgsPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("go test ran for setup-failed world, stat err = %v", err)
	}
	world, loadErr := LoadWorld(filepath.Join(runDir, "world.json"))
	if loadErr != nil {
		t.Fatalf("LoadWorld() error = %v", loadErr)
	}
	if world.Capabilities["bridge"] != WorldStatusFailed {
		t.Fatalf("bridge capability = %q", world.Capabilities["bridge"])
	}
	caps := readCapabilities(t, filepath.Join(runDir, "host-capabilities.json"))
	if !contains(caps.Missing, "qemu-bridge-acl") {
		t.Fatalf("missing capabilities = %#v", caps.Missing)
	}
	summary := readSummary(t, filepath.Join(runDir, "summary.json"))
	if summary.Status != "setup-failed" || summary.ExitCode != 1 {
		t.Fatalf("summary = %#v", summary)
	}
}

func TestVMTestRunBridgeACLAllowsFinalLineWithoutNewline(t *testing.T) {
	repo := scriptTestRepoRoot(t)
	tmp := t.TempDir()
	fakeGo, fakeChild := writeFakeGoTools(t, tmp)
	host := writeFakeHostTools(t, tmp, true)
	if err := os.WriteFile(host.bridgeConf, []byte("allow katl-vmtest0"), 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", host.bridgeConf, err)
	}
	runDir := filepath.Join(tmp, "run")

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
	summary := readSummary(t, filepath.Join(runDir, "summary.json"))
	if summary.Status != "passed" || summary.ExitCode != 0 {
		t.Fatalf("summary = %#v", summary)
	}
}

func TestVMTestRunHostCapabilityDiagnostics(t *testing.T) {
	repo := scriptTestRepoRoot(t)
	tmp := t.TempDir()
	fakeGo, fakeChild := writeFakeGoTools(t, tmp)
	host := writeFakeHostTools(t, tmp, true)
	if err := os.WriteFile(host.bridgeConf, []byte("allow otherbr0\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", host.bridgeConf, err)
	}
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
		"KATL_NSPAWN_ALLOW_UNPRIVILEGED=0",
		"KATL_VMTEST_RUN_ID=run-host-diagnostics",
		"KATL_VMTEST_RUN_DIR="+runDir,
		"TMPDIR="+tmp,
	)
	output, err := cmd.CombinedOutput()
	if exitCode(err) != 1 {
		t.Fatalf("vmtest-run exit = %v, want 1\n%s", err, output)
	}
	if _, err := os.Stat(goArgsPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("go test ran for setup-failed world, stat err = %v", err)
	}
	for _, want := range []string{
		"setup failed: missing required host capabilities",
		"  - nspawn-privileges:",
		"run with sudo",
		"KATL_NSPAWN_ALLOW_UNPRIVILEGED=1",
		"  - qemu-bridge-acl:",
		host.bridgeConf,
		"allow katl-vmtest0",
		"KATL_QEMU_BRIDGE_CONF",
		"vmtest run dir: " + runDir,
	} {
		if !strings.Contains(string(output), want) {
			t.Fatalf("output missing %q:\n%s", want, output)
		}
	}
	caps := readCapabilities(t, filepath.Join(runDir, "host-capabilities.json"))
	for _, want := range []string{"nspawn-privileges", "qemu-bridge-acl"} {
		if !contains(caps.Missing, want) {
			t.Fatalf("missing capabilities = %#v", caps.Missing)
		}
	}
}

func TestVMTestRunBridgeCreateFailureIsStructured(t *testing.T) {
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
	if exitCode(err) != 1 {
		t.Fatalf("vmtest-run exit = %v, want 1\n%s", err, output)
	}
	if strings.Contains(string(output), "synthetic netlink failure") {
		t.Fatalf("vmtest-run leaked raw ip stderr:\n%s", output)
	}
	if _, err := os.Stat(goArgsPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("go test ran for setup-failed world, stat err = %v", err)
	}
	caps := readCapabilities(t, filepath.Join(runDir, "host-capabilities.json"))
	if !contains(caps.Missing, "bridge") {
		t.Fatalf("missing capabilities = %#v", caps.Missing)
	}
	summary := readSummary(t, filepath.Join(runDir, "summary.json"))
	if summary.Status != "setup-failed" || summary.ExitCode != 1 {
		t.Fatalf("summary = %#v", summary)
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
	summary := readSummary(t, filepath.Join(runDir, "summary.json"))
	if summary.Status != "setup-failed" || summary.ExitCode != 2 {
		t.Fatalf("summary = %#v", summary)
	}
}

func TestVMTestRunCleansCreatedBridge(t *testing.T) {
	repo := scriptTestRepoRoot(t)
	tmp := t.TempDir()
	fakeGo, fakeChild := writeFakeGoTools(t, tmp)
	host := writeFakeHostTools(t, tmp, false)
	runDir := filepath.Join(tmp, "run")

	cmd := exec.Command(filepath.Join(repo, "scripts", "vmtest-run"), "./internal/vmtest")
	cmd.Dir = repo
	cmd.Env = appendHostEnv(os.Environ(), host,
		"KATL_VMTEST_GO="+fakeGo,
		"KATL_FAKE_GO_ARGS="+filepath.Join(tmp, "go-args.txt"),
		"KATL_FAKE_CHILD="+fakeChild,
		"KATL_FAKE_CHILD_ARGS="+filepath.Join(tmp, "child-args.txt"),
		"KATL_FAKE_CHILD_ENV="+filepath.Join(tmp, "child-env.txt"),
		"KATL_VMTEST_KEEP=never",
		"KATL_VMTEST_RUN_ID=run-cleanup",
		"KATL_VMTEST_RUN_DIR="+runDir,
		"TMPDIR="+tmp,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("vmtest-run failed: %v\n%s", err, output)
	}
	ipLog := readScriptFile(t, host.ipLog)
	for _, want := range []string{
		"link add name katl-vmtest0 type bridge",
		"addr add 10.77.0.1/24 dev katl-vmtest0",
		"link set katl-vmtest0 up",
		"link del katl-vmtest0",
	} {
		if !strings.Contains(ipLog, want) {
			t.Fatalf("ip log missing %q:\n%s", want, ipLog)
		}
	}
}

func writeFakeGoTools(t *testing.T, dir string) (string, string) {
	t.Helper()
	fakeGo := filepath.Join(dir, "go")
	fakeChild := filepath.Join(dir, "fake-test-binary")
	writeExecutable(t, fakeGo, `#!/usr/bin/env bash
set -euo pipefail
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
    printf '{"Action":"pass","Package":"fake/vmtest"}\n'
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

type vmtestRunSummary struct {
	Status           string                  `json:"status"`
	ExitCode         int                     `json:"exitCode"`
	ChildExitCode    int                     `json:"childExitCode"`
	GoTestLog        string                  `json:"goTestLog"`
	Args             []string                `json:"args"`
	SelectedPackages []string                `json:"selectedPackages"`
	Counts           map[string]int          `json:"counts"`
	Scenarios        []vmtestScenarioSummary `json:"scenarios"`
}

type vmtestScenarioSummary struct {
	Name           string `json:"name"`
	Status         string `json:"status"`
	ManifestPath   string `json:"manifestPath"`
	ResultPath     string `json:"resultPath"`
	FailureSummary string `json:"failureSummary"`
}

func readSummary(t *testing.T, path string) vmtestRunSummary {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, err)
	}
	var summary vmtestRunSummary
	if err := json.Unmarshal(data, &summary); err != nil {
		t.Fatalf("Unmarshal(%s) error = %v", path, err)
	}
	return summary
}

type vmtestHostCapabilities struct {
	Missing []string `json:"missing"`
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
