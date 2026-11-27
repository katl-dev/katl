package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const (
	configFixture   = "internal/vmtest/testdata/config-apply/live-udev.yaml"
	workloadFixture = "internal/vmtest/scenarios/testdata/bootstrap/cross-node-workload.yaml"
)

func noSharedInputs() (vmInputs, error) { return vmInputs{}, nil }

func TestAffectedSelection(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		paths, want  []string
	}{
		{"docs", "docs-only diff", []string{"docs/development/ci.md", "README.md", "extensions/bird/README.md"}, nil},
		{"release notes", "docs-only diff", []string{"docs/releases/next.md"}, nil},
		{"VM harness", "shared VM input", []string{"internal/vmtest/installed_smoke_test.go"}, allNames()},
		{"mkosi profile", "shared VM input", []string{"mkosi.profiles/runtime/mkosi.build"}, allNames()},
		{"mkosi documentation input", "shared VM input", []string{"mkosi.profiles/runtime/mkosi.extra/etc/README.md"}, allNames()},
		{"packaging", "shared VM input", []string{".github/scripts/hosted-vmtest-package"}, allNames()},
		{"Go module", "shared VM input", []string{"go.mod"}, allNames()},
		{"builder pin", "shared VM input", []string{"mkosi-builder.lock"}, allNames()},
		{"builder owner", "shared VM input", []string{"scripts/mkosi-builder"}, allNames()},
		{"shared CLI binary", "unmapped path", []string{"cmd/katlctl/management_rotation.go"}, allNames()},
		{"unknown path with docs", "unmapped path", []string{"some-new-file", "docs/change.md"}, allNames()},
		{"unmapped fixture", "unmapped path", []string{"internal/other/testdata/change.md"}, allNames()},
		{"config fixture", "mapped fixture paths", []string{configFixture}, []string{"config"}},
		{"config with docs", "mapped fixture paths", []string{configFixture, "docs/change.md"}, []string{"config"}},
		{"cluster fixture", "mapped fixture paths", []string{workloadFixture}, []string{"readdress", "two-node", "worker-join"}},
		{"shared input overrides a rule", "shared VM input", []string{configFixture, "scripts/vmtest-run"}, allNames()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			selected, shadow := planVMs(tc.paths, affectedVMs, noSharedInputs)
			got := selected.summary()
			slices.Sort(got.Suites)
			slices.Sort(tc.want)
			if !slices.Equal(got.Suites, tc.want) || got.Reason != tc.reason || shadow != nil {
				t.Fatalf("selection = %+v, want %v (%s) with no shadow", got, tc.want, tc.reason)
			}
		})
	}
}

func TestUnavailableInventory(t *testing.T) {
	selection, _ := planVMs([]string{"docs/change.md"}, affectedVMs, func() (vmInputs, error) {
		return vmInputs{}, errors.New("go list failed")
	})
	if len(selection.groups) != len(allNames()) || !strings.Contains(selection.reason, "go list failed") {
		t.Fatalf("selection = %+v, want full plan with inventory error", selection.summary())
	}
}

func TestVMPlanModes(t *testing.T) {
	base := testBase(t)
	t.Setenv("GITHUB_EVENT_NAME", "push")
	t.Setenv("GITHUB_REF", "refs/heads/release/test")
	configTests := map[string][]string{
		"./internal/vmtest": {"TestInstalledRuntimeRequiredLinkHealth", "TestInstalledRuntimeConfigApplyModesSmoke", "TestInstalledRuntimeServiceSecuritySmoke", "TestInstalledRuntimeCarrierRecovery"},
	}
	clusterTests := map[string][]string{
		"./internal/vmtest/scenarios": {"TestInstalledRuntimeTwoNodeOperationBackedBootstrapSmoke", "TestExternalWorkerJoin", "TestWorkerReaddress"},
	}
	both := []string{"standard", "lts"}
	for _, tc := range []struct {
		name, mode, input string
		wantVMs           []string
		wantGroups        []string
		wantTests         map[string][]string
		shadow            bool
	}{
		{"labelled main PR", "affected", configFixture, both, []string{"config"}, configTests, false},
		{"labelled main PR cluster fixture", "affected", workloadFixture, both, []string{"two-node", "worker-join", "readdress"}, clusterTests, false},
		{"release PR", "full", configFixture, both, allNames(), fullMatrixTests(), false},
		{"merge group", "merge-group", configFixture, both, allNames(), fullMatrixTests(), true},
		{"merge group docs", "merge-group", "extensions/bird/README.md", nil, nil, nil, false},
		{"release PR docs", "full", "docs/releases/next.md", nil, nil, nil, false},
		{"scheduled or dispatched run", "always", "README.md", both, allNames(), fullMatrixTests(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"--ci", "--vm-mode", tc.mode, "--base", base}
			if tc.mode != "always" {
				args = append(args, "--regression")
			}
			var output bytes.Buffer
			if err := run(args, strings.NewReader(tc.input+"\x00"), &output); err != nil {
				t.Fatal(err)
			}
			var plan ciPlan
			if err := json.Unmarshal(output.Bytes(), &plan); err != nil {
				t.Fatal(err)
			}
			got := plan.VMSelection.Suites
			slices.Sort(got)
			slices.Sort(tc.wantGroups)
			if !slices.Equal(got, tc.wantGroups) || !slices.Equal(plan.VMFlavours, tc.wantVMs) || (plan.VMShadow != nil) != tc.shadow {
				t.Fatalf("plan = %s; want flavours %v, suites %v, shadow %v", &output, tc.wantVMs, tc.wantGroups, tc.shadow)
			}
			if plan.VMFlavours == nil || (len(plan.Groups) == 0) != (len(tc.wantVMs) == 0) {
				t.Fatalf("hosted outer and scenario matrices disagree: %s", &output)
			}
			assertMatrixTests(t, plan.Groups, tc.wantTests)
			if tc.shadow && !slices.Equal(plan.VMShadow.Suites, []string{"config"}) {
				t.Fatalf("shadow suites = %v, want config", plan.VMShadow.Suites)
			}
		})
	}
}

func TestMergeGroupShadow(t *testing.T) {
	repo := t.TempDir()
	gitIn(t, repo, "init", "--quiet", "--initial-branch=main")
	put(t, repo, configFixture, "initial config\n")
	put(t, repo, workloadFixture, "initial workload\n")
	gitIn(t, repo, "add", ".")
	gitIn(t, repo, "commit", "--quiet", "-m", "main")
	base := strings.TrimSpace(gitIn(t, repo, "rev-parse", "HEAD"))

	for _, pr := range []struct{ branch, file string }{
		{"config-pr", configFixture},
		{"workload-pr", workloadFixture},
	} {
		gitIn(t, repo, "switch", "--quiet", "-c", pr.branch, base)
		put(t, repo, pr.file, "changed\n")
		gitIn(t, repo, "commit", "--quiet", "-am", pr.branch)
	}
	gitIn(t, repo, "switch", "--quiet", "-c", "merge-group", base)
	gitIn(t, repo, "merge", "--quiet", "--no-ff", "config-pr", "-m", "queue config PR")
	gitIn(t, repo, "merge", "--quiet", "--no-ff", "workload-pr", "-m", "queue workload PR")

	diff := gitIn(t, repo, "diff", "--name-only", "--no-renames", "-z", base, "HEAD")
	paths := strings.Split(strings.TrimSuffix(diff, "\x00"), "\x00")
	actual, shadow := planVMs(paths, mergeGroupVMs, noSharedInputs)
	plan := planCI(paths, true, actual, shadow)
	want := []string{"two-node", "worker-join", "readdress", "config"}
	if plan.VMShadow == nil || !slices.Equal(plan.VMShadow.Suites, want) || len(plan.VMSelection.Suites) != len(allNames()) {
		t.Fatalf("plan = %+v, shadow = %+v, want all suites with shadow %v", plan.VMSelection, plan.VMShadow, want)
	}
	// Two flavours omit 1,367 seconds each across the remaining suites,
	// including rotation, recovery and mTLS enablement.
	if want := float64(2734) / 60; plan.VMShadow.RunnerMinutesSaved != want {
		t.Fatalf("savings = %g runner-minutes, want %g", plan.VMShadow.RunnerMinutesSaved, want)
	}
}

func TestCodeRenamedToDocs(t *testing.T) {
	repo := t.TempDir()
	gitIn(t, repo, "init", "--quiet")
	const source = "cmd/katlctl/management_rotation.go"
	put(t, repo, source, "package main\n")
	gitIn(t, repo, "add", ".")
	gitIn(t, repo, "commit", "--quiet", "-m", "code")
	if err := os.Mkdir(filepath.Join(repo, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "mv", source, "docs/moved.md")
	gitIn(t, repo, "commit", "--quiet", "-m", "move source to docs")

	diff := gitIn(t, repo, "diff", "--name-only", "--no-renames", "-z", "HEAD^", "HEAD")
	paths := strings.Split(strings.TrimSuffix(diff, "\x00"), "\x00")
	selection, _ := planVMs(paths, affectedVMs, noSharedInputs)
	if len(selection.groups) != len(allNames()) || !slices.Equal(selection.paths, []string{source}) {
		t.Fatalf("selection = %+v, want full plan because %s was deleted", selection.summary(), source)
	}
}
