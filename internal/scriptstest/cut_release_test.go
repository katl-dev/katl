package scriptstest

import (
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestCutReleaseWorkflow(t *testing.T) {
	// A second trigger or job, an approval wait, or another credential
	// path would break the protected, unattended cut workflow.
	workflow, _ := readWorkflow(t, "cut-release.yml")
	if workflow.Name != "Cut release" || workflow.RunName != "Cut release ${{ inputs.version }} at ${{ inputs.revision }}" {
		t.Fatalf("workflow name = %q, run name = %q", workflow.Name, workflow.RunName)
	}
	var triggers map[string]struct {
		Inputs map[string]struct {
			Type     string
			Required bool
		}
	}
	if err := workflow.On.Decode(&triggers); err != nil {
		t.Fatal(err)
	}
	dispatch, ok := triggers["workflow_dispatch"]
	if !ok || len(triggers) != 1 || len(dispatch.Inputs) != 2 {
		t.Fatalf("triggers = %+v, want workflow_dispatch with two inputs", triggers)
	}
	for _, name := range []string{"version", "revision"} {
		if input := dispatch.Inputs[name]; input.Type != "string" || !input.Required {
			t.Errorf("input %q = %+v, want a required string", name, input)
		}
	}
	var concurrency struct {
		Group            string
		CancelInProgress *bool `yaml:"cancel-in-progress"`
	}
	if err := workflow.Concurrency.Decode(&concurrency); err != nil {
		t.Fatal(err)
	}
	if concurrency.Group != "cut-release-${{ inputs.version }}" || concurrency.CancelInProgress == nil || *concurrency.CancelInProgress {
		t.Error("cuts must be serialized by version without cancelling an in-flight run")
	}

	job, ok := workflow.Jobs["cut"]
	if !ok || len(workflow.Jobs) != 1 || job.Name != "Create release branch" || job.Environment.Value != "release-cut" {
		t.Fatal("want one Create release branch job using release-cut")
	}
	var permissions map[string]string
	if err := job.Permissions.Decode(&permissions); err != nil {
		t.Fatal(err)
	}
	readContents := map[string]string{"contents": "read"}
	if !maps.Equal(workflow.Permissions, readContents) || !maps.Equal(permissions, readContents) {
		t.Error("workflow and job permissions must both be contents: read")
	}

	credential, create := -1, -1
	for i, step := range job.Steps {
		if step.ID == "credential" {
			if credential >= 0 {
				t.Fatal("more than one credential step")
			}
			credential = i
			want := map[string]string{
				"app-id":              "${{ vars.KATL_RELEASE_APP_ID }}",
				"private-key":         "${{ secrets.KATL_RELEASE_APP_PRIVATE_KEY }}",
				"owner":               "${{ github.repository_owner }}",
				"repositories":        "${{ github.event.repository.name }}",
				"permission-contents": "write",
			}
			if step.Uses != "actions/create-github-app-token@v3" || !maps.Equal(step.With, want) || step.If != "" || step.ContinueOnError {
				t.Error("credential step must request the repository-scoped App token unconditionally")
			}
		}
		if strings.Contains(step.Run, "cut-branch") {
			if create >= 0 {
				t.Fatal("more than one cut-branch step")
			}
			create = i
			want := map[string]string{
				"GH_TOKEN": "${{ steps.credential.outputs.token }}",
				"VERSION":  "${{ inputs.version }}",
				"REVISION": "${{ inputs.revision }}",
			}
			if !maps.Equal(step.Env, want) || strings.TrimSpace(step.Run) != `scripts/katl-release cut-branch "$VERSION" "$REVISION"` {
				t.Error("cut-branch must receive the App token and the two dispatch inputs through its environment")
			}
		}
	}
	if credential < 1 || create <= credential {
		t.Fatal("want the main guard before the credential step, followed by cut-branch")
	}
}

func TestCutReleaseEnvironment(t *testing.T) {
	// Another job using release-cut would also receive its environment secret.
	entries, err := os.ReadDir(filepath.Join(repoRoot(t), ".github", "workflows"))
	if err != nil {
		t.Fatal(err)
	}
	var jobs []string
	for _, entry := range entries {
		name := entry.Name()
		if ext := filepath.Ext(name); ext != ".yaml" && ext != ".yml" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(repoRoot(t), ".github", "workflows", name))
		if err != nil {
			t.Fatal(err)
		}
		var workflow struct {
			Jobs map[string]struct{ Environment yaml.Node }
		}
		if err := yaml.Unmarshal(data, &workflow); err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for id, job := range workflow.Jobs {
			environment := job.Environment.Value
			if job.Environment.Kind == yaml.MappingNode {
				var spec struct{ Name string }
				if err := job.Environment.Decode(&spec); err != nil {
					t.Fatal(err)
				}
				environment = spec.Name
			}
			if environment == "release-cut" {
				jobs = append(jobs, name+"/"+id)
			}
		}
	}
	if !slices.Equal(jobs, []string{"cut-release.yml/cut"}) {
		t.Fatalf("jobs using release-cut = %q, want cut-release.yml/cut only", jobs)
	}
}

func TestCutReleaseRequiresMain(t *testing.T) {
	// A mistakenly dispatched feature-branch copy must stop before checkout.
	workflow, _ := readWorkflow(t, "cut-release.yml")
	steps := workflow.Jobs["cut"].Steps
	if len(steps) == 0 || steps[0].Run == "" || steps[0].If != "" || steps[0].ContinueOnError {
		t.Fatal("the first step must always run a blocking guard")
	}
	for _, ref := range []string{"refs/heads/main", "refs/heads/rel/x"} {
		t.Run(ref, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), "bash", "-e", "-c", steps[0].Run)
			cmd.Env = []string{"GITHUB_REF=" + ref}
			out, err := cmd.CombinedOutput()
			if ref == "refs/heads/main" {
				if err != nil {
					t.Fatalf("guard rejected main: %v: %s", err, out)
				}
				return
			}
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.Contains(string(out), "dispatch it on main") {
				t.Fatalf("guard = %v, %q; want a refusal explaining that dispatch is on main", err, out)
			}
		})
	}
}
