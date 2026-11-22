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
    printf '{"vmFlavours":[],"driverFlavours":[],"groups":[]}\n' ;;
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
	data, err := os.ReadFile(filepath.Join(repoRoot(t), ".github/workflows/ci.yml"))
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
	script := workflow.Jobs["result"].Steps[0].Run
	for _, tc := range []struct {
		name, checks, nix, vms, drivers, vmFlavours, driverFlavours string
		pass                                                        bool
	}{
		{"documentation", "success", "success", "skipped", "skipped", `[]`, `[]`, true},
		{"product", "success", "success", "success", "skipped", `["standard"]`, `[]`, true},
		{"kernel", "success", "success", "success", "success", `["lts"]`, `["lts"]`, true},
		{"Nix failure", "success", "failure", "skipped", "skipped", `[]`, `[]`, false},
		{"Nix cancelled", "success", "cancelled", "skipped", "skipped", `[]`, `[]`, false},
		{"Nix skipped", "success", "skipped", "skipped", "skipped", `[]`, `[]`, false},
		{"missing Nix result", "success", "", "skipped", "skipped", `[]`, `[]`, false},
		{"fast failure", "failure", "success", "skipped", "skipped", `[]`, `[]`, false},
		{"fast cancelled", "cancelled", "success", "skipped", "skipped", `[]`, `[]`, false},
		{"VM failure", "success", "success", "failure", "success", `["lts"]`, `["lts"]`, false},
		{"VM cancelled", "success", "success", "cancelled", "success", `["lts"]`, `["lts"]`, false},
		{"selected VM skipped", "success", "success", "skipped", "skipped", `["standard"]`, `[]`, false},
		{"driver failure", "success", "success", "success", "failure", `["lts"]`, `["lts"]`, false},
		{"selected driver skipped", "success", "success", "success", "skipped", `["lts"]`, `["lts"]`, false},
		{"missing plan", "success", "success", "skipped", "skipped", ``, ``, false},
		{"invalid plan", "success", "success", "skipped", "skipped", `false`, `[]`, false},
		{"unexpected execution", "success", "success", "success", "skipped", `[]`, `[]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			needs, err := json.Marshal(map[string]any{
				"checks": map[string]any{"result": tc.checks, "outputs": map[string]string{
					"vm-flavours": tc.vmFlavours, "driver-flavours": tc.driverFlavours,
				}},
				"vms": map[string]string{"result": tc.vms}, "drivers": map[string]string{"result": tc.drivers},
				"nix": map[string]string{"result": tc.nix},
			})
			if err != nil {
				t.Fatal(err)
			}

			command := exec.Command("bash", "-euo", "pipefail", "-c", script)
			command.Env = append(os.Environ(), "NEEDS="+string(needs))
			output, err := command.CombinedOutput()
			if (err == nil) != tc.pass {
				t.Fatalf("CI passed = %v, want %v: %s", err == nil, tc.pass, output)
			}
		})
	}
}

func TestCIEventSelection(t *testing.T) {
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

	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet", "--initial-branch=main")
	runGit(t, dir, "config", "user.email", "test@katl.dev")
	runGit(t, dir, "config", "user.name", "Katl Test")
	if err := os.WriteFile(filepath.Join(dir, "product.go"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "product.go")
	runGit(t, dir, "commit", "--quiet", "-m", "initial")
	base := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))
	if err := os.WriteFile(filepath.Join(dir, "product.go"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "commit", "--quiet", "-am", "change")

	// The fake planner selects everything unless asked for the regression set,
	// which makes dropped selections visible in the outputs.
	tools := t.TempDir()
	writeFakeExecutable(t, tools, "go", `
case "$2" in
  ./cmd/katl-vmtest-plan)
    cat > /dev/null
    printf '%s\n' "$*" > "$PLANNER_ARGS"
    printf '{"vmFlavours":["standard","lts"],"driverFlavours":["standard"],"groups":[{"name":"cluster"}]}\n' ;;
  ./cmd/katl-kubernetes-release)
    printf '{"include":[]}\n' ;;
  *) exit 2 ;;
esac
`)

	for _, tc := range []struct {
		name, event, label string
		regression         bool
		vms, drivers       string
	}{
		{"pull request", "pull_request", "false", false, `[]`, `["standard"]`},
		{"labelled pull request", "pull_request", "true", true, `["standard","lts"]`, `["standard"]`},
		{"merge group", "merge_group", "false", true, `["standard","lts"]`, `[]`},
		{"push to main", "push", "false", false, `[]`, `[]`},
		{"manual dispatch", "workflow_dispatch", "false", false, `["standard","lts"]`, `["standard"]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := t.TempDir()
			outputPath := filepath.Join(run, "output")
			argsPath := filepath.Join(run, "args")
			command := exec.Command("bash", "-e", "-c", script)
			command.Dir = dir
			command.Env = append(os.Environ(), "PATH="+tools+string(os.PathListSeparator)+os.Getenv("PATH"),
				"GITHUB_EVENT_NAME="+tc.event, "BASE_SHA="+base, "BEFORE_SHA="+base, "VMTEST_LABEL="+tc.label,
				"PLANNER_ARGS="+argsPath, "GITHUB_OUTPUT="+outputPath, "GITHUB_STEP_SUMMARY="+filepath.Join(run, "summary"))
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("plan: %v: %s", err, output)
			}
			outputs, err := os.ReadFile(outputPath)
			if err != nil {
				t.Fatal(err)
			}
			if want := "vm-flavours=" + tc.vms + "\n"; !strings.Contains(string(outputs), want) {
				t.Fatalf("outputs = %s, want %q", outputs, want)
			}
			if want := "driver-flavours=" + tc.drivers + "\n"; !strings.Contains(string(outputs), want) {
				t.Fatalf("outputs = %s, want %q", outputs, want)
			}
			if tc.vms == `[]` && !strings.Contains(string(outputs), "scenarios={\"include\":[]}\n") {
				t.Fatalf("outputs = %s, want no scenarios without VM flavours", outputs)
			}
			args, err := os.ReadFile(argsPath)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(string(args), "--regression"); got != tc.regression {
				t.Fatalf("planner args = %q, regression = %v, want %v", args, got, tc.regression)
			}
		})
	}
}
