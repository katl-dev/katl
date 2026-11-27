package scriptstest

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// adHocBuilders build Containerfile.mkosi themselves instead of preparing the
// pinned builder. Moving one changes the Kubernetes bundle recipe, so it needs
// its own rebuild of the published bundles.
var adHocBuilders = map[string]bool{
	"kubernetes-bundles.yml build": true,
}

var (
	builderImageReference = regexp.MustCompile(`\$\{?KATL_MKOSI_IMAGE\}?`)
	builderBuild          = regexp.MustCompile(`\b(docker|podman) (buildx )?build\b`)
	mkosiInvocation       = regexp.MustCompile(`(^|[\s;&|(])scripts/mkosi(\s|$)`)
)

// Every job that runs the builder by its local name first prepares it through
// the owner, either with the mkosi-builder action or through scripts/mkosi.
// Wrong behaviour this catches: a job that runs or builds the image by name
// and so uses whatever builder it happened to produce, unpinned and
// unrecorded.
func TestImageJobsPreparePinnedBuilder(t *testing.T) {
	root := repoRoot(t)
	entries, err := os.ReadDir(filepath.Join(root, ".github", "workflows"))
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".yml" {
			continue
		}
		var workflow struct {
			Env  map[string]string
			Jobs map[string]struct {
				Env   map[string]string
				Steps []workflowStep
			}
		}
		if err := yaml.Unmarshal(mustReadFile(t, filepath.Join(root, ".github", "workflows", entry.Name())), &workflow); err != nil {
			t.Fatalf("parse %s: %v", entry.Name(), err)
		}
		for name, job := range workflow.Jobs {
			if job.Env["KATL_MKOSI_IMAGE"] == "" && workflow.Env["KATL_MKOSI_IMAGE"] == "" {
				continue
			}
			id := entry.Name() + " " + name
			prepared := false
			for _, step := range job.Steps {
				if path.Base(strings.Split(step.Uses, "@")[0]) == "mkosi-builder" {
					prepared = true
					checked++
					continue
				}
				for _, line := range strings.Split(step.Run, "\n") {
					if strings.Contains(line, "Containerfile.mkosi") && builderBuild.MatchString(line) {
						if !adHocBuilders[id] {
							t.Errorf("%s builds Containerfile.mkosi itself; use .github/actions/mkosi-builder", id)
						}
						prepared = true
					}
					if mkosiInvocation.MatchString(line) {
						prepared = true
					}
					if builderImageReference.MatchString(line) && !prepared {
						t.Errorf("%s step %q runs the builder before preparing it", id, step.Name)
					}
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no workflow job prepares the builder through the mkosi-builder action")
	}
}

// The builder layers are no longer kept in the Actions cache. Wrong behaviour
// this catches: a buildkit gha cache export or import surviving in any
// workflow or action.
func TestNoBuilderLayerCache(t *testing.T) {
	root := os.DirFS(repoRoot(t))
	gha := regexp.MustCompile(`type=gha|cache-(from|to):`)
	err := fs.WalkDir(root, ".github", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		if ext := path.Ext(name); ext != ".yml" && ext != ".yaml" {
			return nil
		}
		data, err := fs.ReadFile(root, name)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(data), "\n") {
			if gha.MatchString(line) {
				t.Errorf("%s:%d keeps a Docker layer cache: %s", name, i+1, strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	action := string(mustReadFile(t, filepath.Join(repoRoot(t), ".github", "actions", "mkosi-builder", "action.yml")))
	if !strings.Contains(action, "scripts/mkosi-builder\" prepare") {
		t.Fatal("the mkosi-builder action no longer prepares the builder through scripts/mkosi-builder")
	}
}

var (
	builderPublish = regexp.MustCompile(`mkosi-builder["']?\s+publish\b`)
	imagePush      = regexp.MustCompile(`\b(docker|podman) push\b|--push\b|imagetools create\b|\boras push\b|\bskopeo copy\b|\bcrane (push|copy|tag)\b|push: true`)
)

// logicalLines returns a script's or workflow's lines with comments dropped
// and backslash continuations joined, each with the number of its first line.
func logicalLines(data string) (lines []string, numbers []int) {
	var pending strings.Builder
	start := 0
	for i, line := range strings.Split(data, "\n") {
		if pending.Len() == 0 {
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			start = i + 1
		}
		if continued, ok := strings.CutSuffix(strings.TrimRight(line, " \t"), `\`); ok {
			pending.WriteString(continued + " ")
			continue
		}
		pending.WriteString(line)
		lines, numbers = append(lines, pending.String()), append(numbers, start)
		pending.Reset()
	}
	return lines, numbers
}

func TestLogicalLines(t *testing.T) {
	lines, numbers := logicalLines("a \\\n  b\n# scripts/mkosi-builder publish push\n  # indented comment\nc\n")
	if strings.Join(lines, "|") != "a    b|c|" || fmt.Sprint(numbers) != "[1 5 6]" {
		t.Fatalf("lines = %q at %v", lines, numbers)
	}
}

// grantsPackageWrite reports whether a permissions block lets a job write
// packages.
func grantsPackageWrite(t *testing.T, permissions yaml.Node) bool {
	t.Helper()
	if permissions.Kind == yaml.ScalarNode {
		return permissions.Value == "write-all"
	}
	var scopes map[string]string
	if err := permissions.Decode(&scopes); err != nil {
		t.Fatal(err)
	}
	return scopes["packages"] == "write"
}

// Builder Image is the only workflow that publishes this package. It runs only
// for main, only its publish job can write packages, and that job publishes
// only through the owner. Reading or pulling the package is unrestricted.
// Wrong behaviour this catches: a pull request or merge group reaching
// packages: write, a second Builder Image job that can write packages, a
// direct push beside the owner, and any other workflow, action or script that
// publishes the builder, including through a quoted, continued or
// variable-separated owner invocation.
func TestBuilderPublication(t *testing.T) {
	workflow, _ := readWorkflow(t, "mkosi-builder.yml")
	var on map[string]yaml.Node
	if err := workflow.On.Decode(&on); err != nil {
		t.Fatal(err)
	}
	if _, ok := on["pull_request"]; ok {
		t.Fatal("Builder Image must not run for pull requests")
	}
	if _, ok := on["merge_group"]; ok {
		t.Fatal("Builder Image must not run for merge groups")
	}
	if workflow.Permissions["packages"] != "" {
		t.Fatal("only the publish job may write packages")
	}
	for name, job := range workflow.Jobs {
		writes := grantsPackageWrite(t, job.Permissions)
		if name == "publish" {
			if !writes || job.If != mainRef {
				t.Fatalf("publish job writes packages %t under condition %q, want true under %q", writes, job.If, mainRef)
			}
		} else if writes {
			t.Errorf("Builder Image job %s grants packages: write; only publish may", name)
		}
	}

	output, err := exec.Command(filepath.Join(repoRoot(t), "scripts", "mkosi-builder"), "repository").Output()
	if err != nil {
		t.Fatal(err)
	}
	repository := strings.TrimSpace(string(output))
	const builderWorkflow, owner = ".github/workflows/mkosi-builder.yml", "scripts/mkosi-builder"
	publishes := 0
	scanned := map[string]bool{}
	for _, dir := range []string{".github", "scripts"} {
		err := fs.WalkDir(os.DirFS(repoRoot(t)), dir, func(name string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() || name == owner || path.Ext(name) == ".md" {
				return err
			}
			if strings.HasPrefix(name, ".github/") && path.Ext(name) != ".yml" && path.Ext(name) != ".yaml" && !strings.HasPrefix(name, ".github/scripts/") {
				return nil
			}
			scanned[name] = true
			lines, numbers := logicalLines(string(mustReadFile(t, filepath.Join(repoRoot(t), name))))
			for i, line := range lines {
				where := fmt.Sprintf("%s:%d", name, numbers[i])
				if builderPublish.MatchString(line) {
					if name != builderWorkflow {
						t.Errorf("%s publishes the builder outside Builder Image", where)
					}
					publishes++
				}
				if !imagePush.MatchString(line) {
					continue
				}
				if name == builderWorkflow {
					t.Errorf("%s pushes directly; Builder Image publishes only through scripts/mkosi-builder publish", where)
				} else if strings.Contains(line, "mkosi-builder") || strings.Contains(line, repository) {
					t.Errorf("%s pushes a builder image", where)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if !scanned[".github/scripts/hosted-download"] || !scanned["scripts/mkosi"] {
		t.Fatal("the scan missed .github/scripts or scripts")
	}
	if publishes != 1 {
		t.Fatalf("Builder Image publishes %d times, want once", publishes)
	}
}
