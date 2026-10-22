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
		{"CLI configuration", []string{"cmd/katlctl/config_compatibility.go"}, []string{"config", "management", "two-node"}},
		{"CLI rotation", []string{"cmd/katlctl/management_rotation.go"}, []string{"management"}},
		{"CLI rotation and docs", []string{"cmd/katlctl/management_rotation.go", "docs/operations/install.md"}, []string{"management"}},
		{"shared CLI", []string{"cmd/katlctl/main.go"}, allNames()},
		{"shared configuration input", []string{"cmd/katlctl/config_input.go"}, allNames()},
		{"CLI install", []string{"cmd/katlctl/install.go"}, allNames()},
		{"CLI upgrade", []string{"cmd/katlctl/upgrade_reboot.go"}, allNames()},
		{"internal CLI", []string{"internal/katlctl/workstation/config.go"}, allNames()},
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
			slices.Sort(got)
			slices.Sort(tc.want)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("groups = %v, want %v", got, tc.want)
			}
		})
	}
}

func allNames() []string {
	return []string{"boot", "install", "storage", "config", "management", "update", "two-node", "three-node-install"}
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
			slices.Sort(names)
			slices.Sort(tc.want)
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

func TestKernelChanges(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		want        bool
	}{
		{"empty", "", false},
		{"documentation", "docs/development/ci.md\x00", false},
		{"unrelated command", "cmd/katlctl/main.go\x00", false},
		{"mixed with lock", "README.md\x00kernels/fedora-44/x86_64/standard.json\x00", true},
		{"LTS lock", "kernels/fedora-44/x86_64/lts.json\x00", true},
		{"module source", "internal/kernelmodule/compose.go\x00", true},
		{"extension recipe", "extensions/drbd9/build.sh\x00", true},
		{"runtime hook", "mkosi.profiles/runtime/mkosi.postinst\x00", true},
		{"builder configuration", "mkosi.conf.d/packages.conf\x00", true},
		{"Go dependency", "go.sum\x00", true},
		{"builder tool", "scripts/mkosi\x00", true},
		{"workflow", ".github/workflows/kernel-updates.yml\x00", true},
		{"planner", "cmd/katl-vmtest-plan/main.go\x00", true},
		{"directory boundary", "kernels-not-related/file\x00", false},
		{"deleted or renamed module", "docs/moved.go\x00internal/kernelmodule/old.go\x00", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			if err := run([]string{"--kernel-changes"}, strings.NewReader(tc.input), &output); err != nil {
				t.Fatal(err)
			}
			var got bool
			if err := json.Unmarshal(output.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("kernel qualification = %v, want %v", got, tc.want)
			}
		})
	}
}
