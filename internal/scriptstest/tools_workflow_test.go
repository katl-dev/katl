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

const (
	setupTools = "./.github/actions/setup-tools"
	ciShell    = "nix develop --option builders '' .#ci -c "
)

// Jobs that run the Go unit gate take their Go and test tools from the flake's
// ci shell, so a test sees the versions it sees in the development shell.
func TestUnitGateJobsUseCIShell(t *testing.T) {
	jobs := []struct{ workflow, job string }{
		{"ci.yml", "checks"},
		{"release-artifacts.yml", "unit"},
	}
	for _, tc := range jobs {
		t.Run(tc.workflow+"/"+tc.job, func(t *testing.T) {
			job, ok := readWorkflowSteps(t, tc.workflow)[tc.job]
			if !ok {
				t.Fatalf("%s has no job %s", tc.workflow, tc.job)
			}

			setup := -1
			for i, step := range job.Steps {
				action, _, _ := strings.Cut(step.Uses, "@")
				switch {
				case action == setupTools:
					setup = i
				case action == "actions/setup-go" || strings.HasSuffix(action, "/.github/actions/setup-go"):
					t.Errorf("step %d uses %s; the ci shell provides Go", i, step.Uses)
				case strings.Contains(step.Run, "hosted-apt-install"):
					t.Errorf("step %d installs tools with apt; the ci shell provides them", i)
				}
			}
			if setup < 0 {
				t.Fatalf("no step uses %s", setupTools)
			}

			for i, step := range job.Steps {
				if step.Run == "" {
					continue
				}
				if i < setup {
					t.Errorf("step %d runs before %s", i, setupTools)
				}
				if !strings.HasPrefix(strings.TrimSpace(step.Run), ciShell) {
					t.Errorf("step %d runs %q outside the ci shell; prefix it with %q", i, step.Run, ciShell)
				}
			}
		})
	}
}

var nixCommand = regexp.MustCompile(`(^|[\s;&|(])nix\s+(build|develop|shell|run|copy|eval|flake|path-info|profile|store)\b`)

// Hosted runners have no remote builders, and a builders setting inherited
// from elsewhere would send work to a machine the job cannot reach.
func TestWorkflowNixCommandsDisableBuilders(t *testing.T) {
	root := os.DirFS(repoRoot(t))
	var commands int
	for _, dir := range []string{".github/workflows", ".github/actions"} {
		err := fs.WalkDir(root, dir, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if ext := filepath.Ext(path); entry.IsDir() || (ext != ".yml" && ext != ".yaml") {
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
			steps := file.Runs.Steps
			for _, job := range file.Jobs {
				steps = append(steps, job.Steps...)
			}
			for _, step := range steps {
				for _, line := range strings.Split(step.Run, "\n") {
					for _, match := range nixCommand.FindAllStringIndex(line, -1) {
						commands++
						if !strings.HasPrefix(strings.TrimLeft(line[match[1]:], " "), "--option builders ''") {
							t.Errorf("%s: %q must pass --option builders '' right after the subcommand", path, strings.TrimSpace(line))
						}
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if commands == 0 {
		t.Fatal("no nix commands found in workflows or actions")
	}
}
