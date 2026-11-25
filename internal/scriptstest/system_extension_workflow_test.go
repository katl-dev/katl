package scriptstest

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/katl-dev/katl/internal/gittest"
)

func TestSystemExtensionPublication(t *testing.T) {
	contents, err := os.ReadFile(filepath.Join(repoRoot(t), ".github/workflows/system-extensions.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Env   map[string]string                `yaml:"env"`
			Steps []struct{ ID, Run, Uses string } `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(contents, &workflow); err != nil {
		t.Fatal(err)
	}
	bird := workflow.Jobs["bird"]
	pattern, err := regexp.Compile(bird.Env["KATL_BIRD_RECIPE_PATTERN"])
	if err != nil {
		t.Fatal(err)
	}
	for path, needsRelease := range map[string]bool{
		"extensions/bird/extension.env":                   true,
		"extensions/bird/bird.conf":                       true,
		"mkosi.profiles/system-extension-bird/mkosi.conf": true,
		"mkosi.profiles/runtime/mkosi.conf":               true,
		"Containerfile.mkosi":                             true,
		"scripts/build-system-extension":                  true,
		".github/workflows/system-extensions.yml":         false,
		"cmd/katlctl/system_extension.go":                 false,
	} {
		if got := pattern.MatchString(path); got != needsRelease {
			t.Errorf("recipe classification of %s = %t, want %t", path, got, needsRelease)
		}
	}
	var revision, publication string
	for _, step := range bird.Steps {
		switch step.ID {
		case "revision":
			revision = step.Run
		case "publication":
			publication = step.Run
		}
	}
	if revision == "" || publication == "" {
		t.Fatal("revision or publication decision missing")
	}

	for _, tc := range []struct {
		name, changed, previous, current, event, manual string
		wantError, wantPublish                          bool
	}{
		{name: "unchanged recipe", changed: "extensions/bird/bird.conf", previous: "v1", current: "v1", event: "pull_request", wantError: true},
		{name: "advanced recipe", changed: "extensions/bird/bird.conf", previous: "v1", current: "v2", event: "pull_request"},
		{name: "unrelated change", changed: "docs/README.md", previous: "v1", current: "v1", event: "push"},
		{name: "test change", changed: "extensions/bird/bird_test.go", previous: "v1", current: "v1", event: "push"},
		{name: "push recipe", changed: "extensions/bird/bird.conf", previous: "v1", current: "v2", event: "push", wantPublish: true},
		{name: "manual publish", event: "workflow_dispatch", manual: "true", wantPublish: true},
		{name: "manual dry run", event: "workflow_dispatch", manual: "false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			runGit(t, dir, "init", "--quiet")
			if err := os.MkdirAll(filepath.Join(dir, "extensions/bird"), 0o755); err != nil {
				t.Fatal(err)
			}
			previous := tc.previous
			if previous == "" {
				previous = "v1"
			}
			versionPath := filepath.Join(dir, "extensions/bird/extension.env")
			if err := os.WriteFile(versionPath, []byte("KATL_EXTENSION_ARTIFACT_VERSION="+previous+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			runGit(t, dir, "add", ".")
			runGit(t, dir, "commit", "--quiet", "-m", "initial recipe")
			base := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))
			if tc.changed != "" {
				changedPath := filepath.Join(dir, tc.changed)
				if err := os.MkdirAll(filepath.Dir(changedPath), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(changedPath, []byte("changed\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				if tc.current != tc.previous {
					if err := os.WriteFile(versionPath, []byte("KATL_EXTENSION_ARTIFACT_VERSION="+tc.current+"\n"), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				runGit(t, dir, "add", ".")
				runGit(t, dir, "commit", "--quiet", "-m", "change recipe")
			}
			head := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))
			outputPath := filepath.Join(dir, "output")
			env := append(gittest.Env(os.Environ(), dir),
				"KATL_BIRD_RECIPE_PATTERN="+bird.Env["KATL_BIRD_RECIPE_PATTERN"],
				"BASE_SHA="+base, "GITHUB_SHA="+head, "GITHUB_OUTPUT="+outputPath,
				"EVENT_NAME="+tc.event, "MANUAL_PUBLISH="+tc.manual,
			)
			if tc.event == "pull_request" {
				cmd := exec.Command("bash", "-euo", "pipefail", "-c", revision)
				cmd.Dir, cmd.Env = dir, env
				output, err := cmd.CombinedOutput()
				if (err != nil) != tc.wantError {
					t.Fatalf("revision exit = %v, want error=%t:\n%s", err, tc.wantError, output)
				}
			}
			cmd := exec.Command("bash", "-euo", "pipefail", "-c", publication)
			cmd.Dir, cmd.Env = dir, env
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("publication decision: %v\n%s", err, output)
			}
			got := string(mustReadFile(t, outputPath))
			want := "publish=false\n"
			if tc.wantPublish {
				want = "publish=true\n"
			}
			if got != want {
				t.Fatalf("publication = %q, want %q", got, want)
			}
		})
	}
}

// The build job runs on pull requests, so credentials able to write packages
// belong only to a job that does nothing but publish the built bundle.
func TestSystemExtensionPublishJob(t *testing.T) {
	workflow, _ := readWorkflow(t, "system-extensions.yml")

	var writers []string
	for name, job := range workflow.Jobs {
		var permissions map[string]string
		if err := job.Permissions.Decode(&permissions); err != nil {
			t.Fatalf("%s permissions: %v", name, err)
		}
		if permissions["packages"] == "write" {
			writers = append(writers, name)
		}
		for _, step := range job.Steps {
			if strings.HasPrefix(step.Uses, "docker/login-action@") && permissions["packages"] != "write" {
				t.Errorf("%s logs in to a registry without packages: write", name)
			}
		}
	}
	if len(writers) != 1 {
		t.Fatalf("jobs with packages: write = %v, want exactly the publish job", writers)
	}

	publish := workflow.Jobs[writers[0]]
	if !publish.needs("bird") || strings.TrimSpace(publish.If) != "${{ needs.bird.outputs.publish == 'true' }}" {
		t.Errorf("publish job must wait for the build and its publication decision; needs=%v if=%q", publish.Needs.Content, publish.If)
	}
	if len(workflow.Permissions) != 1 || workflow.Permissions["contents"] != "read" {
		t.Errorf("workflow permissions = %v, want only contents: read", workflow.Permissions)
	}

	var uploaded, downloaded string
	for _, job := range workflow.Jobs {
		for _, step := range job.Steps {
			switch {
			case strings.HasPrefix(step.Uses, "actions/upload-artifact@"):
				uploaded = step.With["name"]
			case strings.HasPrefix(step.Uses, "actions/download-artifact@"):
				downloaded = step.With["name"]
			}
		}
	}
	if uploaded == "" || uploaded != downloaded {
		t.Errorf("publish job downloads artifact %q, build uploads %q", downloaded, uploaded)
	}
	for _, step := range publish.Steps {
		if strings.Contains(step.Run, "scripts/mkosi") || strings.Contains(step.Run, "build-system-extension") {
			t.Errorf("publish job rebuilds the bundle: %q", step.Run)
		}
	}
}

// Every recipe input that decides whether a revision must be published has to
// start the workflow, or the change is never built, checked or published.
func TestSystemExtensionTriggerCoversRecipe(t *testing.T) {
	workflow, _ := readWorkflow(t, "system-extensions.yml")

	var on struct {
		PullRequest struct{ Paths []string } `yaml:"pull_request"`
		Push        struct{ Paths []string } `yaml:"push"`
	}
	if err := workflow.On.Decode(&on); err != nil {
		t.Fatal(err)
	}
	if strings.Join(on.PullRequest.Paths, "\n") != strings.Join(on.Push.Paths, "\n") {
		t.Errorf("pull_request paths %v differ from push paths %v", on.PullRequest.Paths, on.Push.Paths)
	}

	pattern := workflow.Jobs["bird"].Env["KATL_BIRD_RECIPE_PATTERN"]
	body := strings.TrimSuffix(strings.TrimPrefix(pattern, "^("), ")")
	for _, alt := range strings.Split(body, "|") {
		sample := strings.ReplaceAll(strings.TrimSuffix(alt, "$"), `\.`, ".")
		if !strings.HasSuffix(alt, "$") {
			sample += "file"
		}
		if !pathTriggers(on.Push.Paths, sample) {
			t.Errorf("recipe input %s (from %q) does not trigger the workflow", sample, alt)
		}
	}
}

// pathTriggers reports whether a GitHub Actions paths filter selects path.
func pathTriggers(globs []string, path string) bool {
	for _, glob := range globs {
		expr := regexp.QuoteMeta(glob)
		expr = strings.ReplaceAll(expr, `\*\*`, ".*")
		expr = strings.ReplaceAll(expr, `\*`, "[^/]*")
		if regexp.MustCompile("^" + expr + "$").MatchString(path) {
			return true
		}
	}
	return false
}
