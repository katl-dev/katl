package scriptstest

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	for _, step := range workflow.Jobs["plan"].Steps {
		if step.ID == "plan" {
			script = step.Run
		}
	}
	if script == "" {
		t.Fatal("missing CI plan")
	}

	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet", "--initial-branch=main")
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
	command.Env = append(hermeticGitEnv(os.Environ(), dir), "PATH="+tools+string(os.PathListSeparator)+os.Getenv("PATH"),
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
		{"missing plan outputs", "success", "success", "skipped", "skipped", ``, ``, false},
		{"invalid plan", "success", "success", "skipped", "skipped", `false`, `[]`, false},
		{"unexpected execution", "success", "success", "success", "skipped", `[]`, `[]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			needs, err := json.Marshal(map[string]any{
				"plan": map[string]any{"result": "success", "outputs": map[string]string{
					"vm-flavours": tc.vmFlavours, "driver-flavours": tc.driverFlavours,
				}},
				"checks": map[string]string{"result": tc.checks},
				"vms":    map[string]string{"result": tc.vms}, "drivers": map[string]string{"result": tc.drivers},
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
	for _, step := range workflow.Jobs["plan"].Steps {
		if step.ID == "plan" {
			script = step.Run
		}
	}

	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet", "--initial-branch=main")
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
		{"merge group", "merge_group", "false", true, `["standard","lts"]`, `["standard"]`},
		{"push to main", "push", "false", false, `[]`, `[]`},
		{"manual dispatch", "workflow_dispatch", "false", false, `["standard","lts"]`, `["standard"]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := t.TempDir()
			outputPath := filepath.Join(run, "output")
			argsPath := filepath.Join(run, "args")
			command := exec.Command("bash", "-e", "-c", script)
			command.Dir = dir
			command.Env = append(hermeticGitEnv(os.Environ(), dir), "PATH="+tools+string(os.PathListSeparator)+os.Getenv("PATH"),
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

// The required CI check must never pass because a planned job did not run, for
// any event the workflow handles.
func TestCIGatesPlannedJobs(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), ".github/workflows/ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		On struct {
			PullRequest struct{ Types []string } `yaml:"pull_request"`
		} `yaml:"on"`
		Jobs map[string]struct {
			If    string    `yaml:"if"`
			Needs yaml.Node `yaml:"needs"`
			Steps []struct{ ID, Run string }
		}
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}

	// The model below depends on these job conditions: every event runs the plan,
	// checks, Nix and result jobs, and the VM and driver jobs run exactly when
	// the plan selects them. Any other condition can skip a job without the plan
	// knowing, and a skipped job counts as passing for a required check.
	for name, want := range map[string]string{
		"plan":    "",
		"checks":  "",
		"nix":     "",
		"result":  "always()",
		"vms":     "needs.plan.outputs.vm-flavours != '[]'",
		"drivers": "needs.plan.outputs.driver-flavours != '[]'",
	} {
		if got := strings.TrimSpace(workflow.Jobs[name].If); got != want {
			t.Errorf("%s condition = %q, want %q", name, got, want)
		}
	}
	// The result job gates on every other job, so none can be skipped unnoticed.
	needsOf := func(job string) []string {
		t.Helper()
		var list []string
		node := workflow.Jobs[job].Needs
		switch node.Kind {
		case 0:
		case yaml.ScalarNode:
			list = []string{node.Value}
		default:
			if err := node.Decode(&list); err != nil {
				t.Fatal(err)
			}
		}
		slices.Sort(list)
		return list
	}
	var gating []string
	for name := range workflow.Jobs {
		if name != "result" {
			gating = append(gating, name)
		}
	}
	slices.Sort(gating)
	if got := needsOf("result"); !slices.Equal(got, gating) {
		t.Errorf("result needs %v, want every other job %v", got, gating)
	}
	for _, name := range []string{"vms", "drivers"} {
		if got := needsOf(name); !slices.Equal(got, []string{"plan"}) {
			t.Errorf("%s needs %v, want only plan", name, got)
		}
	}
	if !slices.Contains(workflow.On.PullRequest.Types, "labeled") {
		t.Errorf("pull request types %v must include labeled to start VM runs", workflow.On.PullRequest.Types)
	}

	var plan string
	for _, step := range workflow.Jobs["plan"].Steps {
		if step.ID == "plan" {
			plan = step.Run
		}
	}
	gate := workflow.Jobs["result"].Steps[0].Run

	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet", "--initial-branch=main")
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
	tools := t.TempDir()
	writeFakeExecutable(t, tools, "go", `
case "$2" in
  ./cmd/katl-vmtest-plan)
    cat > /dev/null
    printf '{"vmFlavours":["standard","lts"],"driverFlavours":["standard","lts"],"groups":[{"name":"cluster"}]}\n' ;;
  ./cmd/katl-kubernetes-release)
    printf '{"include":[]}\n' ;;
  *) exit 2 ;;
esac
`)

	for _, tc := range []struct{ name, event, label string }{
		{"pull request", "pull_request", "false"},
		{"labelled pull request", "pull_request", "true"},
		{"merge group", "merge_group", "false"},
		{"push", "push", "false"},
		{"dispatch", "workflow_dispatch", "false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			outputPath := filepath.Join(t.TempDir(), "output")
			command := exec.Command("bash", "-e", "-c", plan)
			command.Dir = dir
			command.Env = append(hermeticGitEnv(os.Environ(), dir), "PATH="+tools+string(os.PathListSeparator)+os.Getenv("PATH"),
				"GITHUB_EVENT_NAME="+tc.event, "BASE_SHA="+base, "BEFORE_SHA="+base, "VMTEST_LABEL="+tc.label,
				"GITHUB_OUTPUT="+outputPath, "GITHUB_STEP_SUMMARY="+filepath.Join(t.TempDir(), "summary"))
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("plan: %v: %s", err, output)
			}
			outputs, err := os.ReadFile(outputPath)
			if err != nil {
				t.Fatal(err)
			}
			planned := map[string]bool{"plan": true, "checks": true, "nix": true}
			for _, line := range strings.Split(string(outputs), "\n") {
				switch {
				case strings.HasPrefix(line, "vm-flavours="):
					planned["vms"] = line != "vm-flavours=[]"
				case strings.HasPrefix(line, "driver-flavours="):
					planned["drivers"] = line != "driver-flavours=[]"
				}
			}

			needs := func(results map[string]string) bool {
				t.Helper()
				body, err := json.Marshal(map[string]any{
					"checks": map[string]string{"result": results["checks"]},
					"plan": map[string]any{"result": results["plan"], "outputs": map[string]string{
						"vm-flavours":     map[bool]string{true: `["standard"]`, false: `[]`}[planned["vms"]],
						"driver-flavours": map[bool]string{true: `["standard"]`, false: `[]`}[planned["drivers"]],
					}},
					"nix": map[string]string{"result": results["nix"]},
					"vms": map[string]string{"result": results["vms"]}, "drivers": map[string]string{"result": results["drivers"]},
				})
				if err != nil {
					t.Fatal(err)
				}
				command := exec.Command("bash", "-euo", "pipefail", "-c", gate)
				command.Env = append(os.Environ(), "NEEDS="+string(body))
				return command.Run() == nil
			}
			// A job runs when planned and is skipped otherwise.
			healthy := func() map[string]string {
				results := map[string]string{}
				for _, job := range []string{"plan", "checks", "nix", "vms", "drivers"} {
					results[job] = "skipped"
					if planned[job] {
						results[job] = "success"
					}
				}
				return results
			}
			if !needs(healthy()) {
				t.Fatalf("CI failed although every planned job succeeded: %v", planned)
			}
			for job, isPlanned := range planned {
				if !isPlanned {
					continue
				}
				for _, result := range []string{"skipped", "failure", "cancelled", ""} {
					results := healthy()
					results[job] = result
					if needs(results) {
						t.Errorf("CI passed with %s %q although it was planned", job, result)
					}
				}
			}
		})
	}
}
