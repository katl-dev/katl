package scriptstest

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const mainRef = "github.ref == 'refs/heads/main'"

var mainRefGuard = regexp.MustCompile(`^github\.ref\s*==\s*'refs/heads/main'(\s*&&|$)`)
var cacheExport = regexp.MustCompile(`^\$\{\{ github\.ref == 'refs/heads/main' && '[^']+' \|\| '' \}\}$`)

func TestActionsCachesSaveOnlyOnMain(t *testing.T) {
	checkSteps := func(path string, steps []workflowStep) {
		for _, step := range steps {
			action, _, _ := strings.Cut(strings.ToLower(step.Uses), "@")
			switch action {
			case "actions/cache":
				t.Errorf("%s: %s saves implicitly; use actions/cache/restore and guard actions/cache/save", path, step.Uses)
			case "actions/cache/save":
				condition := strings.TrimSpace(step.If)
				condition = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(condition, "${{"), "}}"))
				// Require the common guard as a leading conjunction. A disjunction
				// could otherwise let an event bypass the guard.
				if !mainRefGuard.MatchString(condition) || strings.Contains(condition, "||") {
					t.Errorf("%s: %s condition %q must begin with %q and use only conjunctions", path, step.Uses, step.If, mainRef)
				}
			case "actions/setup-go":
				if step.With["cache"] != "false" {
					t.Errorf("%s: %s must disable its implicit cache save", path, step.Uses)
				}
			case "docker/setup-buildx-action":
				if step.With["cache-binary"] != "${{ "+mainRef+" }}" && step.With["cache-binary"] != "false" {
					t.Errorf("%s: %s must guard or disable its Buildx binary cache", path, step.Uses)
				}
			case "docker/build-push-action":
				if cacheTo := step.With["cache-to"]; cacheTo != "" && !cacheExport.MatchString(cacheTo) {
					t.Errorf("%s: %s must guard its Actions cache export", path, step.Uses)
				}
			}
		}
	}

	root := os.DirFS(repoRoot(t))
	for _, dir := range []string{".github/workflows", ".github/actions"} {
		err := fs.WalkDir(root, dir, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			ext := filepath.Ext(path)
			if entry.IsDir() || (ext != ".yml" && ext != ".yaml") {
				return nil
			}
			data, err := fs.ReadFile(root, path)
			if err != nil {
				return err
			}
			var file struct {
				Jobs map[string]struct{ Steps []workflowStep } `yaml:"jobs"`
				Runs struct{ Steps []workflowStep }            `yaml:"runs"`
			}
			if err := yaml.Unmarshal(data, &file); err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			for job, config := range file.Jobs {
				checkSteps(path+" job "+job, config.Steps)
			}
			checkSteps(path, file.Runs.Steps)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
