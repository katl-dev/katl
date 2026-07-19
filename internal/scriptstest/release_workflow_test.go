package scriptstest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestReleaseWorkflowBuildsKatlOSImageDependencies(t *testing.T) {
	repo := repoRoot(t)
	contents, err := os.ReadFile(filepath.Join(repo, ".github", "workflows", "release-artifacts.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Name string `yaml:"name"`
				Run  string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(contents, &workflow); err != nil {
		t.Fatalf("parse release workflow: %v", err)
	}

	runtime, ok := workflow.Jobs["runtime"]
	if !ok {
		t.Fatal("release workflow has no runtime job")
	}
	dependencyStep := -1
	packageStep := -1
	for index, step := range runtime.Steps {
		if strings.Contains(step.Run, "scripts/build-endpoint-advertiser-sysext") {
			dependencyStep = index
		}
		if strings.Contains(step.Run, "scripts/build-katlos-install-image") {
			packageStep = index
		}
	}
	if dependencyStep < 0 {
		t.Fatal("release runtime job does not build the endpoint advertiser sysext")
	}
	if packageStep < 0 {
		t.Fatal("release runtime job does not package KatlOS images")
	}
	if dependencyStep >= packageStep {
		t.Fatal("release runtime job must build the endpoint advertiser sysext before packaging KatlOS images")
	}
}
