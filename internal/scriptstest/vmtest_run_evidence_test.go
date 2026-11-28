package scriptstest

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/workstationtest"
)

const evidenceFakeGo = `#!/usr/bin/env bash
if [[ -n "$KATL_SCRIPTTEST_ENV_DUMP" ]]; then
  { printf "ARGS %s\n" "$*"; env; } >"$KATL_SCRIPTTEST_ENV_DUMP"
  [[ ! -e "$HOME/operator-context" ]] || exit 10
  # The unrelated path travels outside the environment, which names inputs.
  unbound="$(<"$KATL_SCRIPTTEST_ENV_DUMP.unbound")"
  [[ -n "$unbound" && ! -e "$unbound" ]] || exit 11
  [[ -d "$KATL_VMTEST_HOST_STATE_DIR" && -d "$KATL_VMTEST_CACHE_DIR" ]] || exit 12
  [[ "$(<"$KATL_SCRIPTTEST_INPUT")" == "input artifact" ]] || exit 13
  ! (printf changed >>"$KATL_SCRIPTTEST_INPUT") 2>/dev/null || exit 14
  printf 'guest input\n' >"$TMPDIR/guest-input"
fi
node="$KATL_VMTEST_RUN_DIR/scenarios/smoke/nodes/n1"
mkdir -p "$node/disks" "$node/vm"
mkdir -p "$node/vm/runtime-serial-network-evidence/files"
printf '{}\n' >"$KATL_VMTEST_RUN_DIR/scenarios/smoke/result.json"
printf 'disk\n' >"$node/disks/root.qcow2"
printf 'serial\n' >"$node/vm/runtime-serial.log"
printf '0\n' >"$node/vm/runtime-serial-network-evidence/files/content"
if [[ -n "$KATL_SCRIPTTEST_NO_TESTS" ]]; then
  printf 'testing: warning: no tests to run\nok  \texample/pkg\t0.0s [no tests to run]\n'
  exit 0
fi
printf '=== RUN   TestAlpha\n'
if [[ -n "$KATL_SCRIPTTEST_JOINED" ]]; then
  printf 'sshd-session[335]: Recei'
fi
printf -- '--- PASS: TestAlpha (1.00s)\n--- SKIP: TestBeta (0.00s)\n'
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
	return runEvidenceVMTestPattern(t, tmpDir, runID, "^TestAlpha$", fail)
}

func runEvidenceVMTestPattern(t *testing.T, tmpDir, runID, pattern string, fail bool) (string, string, bool) {
	t.Helper()
	repo := repoRoot(t)
	work := workstationtest.WorktreeTempDir(t, repo)
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
	cmd := exec.Command(filepath.Join(repo, "scripts", "vmtest-run"), "--artifact-set=none", "./internal/vmtest", "-run", pattern, "-count=1")
	cmd.Dir = repo
	cmd.Env = append(os.Environ(),
		"TMPDIR="+tmpDir,
		"KATL_SCRIPTTEST_STORAGE="+work,
		"KATL_VMTEST_GO="+filepath.Join(work, "fake-go"),
		"KATL_VMTEST_IMAGE_TOOL="+filepath.Join(work, "fake-tool"),
		"KATL_VMTEST_KUBECTL="+filepath.Join(work, "fake-tool"),
		"KATL_VMTEST_VIRSH="+filepath.Join(work, "fake-virsh"),
		"KATL_VMTEST_RUN_ID="+runID,
		"KATL_VMTEST_CACHE_DIR="+filepath.Join(tmpDir, "cache"),
	)
	if fail {
		cmd.Env = append(cmd.Env, "KATL_SCRIPTTEST_FAIL=1")
	}
	if strings.Contains(pattern, "Gone$") && !strings.Contains(pattern, "|") {
		cmd.Env = append(cmd.Env, "KATL_SCRIPTTEST_NO_TESTS=1")
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
		"scenarios/smoke/nodes/n1/vm/runtime-serial-network-evidence/files/content",
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

func TestVMTestRunFailsWhenRequestedTestsDidNotRun(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		wantOK  bool
		missing []string
	}{
		{"every requested test ran", "^(TestAlpha)$", true, nil},
		{"requested test absent", "^(TestAlpha|TestGone|TestAlsoGone)$", false, []string{"TestGone", "TestAlsoGone"}},
		{"requested test skipped", "^(TestAlpha|TestBeta)$", false, []string{"TestBeta"}},
		{"pattern matches nothing", "^TestGone$", false, []string{"matched no tests"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runDir, output, ok := runEvidenceVMTestPattern(t, t.TempDir(), "unmatched-run", tc.pattern, false)
			if ok != tc.wantOK {
				t.Fatalf("vmtest-run success = %v, want %v:\n%s", ok, tc.wantOK, output)
			}
			if tc.wantOK != strings.Contains(output, "vmtest passed") {
				t.Errorf("vmtest passed in output = %v, want %v:\n%s", !tc.wantOK, tc.wantOK, output)
			}
			for _, want := range tc.missing {
				if !strings.Contains(output, want) {
					t.Errorf("output does not name %q:\n%s", want, output)
				}
			}
			if !exists(filepath.Join(runDir, "go-test.log")) {
				t.Errorf("run lost its go test log:\n%s", output)
			}
		})
	}
}

func TestVMTestRunCountsResultAfterUnterminatedOutput(t *testing.T) {
	t.Setenv("KATL_SCRIPTTEST_JOINED", "1")

	runDir, output, ok := runEvidenceVMTest(t, t.TempDir(), "joined-run", false)
	if !ok {
		t.Fatalf("vmtest-run failed on a passing test whose result follows an unterminated line:\n%s", output)
	}

	summary, err := os.ReadFile(filepath.Join(runDir, "test-summary.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(summary), "--- PASS: TestAlpha") {
		t.Errorf("test summary = %q, want the test listed", summary)
	}
}

func TestVMTestRunRecordsHostHealth(t *testing.T) {
	for _, tt := range []struct {
		name string
		fail bool
		want string
	}{
		{"pass", false, `"outcome": "pass"`},
		{"deterministic failure", true, `"outcome": "fail"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			runDir, output, _ := runEvidenceVMTest(t, t.TempDir(), "health-run", tt.fail)

			data, err := os.ReadFile(filepath.Join(runDir, "host-health.json"))
			if err != nil {
				t.Fatalf("host-health.json missing: %v\n%s", err, output)
			}
			if !strings.Contains(string(data), tt.want) {
				t.Errorf("host-health.json lacks %s:\n%s", tt.want, data)
			}
			if !strings.Contains(output, "vmtest host health: "+strings.Trim(strings.TrimPrefix(tt.want, `"outcome": `), `"`)) {
				t.Errorf("output lacks host health line:\n%s", output)
			}
		})
	}
}

func TestVMTestRunIsolatesWorkstationAndSharesRunDirectory(t *testing.T) {
	poisoned := t.TempDir()
	writeArtifact(t, poisoned, "operator-context", "operator configuration")
	unbound := writeArtifact(t, t.TempDir(), "unrelated", "unrelated temporary file")
	// An explicit input outside the worktree, such as a published bundle.
	input := writeArtifact(t, t.TempDir(), "bundle", "input artifact")
	dump := filepath.Join(workstationtest.WorktreeTempDir(t, repoRoot(t)), "go-env.txt")
	writeArtifact(t, filepath.Dir(dump), "go-env.txt.unbound", unbound)
	goCache := os.Getenv("GOCACHE")
	t.Setenv("KATL_SCRIPTTEST_ENV_DUMP", dump)
	t.Setenv("KATL_SCRIPTTEST_INPUT", input)
	t.Setenv("HOME", poisoned)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(poisoned, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(poisoned, "data"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(poisoned, "runtime"))
	t.Setenv("CONTAINERS_STORAGE_CONF", filepath.Join(poisoned, "storage.conf"))
	t.Setenv("SSH_AUTH_SOCK", filepath.Join(poisoned, "agent.sock"))
	t.Setenv("GH_TOKEN", "poisoned-token")
	t.Setenv("KATLCTL_CONTEXT", "poisoned-cluster")

	t.Setenv("KATL_VMTEST_KEEP", "always")
	runDir, output, ok := runEvidenceVMTest(t, t.TempDir(), "env-run", false)
	if !ok {
		t.Fatalf("vmtest-run failed:\n%s", output)
	}

	raw, err := os.ReadFile(dump)
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	var args string
	for _, line := range strings.Split(string(raw), "\n") {
		if rest, ok := strings.CutPrefix(line, "ARGS "); ok {
			args = rest
		} else if name, value, ok := strings.Cut(line, "="); ok {
			env[name] = value
		}
	}

	for _, name := range []string{"SSH_AUTH_SOCK", "GH_TOKEN", "KATLCTL_CONTEXT", "CONTAINERS_STORAGE_CONF", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_RUNTIME_DIR"} {
		if v, found := env[name]; found {
			t.Errorf("go test inherited %s=%q", name, v)
		}
	}
	if env["HOME"] != poisoned {
		t.Errorf("HOME = %q, want masked caller home at %q", env["HOME"], poisoned)
	}
	if input, err := os.ReadFile(filepath.Join(runDir, "tmp", "guest-input")); err != nil || string(input) != "guest input\n" {
		t.Errorf("guest input must be visible to the host, got %q, %v", input, err)
	}
	if env["GOCACHE"] != goCache {
		t.Errorf("GOCACHE = %q, want the caller's %q so the run keeps a warm build cache", env["GOCACHE"], goCache)
	}
	if env["PATH"] == "" || env["KATL_VMTEST_RUN_DIR"] == "" {
		t.Errorf("go test lost PATH or the run's own settings: PATH=%q KATL_VMTEST_RUN_DIR=%q", env["PATH"], env["KATL_VMTEST_RUN_DIR"])
	}
	if !strings.Contains(" "+args+" ", " -json ") {
		t.Errorf("go test args = %q, want -json", args)
	}
}

func TestVMTestRunRecordsSourceInRunIndex(t *testing.T) {
	t.Setenv("KATL_VMTEST_KEEP", "always")

	runDir, output, ok := runEvidenceVMTest(t, t.TempDir(), "source-run", false)
	if !ok {
		t.Fatalf("vmtest-run failed:\n%s", output)
	}

	raw, err := os.ReadFile(filepath.Join(runDir, "run.json"))
	if err != nil {
		t.Fatal(err)
	}
	var index struct{ Revision, UncommittedDigest string }
	if err := json.Unmarshal(raw, &index); err != nil {
		t.Fatal(err)
	}
	head, err := exec.Command("git", "-C", repoRoot(t), "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	if index.Revision != strings.TrimSpace(string(head)) {
		t.Errorf("run.json revision = %q, want %q", index.Revision, head)
	}
}

func TestVMTestRunChecksTestsRanInHermeticJob(t *testing.T) {
	realGo := workstationtest.Tool(t, "go")
	poisoned := t.TempDir()
	writeArtifact(t, poisoned, "operator-context", "operator configuration")
	t.Setenv("HOME", poisoned)
	t.Setenv("GH_TOKEN", "poisoned-token")
	// The sandbox passes no KATL_ settings to the checker, so the wrapper names
	// its record directly. The worktree is bound into every hermetic job.
	work := workstationtest.WorktreeTempDir(t, repoRoot(t))
	record := filepath.Join(work, "checker-env.txt")
	resourceGo := filepath.Join(work, "resource-go")
	writeExecutable(t, resourceGo, fmt.Sprintf(`#!/usr/bin/env bash
if [[ "$2" == ./cmd/katl-vmtest-ran ]]; then
  { env; [[ ! -e "$HOME/operator-context" ]] || echo CALLER_HOME_VISIBLE; } >'%s'
fi
exec '%s' "$@"
`, record, realGo))
	t.Setenv("KATL_VMTEST_RESOURCE_GO", resourceGo)

	_, output, ok := runEvidenceVMTest(t, t.TempDir(), "ran-run", false)
	if !ok {
		t.Fatalf("vmtest-run failed:\n%s", output)
	}

	raw, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("katl-vmtest-ran did not run: %v", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if line == "CALLER_HOME_VISIBLE" || strings.HasPrefix(line, "GH_TOKEN=") {
			t.Errorf("katl-vmtest-ran ran with the caller's environment: %s", line)
		}
	}
}
