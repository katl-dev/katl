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

	runtime, ok := workflow.Jobs["images"]
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
	for _, job := range []string{"runtime", "extensions", "images", "installer", "assemble"} {
		build := workflow.Jobs[job]
		if strings.Join(build.Strategy.Matrix.Flavour, ",") != "standard,lts" || build.Env["KATL_FLAVOUR"] != "${{ matrix.flavour }}" {
			t.Fatalf("%s does not build and propagate both kernel flavours", job)
		}
	}
}

func TestReleasePublishesKernelExtensions(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), ".github/workflows/release-artifacts.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			If              string    `yaml:"if"`
			Needs           yaml.Node `yaml:"needs"`
			ContinueOnError bool      `yaml:"continue-on-error"`
			Steps           []struct {
				Run string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}

	for _, edge := range [][2]string{
		{"publish-tag", "publish-extensions"},
		{"publish-extensions", "images"},
		{"images", "extensions"},
		{"extensions", "runtime"},
		{"extensions", "extension-inventory"},
	} {
		found := false
		needs := workflow.Jobs[edge[0]].Needs
		dependencies := needs.Content
		if needs.Kind == yaml.ScalarNode {
			dependencies = []*yaml.Node{&needs}
		}
		for _, dependency := range dependencies {
			found = found || dependency.Value == edge[1]
		}
		if !found {
			t.Fatalf("%s can publish before %s succeeds", edge[0], edge[1])
		}
	}
	publication := workflow.Jobs["publish-extensions"]
	if publication.If != "github.event_name == 'push' && github.ref_type == 'tag'" || publication.ContinueOnError {
		t.Fatal("extension publication must run automatically on tags and block release assets on failure")
	}
	found := false
	for _, job := range workflow.Jobs {
		for _, step := range job.Steps {
			if strings.Contains(step.Run, "scripts/vmtest-run") {
				t.Fatal("VM tests are not supported in CI")
			}
		}
	}
	for _, step := range publication.Steps {
		found = found || strings.Contains(step.Run, "publish-release-extensions")
	}
	if !found {
		t.Fatal("release does not publish its extension closures")
	}
}

func TestReleaseEmbedsKernelExtensions(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), ".github/workflows/release-artifacts.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Run             string            `yaml:"run"`
				ContinueOnError bool              `yaml:"continue-on-error"`
				Env             map[string]string `yaml:"env"`
				With            map[string]string `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	composition, packaging := -1, -1
	carried := false
	for i, step := range workflow.Jobs["images"].Steps {
		if strings.Contains(step.Run, "scripts/assemble-release-extensions") {
			composition = i
			if step.ContinueOnError {
				t.Fatal("composition failure must block packaging")
			}
		}
		if strings.Contains(step.Run, "scripts/build-katlos-install-image") {
			packaging = i
			if step.Env["KATL_EXTENSION_RELEASE"] != "_build/mkosi/release-extensions.json" {
				t.Fatal("packaging must consume the aggregate release mapping")
			}
		}
		paths := step.With["path"]
		carried = carried || (strings.Contains(paths, "_build/mkosi/extension-bundles/") && strings.Contains(paths, "_build/mkosi/release-extensions.json"))
	}
	if composition < 0 || packaging <= composition {
		t.Fatal("complete composition must be verified before packaging")
	}
	if !carried {
		t.Fatal("image pipeline loses release mapping or closure")
	}
}

func TestReleaseUsesRecipeMatrix(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), ".github/workflows/release-artifacts.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Strategy struct {
				Matrix struct {
					Extension string `yaml:"extension"`
				} `yaml:"matrix"`
			} `yaml:"strategy"`
			Steps []struct {
				Run string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	if workflow.Jobs["extensions"].Strategy.Matrix.Extension != "${{ fromJSON(needs.extension-inventory.outputs.extensions) }}" {
		t.Fatal("extension matrix must come from recipe inventory")
	}
	found := false
	for _, step := range workflow.Jobs["extensions"].Steps {
		found = found || step.Run == "scripts/build-release-extensions \"${{ matrix.extension }}\""
	}
	if !found {
		t.Fatal("CI must invoke the shared local build command")
	}
	for _, name := range []string{"drbd", "nvidia"} {
		if strings.Contains(strings.ToLower(string(data)), name) {
			t.Fatalf("workflow hardcodes extension %s", name)
		}
	}
}

func TestReleaseCarriesKernelInputs(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), ".github/workflows/release-artifacts.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Uses string            `yaml:"uses"`
				With map[string]string `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	carried := false
	for _, step := range workflow.Jobs["runtime"].Steps {
		if strings.HasPrefix(step.Uses, "actions/upload-artifact@") {
			carried = carried || strings.Contains(step.With["path"], "_build/mkosi/katl-kernel-inputs.tar*")
		}
	}
	if !carried {
		t.Fatal("runtime handoff omits prepared kernel inputs and their integrity metadata")
	}
	received := false
	for _, step := range workflow.Jobs["extensions"].Steps {
		if strings.HasPrefix(step.Uses, "actions/download-artifact@") {
			received = received || step.With["name"] == "katl-runtime-${{ matrix.flavour }}-${{ github.sha }}"
		}
	}
	if !received {
		t.Fatal("extension jobs must consume their runtime's build inputs")
	}
}
