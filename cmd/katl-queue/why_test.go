package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestReportArtifactFailures(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	t.Setenv("PASS_LOG", `{"Action":"output","Package":"p","Test":"TestPass","Output":"    pass_test.go:12: simulated failure\n"}
{"Action":"pass","Package":"p","Test":"TestPass"}`)
	t.Setenv("FAIL_LOG", `=== RUN   TestPass
    pass_test.go:12: simulated failure
--- PASS: TestPass (0.00s)
=== RUN   TestFail
    fail_test.go:20: configuration was lost
--- FAIL: TestFail (0.01s)`)
	script := `case "$1 $2" in
"api --paginate") printf '%s\n' passing-123 failing-123 ;;
"run download")
  case "$7" in
  passing-123) printf '%s\n' "$PASS_LOG" ;;
  failing-123) printf '%s\n' "$FAIL_LOG" ;;
  *) exit 90 ;;
  esac > "$9/go-test.log" ;;
*) exit 91 ;;
esac`
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte("#!"+sh+"\n"+script+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	reportArtifactFailures(t.Context(), newGH(), 123, &out)

	want := "\ngo test failures in artifact failing-123\n  | --- FAIL: TestFail (0.01s)\n  | fail_test.go:20: configuration was lost\n"
	if got := out.String(); got != want {
		t.Errorf("artifact report = %q, want %q", got, want)
	}
}

func TestRankExcerpt(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
		limit int
		want  []string
	}{
		{
			name: "test failures outrank earlier setup errors",
			lines: []string{
				"error: could not reach registry",
				"    drbd_test.go:210: context: katlctl failed: exit status 1",
				"--- FAIL: TestDRBD (132.15s)",
				"FAIL\texample.com/katl/internal/vmtest\t132.214s",
				"##[error]Process completed with exit code 1.",
			},
			limit: 8,
			want: []string{
				"--- FAIL: TestDRBD (132.15s)",
				"drbd_test.go:210: context: katlctl failed: exit status 1",
				"FAIL\texample.com/katl/internal/vmtest\t132.214s",
			},
		},
		{
			name:  "panic is a test failure",
			lines: []string{"error: noise", "panic: runtime error: index out of range"},
			limit: 1,
			want:  []string{"panic: runtime error: index out of range"},
		},
		{
			name: "pool probe is suppressed",
			lines: []string{
				"error: failed to get pool 'default'",
				"error: Storage pool not found: no storage pool with matching name 'default'",
			},
			limit: 8,
		},
		{
			name: "similar lines that are not the pool probe are kept",
			lines: []string{
				"error: failed to get pool 'tank'",
				"error: failed to get pool 'default': permission denied",
			},
			limit: 8,
			want: []string{
				"error: failed to get pool 'tank'",
				"error: failed to get pool 'default': permission denied",
			},
		},
		{
			name: "test lines that mention the pool probe are kept",
			lines: []string{
				"error: failed to get pool 'default'",
				"--- FAIL: TestPool (0.1s) error: failed to get pool 'default'",
				"pool_test.go:9: error: failed to get pool 'default'",
			},
			limit: 8,
			want: []string{
				"--- FAIL: TestPool (0.1s) error: failed to get pool 'default'",
				"pool_test.go:9: error: failed to get pool 'default'",
			},
		},
		{
			name:  "repeats are reported once and environment listings skipped",
			lines: []string{"KATL_VMTEST_DEBUG_ON_FAILURE: 0", "--- FAIL: TestA", "--- FAIL: TestA"},
			limit: 8,
			want:  []string{"--- FAIL: TestA"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rankExcerpt(tt.lines, tt.limit); !slices.Equal(got, tt.want) {
				t.Errorf("rankExcerpt() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestErrorExcerptsPrefersTestFailure(t *testing.T) {
	var log []string
	log = append(log, "Drivers\tRun\t2026-11-24T22:00:00.0Z error: failed to get pool 'default'")
	for range 20 {
		log = append(log, "Drivers\tRun\t2026-11-24T22:00:01.0Z digest-mismatch: error")
	}
	log = append(log, "Drivers\tRun\t2026-11-24T22:00:02.0Z --- FAIL: TestLate (1.00s)")
	for i := range 4 {
		log = append(log, "Setup\tRun\t2026-11-24T22:00:03.0Z error: step "+string(rune('a'+i)))
	}

	excerpts := errorExcerpts([]byte(strings.Join(log, "\n")), 2)
	got := excerpts["Drivers"]
	want := []string{"--- FAIL: TestLate (1.00s)"}
	if !slices.Equal(got, want) {
		t.Errorf("errorExcerpts() = %q, want %q", got, want)
	}
	if want := []string{"error: step a", "error: step b"}; !slices.Equal(excerpts["Setup"], want) {
		t.Errorf("errorExcerpts() for a job without test failures = %q, want %q", excerpts["Setup"], want)
	}
}

func TestTestLogExcerpt(t *testing.T) {
	tests := []struct {
		name string
		log  string
		want []string
	}{
		{
			name: "passing test logs",
			log: `=== RUN   TestPass
    pass_test.go:12: read-only ESP failure_reason: simulated
--- PASS: TestPass (0.00s)
PASS`,
		},
		{
			name: "verbose output follows test ownership",
			log: `=== RUN   TestParent
=== RUN   TestParent/pass
    parent_test.go:12: read-only ESP failure_reason: simulated
=== RUN   TestParent/fail
    parent_test.go:15: configuration was lost
=== NAME  TestParent
    parent_test.go:17: collecting diagnostics
--- FAIL: TestParent (0.01s)
    --- PASS: TestParent/pass (0.00s)
    --- FAIL: TestParent/fail (0.01s)`,
			want: []string{
				"--- FAIL: TestParent (0.01s)",
				"--- FAIL: TestParent/fail (0.01s)",
				"parent_test.go:17: collecting diagnostics",
				"parent_test.go:15: configuration was lost",
			},
		},
		{
			name: "buffered output follows result indentation",
			log: `--- FAIL: TestParent (0.01s)
    --- PASS: TestParent/pass (0.00s)
        parent_test.go:12: expected setup error
    --- FAIL: TestParent/fail (0.01s)
        parent_test.go:15: configuration was lost
    parent_test.go:17: collecting diagnostics`,
			want: []string{
				"--- FAIL: TestParent (0.01s)",
				"--- FAIL: TestParent/fail (0.01s)",
				"parent_test.go:15: configuration was lost",
				"parent_test.go:17: collecting diagnostics",
			},
		},
		{
			name: "JSON results distinguish packages and attempts",
			log: `{"Action":"output","Package":"p","Test":"TestX","Output":"    x_test.go:10: read-only ESP failure_reason: simulated\n"}
{"Action":"pass","Package":"p","Test":"TestX"}
{"Action":"output","Package":"q","Test":"TestX","Output":"    x_test.go:20: error in passing package\n"}
{"Action":"output","Package":"p","Test":"TestX","Output":"    x_test.go:30: configuration was lost\n"}
{"Action":"fail","Package":"p","Test":"TestX"}
{"Action":"pass","Package":"q","Test":"TestX"}
{"Action":"output","Package":"p","Output":"FAIL\tp\t0.01s\n"}
{"Action":"fail","Package":"p"}`,
			want: []string{"x_test.go:30: configuration was lost", "FAIL\tp\t0.01s"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := testLogExcerpt([]byte(tt.log), 8)
			slices.Sort(got)
			slices.Sort(tt.want)
			if !slices.Equal(got, tt.want) {
				t.Errorf("testLogExcerpt() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestTestLogExcerptGoTest reads the logs of real go test runs in both output
// formats, so that the parser is checked against what go test prints rather
// than against hand-written logs.
func TestTestLogExcerptGoTest(t *testing.T) {
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		source string
		want   []string
	}{
		{
			name: "test timeout",
			source: `package fixture

import (
	"testing"
	"time"
)

func TestDone(t *testing.T) { t.Log("setup finished") }

func TestHang(t *testing.T) {
	t.Log("waiting for the node")
	time.Sleep(time.Minute)
}
`,
			want: []string{"panic: test timed out after 1s", "x_test.go:11: waiting for the node", "FAIL\texample.com/fixture"},
		},
		{
			name: "build failure",
			source: `package fixture

import "testing"

func TestBuild(t *testing.T) { undefinedHelper(t) }
`,
			want: []string{"./x_test.go:5:32: undefined: undefinedHelper", "FAIL\texample.com/fixture [build failed]"},
		},
		{
			name: "TestMain exit",
			source: `package fixture

import (
	"log"
	"testing"
)

func TestMain(m *testing.M) {
	log.SetFlags(log.Lshortfile)
	log.Fatal("fixture setup failed")
}

func TestNever(t *testing.T) {}
`,
			want: []string{"x_test.go:10: fixture setup failed", "FAIL\texample.com/fixture"},
		},
		{
			name: "panic",
			source: `package fixture

import "testing"

func TestPass(t *testing.T) { t.Log("expected failure_reason: simulated") }

func TestPanic(t *testing.T) {
	t.Log("loading state")
	var state map[string]int
	state["node"] = 1
}
`,
			want: []string{"--- FAIL: TestPanic", "panic: assignment to entry in nil map", "x_test.go:8: loading state", "FAIL\texample.com/fixture"},
		},
		{
			name: "passing package",
			source: `package fixture

import (
	"fmt"
	"testing"
)

func TestPass(t *testing.T) {
	fmt.Println("error: simulated failure")
	t.Log("read-only ESP failure_reason: simulated")
}
`,
		},
	}
	// Durations and the panic recovery annotation vary between runs and Go
	// releases.
	variable := regexp.MustCompile(`( \([0-9.]+s\)|\t[0-9.]+s| \[recovered.*\])$`)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/fixture\n\ngo 1.22\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "x_test.go"), []byte(tt.source), 0o600); err != nil {
				t.Fatal(err)
			}

			for _, format := range []string{"-v", "-json"} {
				cmd := exec.CommandContext(t.Context(), goTool, "test", format, "-count=1", "-timeout=1s", ".")
				cmd.Dir = dir
				cmd.Env = append(os.Environ(), "GOFLAGS=", "GOWORK=off", "GOTOOLCHAIN=local")
				// Every fixture but the passing one makes go test fail.
				log, _ := cmd.CombinedOutput()

				var got []string
				for _, line := range testLogExcerpt(log, 8) {
					got = append(got, variable.ReplaceAllString(line, ""))
				}
				if !slices.Equal(got, tt.want) {
					t.Errorf("go test %s: testLogExcerpt() = %q, want %q\nlog:\n%s", format, got, tt.want, log)
				}
			}
		})
	}
}

func TestDiagnosticsArtifacts(t *testing.T) {
	names := "hosted-vmtest-standard-inputs-0123abcd\nkernel-driver-standard-45502713541\nhosted-vmtest-standard-vm-evidence-45502713541\nkatlctl-0123abcd"
	got := diagnosticsArtifacts(names, 45502713541)
	want := []string{"kernel-driver-standard-45502713541", "hosted-vmtest-standard-vm-evidence-45502713541"}
	if !slices.Equal(got, want) {
		t.Errorf("diagnosticsArtifacts() = %q, want %q", got, want)
	}
}
