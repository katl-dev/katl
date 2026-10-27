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

func TestCIChanges(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), ".github/workflows/ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct{ Steps []struct{ ID, Run string } }
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	var script string
	for _, step := range workflow.Jobs["checks"].Steps {
		if step.ID == "plan" {
			script = step.Run
		}
	}
	if script == "" {
		t.Fatal("missing CI plan")
	}

	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet", "--initial-branch=main")
	runGit(t, dir, "config", "user.email", "test@katl.dev")
	runGit(t, dir, "config", "user.name", "Katl Test")
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("product.go", "original\n")
	runGit(t, dir, "add", "product.go")
	runGit(t, dir, "commit", "--quiet", "-m", "initial")
	runGit(t, dir, "checkout", "--quiet", "-b", "feature")
	write("README.md", "documentation\n")
	runGit(t, dir, "add", "README.md")
	runGit(t, dir, "commit", "--quiet", "-m", "documentation")
	head := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))
	runGit(t, dir, "checkout", "--quiet", "main")
	write("product.go", "updated on main\n")
	runGit(t, dir, "commit", "--quiet", "-am", "product update")
	base := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))
	// Actions tests the merge of the feature with the current base branch.
	runGit(t, dir, "merge", "--quiet", "--no-ff", "feature", "-m", "PR merge")

	tools := t.TempDir()
	writeFakeExecutable(t, tools, "go", `
case "$2" in
  ./cmd/katl-vmtest-plan)
    cat > observed-paths
    printf '{"vmFlavours":[],"kernelFlavours":[],"groups":[]}\n' ;;
  ./cmd/katl-kubernetes-release)
    printf '{"include":[]}\n' ;;
  *) exit 2 ;;
esac
`)
	command := exec.Command("bash", "-euo", "pipefail", "-c", script)
	command.Dir = dir
	command.Env = append(os.Environ(), "PATH="+tools+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GITHUB_EVENT_NAME=pull_request", "BASE_SHA="+base, "HEAD_SHA="+head,
		"GITHUB_OUTPUT="+filepath.Join(dir, "output"), "GITHUB_STEP_SUMMARY="+filepath.Join(dir, "summary"))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("plan: %v: %s", err, output)
	}
	paths, err := os.ReadFile(filepath.Join(dir, "observed-paths"))
	if err != nil {
		t.Fatal(err)
	}
	if string(paths) != "README.md\x00" {
		t.Fatalf("PR changes = %q; must exclude changes already on main", paths)
	}
}

func TestCIGate(t *testing.T) {
	for _, workflowFile := range []string{"ci.yml", "kernel-qualification.yml"} {
		t.Run(workflowFile, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(repoRoot(t), ".github/workflows", workflowFile))
			if err != nil {
				t.Fatal(err)
			}
			var workflow struct {
				Jobs map[string]struct{ Steps []struct{ Run string } }
			}
			if err := yaml.Unmarshal(data, &workflow); err != nil {
				t.Fatal(err)
			}
			script := workflow.Jobs["result"].Steps[0].Run
			for _, tc := range []struct {
				name, planned, vms, drivers, flavours string
				pass                                  bool
			}{
				{"unselected", "success", "skipped", "skipped", `[]`, true},
				{"selected", "success", "success", "success", `["lts"]`, true},
				{"plan failed", "failure", "skipped", "skipped", `[]`, false},
				{"plan cancelled", "cancelled", "skipped", "skipped", `[]`, false},
				{"VM failed", "success", "failure", "success", `["lts"]`, false},
				{"VM cancelled", "success", "cancelled", "success", `["lts"]`, false},
				{"VM skipped", "success", "skipped", "success", `["lts"]`, false},
				{"driver failed", "success", "success", "failure", `["lts"]`, false},
				{"driver cancelled", "success", "success", "cancelled", `["lts"]`, false},
				{"driver skipped", "success", "success", "skipped", `["lts"]`, false},
				{"missing selection", "success", "skipped", "skipped", ``, false},
				{"invalid selection", "success", "skipped", "skipped", `false`, false},
				{"unexpected execution", "success", "success", "success", `[]`, false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					needs := map[string]any{
						"vms":     map[string]string{"result": tc.vms},
						"drivers": map[string]string{"result": tc.drivers},
					}
					if workflowFile == "ci.yml" {
						needs["checks"] = map[string]any{"result": tc.planned, "outputs": map[string]string{"vm-flavours": tc.flavours, "kernel-flavours": tc.flavours}}
					} else {
						needs["plan"] = map[string]any{"result": tc.planned, "outputs": map[string]string{"flavours": tc.flavours}}
					}
					data, err := json.Marshal(needs)
					if err != nil {
						t.Fatal(err)
					}
					command := exec.Command("bash", "-euo", "pipefail", "-c", script)
					command.Env = append(os.Environ(), "NEEDS="+string(data))
					output, err := command.CombinedOutput()
					if (err == nil) != tc.pass {
						t.Fatalf("gate passed=%v, want %v: %s", err == nil, tc.pass, output)
					}
				})
			}
		})
	}
}
