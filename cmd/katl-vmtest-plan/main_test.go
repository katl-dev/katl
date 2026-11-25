package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func allNames() []string {
	return []string{"boot", "install", "storage", "volume-binding", "config", "management", "enable-mtls", "update", "two-node", "worker-join", "readdress", "three-node-install"}
}

func TestPlan(t *testing.T) {
	for _, tc := range []struct {
		name  string
		args  []string
		input string
		want  []string
	}{
		{"full by default", nil, "", allLanes},
		{"empty diff", []string{"--changed"}, "", []string{}},
		{"docs with spaces", []string{"--changed"}, "docs/design notes.md\x00", []string{}},
		{"newline in unknown path", []string{"--changed"}, "source\nname\x00", allLanes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			if err := run(tc.args, strings.NewReader(tc.input), &output); err != nil {
				t.Fatal(err)
			}
			var plan struct{ Include []group }
			if err := json.Unmarshal(output.Bytes(), &plan); err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, selected := range plan.Include {
				names = append(names, selected.Name)
			}
			slices.Sort(names)
			slices.Sort(tc.want)
			if !slices.Equal(names, tc.want) || strings.Contains(output.String(), "null") {
				t.Fatalf("plan = %s, want groups %v", &output, tc.want)
			}
		})
	}
}

func TestInvalidChanges(t *testing.T) {
	for _, input := range []string{"docs/file.md", "/etc/file\x00", "../file\x00", "a/../file\x00", ".\x00", "\x00"} {
		if err := run([]string{"--changed"}, strings.NewReader(input), &bytes.Buffer{}); err == nil {
			t.Fatalf("accepted invalid changed paths %q", input)
		}
	}
}

func TestCoverage(t *testing.T) {
	t.Run("default", func(t *testing.T) { testCoverage(t, nil, "") })
	t.Run("regression", func(t *testing.T) {
		t.Setenv("GITHUB_EVENT_NAME", "push")
		t.Setenv("GITHUB_REF", "refs/heads/release/test")
		testCoverage(t, []string{"--ci", "--regression"}, "cmd/katlctl/management_rotation.go\x00")
	})
	t.Run("install artifacts", func(t *testing.T) {
		// Every planned lane uses the same scenario job and its shared install inputs.
		data, err := os.ReadFile("../../.github/workflows/hosted-vmtests.yml")
		if err != nil {
			t.Fatal(err)
		}
		var workflow struct {
			Jobs struct {
				Scenarios struct {
					Needs []string
					Env   map[string]string
				}
			}
		}
		if err := yaml.Unmarshal(data, &workflow); err != nil {
			t.Fatal(err)
		}
		scenarios := workflow.Jobs.Scenarios
		if set := scenarios.Env["ARTIFACT_SET"]; set != "install" && set != "default" {
			t.Errorf("scenario lanes use artifact set %q, which does not supply the install image", set)
		}
		if !slices.Contains(scenarios.Needs, "images") {
			t.Error("scenario lanes can start before their shared images are available")
		}
	})
}

func testCoverage(t *testing.T, args []string, input string) {
	t.Helper()
	var output bytes.Buffer
	if err := run(args, strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	var plan struct{ Include, Groups []group }
	if err := json.Unmarshal(output.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if len(args) != 0 {
		plan.Include = plan.Groups
	}
	want := map[string][]string{
		"./internal/vmtest/scenarios": {
			"TestInstalledRuntimeTwoNodeOperationBackedBootstrapSmoke",
			"TestExternalWorkerJoin",
			"TestWorkerReaddress",
			"TestThreeNodeGeneration0InstallSmoke",
			"TestInstallerISOFirstInstallStorageAuthority",
			"TestInstallerISOBootSmoke",
			"TestInstallerPXEBootSmoke",
			"TestInstallerESPSameDiskInstall",
			"TestInstallerReinstallBesideOldKatlDisk",
			"TestVolumeBindingGate",
			"TestManagementEnableMTLS",
		},
		"./internal/vmtest": {
			"TestFirstInstallTargetDiskBootSmoke",
			"TestFirstInstallTargetDiskLocalHandoffSmoke",
			"TestInstalledRuntimeConfigApplyModesSmoke",
			"TestInstalledRuntimeServiceSecuritySmoke",
			"TestInstalledRuntimeSysupdateRootUKITransfer",
			"TestInstalledRuntimeManagementRotationSmoke",
		},
	}
	if len(plan.Include) != len(lanes) {
		t.Fatalf("expected %d VM lanes, got %s", len(lanes), &output)
	}
	// A test dropped from every lane or run by two lanes must fail, so collect
	// across lanes without deduplicating.
	got := map[string][]string{}
	for _, lane := range plan.Include {
		got[lane.Package] = append(got[lane.Package], strings.Split(lane.Tests, "|")...)
	}
	for pkg, expected := range want {
		slices.Sort(got[pkg])
		slices.Sort(expected)
		if !slices.Equal(got[pkg], expected) {
			t.Errorf("%s tests = %v, want %v", pkg, got[pkg], expected)
		}
	}
	if len(got) != len(want) {
		t.Errorf("packages = %v, want %v", got, want)
	}
}

var allLanes = []string{"readdress", "two-node", "cluster", "config", "lifecycle"}

// TestLaneBalance keeps every lane within the measured time budget, so a
// test added to the wrong lane, or a lane that grows, cannot lengthen the
// merge group without failing here.
func TestLaneBalance(t *testing.T) {
	seconds := map[string]int{}
	for _, g := range groups {
		if !slices.ContainsFunc(lanes, func(l lane) bool { return l.Name == g.Lane && l.Package == g.Package }) {
			t.Errorf("group %s names lane %q, which does not run package %s", g.Name, g.Lane, g.Package)
		}
		seconds[g.Lane] += g.Seconds
	}
	for _, l := range lanes {
		if seconds[l.Name] == 0 {
			t.Errorf("lane %s has no groups", l.Name)
		}
		if seconds[l.Name] > laneBudget {
			t.Errorf("lane %s takes %ds, over the %ds budget; move a group to a shorter lane", l.Name, seconds[l.Name], laneBudget)
		}
	}
}

// TestBatchGroupsUsesLanes pins the assignment that matters most: the two
// longest tests never share a lane.
func TestBatchGroupsUsesLanes(t *testing.T) {
	for _, l := range batchGroups(groups) {
		if strings.Contains(l.Tests, "TestWorkerReaddress") && strings.Contains(l.Tests, "TestInstalledRuntimeTwoNodeOperationBackedBootstrapSmoke") {
			t.Errorf("lane %s runs the two longest scenarios serially", l.Name)
		}
	}
}

func TestCIScenarios(t *testing.T) {
	t.Setenv("GITHUB_EVENT_NAME", "push")
	t.Setenv("GITHUB_REF", "refs/heads/release/test")
	var output bytes.Buffer
	if err := run([]string{"--ci"}, strings.NewReader(configFixture+"\x00"), &output); err != nil {
		t.Fatal(err)
	}
	var plan ciPlan
	if err := json.Unmarshal(output.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	want := []group{{Name: "config", Package: "./internal/vmtest", Tests: "TestInstalledRuntimeConfigApplyModesSmoke|TestInstalledRuntimeServiceSecuritySmoke"}}
	if !slices.Equal(plan.Groups, want) {
		t.Fatalf("scenarios = %v, want %v", plan.Groups, want)
	}
}

func TestCIVMFlavours(t *testing.T) {
	both := []string{"standard", "lts"}
	for _, tc := range []struct {
		name, paths string
		regression  bool
		vms         []string
	}{
		{name: "empty"},
		{name: "documentation", paths: "docs/development/ci.md"},
		{name: "unit only", paths: "internal/installer/run_test.go", vms: []string{"standard"}},
		{name: "release tooling", paths: "cmd/katl-release/main.go", vms: []string{"standard"}},
		{name: "capture tooling", paths: "cmd/katl-kernel-release/candidate.go", vms: []string{"standard"}},
		{name: "proposal workflow", paths: ".github/workflows/kernel-updates.yml", vms: []string{"standard"}},
		{name: "kernel schedule and docs", paths: ".github/workflows/kernel-updates.yml\x00docs/developing.md", vms: []string{"standard"}},
		{name: "regression kernel schedule and docs", paths: ".github/workflows/kernel-updates.yml\x00docs/developing.md", regression: true, vms: both},
		{name: "kernel schedule and lock", paths: ".github/workflows/kernel-updates.yml\x00kernels/fedora-45/x86_64/standard.json", vms: []string{"standard"}},
		{name: "release workflow", paths: ".github/workflows/release-artifacts.yml", vms: both},
		{name: "production qualification workflow", paths: ".github/workflows/production-vmtests.yml", vms: both},
		{name: "production build workflow", paths: ".github/workflows/production-build.yml", vms: both},
		{name: "production build and release", paths: ".github/workflows/production-build.yml\x00.github/workflows/release-artifacts.yml", vms: both},
		{name: "production build and image builder", paths: ".github/workflows/production-build.yml\x00scripts/mkosi", vms: both},
		{name: "release orchestration and docs", paths: ".github/workflows/release-artifacts.yml\x00.github/workflows/production-vmtests.yml\x00docs/developing.md", vms: both},
		{name: "regression release orchestration", paths: ".github/workflows/release-artifacts.yml\x00.github/workflows/production-vmtests.yml", regression: true, vms: both},
		{name: "release and image builder", paths: ".github/workflows/release-artifacts.yml\x00scripts/mkosi", vms: both},
		{name: "release and standard lock", paths: ".github/workflows/release-artifacts.yml\x00kernels/fedora-44/x86_64/standard.json", vms: both},
		{name: "production and scenario", paths: ".github/workflows/production-vmtests.yml\x00internal/vmtest/release_lifecycle_test.go", vms: both},
		{name: "production and shared VM action", paths: ".github/workflows/production-vmtests.yml\x00.github/actions/hosted-vmtest/action.yml", vms: both},
		{name: "release and unknown workflow", paths: ".github/workflows/release-artifacts.yml\x00.github/workflows/new-release.yml", vms: both},
		{name: "renamed release workflow", paths: ".github/workflows/release-artifacts.yml\x00.github/workflows/release-artifacts-new.yml", vms: both},
		{name: "driver qualification workflow", paths: ".github/workflows/kernel-qualification.yml", vms: both},
		{name: "product", paths: "cmd/katlctl/main.go", vms: []string{"standard"}},
		{name: "regression product", paths: "cmd/katlctl/main.go", regression: true, vms: both},
		{name: "regression docs", paths: "README.md", regression: true},
		{name: "standard lock", paths: "kernels/fedora-44/x86_64/standard.json", vms: []string{"standard"}},
		{name: "LTS lock", paths: "kernels/fedora-44/x86_64/lts.json", vms: []string{"lts"}},
		{name: "custom LTS lock", paths: "kernels/custom/lts.json", vms: []string{"lts"}},
		{name: "custom standard lock", paths: "kernels/custom/standard.json", vms: []string{"standard"}},
		{name: "regression LTS lock", paths: "kernels/fedora-44/x86_64/lts.json", regression: true, vms: both},
		{name: "regression shared ABI", paths: "extensions/drbd9/build.sh", regression: true, vms: both},
		{name: "both locks", paths: "kernels/fedora-44/x86_64/standard.json\x00kernels/fedora-44/x86_64/lts.json", vms: both},
		{name: "DRBD recipe", paths: "extensions/drbd9/build.sh", vms: both},
		{name: "driver builder", paths: "scripts/build-kernel-extension", vms: both},
		{name: "DRBD tools", paths: "mkosi.profiles/vmtest-drbd-tools/mkosi.conf", vms: both},
		{name: "DRBD journey", paths: "internal/vmtest/drbd_smoke_test.go", vms: both},
		{name: "module source", paths: "internal/kernelmodule/compose.go", vms: both},
		{name: "runtime source", paths: "packages/containerd/source.json", vms: both},
		{name: "runtime spec", paths: "packages/crun/crun.spec", vms: both},
		{name: "runtime builder", paths: "cmd/katl-runtime-rpms/main.go", vms: both},
		{name: "runtime hook", paths: "mkosi.profiles/runtime/mkosi.postinst", vms: both},
		{name: "shared config", paths: "mkosi.conf.d/packages.conf", vms: both},
		{name: "Go dependency", paths: "go.sum", vms: both},
		{name: "builder", paths: "scripts/mkosi", vms: both},
		{name: "VM test", paths: "internal/vmtest/installed_smoke_test.go", vms: both},
		{name: "VM workflow", paths: ".github/workflows/hosted-vmtests.yml", vms: both},
		{name: "CI workflow", paths: ".github/workflows/ci.yml", vms: both},
		{name: "planner", paths: "cmd/katl-vmtest-plan/main.go", vms: both},
		{name: "directory boundary", paths: "kernels-not-related/file", vms: []string{"standard"}},
		{name: "renamed module", paths: "docs/moved.go\x00internal/kernelmodule/old.go", vms: both},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var paths []string
			if tc.paths != "" {
				paths = strings.Split(tc.paths, "\x00")
			}
			decision, shadow := planVMs(paths, affectedVMs, func() (map[string]bool, error) { return nil, nil })
			got := planCI(paths, tc.regression, decision, shadow)
			if !slices.Equal(got.VMFlavours, tc.vms) || got.VMFlavours == nil {
				t.Fatalf("VM flavours = %v, want %v", got.VMFlavours, tc.vms)
			}
			if (len(got.Groups) == 0) != (len(tc.vms) == 0) {
				t.Fatalf("CI plan scenarios disagree with selected VM flavours: %+v", got)
			}
		})
	}
}

func TestCIProductionPlan(t *testing.T) {
	both := []string{"standard", "lts"}
	for _, tc := range []struct {
		name, mode, paths string
		want              []string
	}{
		{"audited prose", "non-prose", "README.md\x00docs/development/ci.md\x00", []string{}},
		{"empty diff", "non-prose", "", []string{}},
		{"mixed diff", "non-prose", "docs/development/ci.md\x00cmd/katlctl/main.go\x00", both},
		{"release support", "non-prose", "docs/support.md\x00", both},
		{"release guidance", "non-prose", "docs/releases/next.md\x00", both},
		{"other docs Markdown", "non-prose", "docs/development/new-page.md\x00docs/operations/upgrade.md\x00", []string{}},
		{"non-Markdown under docs", "non-prose", "docs/development/diagram.svg\x00", both},
		{"unknown path", "non-prose", "LICENSE\x00", both},
		{"unknown file", "non-prose", "packaging-input\x00", both},
		{"release series prose", "always", "README.md\x00", both},
		{"production off", "off", "cmd/katlctl/main.go\x00", []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			if err := run([]string{"--ci", "--production=" + tc.mode}, strings.NewReader(tc.paths), &output); err != nil {
				t.Fatal(err)
			}
			var plan ciPlan
			if err := json.Unmarshal(output.Bytes(), &plan); err != nil {
				t.Fatal(err)
			}
			if plan.ProductionFlavours == nil || !slices.Equal(plan.ProductionFlavours, tc.want) {
				t.Fatalf("production flavours = %v, want %v: %s", plan.ProductionFlavours, tc.want, &output)
			}
		})
	}
}

// TestReleaseLifecycle needs an assembled production bundle, so the regression
// lanes cannot run it. Production VM runs it instead, and a test renamed away
// from that workflow would otherwise stop running without failing anything.
func TestProductionCoverage(t *testing.T) {
	const name = "TestReleaseLifecycle"

	for _, lane := range batchGroups(groups) {
		if slices.Contains(strings.Split(lane.Tests, "|"), name) {
			t.Errorf("regression lane %s runs %s, which skips without a production bundle", lane.Name, name)
		}
	}

	workflow, err := os.ReadFile("../../.github/workflows/production-vmtests.yml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(workflow), "-run '^"+name+"$'") {
		t.Errorf("production-vmtests.yml does not run %s", name)
	}

	files, err := filepath.Glob("../../internal/vmtest/*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		source, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(source), "\nfunc "+name+"(t *testing.T)") {
			return
		}
	}
	t.Errorf("internal/vmtest defines no %s", name)
}
