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
		name, author, head, state, file, qualification, requiredState, actions string
		fork, checkError, truncated                                            bool
	}{
		{name: "ready", actions: "merge\ndispatch\n"},
		{name: "merged", state: "MERGED", actions: "dispatch\n"},
		{name: "stale revision", head: "another"},
		{name: "different author", author: "operator"},
		{name: "fork", fork: true},
		{name: "unrelated file", file: ".github/workflows/release-artifacts.yml"},
		{name: "truncated files", truncated: true},
		{name: "unqualified", qualification: "FAILURE"},
		{name: "required check pending", requiredState: "PENDING", actions: "merge\n"},
		{name: "required check failed", requiredState: "FAILURE", actions: "merge\n"},
		{name: "check query failed", checkError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			directory := t.TempDir()
			author, head, state := "app/github-actions", "qualified", "OPEN"
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
    if [[ "$*" == *changedFiles* ]]; then cat files-input.json; else echo MERGED; fi ;;
  'pr checks')
    [[ "$CHECK_ERROR" != true ]] || exit 1
    if [[ "$*" == *--required* ]]; then
      printf '[{"state":"%s"}]\n' "$REQUIRED_STATE"
    else
      cat qualification-input.json
    fi ;;
  'pr merge')
    [[ "$*" == *'--auto --squash --match-head-commit qualified' ]]
    echo merge >> actions ;;
  'api --method')
    [[ "$*" == 'api --method POST repos/katl-dev/katl/dispatches --input dispatch.json' ]]
    jq -e '.event_type == "main-build"' dispatch.json >/dev/null
    echo dispatch >> actions ;;
  *) exit 2 ;;
esac
`,
			} {
				if err := os.WriteFile(filepath.Join(directory, name), []byte(contents), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			requiredState, checkError := "SUCCESS", "false"
			if tc.requiredState != "" {
				requiredState = tc.requiredState
			}
			if tc.checkError {
				checkError = "true"
			}
			command := exec.Command("bash", "-euo", "pipefail", "-c", strings.Join(scripts, "\n"))
			command.Dir = directory
			command.Env = append(os.Environ(), "PATH="+directory+string(os.PathListSeparator)+os.Getenv("PATH"),
				"GH_REPO=katl-dev/katl", "BRANCH=automation/kernels-test", "QUALIFIED_SHA=qualified",
				"REQUIRED_STATE="+requiredState, "CHECK_ERROR="+checkError)
			output, runErr := command.CombinedOutput()
			actions, err := os.ReadFile(filepath.Join(directory, "actions"))
			if err != nil {
				t.Fatal(err)
			}
			if string(actions) != tc.actions || tc.actions != "" && runErr != nil {
				t.Fatalf("promotion actions = %q, want %q (error %v): %s", actions, tc.actions, runErr, output)
			}
		})
	}
}
