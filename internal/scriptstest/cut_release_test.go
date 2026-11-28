package scriptstest

import (
	"encoding/json"
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

func TestReleaseBranchGuard(t *testing.T) {
	// Matching the source or any release/ substring blocks normal work.
	// Missing a destination after a colon leaves the usual push form open.
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skipf("release branch guard requires jq: %v", err)
	}
	script := filepath.Join(repoRoot(t), ".claude", "hooks", "release-branch-guard")
	const refusal = "Release branches are created by scripts/katl-release cut VERSION REVISION, not by a push."
	for _, tc := range []struct {
		command string
		block   bool
	}{
		{"git push origin HEAD:release/2026.12.1", true},
		{"git push -u ssh://git@github.com/katl-dev/katl +a:refs/heads/release/2026.12.1", true},
		{"git push origin release/2026.12.1", true},
		{"git push --repo=origin -- 'HEAD:refs/heads/release/2026.12.1'", true},
		{"git push --receive-pack receive-pack origin refs/heads/release/2026.12.1", true},
		{"jj git push --remote origin -b release/2026.12.1", true},
		{"jj git push --bookmark release/2026.12.1", true},
		{"jj git push --branch=release/2026.12.1", true},
		{"gh api --method POST repos/katl-dev/katl/git/refs -f ref=refs/heads/release/2026.12.1", true},
		{`gh api repos/katl-dev/katl/git/refs --raw-field='ref=refs/heads/release/2026.12.1'`, true},
		{"git status && git push origin HEAD:refs/heads/release/2026.12.1", true},
		{"git push origin rel/878-cut-release", false},
		{"git fetch origin release/2026.12.1", false},
		{"git log origin/release/2026.12.1", false},
		{"gh pr create --base release/2026.12.1", false},
		{"gh workflow run release.yml --ref release/2026.12.1", false},
		{"scripts/katl-release cut 2026.12.1 main", false},
		{"git push origin rel/release-notes", false},
		{"git push origin release/2026.12.1:rel/2026.12.1", false},
		{"git push -o release/2026.12.1 origin rel/2026.12.1", false},
		{"echo 'git push origin release/2026.12.1'", false},
		{"gh api repos/katl-dev/katl/git/refs -f ref=refs/tags/v2026.12.1", false},
		{"jj git push -b rel/878-cut-release", false},
	} {
		t.Run(tc.command, func(t *testing.T) {
			stdin, err := json.Marshal(map[string]any{"tool_input": map[string]string{"command": tc.command}})
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.CommandContext(t.Context(), script)
			cmd.Stdin = strings.NewReader(string(stdin))
			out, err := cmd.CombinedOutput()
			if tc.block {
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 2 || strings.TrimSpace(string(out)) != refusal {
					t.Fatalf("guard = %v, %q; want exit 2 and cut guidance", err, out)
				}
			} else if err != nil || len(out) != 0 {
				t.Fatalf("guard = %v, %q; want quiet success", err, out)
			}
		})
	}
	t.Run("malformed input", func(t *testing.T) {
		cmd := exec.CommandContext(t.Context(), script)
		cmd.Stdin = strings.NewReader("{")
		if out, err := cmd.CombinedOutput(); err != nil || len(out) != 0 {
			t.Fatalf("guard = %v, %q; want quiet success", err, out)
		}
	})
	t.Run("jq unavailable", func(t *testing.T) {
		cmd := exec.CommandContext(t.Context(), "bash", script)
		cmd.Env = []string{"PATH=" + t.TempDir()}
		cmd.Stdin = strings.NewReader(`{"tool_input":{"command":"git push origin release/2026.12.1"}}`)
		if out, err := cmd.CombinedOutput(); err != nil || len(out) != 0 {
			t.Fatalf("guard = %v, %q; want quiet success", err, out)
		}
	})
}

func TestReleaseBranchGuardRegistration(t *testing.T) {
	// A script outside the Bash hook or without its executable bit never runs.
	root := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(root, ".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var settings struct {
		Hooks map[string][]struct {
			Matcher string
			Hooks   []struct{ Type, Command string }
		}
		Permissions struct{ Allow, Deny []string }
		AutoMode    struct{ Allow []string }
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatal(err)
	}
	const script = ".claude/hooks/release-branch-guard"
	registered := false
	for _, binding := range settings.Hooks["PreToolUse"] {
		for _, hook := range binding.Hooks {
			if binding.Matcher == "Bash" && hook.Type == "command" && hook.Command == `"$CLAUDE_PROJECT_DIR"/`+script {
				registered = true
			}
		}
	}
	if !registered {
		t.Error("release branch guard is missing from PreToolUse / Bash")
	}
	info, err := os.Stat(filepath.Join(root, script))
	if err != nil || info.Mode().Perm()&0111 == 0 {
		t.Fatalf("release branch guard must be executable: %v", err)
	}
	for _, entry := range []string{
		"Bash(scripts/vmtest-clean:*)", "Bash(./scripts/vmtest-clean:*)",
		"Bash(scripts/worktree-clean:*)", "Bash(./scripts/worktree-clean:*)",
		"Bash(katl-dev-evidence release:*)", "Bash(katl-dev-evidence prune:*)",
		"Bash(gh pr comment * -R katl-dev/katl --body-file *)",
		"Bash(gh pr edit * -R katl-dev/katl --add-label 'journey: passed')",
		"Bash(gh pr edit * -R katl-dev/katl --add-label 'journey: gap')",
		"Bash(gh pr edit * -R katl-dev/katl --add-label 'journey: passed' --remove-label 'journey: gap')",
	} {
		if !slices.Contains(settings.Permissions.Allow, entry) {
			t.Errorf("existing allow entry %q is missing", entry)
		}
	}
	if !slices.Contains(settings.Permissions.Deny, "Bash(codex exec:*)") ||
		!slices.Contains(settings.AutoMode.Allow, "$defaults") ||
		!slices.ContainsFunc(settings.AutoMode.Allow, func(s string) bool { return strings.Contains(s, "Recording a Katl user-journey result") }) {
		t.Error("existing deny and auto-mode entries must remain")
	}
}
