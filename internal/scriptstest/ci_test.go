package scriptstest

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/katl-dev/katl/internal/gittest"
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
	command.Env = append(gittest.Env(os.Environ(), dir), "PATH="+tools+string(os.PathListSeparator)+os.Getenv("PATH"),
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
		name, event, label, baseRef string
		regression                  bool
		vms, drivers                string
	}{
		{"pull request", "pull_request", "false", "main", false, `[]`, `["standard"]`},
		{"pull request into release branch", "pull_request", "false", "release/2026.12.x", true, `["standard","lts"]`, `["standard"]`},
		{"pull request into non-series release branch", "pull_request", "false", "release/foo", false, `[]`, `["standard"]`},
		{"labelled pull request", "pull_request", "true", "main", true, `["standard","lts"]`, `["standard"]`},
		{"merge group", "merge_group", "false", "main", true, `["standard","lts"]`, `["standard"]`},
		{"push to main", "push", "false", "main", false, `[]`, `[]`},
		{"push to release branch", "push", "false", "", false, `[]`, `[]`},
		{"manual dispatch", "workflow_dispatch", "false", "main", false, `["standard","lts"]`, `["standard"]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := t.TempDir()
			outputPath := filepath.Join(run, "output")
			argsPath := filepath.Join(run, "args")
			command := exec.Command("bash", "-e", "-c", script)
			command.Dir = dir
			command.Env = append(gittest.Env(os.Environ(), dir), "PATH="+tools+string(os.PathListSeparator)+os.Getenv("PATH"),
				"GITHUB_EVENT_NAME="+tc.event, "BASE_SHA="+base, "BEFORE_SHA="+base, "VMTEST_LABEL="+tc.label, "BASE_REF="+tc.baseRef,
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
			if want := fmt.Sprintf("full-plan=%t\n", tc.regression); !strings.Contains(string(outputs), want) {
				t.Fatalf("outputs = %s, want %q", outputs, want)
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
			command.Env = append(gittest.Env(os.Environ(), dir), "PATH="+tools+string(os.PathListSeparator)+os.Getenv("PATH"),
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

// Only adding vmtest or changing the base may restart CI, and no workflow
// triggered by those events may publish a CI result of its own: a skipped or
// cancelled job counts against the required check for the head SHA.
func TestRestartWorkflowPublishesNoCI(t *testing.T) {
	type workflowFile struct {
		On struct {
			PullRequest struct{ Types []string } `yaml:"pull_request"`
		} `yaml:"on"`
		Jobs map[string]struct {
			Name string
			If   string `yaml:"if"`
		}
	}
	paths, err := filepath.Glob(filepath.Join(repoRoot(t), ".github/workflows/*.yml"))
	if err != nil {
		t.Fatal(err)
	}

	var sawRestart bool
	for _, path := range paths {
		name := filepath.Base(path)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var workflow workflowFile
		if err := yaml.Unmarshal(data, &workflow); err != nil {
			t.Fatal(err)
		}
		types := workflow.On.PullRequest.Types
		labeled, edited := slices.Contains(types, "labeled"), slices.Contains(types, "edited")
		if !labeled && !edited {
			continue
		}
		if name == "ci.yml" {
			t.Errorf("ci.yml pull request types %v include labeled or edited; any label or edit would cancel the run", types)
		}
		sawRestart = true
		for id, job := range workflow.Jobs {
			if job.Name == "CI" || id == "CI" {
				t.Errorf("%s job %s publishes a check named CI", name, id)
			}
			if labeled && (!strings.Contains(job.If, "github.event.label.name") || !strings.Contains(job.If, "'vmtest'")) {
				t.Errorf("%s job %s condition = %q, want labeled limited to the vmtest label", name, id, job.If)
			}
			if edited && !strings.Contains(job.If, "github.event.changes.base") {
				t.Errorf("%s job %s condition = %q, want edited limited to base changes", name, id, job.If)
			}
		}
	}
	if !sawRestart {
		t.Error("no workflow restarts CI when vmtest is added or the base changes")
	}
}

// A pull request retargeted onto a release branch must be replanned from the
// live base, which a rerun cannot read from its frozen event payload.
func TestPlanReadsLiveBase(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), ".github/workflows/ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				ID  string
				Env map[string]string
				Run string
			}
		}
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	var read, plan struct {
		Env map[string]string
		Run string
	}
	for _, step := range workflow.Jobs["plan"].Steps {
		switch step.ID {
		case "pr":
			read.Env, read.Run = step.Env, step.Run
		case "plan":
			plan.Env, plan.Run = step.Env, step.Run
		}
	}

	if got, want := plan.Env["BASE_REF"], "${{ steps.pr.outputs.base }}"; got != want {
		t.Errorf("plan BASE_REF = %q, want %q: the base must come from the live read, not the event payload", got, want)
	}
	if got, want := plan.Env["VMTEST_LABEL"], "${{ steps.pr.outputs.vmtest }}"; got != want {
		t.Errorf("plan VMTEST_LABEL = %q, want %q", got, want)
	}
	if strings.Count(read.Run, "gh pr view") != 1 || !strings.Contains(read.Run, "labels,baseRefName") || !strings.Contains(read.Run, "base=") {
		t.Errorf("pull request read = %q, want one gh pr view call for labels and baseRefName", read.Run)
	}
}

// Version-less series branches get fast checks on push and build no release
// artifacts, which are produced for tags and manual dispatch only.
func TestReleaseBranchTriggers(t *testing.T) {
	pushBranches := func(file string) []string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(repoRoot(t), ".github/workflows", file))
		if err != nil {
			t.Fatal(err)
		}
		var workflow struct {
			On struct {
				Push struct{ Branches []string } `yaml:"push"`
			} `yaml:"on"`
		}
		if err := yaml.Unmarshal(data, &workflow); err != nil {
			t.Fatal(err)
		}
		return workflow.On.Push.Branches
	}
	triggers := func(patterns []string, branch string) bool {
		t.Helper()
		for _, pattern := range patterns {
			// path.Match models only * (not crossing a slash), ? and [...]; GitHub
			// filters give other metacharacters a meaning it does not share.
			if strings.ContainsAny(pattern, `+!\`) || strings.Contains(pattern, "**") {
				t.Fatalf("branch filter %q uses syntax path.Match does not model", pattern)
			}
			if ok, err := path.Match(pattern, branch); err != nil {
				t.Fatal(err)
			} else if ok {
				return true
			}
		}
		return false
	}

	ci, artifacts := pushBranches("ci.yml"), pushBranches("release-artifacts.yml")
	if len(artifacts) != 0 {
		t.Errorf("release artifacts branch filters = %v, want none: only tags and dispatch carry a version", artifacts)
	}
	for _, tc := range []struct {
		branch string
		ci     bool
	}{
		{"main", true},
		{"release/2026.12.x", true},
		{"release/2026.12.0", true},
		{"release/foo", false},
		{"release/experiment/2026.12.x", false},
	} {
		if got := triggers(ci, tc.branch); got != tc.ci {
			t.Errorf("push to %s triggers CI = %v, want %v", tc.branch, got, tc.ci)
		}
		if triggers(artifacts, tc.branch) {
			t.Errorf("push to %s triggers release artifacts", tc.branch)
		}
	}
}

// A rerun after a retarget replays the old merge commit, so CI must fail rather
// than pass for a base it did not test.
func TestPlanRequiresTestedBase(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), ".github/workflows/ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Name string
				If   string `yaml:"if"`
				Env  map[string]string
				Run  string
			}
		}
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	var script string
	for _, step := range workflow.Jobs["plan"].Steps {
		if step.Name == "Require the tested base" {
			script = step.Run
			if step.If != "github.event_name == 'pull_request'" {
				t.Errorf("step condition = %q, want pull_request events only", step.If)
			}
			if step.Env["TESTED_BASE"] != "${{ github.base_ref }}" || step.Env["LIVE_BASE"] != "${{ steps.pr.outputs.base }}" {
				t.Errorf("step env = %v, want the payload base and the live base", step.Env)
			}
		}
	}
	if script == "" {
		t.Fatal("plan job has no step requiring the tested base")
	}

	for _, tc := range []struct {
		name, tested, live string
		wantFail           bool
	}{
		{"same base", "main", "main", false},
		{"retargeted to release branch", "main", "release/2026.12.x", true},
		{"retargeted to main", "release/2026.12.x", "main", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			command := exec.Command("bash", "-e", "-c", script)
			command.Env = append(os.Environ(), "TESTED_BASE="+tc.tested, "LIVE_BASE="+tc.live)
			output, err := command.CombinedOutput()
			if (err != nil) != tc.wantFail {
				t.Fatalf("err = %v, want failure %v: %s", err, tc.wantFail, output)
			}
			if tc.wantFail {
				for _, want := range []string{"tested " + tc.tested, "now targets " + tc.live, "close and reopen the pull request, or push a commit"} {
					if !strings.Contains(string(output), want) {
						t.Errorf("output = %q, want %q", output, want)
					}
				}
			}
		})
	}
}
