package main

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestSelectGroups(t *testing.T) {
	for _, tc := range []struct {
		name  string
		paths []string
		want  []string
	}{
		{"docs", []string{"docs/development/releases.md", "README.md"}, []string{}},
		{"unit tests", []string{"internal/installer/operation/run_test.go"}, []string{}},
		{"CLI unit tests", []string{"cmd/katlctl/main_test.go"}, []string{}},
		{"maintainer tooling", []string{"cmd/katl-release/main.go", "scripts/katl-release"}, []string{}},
		{"CLI", []string{"cmd/katlctl/main.go"}, []string{"config", "management", "two-node"}},
		{"CLI and docs", []string{"internal/katlctl/root.go", "docs/operations/install.md"}, []string{"config", "management", "two-node"}},
		{"shared product", []string{"internal/installer/operation/run.go"}, allNames()},
		{"VM tests", []string{"internal/vmtest/installed_smoke_test.go"}, allNames()},
		{"builder", []string{"scripts/mkosi"}, allNames()},
		{"unknown with docs", []string{"docs/readme.md", "new-input"}, allNames()},
		{"CLI with shared change", []string{"cmd/katlctl/main.go", "go.mod"}, allNames()},
		{"rename into docs", []string{"internal/installer/old.go", "docs/moved.go"}, allNames()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, selected := range selectGroups(tc.paths) {
				got = append(got, selected.Name)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("groups = %v, want %v", got, tc.want)
			}
		})
	}
}

func allNames() []string {
	return []string{"boot", "install", "config", "management", "update", "two-node", "three-node-install"}
}

func TestPlan(t *testing.T) {
	for _, tc := range []struct {
		name  string
		args  []string
		input string
		want  []string
	}{
		{"full by default", nil, "", allNames()},
		{"empty diff", []string{"--changed"}, "", []string{}},
		{"docs with spaces", []string{"--changed"}, "docs/design notes.md\x00", []string{}},
		{"newline in unknown path", []string{"--changed"}, "source\nname\x00", allNames()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			if err := run(tc.args, strings.NewReader(tc.input), &output); err != nil {
				t.Fatal(err)
			}
			var plan struct{ Include []group }
			if err := json.Unmarshal(output.Bytes(), &plan); err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, selected := range plan.Include {
				names = append(names, selected.Name)
			}
			if !slices.Equal(names, tc.want) || strings.Contains(output.String(), "null") {
				t.Fatalf("plan = %s, want groups %v", &output, tc.want)
			}
		})
	}
}

func TestInvalidChanges(t *testing.T) {
	for _, input := range []string{"docs/file.md", "/etc/file\x00", "../file\x00", "a/../file\x00", ".\x00", "\x00"} {
		if err := run([]string{"--changed"}, strings.NewReader(input), &bytes.Buffer{}); err == nil {
			t.Fatalf("accepted invalid changed paths %q", input)
		}
	}
}
