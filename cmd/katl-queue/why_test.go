package main

import (
	"slices"
	"strings"
	"testing"
)

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
	jsonLog := strings.Join([]string{
		`{"Action":"start","Package":"p"}`,
		`{"Action":"output","Package":"p","Test":"TestX","Output":"=== RUN   TestX\n"}`,
		`{"Action":"output","Package":"p","Test":"TestX","Output":"    x_test.go:210: context: katlctl failed: exit status 1\n"}`,
		`{"Action":"output","Package":"p","Test":"TestX","Output":"--- FAIL: TestX (132.15s)\n"}`,
		`{"Action":"output","Package":"p","Output":"FAIL\tp\t132.214s\n"}`,
		`{"Action":"fail","Package":"p"}`,
	}, "\n")
	plainLog := "=== RUN   TestX\n    x_test.go:210: context: katlctl failed: exit status 1\n--- FAIL: TestX (132.15s)\nFAIL\tp\t132.214s\n"
	want := []string{
		"--- FAIL: TestX (132.15s)",
		"x_test.go:210: context: katlctl failed: exit status 1",
		"FAIL\tp\t132.214s",
	}
	for name, log := range map[string]string{"json": jsonLog, "text": plainLog} {
		if got := testLogExcerpt([]byte(log), 8); !slices.Equal(got, want) {
			t.Errorf("%s: testLogExcerpt() = %q, want %q", name, got, want)
		}
	}

	if got := testLogExcerpt([]byte("error: failed to get pool 'tank'\nall good\n"), 8); len(got) != 0 {
		t.Errorf("testLogExcerpt() on a log without test failures = %q, want none", got)
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
