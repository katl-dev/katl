package scriptstest

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/gittest"
	"gopkg.in/yaml.v3"
)

const calledCommit = "0123456789abcdef0123456789abcdef01234567"

// releaseTriggers are the expression contexts of each way release-artifacts.yml
// starts. A called run sees its caller's github context, so the call cases
// carry the caller's event. An unset number input compares equal to zero in
// GitHub Actions, so runs without qualify_run carry "0".
var releaseTriggers = map[string]map[string]string{
	"tag":            {"github.event_name": "push", "github.ref_type": "tag", "inputs.qualify_run": "0", "needs.identity.outputs.regression": "run"},
	"tag, CI passed": {"github.event_name": "push", "github.ref_type": "tag", "inputs.qualify_run": "0", "needs.identity.outputs.regression": "skip"},
	"nightly":        {"github.event_name": "schedule", "inputs.qualify_run": "0"},
	"dispatch":       {"github.event_name": "workflow_dispatch", "github.ref_type": "branch", "inputs.qualify_run": "0"},
	"requalify":      {"github.event_name": "workflow_dispatch", "github.ref_type": "branch", "inputs.qualify_run": "12345"},
	"call":           {"github.event_name": "workflow_dispatch", "github.ref_type": "branch", "inputs.qualify_run": "0", "inputs.commit": calledCommit},
	"call from tag":  {"github.event_name": "push", "github.ref_type": "tag", "inputs.qualify_run": "0", "inputs.commit": calledCommit},
}

func conditionHolds(t *testing.T, condition string, ctx map[string]string) bool {
	t.Helper()
	if condition == "" {
		return true
	}
	// GitHub Actions coerces a condition's value to a boolean.
	value := expandExpressions(t, "${{ "+condition+" }}", ctx)
	return value != "" && value != "false"
}

// jobsRun returns the jobs GitHub Actions starts for ctx: a job runs when its
// condition holds and every job it needs ran, unless its condition calls
// always(), which runs it whatever its dependencies did. Every job that runs is
// taken to succeed.
func jobsRun(t *testing.T, workflow workflowFile, ctx map[string]string) []string {
	t.Helper()
	ran := map[string]bool{}
	var runs func(name string) bool
	runs = func(name string) bool {
		if result, ok := ran[name]; ok {
			return result
		}
		job, ok := workflow.Jobs[name]
		if !ok {
			t.Fatalf("job needs undefined job %s", name)
		}
		dependencies := job.Needs.Content
		if job.Needs.Kind == yaml.ScalarNode {
			dependencies = []*yaml.Node{&job.Needs}
		}
		result := true
		if !strings.Contains(job.If, "always()") {
			for _, dependency := range dependencies {
				result = runs(dependency.Value) && result
			}
		}
		result = result && conditionHolds(t, job.If, ctx)
		ran[name] = result
		return result
	}

	var names []string
	for name := range workflow.Jobs {
		if runs(name) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

// Writing to repository contents or packages is what publication needs, so a
// job that asks for either is a publication job whatever it is called.
func publishes(t *testing.T, job workflowJob) bool {
	t.Helper()
	var permissions map[string]string
	if job.Permissions.Kind == yaml.MappingNode {
		if err := job.Permissions.Decode(&permissions); err != nil {
			t.Fatal(err)
		}
	}
	return permissions["contents"] == "write" || permissions["packages"] == "write"
}

// The tag, nightly and dispatch triggers keep the job graph they had before the
// workflow became callable, and a call runs the nightly's build and
// qualification graph with no publication, whichever event started its caller.
func TestReleaseTriggerJobs(t *testing.T) {
	workflow, _ := readWorkflow(t, "release-artifacts.yml")

	qualify := []string{
		"build-lts", "build-standard", "bundle", "identity",
		"qualification-lts", "qualification-standard", "qualified-bytes",
		"release-validation", "unit", "vmtests",
	}
	publish := append(slices.Clone(qualify), "publish-extensions", "publish-tag")
	want := map[string][]string{
		"tag":            publish,
		"tag, CI passed": slices.DeleteFunc(slices.Clone(publish), func(name string) bool { return name == "vmtests" }),
		"nightly":        qualify,
		"dispatch":       qualify,
		"requalify":      {"existing-bundle", "requalify"},
		"call":           qualify,
		"call from tag":  qualify,
	}
	for trigger, ctx := range releaseTriggers {
		t.Run(trigger, func(t *testing.T) {
			jobs := jobsRun(t, workflow, ctx)
			expected := slices.Clone(want[trigger])
			slices.Sort(expected)
			if !slices.Equal(jobs, expected) {
				t.Errorf("jobs run = %v, want %v", jobs, expected)
			}

			publication := false
			for _, name := range jobs {
				publication = publication || publishes(t, workflow.Jobs[name])
			}
			if want := strings.HasPrefix(trigger, "tag"); publication != want {
				t.Errorf("a publication job runs = %v, want %v", publication, want)
			}
		})
	}
}

// The caller reads the bundle name, its checksum digest and the validation
// verdict from job outputs that a step of a job on the called path sets.
func TestReleaseCallContract(t *testing.T) {
	workflow, _ := readWorkflow(t, "release-artifacts.yml")
	var on struct {
		Call struct {
			Inputs map[string]struct {
				Type     string
				Required bool
			}
			Outputs map[string]struct{ Value string }
		} `yaml:"workflow_call"`
	}
	if err := workflow.On.Decode(&on); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"version", "commit"} {
		if input, ok := on.Call.Inputs[name]; !ok || input.Type != "string" || !input.Required {
			t.Errorf("workflow_call input %s = %+v, want a required string", name, input)
		}
	}

	called := jobsRun(t, workflow, releaseTriggers["call"])
	jobOutput := regexp.MustCompile(`^\$\{\{ jobs\.([a-z-]+)\.outputs\.([a-z0-9-]+) \}\}$`)
	stepValue := regexp.MustCompile(`^\$\{\{ steps\.([a-z0-9-]+)\.(outputs\.[a-z0-9-]+|outcome) \}\}$`)
	var names []string
	for name, output := range on.Call.Outputs {
		names = append(names, name)
		match := jobOutput.FindStringSubmatch(output.Value)
		if match == nil {
			t.Errorf("output %s = %q, want a job output", name, output.Value)
			continue
		}
		job := workflow.Jobs[match[1]]
		if !slices.Contains(called, match[1]) {
			t.Errorf("output %s comes from %s, which a call does not run", name, match[1])
		}
		step := stepValue.FindStringSubmatch(job.Outputs[match[2]])
		if step == nil {
			t.Errorf("output %s reads %s output %s = %q, want a step value", name, match[1], match[2], job.Outputs[match[2]])
			continue
		}
		found := false
		for _, candidate := range job.Steps {
			if candidate.ID == step[1] {
				found = true
				if !conditionHolds(t, candidate.If, releaseTriggers["call"]) {
					t.Errorf("output %s comes from step %s, which a call skips", name, step[1])
				}
			}
		}
		if !found {
			t.Errorf("output %s reads step %s, which %s does not define", name, step[1], match[1])
		}
	}
	slices.Sort(names)
	if want := []string{"artifact", "sha256sums", "validation"}; !slices.Equal(names, want) {
		t.Errorf("workflow_call outputs = %v, want %v", names, want)
	}

	if got, want := workflow.Jobs["identity"].Outputs["artifact-name"], "${{ steps.release.outputs.artifact-name }}"; got != want {
		t.Errorf("identity artifact-name = %q, want the name the bundle upload uses", got)
	}
	validation := workflow.Jobs["release-validation"]
	for _, step := range validation.Steps {
		if validation.Outputs["result"] == "${{ steps."+step.ID+".outcome }}" && step.Env["QUALIFIED"] == "" {
			t.Errorf("validation output reads step %s, which does not judge the dependencies", step.ID)
		}
	}
}

// The digest names the bundle's SHA256SUMS file itself, so the caller can match
// it against what it publishes.
func TestReleaseCallDigest(t *testing.T) {
	workflow, _ := readWorkflow(t, "release-artifacts.yml")
	var script string
	for _, step := range workflow.Jobs["bundle"].Steps {
		if step.ID == "digest" {
			script = step.Run
		}
	}
	if script == "" {
		t.Fatal("the bundle job has no digest step")
	}

	dir := t.TempDir()
	sums := "1111111111111111111111111111111111111111111111111111111111111111  katlos.raw.zst\n"
	if err := os.MkdirAll(filepath.Join(dir, "dist"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "dist", "SHA256SUMS"), []byte(sums), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "dist", "katlos.raw.zst"), []byte("image"), 0o644); err != nil {
		t.Fatal(err)
	}
	outputs := filepath.Join(dir, "output")
	command := exec.Command("bash", "-euo", "pipefail", "-c", script)
	command.Dir = dir
	command.Env = append(os.Environ(), "GITHUB_OUTPUT="+outputs)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("digest step failed: %v\n%s", err, output)
	}

	written, err := os.ReadFile(outputs)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(sums))
	if want := "sha256=" + hex.EncodeToString(digest[:]) + "\n"; string(written) != want {
		t.Errorf("digest output = %q, want %q", written, want)
	}
}

// A called run takes the caller's version whatever event started the caller,
// and refuses to build any commit but the one it was asked for.
func TestReleaseCallIdentity(t *testing.T) {
	workflow, _ := readWorkflow(t, "release-artifacts.yml")
	var step workflowStep
	for _, candidate := range workflow.Jobs["identity"].Steps {
		if strings.Contains(candidate.Run, "katl-release-artifacts version") {
			step = candidate
		}
	}
	if got, want := step.Env["CALLED_COMMIT"], "${{ inputs.commit }}"; got != want {
		t.Fatalf("identity step CALLED_COMMIT = %q, want %q", got, want)
	}

	for _, tt := range []struct {
		name, event, commit, version string
		wantErr                      string
	}{
		{name: "candidate", event: "workflow_dispatch", commit: calledCommit, version: "2026.12.0-rc.1"},
		{name: "final", event: "workflow_dispatch", commit: calledCommit, version: "2026.12.0"},
		{name: "scheduled caller", event: "schedule", commit: calledCommit, version: "2026.12.0-rc.2"},
		{name: "tag caller", event: "push", commit: calledCommit, version: "2026.12.0-rc.3"},
		{name: "other commit", event: "workflow_dispatch", commit: "89abcdef0123456789abcdef0123456789abcdef", version: "2026.12.0-rc.1", wantErr: "Dispatch the Release workflow on the release branch whose head is 89abcdef0123456789abcdef0123456789abcdef."},
		{name: "invalid version", event: "workflow_dispatch", commit: calledCommit, version: "2026.12.0-rc.01", wantErr: "not canonical"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			outputs := filepath.Join(dir, "output")
			command := exec.Command("bash", "-euo", "pipefail", "-c", step.Run)
			command.Dir = repoRoot(t)
			command.Env = append(gittest.Env(os.Environ(), dir), "PREVIOUS_TAG=v2026.11.5",
				"GITHUB_EVENT_NAME="+tt.event, "GITHUB_REF_TYPE=tag", "GITHUB_REF_NAME=v2026.11.5", "MANUAL_VERSION="+tt.version,
				"CALLED_COMMIT="+tt.commit, "GITHUB_OUTPUT="+outputs, "GITHUB_SHA="+calledCommit, "KATL_ARCHITECTURE=x86_64")
			output, err := command.CombinedOutput()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(string(output), tt.wantErr) {
					t.Fatalf("identity step = %v, output %q, want failure containing %q", err, output, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("identity step failed: %v\n%s", err, output)
			}
			written, err := os.ReadFile(outputs)
			if err != nil {
				t.Fatal(err)
			}
			if want := "version=" + tt.version + "\nartifact-name=katl-" + tt.version + "-x86_64-0123456789ab\n"; string(written) != want {
				t.Errorf("identity outputs = %q, want %q", written, want)
			}
		})
	}
}

// The tag, nightly and dispatch groups are unchanged. A called run's github
// context is its caller's, so its group must differ from any group its caller
// would take from the same context, or the two would wait on each other.
func TestReleaseCallConcurrency(t *testing.T) {
	workflow, _ := readWorkflow(t, "release-artifacts.yml")
	var concurrency struct{ Group string }
	if err := workflow.Concurrency.Decode(&concurrency); err != nil {
		t.Fatal(err)
	}
	group := func(event, ref, runID, commit string) string {
		return expandExpressions(t, concurrency.Group, map[string]string{
			"github.workflow": "wf", "github.event_name": event, "github.ref": ref, "github.run_id": runID, "inputs.commit": commit,
		})
	}

	for name, tc := range map[string]struct{ got, want string }{
		"tag":      {group("push", "refs/tags/v2026.11.6-rc.1", "1", ""), "wf-refs/tags/v2026.11.6-rc.1"},
		"nightly":  {group("schedule", "refs/heads/main", "2", ""), "wf-nightly"},
		"dispatch": {group("workflow_dispatch", "refs/heads/main", "3", ""), "wf-3"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s group = %q, want %q", name, tc.got, tc.want)
		}
	}

	call := group("workflow_dispatch", "refs/heads/release/2026.12.0", "4", calledCommit)
	for _, caller := range []string{"wf-4", "wf-refs/heads/release/2026.12.0", "wf-release/2026.12.0", "wf-nightly"} {
		if call == caller {
			t.Errorf("called run shares group %q with its caller", call)
		}
	}
	if other := group("workflow_dispatch", "refs/heads/release/2026.12.0", "5", calledCommit); other == call {
		t.Errorf("calls from two runs share group %q", call)
	}
	if fromTag := group("push", "refs/heads/release/2026.12.0", "6", calledCommit); fromTag == group("push", "refs/heads/release/2026.12.0", "7", calledCommit) {
		t.Errorf("calls from two pushes of one ref share group %q, so GitHub would cancel the queued one", fromTag)
	}

	release, _ := readReleaseWorkflow(t)
	var caller struct{ Group string }
	if err := release.Concurrency.Decode(&caller); err != nil {
		t.Fatal(err)
	}
	ctx := map[string]string{"github.workflow": release.Name, "github.event_name": "workflow_dispatch",
		"github.ref": "refs/heads/release/2026.12.0", "github.run_id": "4", "inputs.commit": calledCommit}
	if got, called := expandExpressions(t, caller.Group, ctx), expandExpressions(t, concurrency.Group, ctx); got == called {
		t.Errorf("Release and its called build share concurrency group %q", got)
	}
}

// A tag created by the release App has already been built and published by
// Release. Its push event must not start the legacy build or publication jobs.
func TestReleaseAppTagPush(t *testing.T) {
	workflow, _ := readWorkflow(t, "release-artifacts.yml")
	ctx := map[string]string{"github.event_name": "push", "github.ref_type": "tag", "github.actor": "katl-release[bot]", "inputs.qualify_run": "0"}
	if got := jobsRun(t, workflow, ctx); len(got) != 0 {
		t.Errorf("App tag push runs legacy jobs %v", got)
	}
	ctx["inputs.commit"] = calledCommit
	if got, want := jobsRun(t, workflow, ctx), jobsRun(t, workflow, releaseTriggers["call from tag"]); !slices.Equal(got, want) {
		t.Errorf("called workflow runs jobs %v in the App's context, want %v", got, want)
	}
}
