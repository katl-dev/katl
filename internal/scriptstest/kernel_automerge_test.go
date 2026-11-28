package scriptstest

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestAutomationPromotion(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), ".github/workflows/kernel-automerge.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct{ Run string }
		}
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	var scripts []string
	for _, step := range workflow.Jobs["merge"].Steps {
		scripts = append(scripts, step.Run)
	}
	type promotionCase struct {
		name, branch, author, head, state, file, actions string
		fork, truncated                                  bool
	}
	cases := []promotionCase{
		{name: "merged", state: "MERGED"},
		{name: "stale revision", head: "another"},
		{name: "different author", author: "operator"},
		{name: "built-in token author", author: "app/github-actions"},
		{name: "fork", fork: true},
		{name: "truncated files", truncated: true},
		{name: "builder branch with two files", branch: "automation/mkosi-builder-test", file: "mkosi-builder.lock", truncated: true},
	}
	// The admission policy restates the same allowlist and reads these
	// samples too.
	samples, err := os.ReadFile(filepath.Join(repoRoot(t), "internal/scriptstest/testdata/automation-paths.json"))
	if err != nil {
		t.Fatal(err)
	}
	var paths []struct {
		Name, Branch, File string
		Accepted           bool
	}
	if err := json.Unmarshal(samples, &paths); err != nil {
		t.Fatal(err)
	}
	for _, p := range paths {
		tc := promotionCase{name: p.Name, branch: p.Branch, file: p.File}
		if p.Accepted {
			tc.actions = "merge\n"
		}
		cases = append(cases, tc)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			directory := t.TempDir()
			branch, author, head, state := "automation/kernels-test", "app/katl-automation", "qualified", "OPEN"
			if tc.branch != "" {
				branch = tc.branch
			}
			if tc.author != "" {
				author = tc.author
			}
			if tc.head != "" {
				head = tc.head
			}
			if tc.state != "" {
				state = tc.state
			}
			proposal, err := json.Marshal(map[string]any{
				"number": 365, "headRefOid": head, "author": map[string]string{"login": author},
				"isCrossRepository": tc.fork, "state": state,
			})
			if err != nil {
				t.Fatal(err)
			}
			file, count := "kernels/fedora-44/x86_64/standard.json", 1
			if tc.file != "" {
				file = tc.file
			}
			if tc.truncated {
				count = 2
			}
			files, err := json.Marshal(map[string]any{"changedFiles": count, "files": []map[string]string{{"path": file}}})
			if err != nil {
				t.Fatal(err)
			}
			for name, contents := range map[string]string{
				"proposal.json": string(proposal), "files-input.json": string(files),
				"actions": "",
				"gh": `#!/usr/bin/env bash
set -euo pipefail
case "$1 $2" in
  'pr list') cat proposal.json ;;
  'pr view')
    cat files-input.json ;;
  'pr merge')
    [[ "$*" == *'--auto --squash --match-head-commit qualified' ]]
    echo merge >> actions ;;
  *) exit 2 ;;
esac
`,
			} {
				if err := os.WriteFile(filepath.Join(directory, name), []byte(contents), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			command := exec.Command("bash", "-euo", "pipefail", "-c", strings.Join(scripts, "\n"))
			command.Dir = directory
			command.Env = append(os.Environ(), "PATH="+directory+string(os.PathListSeparator)+os.Getenv("PATH"),
				"GH_REPO=katl-dev/katl", "BRANCH="+branch, "QUALIFIED_SHA=qualified",
				"EXPECTED_AUTHOR=app/katl-automation")
			output, runErr := command.CombinedOutput()
			actions, err := os.ReadFile(filepath.Join(directory, "actions"))
			if err != nil {
				t.Fatal(err)
			}
			if string(actions) != tc.actions {
				t.Fatalf("promotion actions = %q, want %q (error %v): %s", actions, tc.actions, runErr, output)
			}

			if tc.actions != "" && runErr != nil {
				t.Fatalf("promotion failed: %v: %s", runErr, output)
			}
		})
	}
}
