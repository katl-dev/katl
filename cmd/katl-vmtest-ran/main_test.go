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
