package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/gittest"
)

// Check the inventory against the steps that actually produce the shared
// inputs, including mkosi hook binaries that do not appear in the workflow.
func TestHostedInputInventory(t *testing.T) {
	inputs, err := sharedGoInputs("")
	if err != nil {
		t.Fatal(err)
	}
	goPackages := map[string]bool{}
	for input := range inputs {
		if strings.HasSuffix(input, ".go") {
			goPackages[filepath.Dir(input)] = true
		}
	}

	commandPath := regexp.MustCompile(`\./cmd/[a-z][a-z0-9-]*`)
	scriptPath := regexp.MustCompile(`(?:\.github/scripts/|scripts/)[a-z][a-z0-9-]*`)
	hostedSources := []string{
		".github/workflows/hosted-vmtests.yml",
		".github/actions/hosted-vmtest/action.yml",
		".github/actions/mkosi-builder/action.yml",
		"scripts/mkosi",
		"scripts/build-katlos-install-image",
		"scripts/build-kubernetes-sysext",
		"scripts/vmtest-run",
		"scripts/vmtest-clean",
		"mkosi.profiles/runtime/mkosi.build",
		"mkosi.profiles/installer-image/mkosi.build",
		"mkosi.profiles/endpoint-advertiser-sysext/mkosi.build",
		"internal/vmtest/katlctl.go",
	}
	for _, source := range hostedSources {
		if !sharedVMPath(source) {
			t.Errorf("shared producer or harness input %s is missing", source)
		}
		content, err := os.ReadFile(filepath.Join("../..", source))
		if err != nil {
			t.Fatal(err)
		}
		for _, target := range commandPath.FindAllString(string(content), -1) {
			if !goPackages[strings.TrimPrefix(target, "./")] {
				t.Errorf("%s invokes %s, whose source is missing from the inventory", source, target)
			}
		}
		for _, target := range scriptPath.FindAllString(string(content), -1) {
			if !sharedVMPath(target) {
				t.Errorf("%s refers to %s, which is missing from the shared paths", source, target)
			}
		}
	}
	// Cover transitive dependencies, the generator absent from the mkosi
	// cache-target list, embedded content, and a test-only Go package.
	for _, name := range []string{
		"internal/systemdisk/generator.go",
		"internal/installer/kubernetescompat/catalog.json",
		"internal/kubernetesrelease/supported-versions.json",
		"internal/vmtest/scenarios/worker_readdress_vmtest_test.go",
		"internal/vmtest/testdata/service-security/main.go",
	} {
		if !inputs[name] {
			t.Errorf("shared Go input %s is missing", name)
		}
	}
	for path, suites := range fixtureSuites {
		if inputs[path] {
			t.Errorf("mapped fixture %s is also compiled or embedded", path)
		}
		for _, name := range suites {
			if !slices.Contains(allNames(), name) {
				t.Errorf("%s names unknown suite %q", path, name)
			}
		}
	}
}

func TestEmbeddedInputsInBothTrees(t *testing.T) {
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	repo := t.TempDir()
	gitIn(t, repo, "init", "--quiet")
	put(t, repo, "go.mod", "module example.com/vm-inputs\n\ngo 1.26.0\n")
	put(t, repo, "main.go", `package main
import "embed"
//go:embed docs/*.md
var manuals embed.FS
func main() {}
`)
	put(t, repo, "main_test.go", `package main_test
import "embed"
//go:embed test-manual.md
var manual embed.FS
`)
	put(t, repo, "docs/keep.md", "also embedded\n")
	put(t, repo, "docs/old.md", "will be deleted\n")
	put(t, repo, "test-manual.md", "external-test embed\n")
	gitIn(t, repo, "add", ".")
	gitIn(t, repo, "commit", "--quiet", "-m", "base")
	base := strings.TrimSpace(gitIn(t, repo, "rev-parse", "HEAD"))

	gitIn(t, repo, "mv", "docs/old.md", "docs/new.md")
	gitIn(t, repo, "commit", "--quiet", "-m", "rename")
	inputs, err := goInputsForTrees(repo, base, []string{"."})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"docs/old.md", "docs/new.md", "test-manual.md"} {
		selection, _ := planVMs([]string{path}, affectedVMs, func() (map[string]bool, error) { return inputs, nil })
		if selection.reason != "shared Go build or test input" || len(selection.groups) != len(allNames()) {
			t.Errorf("%s selected %+v, want full plan for embedded input", path, selection.summary())
		}
	}
	if got := gitIn(t, repo, "status", "--short"); got != "" {
		t.Fatalf("inventory changed candidate checkout: %s", got)
	}
}

func gitIn(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	cmd.Env = gittest.Env(os.Environ(), repo)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func put(t *testing.T, repo, name, content string) {
	t.Helper()
	path := filepath.Join(repo, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
