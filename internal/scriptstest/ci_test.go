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
	write("old.go", "package product\n")
	runGit(t, dir, "add", "product.go", "old.go")
	runGit(t, dir, "commit", "--quiet", "-m", "initial")
	runGit(t, dir, "checkout", "--quiet", "-b", "feature")
	write("README.md", "documentation\n")
	runGit(t, dir, "mv", "old.go", "moved.md")
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
    printf '{"vmFlavours":[],"driverFlavours":[],"driverDigests":{},"driverDecisions":[],"productionFlavours":[],"groups":[]}\n' ;;
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
	if want := "README.md\x00moved.md\x00old.go\x00"; string(paths) != want {
		t.Fatalf("PR changes = %q, want %q with both sides of the rename and no changes already on main", paths, want)
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
					"vm-flavours": tc.vmFlavours, "driver-flavours": tc.driverFlavours, "production-flavours": `[]`,
				}},
				"checks": map[string]string{"result": tc.checks},
				"vms":    map[string]string{"result": tc.vms}, "drivers": map[string]string{"result": tc.drivers},
				"nix":           map[string]string{"result": tc.nix},
				"production":    map[string]string{"result": "skipped"},
				"qualification": map[string]string{"result": "skipped"},
				"bundle":        map[string]string{"result": "skipped"},
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
	if err := os.WriteFile(filepath.Join(dir, "first-pr"), []byte("queued before the last change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "first-pr")
	runGit(t, dir, "commit", "--quiet", "-m", "first queued change")
	if err := os.WriteFile(filepath.Join(dir, "product.go"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "commit", "--quiet", "-am", "change")

	// The fake planner returns VM selections and the requested driver fixture
	// so the test can check the workflow's planner arguments and outputs.
	tools := t.TempDir()
	writeFakeExecutable(t, tools, "go", `
case "$2" in
  ./cmd/katl-vmtest-plan)
    cat > "$PLANNER_PATHS"
    printf '%s\n' "$*" > "$PLANNER_ARGS"
    production='[]'
    [[ " $* " != *" --production="* ]] || production='["standard","lts"]'
    vms='["standard","lts"]'
    groups='[{"name":"cluster"}]'
    shadow=''
    case " $* " in
      *' --vm-mode off '*) vms='[]'; groups='[]' ;;
      *' --vm-mode merge-group '*) shadow=',"vmShadow":{"suites":["config"],"runnerMinutesSaved":10}' ;;
    esac
    printf '{"vmFlavours":%s,"driverFlavours":%s,"driverDigests":{"standard":"v1:fixture"},"driverDecisions":["driver-qualification flavour=standard decision=run reason=no-record"],"productionFlavours":%s,"groups":%s%s}\n' "$vms" "$DRIVER_MATRIX" "$production" "$groups" "$shadow" ;;
  ./cmd/katl-kubernetes-release)
    printf '{"include":[]}\n' ;;
  *) exit 2 ;;
esac
`)

	for _, tc := range []struct {
		name, event, label, baseRef string
		vmMode                      string
		regression, fullPlan        bool
		vms, drivers, check         string
	}{
		{"pull request", "pull_request", "false", "main", "off", false, false, `[]`, `["standard"]`, "CI"},
		{"pull request into release series", "pull_request", "false", "release/2026.12.x", "full", true, true, `["standard","lts"]`, `["standard"]`, "CI (release)"},
		{"pull request into non-series release branch", "pull_request", "false", "release/foo", "off", false, false, `[]`, `["standard"]`, "CI"},
		{"pull request into nested release branch", "pull_request", "false", "release/2026.12.x/foo", "off", false, false, `[]`, `["standard"]`, "CI"},
		{"labelled pull request", "pull_request", "true", "main", "affected", true, false, `["standard","lts"]`, `["standard"]`, "CI"},
		{"labelled pull request into release series", "pull_request", "true", "release/2026.12.x", "full", true, true, `["standard","lts"]`, `["standard"]`, "CI (release)"},
		{"merge group", "merge_group", "false", "", "merge-group", true, true, `["standard","lts"]`, `["standard"]`, "CI"},
		{"unqualified main push", "push", "false", "", "off", false, false, `[]`, `["standard"]`, "CI"},
		{"qualified main push", "push", "false", "", "off", false, false, `[]`, `[]`, "CI"},
		{"manual dispatch", "workflow_dispatch", "false", "", "always", false, false, `["standard","lts"]`, `["standard","lts"]`, "CI"},
		{"schedule", "schedule", "false", "", "always", false, false, `["standard","lts"]`, `["standard","lts"]`, "CI"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := t.TempDir()
			outputPath := filepath.Join(run, "output")
			argsPath := filepath.Join(run, "args")
			pathsPath := filepath.Join(run, "paths")
			command := exec.Command("bash", "-e", "-c", script)
			command.Dir = dir
			command.Env = append(gittest.Env(os.Environ(), dir), "PATH="+tools+string(os.PathListSeparator)+os.Getenv("PATH"),
				"GITHUB_EVENT_NAME="+tc.event, "BASE_SHA="+base, "BEFORE_SHA="+base, "VMTEST_LABEL="+tc.label, "BASE_REF="+tc.baseRef, "PAYLOAD_BASE="+tc.baseRef,
				"PLANNER_ARGS="+argsPath, "PLANNER_PATHS="+pathsPath, "DRIVER_MATRIX="+tc.drivers, "GITHUB_OUTPUT="+outputPath, "GITHUB_STEP_SUMMARY="+filepath.Join(run, "summary"))
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
			if want := "driver-digests={\"standard\":\"v1:fixture\"}\n"; !strings.Contains(string(outputs), want) {
				t.Fatalf("outputs = %s, want %q", outputs, want)
			}
			summary := mustReadFile(t, filepath.Join(run, "summary"))
			if want := "\ndriver-qualification flavour=standard decision=run reason=no-record\n"; !strings.Contains(string(summary), want) {
				t.Fatalf("summary = %s, want standalone line %q", summary, want)
			}
			// The fixture changes product.go, so both kinds of full plan build.
			wantProduction := `[]`
			if tc.fullPlan {
				wantProduction = `["standard","lts"]`
			}
			if want := "production-flavours=" + wantProduction + "\n"; !strings.Contains(string(outputs), want) {
				t.Fatalf("outputs = %s, want %q", outputs, want)
			}
			if tc.vms == `[]` && !strings.Contains(string(outputs), "scenarios={\"include\":[]}\n") {
				t.Fatalf("outputs = %s, want no scenarios without VM flavours", outputs)
			}
			if want := fmt.Sprintf("full-plan=%t\n", tc.fullPlan); !strings.Contains(string(outputs), want) {
				t.Fatalf("outputs = %s, want %q", outputs, want)
			}
			check := "CI"
			for _, line := range strings.Split(string(outputs), "\n") {
				if suffix, ok := strings.CutPrefix(line, "check-suffix="); ok && suffix != "" {
					check += " " + suffix
				}
			}
			if check != tc.check {
				t.Fatalf("required check = %q, want %q: outputs = %s", check, tc.check, outputs)
			}
			args, err := os.ReadFile(argsPath)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(string(args), "--regression"); got != tc.regression {
				t.Fatalf("planner args = %q, regression = %v, want %v", args, got, tc.regression)
			}
			if !slices.Contains(strings.Fields(string(args)), tc.vmMode) {
				t.Fatalf("planner args = %q, want VM mode %s", args, tc.vmMode)
			}
			if tc.event == "merge_group" {
				if !strings.Contains(string(args), "--base "+base) {
					t.Fatalf("planner args = %q, want payload base %s", args, base)
				}
				paths, err := os.ReadFile(pathsPath)
				if err != nil {
					t.Fatal(err)
				}
				if want := "first-pr\x00product.go\x00"; string(paths) != want {
					t.Fatalf("merge-group changes = %q, want both queued changes %q", paths, want)
				}
				if !strings.Contains(string(outputs), "vm-shadow={\"suites\":[\"config\"],\"runnerMinutesSaved\":10}\n") {
					t.Fatalf("outputs = %s, want shadow plan", outputs)
				}
				summary, err := os.ReadFile(filepath.Join(run, "summary"))
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(summary), `"runnerMinutesSaved": 10`) {
					t.Fatalf("summary = %s, want shadow savings", summary)
				}
			}
			for _, mode := range []string{"non-prose", "always"} {
				want := tc.fullPlan && (tc.event == "merge_group") == (mode == "non-prose")
				if got := slices.Contains(strings.Fields(string(args)), "--production="+mode); got != want {
					t.Fatalf("planner args = %q, want --production=%s present: %v", args, mode, want)
				}
			}
		})
	}
}

func TestCIProseReleaseNotes(t *testing.T) {
	repo := repoRoot(t)
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct{ ID, If, Run string }
		}
	}
	if err := yaml.Unmarshal(mustReadFile(t, filepath.Join(repo, ".github/workflows/ci.yml")), &workflow); err != nil {
		t.Fatal(err)
	}
	var plan, production string
	for _, step := range workflow.Jobs["plan"].Steps {
		switch step.ID {
		case "plan":
			plan = step.Run
		case "production":
			production = step.Run
			if step.If != "steps.plan.outputs.full-plan == 'true'" {
				t.Fatalf("production identity condition = %q, want every full plan", step.If)
			}
		}
	}
	if plan == "" || production == "" {
		t.Fatal("missing plan or production identity step")
	}

	tools := t.TempDir()
	planner := buildCommand(t, tools, "katl-vmtest-plan")
	writeFakeExecutable(t, tools, "go", `
case "$2" in
  ./cmd/katl-vmtest-plan) shift 2; exec "$PLANNER" "$@" ;;
  ./cmd/katl-kubernetes-release) printf '{"include":[]}\n' ;;
  *) exit 2 ;;
esac
`)
	writeFakeExecutable(t, tools, "gh", `printf 'v2026.10.0\n'`)
	for _, tc := range []struct{ name, trailers, wantError string }{
		{"valid trailer", "Release-Note: Update the documentation", ""},
		{"duplicate trailers", "Release-Note: First note\nRelease-Note: Second note", "multiple Release-Note trailers"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			runGit(t, dir, "init", "--quiet")
			runGit(t, dir, "commit", "--quiet", "--allow-empty", "-m", "stable release")
			runGit(t, dir, "tag", "v2026.10.0")
			runGit(t, dir, "commit", "--quiet", "--allow-empty", "-m", "prepare prerelease")
			runGit(t, dir, "tag", "v2026.10.1-rc.0")
			base := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))
			if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("documentation\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			runGit(t, dir, "add", "README.md")
			runGit(t, dir, "commit", "--quiet", "-m", "docs: update", "-m", tc.trailers)
			if err := os.Symlink(filepath.Join(repo, "scripts"), filepath.Join(dir, "scripts")); err != nil {
				t.Fatal(err)
			}

			outputs := filepath.Join(dir, "output")
			env := append(gittest.Env(environmentWithout("KATL_RELEASE_TARGET", "KATL_RELEASE_PREVIOUS_TAG"), dir),
				"PATH="+tools+string(os.PathListSeparator)+os.Getenv("PATH"), "PLANNER="+planner,
				"GITHUB_EVENT_NAME=merge_group", "BASE_SHA="+base, "GITHUB_OUTPUT="+outputs,
				"GITHUB_STEP_SUMMARY="+filepath.Join(dir, "summary"), "KATL_REPO_ROOT="+dir)
			command := exec.Command("bash", "-e", "-c", plan)
			command.Dir, command.Env = dir, env
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("plan: %v: %s", err, output)
			}
			planOutputs := string(mustReadFile(t, outputs))
			if !strings.Contains(planOutputs, "full-plan=true\n") || !strings.Contains(planOutputs, "production-flavours=[]\n") {
				t.Fatalf("expected a full plan without Production jobs: %s", planOutputs)
			}

			command = exec.Command("bash", "-e", "-c", production)
			command.Dir, command.Env = dir, append(env, "BASE_SHA=", "PRODUCTION_FLAVOURS=[]")
			output, err := command.CombinedOutput()
			if tc.wantError != "" {
				if err == nil || !strings.Contains(string(output), tc.wantError) {
					t.Fatalf("production identity: %v: %s, want %q", err, output, tc.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("production identity: %v: %s", err, output)
			}
			notes := string(mustReadFile(t, filepath.Join(dir, "_build/ci-release-notes.md")))
			for _, want := range []string{"Update the documentation", "prepare prerelease", "/releases/download/v2026.10.1-dev.0/PROVENANCE.md"} {
				if !strings.Contains(notes, want) {
					t.Errorf("release notes missing %q: %s", want, notes)
				}
			}
		})
	}
}

func TestDriverStatusPublication(t *testing.T) {
	caller, _ := readWorkflow(t, "ci.yml")
	workflow, _ := readWorkflow(t, "kernel-qualification.yml")
	for _, scope := range []struct {
		workflow workflowFile
		writers  []string
	}{
		{caller, []string{"drivers"}},
		{workflow, []string{"record-driver-start", "record-driver-result"}},
	} {
		for name, job := range scope.workflow.Jobs {
			var permissions map[string]string
			if err := job.Permissions.Decode(&permissions); err != nil {
				t.Fatal(err)
			}
			if (permissions["statuses"] == "write") != slices.Contains(scope.writers, name) {
				t.Errorf("%s job has statuses permission %q", name, permissions["statuses"])
			}
		}
	}
	requireNeeds(t, workflow, [][2]string{
		{"drivers", "record-driver-start"},
		{"record-driver-result", "record-driver-start"},
		{"record-driver-result", "drivers"},
	})

	start := workflow.Jobs["record-driver-start"]
	result := workflow.Jobs["record-driver-result"]
	if !strings.Contains(result.If, "always()") {
		t.Error("terminal publication must run when driver qualification fails")
	}
	condition := func(job workflowJob, context map[string]string) bool {
		t.Helper()
		expr := strings.NewReplacer("always()", "'true' == 'true'", "!cancelled()", "cancelled != 'true'",
			"needs.record-driver-start.result", "start.result").Replace(job.If)
		return expandExpressions(t, "${{ "+strings.Join(strings.Fields(expr), " ")+" }}", context) == "true"
	}
	for _, tc := range []struct {
		event, ref, base string
		publishes        bool
	}{
		{"push", "refs/heads/main", "", true},
		{"schedule", "refs/heads/main", "", true},
		{"workflow_dispatch", "refs/heads/main", "", true},
		{"workflow_dispatch", "refs/heads/topic", "", false},
		{"push", "refs/heads/release/2026", "", false},
		{"pull_request", "refs/pull/1/merge", "", false},
		{"merge_group", "refs/heads/gh-readonly-queue/main/pr-1", "refs/heads/main", true},
		{"merge_group", "refs/heads/gh-readonly-queue/release/2026/pr-1", "refs/heads/release/2026", false},
	} {
		context := map[string]string{"github.event_name": tc.event, "github.ref": tc.ref, "github.event.merge_group.base_ref": tc.base}
		if got := condition(start, context); got != tc.publishes {
			t.Errorf("pending publication on %s %s = %v, want %v", tc.event, tc.ref, got, tc.publishes)
		}
	}
	for _, tc := range []struct {
		start, drivers string
		runs, finishes bool
	}{
		{"success", "success", true, true},
		{"success", "failure", true, true},
		{"success", "cancelled", true, true},
		{"skipped", "success", true, false},
		{"failure", "skipped", false, false},
	} {
		context := map[string]string{"start.result": tc.start, "needs.drivers.result": tc.drivers}
		if runs := condition(workflow.Jobs["drivers"], context); runs != tc.runs {
			t.Errorf("drivers after pending job %s runs = %v, want %v", tc.start, runs, tc.runs)
		}
		if finishes := condition(result, context); finishes != tc.finishes {
			t.Errorf("publication after pending job %s and drivers %s = %v, want %v", tc.start, tc.drivers, finishes, tc.finishes)
		}
	}

	for _, tc := range []struct {
		flavour, drivers string
		publisher        workflowJob
		want             string
	}{
		{"lts", "", start, "pending"},
		{"lts", "success", result, "success"},
		{"standard", "failure", result, "failure"},
		{"standard", "cancelled", result, "failure"},
	} {
		t.Run(tc.flavour+"/"+tc.want+"/"+tc.drivers, func(t *testing.T) {
			if len(tc.publisher.Steps) != 1 {
				t.Fatal("missing status publication step")
			}
			dir := t.TempDir()
			request := filepath.Join(dir, "request")
			writeFakeExecutable(t, dir, "gh", `printf '%s\n' "$@" >> "$STATUS_REQUEST"`)
			digest := "v3:" + strings.Repeat("a", 64)
			sha := strings.Repeat("1", 40)
			state := expandExpressions(t, tc.publisher.Env["STATE"], map[string]string{"needs.drivers.result": tc.drivers})
			command := exec.Command("bash", "-euo", "pipefail", "-c", tc.publisher.Steps[0].Run)
			command.Env = append(os.Environ(),
				"PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "STATUS_REQUEST="+request,
				"FLAVOUR="+tc.flavour, "DIGEST="+digest, "STATE="+state, "GITHUB_SHA="+sha,
				"GITHUB_REPOSITORY=katl-dev/katl", "GITHUB_SERVER_URL=https://github.com", "GITHUB_RUN_ID=42", "GITHUB_RUN_ATTEMPT=2",
			)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("publish status: %v: %s", err, output)
			}
			data, err := os.ReadFile(request)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(string(data), "api\n") != 1 {
				t.Errorf("wanted one status, got %s", data)
			}
			for _, arg := range []string{
				"repos/katl-dev/katl/statuses/" + sha,
				"context=katl/driver-qualification/" + tc.flavour + "/" + digest,
				"state=" + tc.want, "description=" + digest,
				"target_url=https://github.com/katl-dev/katl/actions/runs/42/attempts/2",
			} {
				if !slices.Contains(strings.Split(string(data), "\n"), arg) {
					t.Errorf("request = %s, want %q", data, arg)
				}
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
		"plan":          "",
		"checks":        "",
		"nix":           "",
		"result":        "always()",
		"vms":           "needs.plan.outputs.vm-flavours != '[]'",
		"drivers":       "needs.plan.outputs.driver-flavours != '[]'",
		"production":    "needs.plan.outputs.production-flavours != '[]'",
		"bundle":        "needs.plan.outputs.production-flavours != '[]'",
		"qualification": "needs.plan.outputs.production-flavours != '[]'",
	} {
		if got := strings.TrimSpace(workflow.Jobs[name].If); got != want {
			t.Errorf("%s condition = %q, want %q", name, got, want)
		}
	}
	// The result job gates on every top-level validation job.
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
		t.Errorf("result needs %v, want every validation job %v", got, gating)
	}
	for _, name := range []string{"vms", "drivers", "production"} {
		if got := needsOf(name); !slices.Equal(got, []string{"plan"}) {
			t.Errorf("%s needs %v, want only plan", name, got)
		}
	}
	if got := needsOf("bundle"); !slices.Equal(got, []string{"plan", "production"}) {
		t.Errorf("bundle needs %v, want plan and production", got)
	}
	if got := needsOf("qualification"); !slices.Equal(got, []string{"plan", "production"}) {
		t.Errorf("qualification needs %v, want plan and production", got)
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
    production='[]'
    [[ " $* " != *" --production="* ]] || production='["standard","lts"]'
    printf '{"vmFlavours":["standard","lts"],"driverFlavours":["standard","lts"],"driverDigests":{},"driverDecisions":[],"productionFlavours":%s,"groups":[{"name":"cluster"}]}\n' "$production" ;;
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
		{"schedule", "schedule", "false"},
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
				case strings.HasPrefix(line, "production-flavours="):
					planned["production"] = line != "production-flavours=[]"
					planned["bundle"] = planned["production"]
					planned["qualification"] = planned["production"]
				}
			}

			needs := func(results map[string]string) bool {
				t.Helper()
				body, err := json.Marshal(map[string]any{
					"checks": map[string]string{"result": results["checks"]},
					"plan": map[string]any{"result": results["plan"], "outputs": map[string]string{
						"vm-flavours":         map[bool]string{true: `["standard"]`, false: `[]`}[planned["vms"]],
						"driver-flavours":     map[bool]string{true: `["standard"]`, false: `[]`}[planned["drivers"]],
						"production-flavours": map[bool]string{true: `["standard","lts"]`, false: `[]`}[planned["production"]],
					}},
					"nix": map[string]string{"result": results["nix"]},
					"vms": map[string]string{"result": results["vms"]}, "drivers": map[string]string{"result": results["drivers"]},
					"production":    map[string]string{"result": results["production"]},
					"qualification": map[string]string{"result": results["qualification"]},
					"bundle":        map[string]string{"result": results["bundle"]},
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
				for _, job := range []string{"plan", "checks", "nix", "vms", "drivers", "production", "qualification", "bundle"} {
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

// Only adding vmtest may restart CI, and no workflow triggered by a label or an
// edit may publish a CI result of either name: a skipped or cancelled job counts
// against the required check for the head SHA.
func TestLabelWorkflowsPublishNoCI(t *testing.T) {
	type workflowFile struct {
		On struct {
			PullRequest struct{ Types []string } `yaml:"pull_request"`
		} `yaml:"on"`
		Concurrency yaml.Node
		Jobs        map[string]struct {
			Name        string
			If          string `yaml:"if"`
			Concurrency yaml.Node
		}
	}
	paths, err := filepath.Glob(filepath.Join(repoRoot(t), ".github/workflows/*.yml"))
	if err != nil {
		t.Fatal(err)
	}

	var sawLabelWorkflow bool
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
		if !slices.Contains(types, "labeled") && !slices.Contains(types, "edited") {
			continue
		}
		if name == "ci.yml" {
			t.Errorf("ci.yml pull request types %v include labeled or edited; any label or edit would cancel the run", types)
		}
		sawLabelWorkflow = true
		// A workflow-level group would admit runs whose jobs are skipped.
		if !workflow.Concurrency.IsZero() {
			t.Errorf("%s has workflow-level concurrency; put it on the job so skipped runs never enter the group", name)
		}
		for id, job := range workflow.Jobs {
			for _, check := range []string{"CI", "CI (release)"} {
				if job.Name == check || id == check {
					t.Errorf("%s job %s publishes a check named %q", name, id, check)
				}
			}
			if !strings.Contains(job.If, "github.event.label.name") || !strings.Contains(job.If, "'vmtest'") {
				t.Errorf("%s job %s condition = %q, want it limited to the vmtest label", name, id, job.If)
			}
			if job.Concurrency.IsZero() {
				t.Errorf("%s job %s has no concurrency group", name, id)
			}
		}
	}
	if !sawLabelWorkflow {
		t.Error("no workflow restarts CI when vmtest is added")
	}
}

// The required check names the base its payload tested, so a rerun, a merge
// group, a push or a dispatch can never publish the release check.
func TestResultCheckNameFollowsPayloadBase(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), ".github/workflows/ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Name    string
			Outputs map[string]string
			Steps   []struct {
				ID  string
				Env map[string]string
			}
		}
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}

	const wantName = "CI${{ needs.plan.outputs.check-suffix && format(' {0}', needs.plan.outputs.check-suffix) || '' }}"
	if got := workflow.Jobs["result"].Name; got != wantName {
		t.Errorf("result job name = %q, want %q", got, wantName)
	}
	if got, want := workflow.Jobs["plan"].Outputs["check-suffix"], "${{ steps.plan.outputs.check-suffix }}"; got != want {
		t.Errorf("plan check-suffix output = %q, want %q", got, want)
	}
	for _, step := range workflow.Jobs["plan"].Steps {
		if step.ID == "plan" {
			if got, want := step.Env["PAYLOAD_BASE"], "${{ github.base_ref }}"; got != want {
				t.Errorf("plan PAYLOAD_BASE = %q, want %q: the name must follow the base the payload tested, not the live base", got, want)
			}
		}
	}
}

// A retargeted pull request must be replanned from the live labels and base,
// which a rerun cannot read from its frozen event payload.
func TestPlanReadsLiveState(t *testing.T) {
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
		t.Errorf("plan BASE_REF = %q, want %q", got, want)
	}
	if got, want := plan.Env["VMTEST_LABEL"], "${{ steps.pr.outputs.vmtest }}"; got != want {
		t.Errorf("plan VMTEST_LABEL = %q, want %q", got, want)
	}

	// The fake gh returns only the JSON fields requested and applies --jq with
	// jq, as gh does, so the step is judged by the outputs it publishes.
	tools := t.TempDir()
	writeFakeExecutable(t, tools, "gh", `
[[ "$1 $2" == "pr view" ]] || exit 2
while (($#)); do
  case "$1" in
    --json) fields="$2"; shift ;;
    --jq) expr="$2"; shift ;;
  esac
  shift
done
jq -c --arg fields "$fields" 'with_entries(select(.key as $k | $fields | split(",") | index($k)))' <<< "$PR_JSON" | jq -r "$expr"
`)
	for _, tc := range []struct {
		name, prJSON, vmtest, base string
	}{
		{"labelled release pull request", `{"labels":[{"name":"area: ci"},{"name":"vmtest"}],"baseRefName":"release/2026.12.x","title":"x"}`, "true", "release/2026.12.x"},
		{"unlabelled pull request", `{"labels":[{"name":"area: ci"}],"baseRefName":"main","title":"x"}`, "false", "main"},
		{"no labels", `{"labels":[],"baseRefName":"main"}`, "false", "main"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			outputPath := filepath.Join(t.TempDir(), "output")
			command := exec.Command("bash", "-e", "-c", read.Run)
			command.Env = append(os.Environ(), "PATH="+tools+string(os.PathListSeparator)+os.Getenv("PATH"),
				"GH_TOKEN=x", "PR=603", "GITHUB_REPOSITORY=katl-dev/katl", "GITHUB_OUTPUT="+outputPath, "PR_JSON="+tc.prJSON)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("read: %v: %s", err, output)
			}
			outputs, err := os.ReadFile(outputPath)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"vmtest=" + tc.vmtest + "\n", "base=" + tc.base + "\n"} {
				if !strings.Contains(string(outputs), want) {
					t.Errorf("outputs = %q, want %q", outputs, want)
				}
			}
		})
	}
}

// Version-less series branches get fast checks on push and build no release
// artifacts, which are produced for tags and manual dispatch only.
func TestReleaseBranchTriggers(t *testing.T) {
	push := func(file string) (branches, tags, ignored []string) {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(repoRoot(t), ".github/workflows", file))
		if err != nil {
			t.Fatal(err)
		}
		var workflow struct {
			On struct {
				Push struct {
					Branches       []string
					Tags           []string
					BranchesIgnore []string `yaml:"branches-ignore"`
				} `yaml:"push"`
			} `yaml:"on"`
		}
		if err := yaml.Unmarshal(data, &workflow); err != nil {
			t.Fatal(err)
		}
		p := workflow.On.Push
		return p.Branches, p.Tags, p.BranchesIgnore
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

	ci, _, _ := push("ci.yml")
	artifactBranches, artifactTags, artifactIgnored := push("release-artifacts.yml")
	if len(artifactBranches) != 0 || len(artifactIgnored) != 0 {
		t.Errorf("release artifacts branches = %v, branches-ignore = %v, want neither: only tags and dispatch carry a version", artifactBranches, artifactIgnored)
	}
	if !slices.Equal(artifactTags, []string{"v*"}) {
		t.Errorf("release artifacts tags = %v, want [v*]", artifactTags)
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
		{"release/2026.12.x/foo", false},
	} {
		if got := triggers(ci, tc.branch); got != tc.ci {
			t.Errorf("push to %s triggers CI = %v, want %v", tc.branch, got, tc.ci)
		}
		if triggers(artifactBranches, tc.branch) {
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

// The production jobs are the reusable production build without publication,
// planned from the matrix the plan job selects.
func TestCIProductionJobs(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), ".github/workflows/ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Uses     string `yaml:"uses"`
			Strategy struct{ Matrix struct{ Flavour string } }
			With     map[string]any
		}
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}

	production := workflow.Jobs["production"]
	if production.Uses != "./.github/workflows/production-build.yml" {
		t.Errorf("production uses %q, want the reusable production build", production.Uses)
	}
	if got := production.With["publication"]; got != false {
		t.Errorf("production publication = %v, want false", got)
	}
	if got := production.Strategy.Matrix.Flavour; got != "${{ fromJSON(needs.plan.outputs.production-flavours) }}" {
		t.Errorf("production matrix = %q, want the planned flavours", got)
	}
}

// Production bytes upgrade from the stable release of their own series: the
// latest release for the merge queue, and the newest stable tag reachable from
// the tested base for a pull request into a release branch.
func TestCIProductionIdentity(t *testing.T) {
	root := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(root, ".github/workflows/ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct{ Steps []struct{ ID, Run string } }
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	var identity string
	for _, step := range workflow.Jobs["plan"].Steps {
		if step.ID == "production" {
			identity = strings.ReplaceAll(step.Run, "scripts/katl-release-artifacts", filepath.Join(root, "scripts", "katl-release-artifacts"))
		}
	}

	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet", "--initial-branch=main")
	commit := func(tag string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "file"), []byte(tag+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runGit(t, dir, "add", "file")
		runGit(t, dir, "commit", "--quiet", "-m", tag)
		runGit(t, dir, "tag", tag)
	}
	commit("v2026.10.2")
	series := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))
	commit("v2026.11.4")
	main := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))

	tools := t.TempDir()
	writeFakeExecutable(t, tools, "gh", `printf 'v2026.11.4\n'`)

	for _, tc := range []struct{ name, baseSHA, previous, version string }{
		{"merge group", "", "v2026.11.4", "2026.11.5-dev.0"},
		{"pull request into main series", main, "v2026.11.4", "2026.11.5-dev.0"},
		{"pull request into older release series", series, "v2026.10.2", "2026.10.3-dev.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			outputPath := filepath.Join(t.TempDir(), "output")
			command := exec.Command("bash", "-e", "-c", identity)
			command.Dir = dir
			command.Env = append(gittest.Env(os.Environ(), dir), "PATH="+tools+string(os.PathListSeparator)+os.Getenv("PATH"),
				"GITHUB_REPOSITORY=katl-dev/katl", "GH_TOKEN=unused", "BASE_SHA="+tc.baseSHA, "GITHUB_OUTPUT="+outputPath)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("identity: %v: %s", err, output)
			}
			outputs, err := os.ReadFile(outputPath)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"previous-tag=" + tc.previous + "\n", "version=" + tc.version + "\n"} {
				if !strings.Contains(string(outputs), want) {
					t.Errorf("outputs = %s, want %q", outputs, want)
				}
			}
		})
	}
}

// A planned production build must succeed for CI to pass, and an unplanned one
// must not run.
func TestCIGateProduction(t *testing.T) {
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
		name, flavours, production, qualification, bundle string
		pass                                              bool
	}{
		{"success", `["standard","lts"]`, "success", "success", "success", true},
		{"production failure", `["standard","lts"]`, "failure", "success", "skipped", false},
		{"production cancelled", `["standard","lts"]`, "cancelled", "success", "skipped", false},
		{"selected but skipped", `["standard","lts"]`, "skipped", "success", "skipped", false},
		{"bundle failure", `["standard","lts"]`, "success", "success", "failure", false},
		{"bundle cancelled", `["standard","lts"]`, "success", "success", "cancelled", false},
		{"selected bundle skipped", `["standard","lts"]`, "success", "success", "skipped", false},
		{"unselected but run", `[]`, "success", "skipped", "skipped", false},
		{"unselected bundle run", `[]`, "skipped", "skipped", "success", false},
		{"qualification failure", `["standard","lts"]`, "success", "failure", "success", false},
		{"qualification cancelled", `["standard","lts"]`, "success", "cancelled", "success", false},
		{"selected qualification skipped", `["standard","lts"]`, "success", "skipped", "success", false},
		{"unselected qualification run", `[]`, "skipped", "success", "skipped", false},
		{"missing plan output", ``, "skipped", "skipped", "skipped", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			needs, err := json.Marshal(map[string]any{
				"plan": map[string]any{"result": "success", "outputs": map[string]string{
					"vm-flavours": `[]`, "driver-flavours": `[]`, "production-flavours": tc.flavours,
				}},
				"checks":        map[string]string{"result": "success"},
				"nix":           map[string]string{"result": "success"},
				"vms":           map[string]string{"result": "skipped"},
				"drivers":       map[string]string{"result": "skipped"},
				"production":    map[string]string{"result": tc.production},
				"qualification": map[string]string{"result": tc.qualification},
				"bundle":        map[string]string{"result": tc.bundle},
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

// Production VM qualifies the assembled bytes the production job uploaded for
// the same commit, upgrading from the release the production identity chose.
func TestCIQualificationJob(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), ".github/workflows/ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Uses        string `yaml:"uses"`
			Strategy    struct{ Matrix struct{ Flavour string } }
			With        map[string]any
			Permissions map[string]string
		}
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}

	qualification := workflow.Jobs["qualification"]
	if qualification.Uses != "./.github/workflows/production-vmtests.yml" {
		t.Errorf("qualification uses %q, want the production VM workflow", qualification.Uses)
	}
	if got := qualification.Strategy.Matrix.Flavour; got != "${{ fromJSON(needs.plan.outputs.production-flavours) }}" {
		t.Errorf("qualification matrix = %q, want the planned production flavours", got)
	}
	for name, want := range map[string]any{
		"flavour":       "${{ matrix.flavour }}",
		"artifact-name": "katl-assembled-${{ matrix.flavour }}-${{ github.sha }}",
		"previous-tag":  "${{ needs.plan.outputs.production-previous-tag }}",
	} {
		if got := qualification.With[name]; got != want {
			t.Errorf("qualification %s = %v, want %v", name, got, want)
		}
	}
	if _, ok := qualification.With["run-id"]; ok {
		t.Error("qualification sets run-id, which would download another run's bytes")
	}
	if got := qualification.Permissions["actions"]; got != "read" {
		t.Errorf("qualification actions permission = %q, want read to download the build artifact", got)
	}
}
