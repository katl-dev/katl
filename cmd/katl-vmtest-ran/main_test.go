package main

import (
	"slices"
	"strings"
	"testing"
)

func TestRequestedTests(t *testing.T) {
	tests := []struct {
		pattern string
		want    []string
	}{
		{"^TestA$", []string{"TestA"}},
		{"^(TestA|TestB|TestC)$", []string{"TestA", "TestB", "TestC"}},
		{"^TestA$|^TestB$", []string{"TestA", "TestB"}},
		{"^(TestA|TestB)$/^sub$", []string{"TestA", "TestB"}},
		{"TestA", nil},
		{"^TestA", nil},
		{"^(TestA|Test.*)$", nil},
		{"^(TestA|TestB)", nil},
		{"^TestA$|Test", nil},
	}
	for _, tc := range tests {
		if got := requestedTests(tc.pattern); !slices.Equal(got, tc.want) {
			t.Errorf("requestedTests(%q) = %v, want %v", tc.pattern, got, tc.want)
		}
	}
}

func TestRunPattern(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"./pkg", "-run", "^A$", "-count=1"}, "^A$"},
		{[]string{"--run=^A$"}, "^A$"},
		{[]string{"-test.run", "^A$"}, "^A$"},
		{[]string{"-run", "^A$", "-run", "^B$"}, "^B$"},
		{[]string{"./pkg", "-args", "-run", "^A$"}, ""},
		{[]string{"./pkg", "-v"}, ""},
	}
	for _, tc := range tests {
		if got := runPattern(tc.args); got != tc.want {
			t.Errorf("runPattern(%v) = %q, want %q", tc.args, got, tc.want)
		}
	}
}

func TestParseOutcomes(t *testing.T) {
	text := "=== RUN   TestA\n--- PASS: TestA (1.00s)\n    --- PASS: TestA/sub (0.5s)\n--- SKIP: TestB (0.00s)\nok  \tpkg\t1s\n"
	json := `{"Action":"pass","Test":"TestC"}` + "\n" + `{"Action":"fail","Test":"TestD/sub"}` + "\n" + `{"Action":"pass","Package":"p"}` + "\n"

	got := parseOutcomes(strings.NewReader(text + json))
	want := map[string]string{"TestA": "pass", "TestB": "skip", "TestC": "pass"}
	if len(got) != len(want) {
		t.Fatalf("outcomes = %v, want %v", got, want)
	}
	for name, action := range want {
		if got[name] != action {
			t.Errorf("outcome[%s] = %q, want %q", name, got[name], action)
		}
	}
}

func TestCheck(t *testing.T) {
	ran := map[string]string{"TestA": "pass", "TestSkipped": "skip"}
	tests := []struct {
		name     string
		pattern  string
		outcomes map[string]string
		wantErr  []string
	}{
		{"all present", "^(TestA)$", ran, nil},
		{"no -run", "", nil, nil},
		{"absent test named", "^(TestA|TestGone)$", ran, []string{"TestGone"}},
		{"every absent test named", "^(TestOne|TestTwo)$", ran, []string{"TestOne", "TestTwo"}},
		{"skipped is not a pass", "^(TestA|TestSkipped)$", ran, []string{"TestSkipped"}},
		{"zero matches", "^TestGone$", nil, []string{"matched no tests"}},
		{"zero matches, other pattern", "Gone.*", nil, []string{"matched no tests"}},
		{"other pattern with a match", "Test.*", ran, nil},
		{"subtest pattern", "^TestA$/^sub$", ran, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := check(tc.pattern, tc.outcomes)
			if len(tc.wantErr) == 0 {
				if err != nil {
					t.Fatalf("check() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("check() = nil, want error naming %v", tc.wantErr)
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

func TestParseOutcomesAfterUnterminatedOutput(t *testing.T) {
	log := "=== RUN   TestA\nsshd-session[335]: Recei--- PASS: TestA (22.24s)\nPASS\n"

	got := parseOutcomes(strings.NewReader(log))
	if got["TestA"] != "pass" {
		t.Errorf("outcomes = %v, want TestA to pass", got)
	}
}

func TestWriteSummary(t *testing.T) {
	tests := []struct {
		name string
		log  string
		want string
	}{
		{
			name: "json events",
			log: `{"Action":"run","Test":"TestWorkerReaddress"}
{"Action":"output","Test":"TestWorkerReaddress/drain","Output":"    --- PASS: TestWorkerReaddress/drain (12.30s)\n"}
{"Action":"pass","Test":"TestWorkerReaddress/drain","Elapsed":12.3}
{"Action":"output","Test":"TestWorkerReaddress","Output":"--- FAIL: TestWorkerReaddress (438.02s)\n"}
{"Action":"fail","Test":"TestWorkerReaddress","Elapsed":438.02}
{"Action":"skip","Test":"TestOther","Elapsed":0}
{"Action":"fail","Package":"example/pkg","Elapsed":440}
`,
			want: "    --- PASS: TestWorkerReaddress/drain (12.30s)\n--- FAIL: TestWorkerReaddress (438.02s)\n--- SKIP: TestOther (0.00s)\n",
		},
		{
			name: "text output",
			log:  "=== RUN   TestA\n    --- PASS: TestA/sub (0.50s)\nsshd-session[335]: Recei--- FAIL: TestA (1.25s)\nFAIL\n",
			want: "    --- PASS: TestA/sub (0.50s)\n--- FAIL: TestA (1.25s)\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got strings.Builder
			if err := writeSummary(&got, strings.NewReader(tc.log)); err != nil {
				t.Fatal(err)
			}
			if got.String() != tc.want {
				t.Errorf("summary =\n%s\nwant\n%s", got.String(), tc.want)
			}
		})
	}
}

func TestParseOutcomesIgnoresQuotedResults(t *testing.T) {
	// TestA prints a header for a test that never ran.
	log := `{"Action":"output","Test":"TestA","Output":"--- PASS: TestGone (1.00s)\n"}` + "\n" +
		`{"Action":"fail","Test":"TestA","Elapsed":1}` + "\n"

	got := parseOutcomes(strings.NewReader(log))
	if len(got) != 1 || got["TestA"] != "fail" {
		t.Errorf("outcomes = %v, want only TestA failing", got)
	}
}
