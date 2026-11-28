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

type ciStep struct {
	path string
	workflowStep
}

// ciSteps returns every step of every workflow and composite action.
func ciSteps(t *testing.T) []ciStep {
	t.Helper()
	root := os.DirFS(repoRoot(t))
	var steps []ciStep
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
			fileSteps := file.Runs.Steps
			for _, job := range file.Jobs {
				fileSteps = append(fileSteps, job.Steps...)
			}
			for _, step := range fileSteps {
				steps = append(steps, ciStep{path, step})
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return steps
}

// Hosted runners have no remote builders, and a builders setting inherited
// from elsewhere would send work to a machine the job cannot reach.
func TestWorkflowNixCommandsDisableBuilders(t *testing.T) {
	var commands int
	for _, step := range ciSteps(t) {
		for _, line := range strings.Split(step.Run, "\n") {
			for _, match := range nixCommand.FindAllStringIndex(line, -1) {
				commands++
				if !strings.HasPrefix(strings.TrimLeft(line[match[1]:], " "), "--option builders ''") {
					t.Errorf("%s: %q must pass --option builders '' right after the subcommand", step.path, strings.TrimSpace(line))
				}
			}
		}
	}
	if commands == 0 {
		t.Fatal("no nix commands found in workflows or actions")
	}
}

// The install-nix action owns CI's Nix configuration, so a job that installs
// Nix another way would run without its substituter and network timeouts.
func TestWorkflowsInstallNixThroughOwner(t *testing.T) {
	const owner = ".github/actions/install-nix/action.yml"
	var installs int
	for _, step := range ciSteps(t) {
		action, _, _ := strings.Cut(step.Uses, "@")
		if action != "cachix/install-nix-action" {
			continue
		}
		installs++
		if step.path != owner {
			t.Errorf("%s uses %s; install Nix with ./.github/actions/install-nix", step.path, action)
		}
	}
	if installs != 1 {
		t.Errorf("found %d cachix/install-nix-action steps; want exactly one, in %s", installs, owner)
	}
}
