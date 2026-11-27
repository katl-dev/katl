package scriptstest

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/gittest"
)

func TestSystemExtensionRevision(t *testing.T) {
	revision := birdRevisionStep(t)

	for _, tc := range []struct {
		name, path, contents string
		advance, wantBump    bool
	}{
		{name: "runtime wiring", path: "mkosi.profiles/runtime/mkosi.build", contents: "refactor\n"},
		{name: "documentation", path: "extensions/bird/README.md", contents: "updated\n"},
		{name: "build wiring", path: "recipe.json", contents: `{"Packages":["bird"],"Environment":{"KATL_EXTRA":"1"},"CacheDirectory":"/elsewhere"}`},
		{name: "recipe", path: "recipe.json", contents: "{\"Packages\":[\"bird\",\"dep\"]}\n", wantBump: true},
		{name: "toolchain", path: "builder", contents: "different layer\n", wantBump: true},
		{name: "RPM resolution", path: "packages.json", contents: `{"packages":[{"type":"rpm","name":"bird","version":"3.4.0-2","architecture":"x86_64"}]}`, wantBump: true},
		{name: "overlay bytes", path: "overlay", contents: "different unit\n", wantBump: true},
		{name: "publication metadata", path: "extensions/bird/extension.env", contents: birdDefinition("v1", "v3.4.1"), wantBump: true},
		{name: "advanced version", path: "overlay", contents: "different unit\n", advance: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := newBirdRepo(t)
			base := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))
			writeBirdFile(t, dir, tc.path, tc.contents, 0o644)
			if tc.advance {
				writeBirdFile(t, dir, "extensions/bird/extension.env", birdDefinition("v2", "v3.4.0"), 0o644)
			}
			runGit(t, dir, "add", ".")
			runGit(t, dir, "commit", "--quiet", "-m", "change extension")
			// The pull request's previous head shares its fingerprint, so only
			// a comparison against the base exposes the change.
			previousHead := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))
			runGit(t, dir, "commit", "--quiet", "--allow-empty", "-m", "push to the pull request")

			output, err := runBirdRevision(t, dir, revision, map[string]string{
				"${{ github.event.pull_request.base.sha }}": base,
				"${{ github.event.before }}":                previousHead,
			})
			if tc.wantBump {
				if err == nil || !strings.Contains(string(output), "Advance KATL_EXTENSION_ARTIFACT_VERSION") {
					t.Fatalf("expected revision error, got %v:\n%s", err, output)
				}
			} else if err != nil {
				t.Fatalf("revision check: %v\n%s", err, output)
			}
		})
	}
}

// Two pull requests can each advance the artifact version to the same value.
// Each passes against its own base, so the push of the second must compare
// against the first, which main already published under that version.
func TestSystemExtensionRevisionAfterMerge(t *testing.T) {
	revision := birdRevisionStep(t)

	for _, tc := range []struct {
		name, path, contents string
		wantBump             bool
	}{
		{name: "second overlay change", path: "overlay", contents: "second change\n", wantBump: true},
		{name: "second documentation change", path: "extensions/bird/README.md", contents: "second change\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := newBirdRepo(t)
			published := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))

			writeBirdFile(t, dir, "overlay", "first change\n", 0o644)
			writeBirdFile(t, dir, "extensions/bird/extension.env", birdDefinition("v2", "v3.4.0"), 0o644)
			runGit(t, dir, "commit", "--quiet", "-am", "first change")
			first := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))
			push := map[string]string{
				"${{ github.event.pull_request.base.sha }}": "",
				"${{ github.event.before }}":                published,
			}
			if output, err := runBirdRevision(t, dir, revision, push); err != nil {
				t.Fatalf("push of the first change: %v\n%s", err, output)
			}

			writeBirdFile(t, dir, tc.path, tc.contents, 0o644)
			runGit(t, dir, "add", ".")
			runGit(t, dir, "commit", "--quiet", "-m", "second change")
			push["${{ github.event.before }}"] = first
			output, err := runBirdRevision(t, dir, revision, push)
			if tc.wantBump {
				if err == nil || !strings.Contains(string(output), "Advance KATL_EXTENSION_ARTIFACT_VERSION") {
					t.Fatalf("expected revision error, got %v:\n%s", err, output)
				}
			} else if err != nil {
				t.Fatalf("push of the second change: %v\n%s", err, output)
			}
		})
	}
}

func birdRevisionStep(t *testing.T) workflowStep {
	t.Helper()
	workflow, _ := readWorkflow(t, "system-extensions.yml")
	for _, step := range workflow.Jobs["bird"].Steps {
		if step.ID == "revision" {
			return step
		}
	}
	t.Fatal("missing revision check")
	return workflowStep{}
}

// newBirdRepo commits a published BIRD extension whose producer, builder and
// publisher are stubs that report the fingerprinted inputs from plain files.
func newBirdRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet")
	writeBirdFile(t, dir, "extensions/bird/extension.env", birdDefinition("v1", "v3.4.0"), 0o644)
	writeBirdFile(t, dir, "scripts/ci-bird-extension", string(mustReadFile(t, filepath.Join(repoRoot(t), "scripts/ci-bird-extension"))), 0o755)
	writeBirdFile(t, dir, "recipe.json", `{"Packages":["bird"],"Environment":{},"CacheDirectory":"/cache"}`, 0o644)
	writeBirdFile(t, dir, "builder", "builder layer\n", 0o644)
	writeBirdFile(t, dir, "packages.json", `{"packages":[{"type":"rpm","name":"bird","version":"3.4.0-1","architecture":"x86_64"}]}`, 0o644)
	writeBirdFile(t, dir, "overlay", "unit\n", 0o644)
	writeBirdFile(t, dir, "scripts/mkosi", `#!/usr/bin/env bash
set -eu
mkdir -p _build/mkosi
`, 0o755)
	writeBirdFile(t, dir, "scripts/build-system-extension", `#!/usr/bin/env bash
set -eu
mkdir -p _build/mkosi
cp overlay _build/mkosi/katl-bird.raw
cp packages.json _build/mkosi/katl-bird.manifest
cp recipe.json _build/mkosi/katl-bird-recipe.json
`, 0o755)
	writeBirdFile(t, dir, "bin/docker", `#!/usr/bin/env bash
set -eu
if [[ "$1" == image ]]; then cat builder; fi
`, 0o755)
	writeBirdFile(t, dir, "bin/go", `#!/usr/bin/env bash
set -eu
printf 'packed: '
{ printf '%s\n' "$@"; cat _build/mkosi/katl-bird.raw; } | sha256sum
`, 0o755)
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "--quiet", "-m", "initial extension")
	return dir
}

// runBirdRevision runs the revision step with its env expressions resolved
// from the given GitHub event context.
func runBirdRevision(t *testing.T, dir string, step workflowStep, event map[string]string) ([]byte, error) {
	t.Helper()
	cmd := exec.Command("bash", "-euo", "pipefail", "-c", step.Run)
	cmd.Dir = dir
	cmd.Env = append(gittest.Env(os.Environ(), dir),
		"PATH="+filepath.Join(dir, "bin")+":"+os.Getenv("PATH"),
		"RUNNER_TEMP="+t.TempDir(), "KATL_MKOSI_IMAGE=test-builder",
		"KATL_ARCHITECTURE=x86_64", "SOURCE_DATE_EPOCH=1780000000",
	)
	for name, expression := range step.Env {
		value, ok := event[expression]
		if !ok {
			t.Fatalf("revision step env %s=%s has no value in the simulated event", name, expression)
		}
		cmd.Env = append(cmd.Env, name+"="+value)
	}
	return cmd.CombinedOutput()
}

func TestSystemExtensionPublication(t *testing.T) {
	workflow, _ := readWorkflow(t, "system-extensions.yml")
	var publication string
	for _, step := range workflow.Jobs["bird"].Steps {
		if step.ID == "publication" {
			publication = step.Run
		}
	}
	if publication == "" {
		t.Fatal("missing publication decision")
	}

	for _, tc := range []struct {
		name, event, version, manual string
		wantPublish                  bool
	}{
		{name: "same revision on push", event: "push", version: "v1"},
		{name: "advanced revision on push", event: "push", version: "v2", wantPublish: true},
		{name: "advanced revision on pull request", event: "pull_request", version: "v2"},
		{name: "manual publish", event: "workflow_dispatch", version: "v1", manual: "true", wantPublish: true},
		{name: "manual dry run", event: "workflow_dispatch", version: "v1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			runGit(t, dir, "init", "--quiet")
			writeBirdFile(t, dir, "scripts/ci-bird-extension", string(mustReadFile(t, filepath.Join(repoRoot(t), "scripts/ci-bird-extension"))), 0o755)
			writeBirdFile(t, dir, "extensions/bird/extension.env", birdDefinition("v1", "v3.4.0"), 0o644)
			runGit(t, dir, "add", ".")
			runGit(t, dir, "commit", "--quiet", "-m", "published revision")
			before := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))
			writeBirdFile(t, dir, "extensions/bird/extension.env", birdDefinition(tc.version, "v3.4.0"), 0o644)
			runGit(t, dir, "commit", "--quiet", "--allow-empty", "-am", "build revision")
			writeBirdFile(t, dir, "scripts/mkosi", "unrelated wiring change\n", 0o644)
			runGit(t, dir, "add", ".")
			runGit(t, dir, "commit", "--quiet", "-m", "change shared build script")
			head := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))

			outputPath := filepath.Join(dir, "output")
			cmd := exec.Command("bash", "-euo", "pipefail", "-c", publication)
			cmd.Dir = dir
			cmd.Env = append(gittest.Env(os.Environ(), dir),
				"BEFORE_SHA="+before, "GITHUB_SHA="+head, "GITHUB_OUTPUT="+outputPath,
				"EVENT_NAME="+tc.event, "MANUAL_PUBLISH="+tc.manual,
			)
			if output, err := cmd.CombinedOutput(); err != nil {
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

func birdDefinition(artifact, payload string) string {
	return "KATL_EXTENSION_NAME=bird\n" +
		"KATL_EXTENSION_ARTIFACT_VERSION=" + artifact + "\n" +
		"KATL_EXTENSION_PAYLOAD_VERSION=" + payload + "\n" +
		"KATL_EXTENSION_RUNTIME_INTERFACE=katl-runtime-1\n"
}

func writeBirdFile(t *testing.T, dir, path, content string, mode os.FileMode) {
	t.Helper()
	file := filepath.Join(dir, path)
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(content), mode); err != nil {
		t.Fatal(err)
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
		if strings.Contains(step.Run, "scripts/mkosi") || strings.Contains(step.Run, "build-system-extension") ||
			strings.Contains(step.Run, "ci-bird-extension build") {
			t.Errorf("publish job rebuilds the bundle: %q", step.Run)
		}
	}
}

func TestSystemExtensionTriggers(t *testing.T) {
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

	for _, path := range []string{
		"Containerfile.mkosi",
		"extensions/bird/extension.env",
		"mkosi.conf",
		"mkosi.conf.d/example.conf",
		"mkosi.profiles/runtime/mkosi.build",
		"mkosi.profiles/system-extension-bird/mkosi.conf",
		"scripts/build-system-extension",
		"scripts/ci-bird-extension",
		"scripts/fedora-release",
		"scripts/mkosi",
	} {
		if !pathTriggers(on.Push.Paths, path) {
			t.Errorf("%s does not trigger the workflow", path)
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
