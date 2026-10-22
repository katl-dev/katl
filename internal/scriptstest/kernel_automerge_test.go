package scriptstest

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestKernelPromotion(t *testing.T) {
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
	for _, tc := range []struct {
		name, author, head, state, file, qualification, actions string
		fork, truncated, queryError                             bool
	}{
		{name: "ready", actions: "merge\n"},
		{name: "merged", state: "MERGED"},
		{name: "stale revision", head: "another"},
		{name: "different author", author: "operator"},
		{name: "built-in token author", author: "app/github-actions"},
		{name: "fork", fork: true},
		{name: "unrelated file", file: ".github/workflows/release-artifacts.yml"},
		{name: "truncated files", truncated: true},
		{name: "unqualified", qualification: "FAILURE"},
		{name: "qualification pending", qualification: "PENDING"},
		{name: "qualification query failed", queryError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			directory := t.TempDir()
			author, head, state := "app/katl-automation", "qualified", "OPEN"
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
			qualification := "SUCCESS"
			if tc.qualification != "" {
				qualification = tc.qualification
			}
			for name, contents := range map[string]string{
				"proposal.json": string(proposal), "files-input.json": string(files),
				"qualification-input.json": `{"name":"Kernel Input Qualification","state":"` + qualification + `"}`,
				"actions":                  "",
				"gh": `#!/usr/bin/env bash
set -euo pipefail
case "$1 $2" in
  'pr list') cat proposal.json ;;
  'pr view')
    cat files-input.json ;;
  'pr checks')
    [[ "$QUERY_ERROR" != true ]] || exit 1
    cat qualification-input.json ;;
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
				"GH_REPO=katl-dev/katl", "BRANCH=automation/kernels-test", "QUALIFIED_SHA=qualified",
				"EXPECTED_AUTHOR=app/katl-automation", "QUERY_ERROR="+fmt.Sprint(tc.queryError))
			output, runErr := command.CombinedOutput()
			actions, err := os.ReadFile(filepath.Join(directory, "actions"))
			if err != nil {
				t.Fatal(err)
			}
			if string(actions) != tc.actions {
				t.Fatalf("promotion actions = %q, want %q (error %v): %s", actions, tc.actions, runErr, output)
			}

			if tc.queryError && runErr == nil {
				t.Fatal("check query error did not fail promotion")
			}
			if !tc.queryError && tc.actions != "" && runErr != nil {
				t.Fatalf("promotion failed: %v: %s", runErr, output)
			}
		})
	}
}
