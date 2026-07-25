package scriptstest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestSystemExtensionWorkflowSeparatesValidationFromPublication(t *testing.T) {
	repo := repoRoot(t)
	contents, err := os.ReadFile(filepath.Join(repo, ".github", "workflows", "system-extensions.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Name string `yaml:"name"`
				ID   string `yaml:"id"`
				If   string `yaml:"if"`
				Run  string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(contents, &workflow); err != nil {
		t.Fatalf("parse system extension workflow: %v", err)
	}

	bird, ok := workflow.Jobs["bird"]
	if !ok {
		t.Fatal("system extension workflow has no bird job")
	}
	var decision, build string
	var loginIf, publishIf string
	for _, step := range bird.Steps {
		switch step.Name {
		case "Determine immutable publication intent":
			if step.ID != "publication" {
				t.Fatalf("publication decision id = %q, want publication", step.ID)
			}
			decision = step.Run
		case "Build and verify generic BIRD extension":
			build = step.Run
		case "Log in to GHCR":
			loginIf = step.If
		case "Publish through the common payload-bundle publisher":
			publishIf = step.If
		}
	}

	for _, contract := range []string{
		`git diff-tree --no-commit-id --name-only -r "$GITHUB_SHA"`,
		`extensions/bird/extension\.env`,
		`mkosi\.profiles/system-extension-bird/`,
		`scripts/mkosi`,
		`echo "publish=$publish" >> "$GITHUB_OUTPUT"`,
	} {
		if !strings.Contains(decision, contract) {
			t.Errorf("publication decision does not enforce %q", contract)
		}
	}
	if !strings.Contains(build, "--pack-only") {
		t.Fatal("system extension workflow must pack-validate on every triggered run")
	}
	const publishCondition = "${{ steps.publication.outputs.publish == 'true' }}"
	if loginIf != publishCondition || publishIf != publishCondition {
		t.Fatalf("publication conditions = login %q publish %q, want %q", loginIf, publishIf, publishCondition)
	}
}
