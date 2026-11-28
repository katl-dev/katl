package scriptstest

import (
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/releasestate"
	"gopkg.in/yaml.v3"
)

func readReleaseWorkflow(t *testing.T) (workflowFile, string) {
	t.Helper()
	return readWorkflow(t, filepath.Base(releasestate.ReleaseWorkflow))
}

func allWorkflows(t *testing.T, visit func(string, workflowFile)) {
	t.Helper()
	dir := filepath.Join(repoRoot(t), filepath.Dir(releasestate.ReleaseWorkflow))
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !entry.IsDir() && (filepath.Ext(entry.Name()) == ".yml" || filepath.Ext(entry.Name()) == ".yaml") {
			workflow, _ := readWorkflow(t, entry.Name())
			visit(entry.Name(), workflow)
		}
	}
}

// Only final publication and the standalone credential check need release
// approval. Candidate jobs must run unattended.
func TestReleaseEnvironment(t *testing.T) {
	var got []string
	allWorkflows(t, func(file string, workflow workflowFile) {
		for id, job := range workflow.Jobs {
			switch job.Environment.Kind {
			case yaml.ScalarNode:
				if job.Environment.Value == releasestate.ReleaseEnvironment {
					got = append(got, file+":"+id)
				}
			case yaml.MappingNode:
				var env struct{ Name string }
				if err := job.Environment.Decode(&env); err != nil {
					t.Fatal(err)
				}
				if env.Name == releasestate.ReleaseEnvironment {
					got = append(got, file+":"+id)
				}
			}
			if file == filepath.Base(releasestate.ReleaseWorkflow) && id != "publish" && job.Environment.Kind != 0 {
				t.Errorf("%s has an environment and must run without approval", id)
			}
		}
	})
	slices.Sort(got)
	want := []string{"release-credential.yml:check", filepath.Base(releasestate.ReleaseWorkflow) + ":publish"}
	if !slices.Equal(got, want) {
		t.Errorf("jobs in %s = %v, want %v", releasestate.ReleaseEnvironment, got, want)
	}
}

var (
	releaseCommand = regexp.MustCompile(`\b(?:katl-release[ \t]+(?:draft|publish)|gh[ \t]+release[ \t]+(?:create|edit|upload|delete))\b`)
	apiCommand     = regexp.MustCompile(`\bgh[ \t]+api\b`)
	apiMutation    = regexp.MustCompile(`(?:--method(?:[ \t]+|=)|-X[ \t]*)(?:POST|PATCH|PUT|DELETE)\b|[ \t](?:-[fF]|--(?:raw-)?field|--input)(?:[ \t=]|$)`)
	apiGet         = regexp.MustCompile(`(?:--method(?:[ \t]+|=)|-X[ \t]*)GET\b`)
)

func apiWrites(run, path string) bool {
	for _, line := range strings.Split(strings.ReplaceAll(run, "\\\n", " "), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "#") && strings.Contains(line, path) &&
			apiCommand.MatchString(line) && apiMutation.MatchString(line) && !apiGet.MatchString(line) {
			return true
		}
	}
	return false
}

func writesRelease(step workflowStep) bool {
	for _, action := range []string{"softprops/action-gh-release@", "ncipollo/release-action@", "actions/create-release@"} {
		if strings.HasPrefix(step.Uses, action) {
			return true
		}
	}
	return releaseCommand.MatchString(step.Run) || apiWrites(step.Run, "/releases")
}

// A publication command elsewhere bypasses the workflow's approval boundary.
func TestReleaseWriters(t *testing.T) {
	var got []string
	allWorkflows(t, func(file string, workflow workflowFile) {
		for id, job := range workflow.Jobs {
			if slices.ContainsFunc(job.Steps, writesRelease) {
				got = append(got, file+":"+id)
			}
		}
	})
	slices.Sort(got)
	release := filepath.Base(releasestate.ReleaseWorkflow)
	want := []string{"release-artifacts.yml:publish-tag", release + ":publish", release + ":publish-candidate"}
	if !slices.Equal(got, want) {
		t.Errorf("release writers = %v, want %v", got, want)
	}
}

// requireCredential checks that step mints the App token through the shared
// action, the only place a caller may reference the App credential.
func requireCredential(t *testing.T, step workflowStep) {
	t.Helper()
	wantInputs := map[string]string{
		"app-id":      "${{ vars.KATL_RELEASE_APP_ID }}",
		"private-key": "${{ secrets.KATL_RELEASE_APP_PRIVATE_KEY }}",
	}
	if step.Uses != "./.github/actions/release-credential" || !maps.Equal(step.With, wantInputs) ||
		step.Run != "" || step.If != "" || step.ContinueOnError {
		t.Errorf("credential step = %+v, want a mandatory shared App credential", step)
	}
}

// Final publication and its check both use the repository-scoped App token.
func TestReleaseCredential(t *testing.T) {
	const checkFile = "release-credential.yml"

	check, source := readWorkflow(t, checkFile)
	var events map[string]any
	if err := check.On.Decode(&events); err != nil {
		t.Fatal(err)
	}
	if _, manual := events["workflow_dispatch"]; !manual || len(events) != 1 ||
		!maps.Equal(check.Permissions, map[string]string{"contents": "read"}) {
		t.Error("credential check must be dispatch-only with contents: read")
	}
	job := check.Jobs["check"]
	if len(check.Jobs) != 1 || job.Permissions.Kind != 0 ||
		job.Environment.Kind != yaml.ScalarNode || job.Environment.Value != releasestate.ReleaseEnvironment {
		t.Error("credential check must have one job in the release environment with inherited read permissions")
	}
	if len(job.Steps) != 3 || !strings.HasPrefix(job.Steps[0].Uses, "actions/checkout@") || job.Steps[2].Uses != "" {
		t.Fatal("credential check must only check out the repository, verify the credential and write the summary")
	}
	requireCredential(t, job.Steps[1])
	secretReferences := regexp.MustCompile(`(?i)\bsecrets\s*[.\[]`)
	if refs := secretReferences.FindAllString(source, -1); len(refs) != 1 {
		t.Errorf("%s references %d secrets, want only the release App private key", checkFile, len(refs))
	}

	checkDir := t.TempDir()
	checkSummary := filepath.Join(checkDir, "summary")
	if output, err := runReleaseStep(checkDir, job.Steps[2].Run, "GITHUB_STEP_SUMMARY="+checkSummary); err != nil {
		t.Fatalf("credential check summary: %v: %s", err, output)
	}
	const wantSummary = "The katl-release App minted a token scoped to this repository with Contents write permission. The token read repository contents.\n"
	if body, err := os.ReadFile(checkSummary); err != nil || string(body) != wantSummary {
		t.Errorf("credential check summary = %q, %v", body, err)
	}

	workflow, _ := readReleaseWorkflow(t)
	credential := -1
	writes := 0
	for i, step := range workflow.Jobs["publish"].Steps {
		if strings.HasPrefix(step.Uses, "actions/create-github-app-token@") {
			t.Error("another App token is created in publish")
		}
		if step.ID == "credential" {
			if credential != -1 {
				t.Fatal("more than one credential step")
			}
			credential = i
			requireCredential(t, step)
		}
		if writesRelease(step) || apiWrites(step.Run, "/git/refs") {
			writes++
			if credential < 0 || i <= credential || step.Env["GH_TOKEN"] != "${{ steps.credential.outputs.token }}" ||
				strings.Contains(step.Run, "github.token") {
				t.Errorf("%s does not use the preceding App token", step.Name)
			}
		}
	}
	if credential == -1 || writes == 0 {
		t.Fatalf("credential step or release writes missing")
	}
	steps := workflow.Jobs["publish"].Steps
	if credential+1 >= len(steps) {
		t.Fatal("missing publishing credential summary")
	}
	dir := t.TempDir()
	summary := filepath.Join(dir, "summary")
	if output, err := runReleaseStep(dir, steps[credential+1].Run, "GITHUB_STEP_SUMMARY="+summary); err != nil {
		t.Fatalf("credential summary: %v: %s", err, output)
	}
	if body, err := os.ReadFile(summary); err != nil || string(body) != "Publishing credential: katl-release App.\n" {
		t.Errorf("credential summary = %q, %v", body, err)
	}
	for _, step := range workflow.Jobs["qualify"].Steps {
		if writesRelease(step) || apiWrites(step.Run, "/git/refs") {
			t.Errorf("qualification step %s mutates a release or tag before approval", step.Name)
		}
	}
}

func TestReleaseAppReferences(t *testing.T) {
	// TestReleaseCredential and TestCutReleaseWorkflow pin both references to
	// the shared action's inputs. Any additional reference, including in a
	// different YAML field, is invalid.
	allowed := map[string]int{
		"workflows/cut-release.yml":                                2,
		"workflows/release-credential.yml":                         2,
		"workflows/" + filepath.Base(releasestate.ReleaseWorkflow): 2,
	}
	root := os.DirFS(filepath.Join(repoRoot(t), ".github"))
	err := fs.WalkDir(root, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		ext := filepath.Ext(path)
		workflow := filepath.Dir(path) == "workflows" && (ext == ".yml" || ext == ".yaml")
		if !workflow && entry.Name() != "action.yml" && entry.Name() != "action.yaml" {
			return nil
		}
		data, err := fs.ReadFile(root, path)
		if err != nil {
			return err
		}
		var document any
		if err := yaml.Unmarshal(data, &document); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		if got, want := releaseAppReferences(document), allowed[path]; got != want {
			t.Errorf("%s has %d KATL_RELEASE_APP_ references, want %d", path, got, want)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func releaseAppReferences(value any) int {
	var count int
	switch value := value.(type) {
	case string:
		return strings.Count(strings.ToUpper(value), "KATL_RELEASE_APP_")
	case []any:
		for _, item := range value {
			count += releaseAppReferences(item)
		}
	case map[string]any:
		for key, item := range value {
			count += releaseAppReferences(key) + releaseAppReferences(item)
		}
	case map[any]any:
		for key, item := range value {
			count += releaseAppReferences(key) + releaseAppReferences(item)
		}
	}
	return count
}

type releaseCredentialStep struct {
	Name, Run, Uses string
	Env, With       map[string]string
	If              yaml.Node
	ContinueOnError yaml.Node `yaml:"continue-on-error"`
}

func releaseCredentialSteps(t *testing.T) []releaseCredentialStep {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(t), ".github", "actions", "release-credential", "action.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var action struct {
		Runs struct {
			Steps []releaseCredentialStep
		}
	}
	if err := yaml.Unmarshal(data, &action); err != nil {
		t.Fatal(err)
	}
	return action.Runs.Steps
}

func releaseAppSlugStep(t *testing.T) releaseCredentialStep {
	t.Helper()
	for _, step := range releaseCredentialSteps(t) {
		if step.Env["EXPECTED_APP_SLUG"] != "" {
			return step
		}
	}
	t.Fatal("release credential action has no expected App slug")
	return releaseCredentialStep{}
}

func expectedReleaseAppSlug(t *testing.T) string {
	t.Helper()
	return releaseAppSlugStep(t).Env["EXPECTED_APP_SLUG"]
}

func TestReleaseCredentialSlug(t *testing.T) {
	step := releaseAppSlugStep(t)
	const source = "${{ steps.mint.outputs.app-slug }}"
	if got := step.Env["APP_SLUG"]; got != source {
		t.Errorf("APP_SLUG = %q, want %q", got, source)
	}
	expected := step.Env["EXPECTED_APP_SLUG"]

	t.Run("match", func(t *testing.T) {
		out, err := runReleaseStep(t.TempDir(), step.Run, "APP_SLUG="+expected, "EXPECTED_APP_SLUG="+expected)
		if err != nil {
			t.Fatalf("matching slug was rejected: %v: %s", err, out)
		}
	})
	t.Run("mismatch", func(t *testing.T) {
		const actual = "other-release-app"
		out, err := runReleaseStep(t.TempDir(), step.Run, "APP_SLUG="+actual, "EXPECTED_APP_SLUG="+expected)
		if _, exited := err.(*exec.ExitError); !exited {
			t.Fatalf("mismatching slug must exit non-zero: %v: %s", err, out)
		}
		if !strings.Contains(string(out), actual) || !strings.Contains(string(out), expected) {
			t.Errorf("output = %q, want actual slug %q and expected slug %q", out, actual, expected)
		}
	})
}

func TestReleaseCredentialScope(t *testing.T) {
	want := map[string]string{
		"app-id":              "${{ inputs.app-id }}",
		"private-key":         "${{ inputs.private-key }}",
		"owner":               "${{ github.repository_owner }}",
		"repositories":        "${{ github.event.repository.name }}",
		"permission-contents": "write",
	}
	var mints, checks int
	var verify string
	for _, step := range releaseCredentialSteps(t) {
		if apiWrites(step.Run, "") {
			t.Errorf("credential step %q makes a mutating API call", step.Name)
		}
		if step.ContinueOnError.Kind != 0 {
			t.Errorf("credential step %q sets continue-on-error", step.Name)
		}
		if step.If.Kind != 0 && (step.Name != "Report a token mint failure" ||
			step.If.Value != "failure() && steps.mint.outcome == 'failure'") {
			t.Errorf("credential step %q is conditional", step.Name)
		}
		if strings.HasPrefix(step.Uses, "actions/create-github-app-token@") {
			mints++
			if !maps.Equal(step.With, want) || step.If.Kind != 0 {
				t.Error("mint step must request a token scoped to the release repository")
			}
		}
		if step.Env["GH_TOKEN"] == "${{ steps.mint.outputs.token }}" {
			checks++
			verify = step.Run
			if step.If.Kind != 0 {
				t.Error("repository scope and access verification must run after minting succeeds")
			}
		}
	}
	if mints != 1 {
		t.Errorf("token mint steps = %d, want 1", mints)
	}
	if checks != 1 || verify == "" {
		t.Fatal("want one verification step using the minted token")
	}

	const repo = "katl-dev/katl"
	for _, tc := range []struct {
		name, repos, readStatus, wantError string
	}{
		{name: "exact", repos: repo, readStatus: "0"},
		{name: "extra repository", repos: repo + "\nkatl-dev/other", readStatus: "0", wantError: "must list only " + repo},
		{name: "wrong repository", repos: "katl-dev/other", readStatus: "0", wantError: "must list only " + repo},
		{name: "unreadable repository", repos: repo, readStatus: "1", wantError: "Could not read repository contents"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			const gh = `#!/bin/sh
case "$*" in
  "api --method GET --paginate /installation/repositories --jq .repositories[].full_name")
    printf '%s\n' "$INSTALLATION_REPOSITORIES";;
  "api --method GET /repos/$GITHUB_REPOSITORY/contents")
    exit "$REPOSITORY_READ_STATUS";;
  *) exit 2;;
esac
`
			if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(gh), 0o755); err != nil {
				t.Fatal(err)
			}
			out, err := runReleaseStep(dir, verify, "PATH="+dir+":"+os.Getenv("PATH"), "GITHUB_REPOSITORY="+repo,
				"INSTALLATION_REPOSITORIES="+tc.repos, "REPOSITORY_READ_STATUS="+tc.readStatus)
			if tc.wantError == "" {
				if err != nil {
					t.Fatalf("verification failed: %v: %s", err, out)
				}
			} else if err == nil || !strings.Contains(string(out), tc.wantError) {
				t.Fatalf("verification output = %q, %v, want error containing %q", out, err, tc.wantError)
			}
		})
	}
}

// These paths catch unattended final publication, resumes that rebuild,
// refusals that act and failed qualification that still reaches publication.
func TestReleaseActions(t *testing.T) {
	workflow, source := readReleaseWorkflow(t)
	if workflow.Name != "Release" || !maps.Equal(workflow.Permissions, map[string]string{"contents": "read"}) {
		t.Fatalf("workflow name or default permissions differ from the release interface")
	}
	for _, forbidden := range []string{"katl/release/", "refs/heads/release/"} {
		if strings.Contains(source, forbidden) {
			t.Errorf("workflow owns a release state name: %s", forbidden)
		}
	}
	for id, name := range map[string]string{
		"decide": "Decide", "start": "Record build start", "build": "Build", "qualify": "Record qualification",
		"publish-candidate": "Publish pre-release", "publish": "Publish",
	} {
		if got := workflow.Jobs[id].Name; got != name {
			t.Errorf("%s name = %q, want %q", id, got, name)
		}
	}
	for _, tc := range []struct {
		action, start, qualify string
		want                   []string
	}{
		{"candidate", "success", "success", []string{"build", "decide", "publish-candidate", "qualify", "start"}},
		{"retry-candidate", "success", "success", []string{"build", "decide", "publish-candidate", "qualify", "start"}},
		{"resume-candidate", "skipped", "skipped", []string{"decide", "publish-candidate"}},
		{"final", "success", "success", []string{"build", "decide", "publish", "qualify", "start"}},
		{"retry-final", "success", "success", []string{"build", "decide", "publish", "qualify", "start"}},
		{"refuse", "skipped", "skipped", []string{"decide"}},
		{"candidate", "success", "failure", []string{"build", "decide", "qualify", "start"}},
	} {
		t.Run(tc.action+"/"+tc.qualify, func(t *testing.T) {
			ctx := map[string]string{
				"needs.decide.outputs.action": tc.action, "needs.decide.outputs.attempt": "1", "github.run_attempt": "1",
				"needs.decide.result": "success", "needs.start.result": tc.start, "needs.qualify.result": tc.qualify,
			}
			if tc.action == "refuse" {
				ctx["needs.decide.result"] = "failure"
			}
			if got := jobsRun(t, workflow, ctx); !slices.Equal(got, tc.want) {
				t.Errorf("jobs = %v, want %v", got, tc.want)
			}
		})
	}
}

// A partial rerun carries earlier outputs but cannot start, rebuild,
// qualify or publish until it makes a decision of its own.
func TestReleaseAttemptGuard(t *testing.T) {
	workflow, _ := readReleaseWorkflow(t)
	ctx := map[string]string{"needs.decide.outputs.action": "final", "needs.decide.outputs.attempt": "1", "github.run_attempt": "2"}
	if conditionHolds(t, workflow.Jobs["build"].If, ctx) {
		t.Fatal("the build accepts a stale decision")
	}
	dir := t.TempDir()
	for _, id := range []string{"start", "qualify", "publish-candidate", "publish"} {
		t.Run(id, func(t *testing.T) {
			steps := workflow.Jobs[id].Steps
			if len(steps) == 0 || steps[0].If != "" || steps[0].ContinueOnError {
				t.Fatal("missing unconditional first step")
			}
			asNew := expandExpressions(t, steps[0].Env["DECISION_ATTEMPT"], ctx)
			out, err := runReleaseStep(dir, steps[0].Run, "DECISION_ATTEMPT="+asNew, "GITHUB_RUN_ATTEMPT=2")
			if err == nil || string(out) != "This attempt has no decision of its own. Dispatch the release action again.\n" {
				t.Errorf("first step = %q, %v, want stale decision refusal", out, err)
			}
			if out, err := runReleaseStep(dir, steps[0].Run, "DECISION_ATTEMPT=2", "GITHUB_RUN_ATTEMPT=2"); err != nil {
				t.Errorf("first step refuses its own decision: %v: %s", err, out)
			}
		})
	}
}

// A change to the caller's grants, inputs or call count can invalidate every
// build or make independent calls collide on artifact names.
func TestReleaseBuildCall(t *testing.T) {
	workflow, _ := readReleaseWorkflow(t)
	const called = "./.github/workflows/release-artifacts.yml"
	var calls []string
	for id, job := range workflow.Jobs {
		if job.Uses == called {
			calls = append(calls, id)
		}
	}
	if !slices.Equal(calls, []string{"build"}) {
		t.Fatalf("calls of %s = %v, want [build]", called, calls)
	}
	build := workflow.Jobs["build"]
	requireNeeds(t, workflow, [][2]string{{"build", "decide"}, {"build", "start"}})
	if want := (map[string]string{"version": "${{ needs.decide.outputs.version }}", "commit": "${{ needs.decide.outputs.commit }}"}); !maps.Equal(build.With, want) {
		t.Errorf("build inputs = %v, want %v", build.With, want)
	}
	var permissions map[string]string
	if err := build.Permissions.Decode(&permissions); err != nil {
		t.Fatal(err)
	}
	if want := (map[string]string{"actions": "read", "attestations": "write", "contents": "write", "id-token": "write", "packages": "write"}); !maps.Equal(permissions, want) {
		t.Errorf("build permissions = %v, want %v", permissions, want)
	}

	var trigger struct {
		Dispatch struct{ Inputs map[string]yaml.Node } `yaml:"workflow_dispatch"`
	}
	artifacts, _ := readWorkflow(t, filepath.Base(called))
	if err := artifacts.On.Decode(&trigger); err != nil {
		t.Fatal(err)
	}
	if _, ok := trigger.Dispatch.Inputs["commit"]; ok {
		t.Error("release-artifacts.yml accepts a commit in workflow_dispatch, which would make it look like a call")
	}
}

func runReleaseStep(dir, script string, env ...string) ([]byte, error) {
	cmd := exec.Command("bash", "-euo", "pipefail", "-c", script)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	return cmd.CombinedOutput()
}

// The decide job cannot hand off a truncated or foreign commit, or lose the
// recorded run ID when mapping the decision's JSON to workflow outputs.
func TestReleaseDecisionCommit(t *testing.T) {
	workflow, _ := readReleaseWorkflow(t)
	job := workflow.Jobs["decide"]
	if got, want := job.Outputs["build-run"], "${{ steps.decision.outputs.build-run }}"; got != want {
		t.Errorf("build-run job output = %q, want %q", got, want)
	}
	var decision workflowStep
	for _, step := range job.Steps {
		if step.ID == "decision" {
			decision = step
		}
	}
	if decision.Run == "" || decision.If != "" || decision.ContinueOnError {
		t.Fatal("missing required decision step")
	}

	for _, tc := range []struct {
		name, commit, runSHA string
		wantSuccess          bool
	}{
		{name: "matching", commit: calledCommit, runSHA: calledCommit, wantSuccess: true},
		{name: "abbreviated", commit: calledCommit[:12], runSHA: calledCommit[:12]},
		{name: "different", commit: strings.Repeat("a", 40), runSHA: calledCommit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			scripts := filepath.Join(dir, "scripts")
			if err := os.Mkdir(scripts, 0o755); err != nil {
				t.Fatal(err)
			}
			stub := "#!/bin/sh\n[ \"$*\" = 'next --retry try again' ] || exit 5\nprintf '{\"action\":\"resume-candidate\",\"commit\":\"%s\",\"build_run\":42}\n' \"$DECIDED_COMMIT\"\n"
			if err := os.WriteFile(filepath.Join(scripts, "katl-release"), []byte(stub), 0o755); err != nil {
				t.Fatal(err)
			}
			outputs := filepath.Join(dir, "output")
			summary := filepath.Join(dir, "summary")
			out, err := runReleaseStep(dir, decision.Run, "DECIDED_COMMIT="+tc.commit, "GITHUB_SHA="+tc.runSHA,
				"RETRY=try again", "GITHUB_RUN_ATTEMPT=1", "GITHUB_OUTPUT="+outputs, "GITHUB_STEP_SUMMARY="+summary)
			if tc.wantSuccess {
				if err != nil {
					t.Fatalf("decision failed: %v: %s", err, out)
				}
				written, err := os.ReadFile(outputs)
				if err != nil {
					t.Fatal(err)
				}
				lines := strings.Split(string(written), "\n")
				if !slices.Contains(lines, "commit="+tc.commit) || !slices.Contains(lines, "build-run=42") {
					t.Errorf("decision outputs = %q, want the bound commit and build-run=42", written)
				}
				return
			}
			if err == nil {
				t.Fatalf("decision accepted %q with GITHUB_SHA=%q", tc.commit, tc.runSHA)
			}
			message, readErr := os.ReadFile(summary)
			if readErr != nil || !strings.Contains(string(message), "The decision names commit "+tc.commit+", but this run builds "+tc.runSHA) {
				t.Errorf("decision summary = %q, %v, want commit binding refusal", message, readErr)
			}
			if _, err := os.Stat(outputs); !os.IsNotExist(err) {
				t.Errorf("refused decision created outputs: %v", err)
			}
		})
	}
}

// A cancelled build can return empty outputs. Only a verifiably successful
// response proceeds, and a failure record cannot follow a successful write.
func TestReleaseQualification(t *testing.T) {
	workflow, _ := readReleaseWorkflow(t)
	qualify := workflow.Jobs["qualify"]
	if !strings.Contains(qualify.If, "always()") {
		t.Error("qualify skips failed or cancelled builds")
	}
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	artifact := "katl-2026.12.0-x86_64-0123456789ab"
	stub := "#!/bin/sh\n[ \"$*\" = 'bundle-name 2026.12.0 " + calledCommit + "' ] || exit 5\necho '" + artifact + "'\n"
	if err := os.WriteFile(filepath.Join(dir, "scripts", "katl-release"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	validation, qualified, failed := workflowStep{}, -1, -1
	for i, step := range qualify.Steps {
		switch {
		case step.ID == "validation":
			validation = step
		case strings.Contains(step.Run, `record "$VERSION" qualified`):
			qualified = i
			if !strings.HasSuffix(strings.TrimSpace(step.Run), `scripts/katl-release record "$VERSION" qualified "$DIGEST"`) {
				t.Error("the qualified write is followed by more operations")
			}
		case strings.Contains(step.Run, `record "$VERSION" failed`):
			failed = i
			if step.If != "failure() && steps.qualification.outcome != 'success' && needs.decide.outputs.attempt == github.run_attempt" {
				t.Errorf("the failed write can run on a completed qualification or stale decision: %s", step.If)
			}
		}
	}
	if validation.Run == "" || qualified < 0 || failed != qualified+1 || failed != len(qualify.Steps)-1 {
		t.Fatal("missing validation or qualification outcomes, or steps run after the qualified/failed writes")
	}
	if got := validation.Env; !maps.Equal(got, map[string]string{
		"VALIDATION": "${{ needs.build.outputs.validation }}", "DIGEST": "${{ needs.build.outputs.sha256sums }}", "ARTIFACT": "${{ needs.build.outputs.artifact }}",
	}) {
		t.Errorf("validation inputs = %v", got)
	}
	for _, verdict := range []string{"success", "failure", ""} {
		for _, digest := range []string{strings.Repeat("1", 64), "", "invalid"} {
			for _, name := range []string{artifact, "wrong-artifact"} {
				out, err := runReleaseStep(dir, validation.Run, "VALIDATION="+verdict, "DIGEST="+digest, "ARTIFACT="+name,
					"VERSION=2026.12.0", "COMMIT="+calledCommit, "GITHUB_STEP_SUMMARY="+filepath.Join(dir, "summary"))
				if want := verdict == "success" && len(digest) == 64 && name == artifact; (err == nil) != want {
					t.Errorf("validation %q, %q, %q = %v: %s, want success=%v", verdict, digest, name, err, out, want)
				}
			}
		}
	}
}

// A candidate tag without its building record looks foreign, and a retry
// must use the tag it already has.
func TestReleaseCandidateTag(t *testing.T) {
	workflow, _ := readReleaseWorkflow(t)
	record, tag := -1, -1
	for i, step := range workflow.Jobs["start"].Steps {
		if strings.Contains(step.Run, `record "$VERSION" building`) {
			record = i
		}
		if apiWrites(step.Run, "/git/refs") {
			tag = i
			for _, action := range []string{"candidate", "retry-candidate", "resume-candidate", "final", "retry-final", "refuse"} {
				if got, want := conditionHolds(t, step.If, map[string]string{"needs.decide.outputs.action": action}), action == "candidate"; got != want {
					t.Errorf("create tag for %s = %v, want %v", action, got, want)
				}
			}
		}
	}
	if record < 0 || tag <= record {
		t.Fatalf("start must record building before creating a tag")
	}
}

// Publication requires the qualified digest and a fresh final decision after
// approval. Every check must run before registry writes, and final publication
// must inherit the default successful-dependency condition.
func TestReleasePublicationOrder(t *testing.T) {
	workflow, _ := readReleaseWorkflow(t)
	publish := workflow.Jobs["publish"]
	if status := regexp.MustCompile(`(?i)\b(always|failure|cancelled|success)\s*\(`).FindString(publish.If); status != "" {
		t.Errorf("publish.if contains a status function %q and overrides the default condition", status)
	}
	requireNeeds(t, workflow, [][2]string{{"publish", "decide"}, {"publish", "qualify"}})
	if got, want := publish.Env["DIGEST"], "${{ needs.qualify.outputs.digest }}"; got != want {
		t.Errorf("publish DIGEST = %q, want %q", got, want)
	}
	for _, id := range []string{"publish-candidate", "publish"} {
		checks := []string{"attempt guard"}
		if id == "publish" {
			checks = append(checks, "checkout", "credential", "confirm-final", "digest check", "equivalent")
		} else {
			checks = append(checks, "require-candidate", "digest check")
		}
		commands := []string{"match-release-extensions", "katl-release draft", "publish-release-extensions", "katl-release publish"}
		want := append(checks, commands...)

		var got []string
		for _, step := range workflow.Jobs[id].Steps {
			before := len(got)
			switch {
			case step.Env["DECISION_ATTEMPT"] != "":
				got = append(got, "attempt guard")
			case id == "publish" && strings.HasPrefix(step.Uses, "actions/checkout@"):
				got = append(got, "checkout")
			case step.ID == "credential":
				got = append(got, "credential")
			case strings.Contains(step.Run, "katl-release confirm-final"):
				got = append(got, "confirm-final")
				if strings.TrimSpace(step.Run) != `scripts/katl-release confirm-final "$VERSION" "$COMMIT" "$DIGEST"` ||
					step.Env["GH_TOKEN"] != "${{ github.token }}" {
					t.Errorf("confirm-final must check this version, commit and digest with github.token: %+v", step)
				}
			case strings.Contains(step.Run, "katl-release require-candidate"):
				got = append(got, "require-candidate")
			case strings.Contains(step.Run, "sha256sum dist/SHA256SUMS") && strings.Contains(step.Run, `!= "$DIGEST"`):
				got = append(got, "digest check")
			case strings.Contains(step.Run, "katl-release equivalent"):
				got = append(got, "equivalent")
			}
			for _, command := range commands {
				if strings.Contains(step.Run, command) {
					got = append(got, command)
				}
			}
			if len(got) == before {
				continue
			}
			if len(got) != before+1 || step.If != "" || step.ContinueOnError {
				t.Errorf("%s requires a separate, mandatory step for %v", id, got[before:])
			}
			if id == "publish" && step.Env["DIGEST"] != "" {
				t.Errorf("publish step %q overrides the qualified DIGEST", step.Name)
			}
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s steps = %v, want %v", id, got, want)
		}
	}
}

// Even when the job condition selects the unattended path, a final or another
// non-candidate version must stop the job before publication.
func TestReleaseCandidatePublicationGuard(t *testing.T) {
	workflow, _ := readReleaseWorkflow(t)
	job := workflow.Jobs["publish-candidate"]
	if got, want := job.Env["VERSION"], "${{ needs.decide.outputs.version }}"; got != want {
		t.Errorf("publish-candidate VERSION = %q, want %q", got, want)
	}
	var guard workflowStep
	for _, step := range job.Steps {
		if strings.Contains(step.Run, "katl-release require-candidate") {
			guard = step
		}
	}
	if guard.Run == "" || guard.Env["VERSION"] != "" {
		t.Fatal("missing candidate publication guard for the decided version")
	}
	for _, tc := range []struct{ version, wantErr string }{
		{version: "2026.12.0-rc.1"},
		{version: "2026.12.0", wantErr: "Automatic pre-release publication requires a release candidate"},
		{version: "2026.12.0-beta.1", wantErr: "neither a release candidate nor a final version"},
		{version: "2026.12.0-rc.0", wantErr: "neither a release candidate nor a final version"},
	} {
		t.Run(tc.version, func(t *testing.T) {
			out, err := runReleaseStep(repoRoot(t), guard.Run, "VERSION="+tc.version)
			if tc.wantErr == "" && err != nil || tc.wantErr != "" && (err == nil || !strings.Contains(string(out), tc.wantErr)) {
				t.Errorf("candidate guard = %v: %s, want error %q", err, out, tc.wantErr)
			}
		})
	}
}

// A version holds its own concurrency group through approval and publication.
func TestReleaseConcurrency(t *testing.T) {
	workflow, _ := readReleaseWorkflow(t)
	var concurrency struct {
		Group  string
		Cancel string `yaml:"cancel-in-progress"`
	}
	if err := workflow.Concurrency.Decode(&concurrency); err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"2026.12.0", "2026.12.1"} {
		ref := "refs/heads/release/" + version
		if got, want := expandExpressions(t, concurrency.Group, map[string]string{"github.ref": ref}), "release-"+ref; got != want {
			t.Errorf("group = %q, want %q", got, want)
		}
	}
	if concurrency.Cancel != "false" {
		t.Errorf("cancel-in-progress = %q, want false", concurrency.Cancel)
	}
}
