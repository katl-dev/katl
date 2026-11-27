package main

import (
	"encoding/json"
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
	inputs, err := sharedVMInputs(testBase(t))
	if err != nil {
		t.Fatal(err)
	}
	goPackages := map[string]bool{}
	for input := range inputs.goFiles {
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
		".github/scripts/hosted-hermetic-setup",
		"scripts/mkosi",
		"scripts/build-katlos-install-image",
		"scripts/build-kubernetes-sysext",
		"scripts/check-kubernetes-sysext",
		"scripts/copy-sysext-root",
		"scripts/hermetic",
		"scripts/job-environment",
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
		if !inputs.goFiles[name] {
			t.Errorf("shared Go input %s is missing", name)
		}
	}
	for path, suites := range fixtureSuites {
		if inputs.goFiles[path] {
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
	inputs, err := vmInputsForTrees(repo, base, []string{"."})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"docs/old.md", "docs/new.md", "test-manual.md"} {
		selection, _ := planVMs([]string{path}, affectedVMs, func() (vmInputs, error) { return inputs, nil })
		if selection.reason != "shared Go build or test input" || len(selection.groups) != len(allNames()) {
			t.Errorf("%s selected %+v, want full plan for embedded input", path, selection.summary())
		}
	}
	if got := gitIn(t, repo, "status", "--short"); got != "" {
		t.Fatalf("inventory changed candidate checkout: %s", got)
	}
}

func TestSymlinkInputsInBothTrees(t *testing.T) {
	testBase(t)
	repo := t.TempDir()
	gitIn(t, repo, "init", "--quiet")
	put(t, repo, "go.mod", "module example.com/vm-inputs\n\ngo 1.26.0\n")
	put(t, repo, "main.go", "package main\nfunc main() {}\n")
	put(t, repo, "docs/config.yaml", "fixture content\n")
	put(t, repo, "docs/examples/build.md", "producer content\n")
	put(t, repo, "docs/unrelated.md", "documentation\n")
	link := func(name, target string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(repo, filepath.Dir(name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(repo, name)); err != nil {
			t.Fatal(err)
		}
	}
	link(configFixture, "../../../../docs/config.yaml")
	gitIn(t, repo, "add", ".")
	gitIn(t, repo, "commit", "--quiet", "-m", "fixture symlink")
	base := strings.TrimSpace(gitIn(t, repo, "rev-parse", "HEAD"))

	if err := os.Remove(filepath.Join(repo, configFixture)); err != nil {
		t.Fatal(err)
	}
	put(t, repo, configFixture, "standalone fixture\n")
	link("scripts/mkosi", "../docs/current")
	link("docs/current", "examples")
	gitIn(t, repo, "add", ".")
	gitIn(t, repo, "commit", "--quiet", "-m", "producer symlink")

	head := strings.TrimSpace(gitIn(t, repo, "rev-parse", "HEAD"))
	for _, tc := range []struct {
		name, base, reason string
		paths              []string
	}{
		{"base fixture target", base, "VM input symlink target", []string{configFixture, "docs/config.yaml"}},
		{"head producer target", base, "VM input symlink target", []string{"docs/examples/build.md"}},
		{"head producer intermediate link", base, "VM input symlink target", []string{"docs/current"}},
		{"unrelated docs", base, "docs-only diff", []string{"docs/unrelated.md"}},
		{"fixture target absent from both trees", head, "mapped fixture paths", []string{configFixture, "docs/config.yaml"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mode := mergeGroupVMs
			if tc.reason == "mapped fixture paths" {
				mode = affectedVMs
			}
			selection, _ := planVMs(tc.paths, mode, func() (vmInputs, error) {
				return vmInputsForTrees(repo, tc.base, []string{"."})
			})
			if selection.reason != tc.reason {
				t.Fatalf("selection = %+v, want %q", selection.summary(), tc.reason)
			}
			if tc.reason == "VM input symlink target" && len(selection.groups) != len(allNames()) {
				t.Fatalf("merge-group selection = %+v, want full coverage", selection.summary())
			}
		})
	}
}

func TestCLIEmbeddedDeletionWithoutBase(t *testing.T) {
	repo := t.TempDir()
	gitIn(t, repo, "init", "--quiet")
	put(t, repo, "go.mod", "module example.com/vm-inputs\n\ngo 1.26.0\n")
	for _, target := range hostedGoTargets {
		dir := strings.TrimPrefix(strings.TrimSuffix(target, "/..."), "./")
		put(t, repo, dir+"/stub.go", "package fixture\n")
	}
	put(t, repo, "internal/vmtest/stub.go", "package fixture\n")
	put(t, repo, "internal/vmtest/scenarios/stub.go", "package fixture\n")
	put(t, repo, "cmd/katlctl/embed.go", "package fixture\nimport \"embed\"\n//go:embed docs/*.md\nvar manuals embed.FS\n")
	put(t, repo, "cmd/katlctl/docs/keep.md", "keeps the embed wildcard valid\n")
	const deleted = "cmd/katlctl/docs/old.md"
	put(t, repo, deleted, "will be deleted\n")
	gitIn(t, repo, "add", ".")
	gitIn(t, repo, "commit", "--quiet", "-m", "embed two documents")
	base := strings.TrimSpace(gitIn(t, repo, "rev-parse", "HEAD"))
	gitIn(t, repo, "rm", "--quiet", deleted)
	gitIn(t, repo, "commit", "--quiet", "-m", "delete an embedded document")
	diff := gitIn(t, repo, "diff", "--name-only", "--no-renames", "-z", base, "HEAD")

	binary := filepath.Join(t.TempDir(), "katl-vmtest-plan")
	if out, err := exec.Command("go", "build", "-buildvcs=false", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, out)
	}
	for _, tc := range []struct {
		args   []string
		reason string
	}{
		{[]string{"--changed"}, ""},
		{[]string{"--ci"}, "--base is required"},
		{[]string{"--ci", "--base", base}, "shared Go build or test input"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			cmd := exec.Command(binary, tc.args...)
			cmd.Dir = repo
			cmd.Env = gittest.Env(os.Environ(), repo)
			cmd.Stdin = strings.NewReader(diff)
			var stderr strings.Builder
			cmd.Stderr = &stderr
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("run CLI: %v\n%s", err, stderr.String())
			}
			var plan struct {
				Include, Groups []group
				VMSelection     vmSelectionSummary
			}
			if err := json.Unmarshal(out, &plan); err != nil {
				t.Fatalf("decode CLI output: %v\n%s", err, out)
			}
			if tc.args[0] == "--changed" {
				assertMatrixTests(t, plan.Include, fullMatrixTests())
				return
			}
			assertMatrixTests(t, plan.Groups, fullMatrixTests())
			if !strings.Contains(plan.VMSelection.Reason, tc.reason) {
				t.Fatalf("selection = %+v, want reason containing %q", plan.VMSelection, tc.reason)
			}
		})
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
