package scriptstest

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestSystemExtensionPublication(t *testing.T) {
	contents, err := os.ReadFile(filepath.Join(repoRoot(t), ".github/workflows/system-extensions.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Env   map[string]string                    `yaml:"env"`
			Steps []struct{ ID, If, Run, Uses string } `yaml:"steps"`
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
	var loginGuard, publishGuard string
	for _, step := range bird.Steps {
		switch step.ID {
		case "revision":
			revision = step.Run
		case "publication":
			publication = step.Run
		}
		if strings.HasPrefix(step.Uses, "docker/login-action@") {
			loginGuard = step.If
		}
		if strings.Contains(step.Run, "system-extension publish") && strings.Contains(step.Run, "--ref ") {
			publishGuard = step.If
		}
	}
	if revision == "" || publication == "" || loginGuard != "${{ steps.publication.outputs.publish == 'true' }}" || publishGuard != loginGuard {
		t.Fatal("revision, decision, or publication guards missing")
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
			runGit(t, dir, "config", "user.name", "Katl Test")
			runGit(t, dir, "config", "user.email", "test@katl.dev")
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
			env := append(os.Environ(),
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
