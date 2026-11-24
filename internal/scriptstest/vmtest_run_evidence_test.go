package scriptstest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const evidenceFakeGo = `#!/usr/bin/env bash
node="$KATL_VMTEST_RUN_DIR/scenarios/smoke/nodes/n1"
mkdir -p "$node/disks" "$node/vm"
printf '{}\n' >"$KATL_VMTEST_RUN_DIR/scenarios/smoke/result.json"
printf 'disk\n' >"$node/disks/root.qcow2"
printf 'serial\n' >"$node/vm/runtime-serial.log"
printf '=== RUN   TestAlpha\n--- PASS: TestAlpha (1.00s)\n--- SKIP: TestBeta (0.00s)\n'
if [[ -n "$KATL_SCRIPTTEST_FAIL" ]]; then
  printf -- '--- FAIL: TestGamma (2.00s)\nFAIL\n'
  exit 1
fi
printf 'ok  \texample/pkg\t3.0s\n'
`

// runEvidenceVMTest runs scripts/vmtest-run against fakes and returns the run
// directory, the combined output and whether the run succeeded.
func runEvidenceVMTest(t *testing.T, tmpDir, runID string, fail bool) (string, string, bool) {
	t.Helper()
	repo := repoRoot(t)
	work := t.TempDir()
	writeExecutable(t, filepath.Join(work, "fake-go"), evidenceFakeGo)
	writeExecutable(t, filepath.Join(work, "fake-tool"), "#!/usr/bin/env bash\nexit 0\n")
	writeExecutable(t, filepath.Join(work, "fake-virsh"), `#!/usr/bin/env bash
shift 2
case "$1" in
  uri) printf 'qemu:///system\n' ;;
  net-info) printf 'Name: default\nActive: yes\n' ;;
  net-dumpxml) printf "<network><ip address='10.77.0.1' prefix='24'/></network>\n" ;;
  pool-info) printf 'Name: default\nState: running\n' ;;
  pool-dumpxml) printf '<pool><target><path>%s</path></target></pool>\n' "$KATL_SCRIPTTEST_STORAGE" ;;
  list) ;;
esac
`)
	runDir := filepath.Join(tmpDir, "katl-vmtest", runID)
	cmd := exec.Command(filepath.Join(repo, "scripts", "vmtest-run"), "--artifact-set=none", "./internal/vmtest", "-run", "^TestAlpha$", "-count=1")
	cmd.Dir = repo
	cmd.Env = append(os.Environ(),
		"TMPDIR="+tmpDir,
		"KATL_SCRIPTTEST_STORAGE="+work,
		"KATL_VMTEST_GO="+filepath.Join(work, "fake-go"),
		"KATL_VMTEST_IMAGE_TOOL="+filepath.Join(work, "fake-tool"),
		"KATL_VMTEST_KUBECTL="+filepath.Join(work, "fake-tool"),
		"KATL_VMTEST_VIRSH="+filepath.Join(work, "fake-virsh"),
		"KATL_VMTEST_RUN_ID="+runID,
		"KATL_VMTEST_CACHE_DIR="+filepath.Join(work, "cache"),
		"KATL_VMTEST_HOST_LOCK="+filepath.Join(work, "host.lock"),
	)
	if fail {
		cmd.Env = append(cmd.Env, "KATL_SCRIPTTEST_FAIL=1")
	}
	output, err := cmd.CombinedOutput()
	return runDir, string(output), err == nil
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestVMTestRunKeepsEvidenceOnPass(t *testing.T) {
	runDir, output, ok := runEvidenceVMTest(t, t.TempDir(), "pass-run", false)
	if !ok {
		t.Fatalf("vmtest-run failed:\n%s", output)
	}

	for _, kept := range []string{
		"go-test.log", "run.json", "world.json", "test-summary.txt",
		"scenarios/smoke/result.json",
	} {
		if !exists(filepath.Join(runDir, kept)) {
			t.Errorf("passing run lost evidence %s:\n%s", kept, output)
		}
	}
	log, err := os.ReadFile(filepath.Join(runDir, "go-test.log"))
	if err != nil || !strings.Contains(string(log), "--- PASS: TestAlpha") {
		t.Errorf("go-test.log = %q, %v; want the test results", log, err)
	}
	for _, heavy := range []string{"scenarios/smoke/nodes/n1/disks", "scenarios/smoke/nodes/n1/vm/runtime-serial.log"} {
		if exists(filepath.Join(runDir, heavy)) {
			t.Errorf("passing run kept heavy artifact %s", heavy)
		}
	}
}

func TestVMTestRunKeepsEverythingOnFailure(t *testing.T) {
	runDir, output, ok := runEvidenceVMTest(t, t.TempDir(), "fail-run", true)
	if ok {
		t.Fatalf("vmtest-run succeeded; want failure:\n%s", output)
	}

	for _, kept := range []string{
		"go-test.log", "scenarios/smoke/result.json",
		"scenarios/smoke/nodes/n1/disks/root.qcow2", "scenarios/smoke/nodes/n1/vm/runtime-serial.log",
	} {
		if !exists(filepath.Join(runDir, kept)) {
			t.Errorf("failing run lost %s:\n%s", kept, output)
		}
	}
}

func TestVMTestRunPrintsTestSummary(t *testing.T) {
	for _, fail := range []bool{false, true} {
		_, output, _ := runEvidenceVMTest(t, t.TempDir(), "summary-run", fail)

		wants := []string{"summary-run", "--- PASS: TestAlpha", "--- SKIP: TestBeta"}
		if fail {
			wants = append(wants, "--- FAIL: TestGamma")
		}
		for _, want := range wants {
			if !strings.Contains(output, want) {
				t.Errorf("fail=%v output missing %q:\n%s", fail, want, output)
			}
		}
	}
}

func TestVMTestRunPrunesOnlyOldRecords(t *testing.T) {
	tmp := t.TempDir()
	parent := filepath.Join(tmp, "katl-vmtest")
	old := time.Now().AddDate(0, 0, -30)
	for _, name := range []string{"old-run", "recent-run", "old-foreign"} {
		dir := filepath.Join(parent, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if name == "old-foreign" {
			if err := os.Chtimes(dir, old, old); err != nil {
				t.Fatal(err)
			}
			continue
		}
		marker := filepath.Join(dir, ".katl-vmtest-run-dir")
		if err := os.WriteFile(marker, []byte(name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if name == "old-run" {
			if err := os.Chtimes(marker, old, old); err != nil {
				t.Fatal(err)
			}
		}
	}

	_, output, ok := runEvidenceVMTest(t, tmp, "prune-run", false)
	if !ok {
		t.Fatalf("vmtest-run failed:\n%s", output)
	}

	if exists(filepath.Join(parent, "old-run")) {
		t.Error("record older than the retention period was kept")
	}
	for _, kept := range []string{"recent-run", "old-foreign", "prune-run"} {
		if !exists(filepath.Join(parent, kept)) {
			t.Errorf("pruning removed %s", kept)
		}
	}
}
