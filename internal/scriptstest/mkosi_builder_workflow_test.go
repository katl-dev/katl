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

// Builder Image is the only workflow that publishes this package. It runs only
// for main and never lets a pull request reach packages: write.
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
	publish := workflow.Jobs["publish"]
	if publish.If != mainRef {
		t.Fatalf("publish job condition = %q, want %q", publish.If, mainRef)
	}
	var permissions map[string]string
	if err := publish.Permissions.Decode(&permissions); err != nil || permissions["packages"] != "write" {
		t.Fatalf("publish permissions = %v (%v)", permissions, err)
	}
	if workflow.Permissions["packages"] != "" {
		t.Fatal("only the publish job may write packages")
	}

	// No other workflow or action can push the package. A push is any line
	// that publishes an image; it must not target the owner's repository, and
	// nothing else may ask the owner to publish or name its repository, which
	// is how a push through a variable would start.
	output, err := exec.Command(filepath.Join(repoRoot(t), "scripts", "mkosi-builder"), "repository").Output()
	if err != nil {
		t.Fatal(err)
	}
	repository := strings.TrimSpace(string(output))
	pushes := regexp.MustCompile(`\b(docker|podman) push\b|--push\b|imagetools create\b|\boras push\b|\bskopeo copy\b|\bcrane (push|copy|tag)\b|push: true`)
	publishes := 0
	err = fs.WalkDir(os.DirFS(repoRoot(t)), ".github", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || (path.Ext(name) != ".yml" && path.Ext(name) != ".yaml") {
			return err
		}
		for i, line := range strings.Split(string(mustReadFile(t, filepath.Join(repoRoot(t), name))), "\n") {
			where := fmt.Sprintf("%s:%d", name, i+1)
			if strings.Contains(line, "scripts/mkosi-builder publish") {
				if name != ".github/workflows/mkosi-builder.yml" {
					t.Errorf("%s publishes the builder outside Builder Image", where)
				}
				publishes++
			}
			if name == ".github/workflows/mkosi-builder.yml" {
				if pushes.MatchString(line) {
					t.Errorf("%s pushes directly; Builder Image publishes only through scripts/mkosi-builder publish", where)
				}
				continue
			}
			if strings.Contains(line, repository) || strings.Contains(line, "scripts/mkosi-builder repository") {
				t.Errorf("%s names the builder package; only Builder Image publishes it", where)
			}
			if pushes.MatchString(line) && strings.Contains(line, "mkosi-builder") {
				t.Errorf("%s pushes a builder image", where)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if publishes != 1 {
		t.Fatalf("Builder Image publishes %d times, want once", publishes)
	}
}
