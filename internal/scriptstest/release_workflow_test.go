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

func TestReleaseBuildsBothKernelFlavours(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), ".github/workflows/release-artifacts.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Strategy struct {
				Matrix struct {
					Flavour []string `yaml:"flavour"`
				} `yaml:"matrix"`
			} `yaml:"strategy"`
			Env map[string]string `yaml:"env"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	for _, job := range []string{"runtime", "installer", "assemble"} {
		build := workflow.Jobs[job]
		if strings.Join(build.Strategy.Matrix.Flavour, ",") != "standard,lts" || build.Env["KATL_FLAVOUR"] != "${{ matrix.flavour }}" {
			t.Fatalf("%s does not build and propagate both kernel flavours", job)
		}
	}
}
