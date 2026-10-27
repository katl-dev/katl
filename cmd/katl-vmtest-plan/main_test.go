package main

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestSelectGroups(t *testing.T) {
	for _, tc := range []struct {
		name  string
		paths []string
		want  []string
	}{
		{"docs", []string{"docs/development/releases.md", "README.md"}, []string{}},
		{"unit tests", []string{"internal/installer/operation/run_test.go"}, []string{}},
		{"CLI unit tests", []string{"cmd/katlctl/main_test.go"}, []string{}},
		{"maintainer tooling", []string{"cmd/katl-release/main.go", "scripts/katl-release"}, []string{}},
		{"CLI configuration", []string{"cmd/katlctl/config_compatibility.go"}, []string{"config", "management", "two-node"}},
		{"CLI rotation", []string{"cmd/katlctl/management_rotation.go"}, []string{"management"}},
		{"CLI rotation and docs", []string{"cmd/katlctl/management_rotation.go", "docs/operations/install.md"}, []string{"management"}},
		{"shared CLI", []string{"cmd/katlctl/main.go"}, allNames()},
		{"shared configuration input", []string{"cmd/katlctl/config_input.go"}, allNames()},
		{"CLI install", []string{"cmd/katlctl/install.go"}, allNames()},
		{"CLI upgrade", []string{"cmd/katlctl/upgrade_reboot.go"}, allNames()},
		{"internal CLI", []string{"internal/katlctl/workstation/config.go"}, allNames()},
		{"shared product", []string{"internal/installer/operation/run.go"}, allNames()},
		{"VM tests", []string{"internal/vmtest/installed_smoke_test.go"}, allNames()},
		{"builder", []string{"scripts/mkosi"}, allNames()},
		{"unknown with docs", []string{"docs/readme.md", "new-input"}, allNames()},
		{"CLI with shared change", []string{"cmd/katlctl/main.go", "go.mod"}, allNames()},
		{"rename into docs", []string{"internal/installer/old.go", "docs/moved.go"}, allNames()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, selected := range selectGroups(tc.paths) {
				got = append(got, selected.Name)
			}
			slices.Sort(got)
			slices.Sort(tc.want)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("groups = %v, want %v", got, tc.want)
			}
		})
	}
}

func allNames() []string {
	return []string{"boot", "install", "storage", "config", "management", "update", "two-node", "three-node-install", "shutdown"}
}

func TestPlan(t *testing.T) {
	for _, tc := range []struct {
		name  string
		args  []string
		input string
		want  []string
	}{
		{"full by default", nil, "", []string{"cluster", "lifecycle"}},
		{"empty diff", []string{"--changed"}, "", []string{}},
		{"docs with spaces", []string{"--changed"}, "docs/design notes.md\x00", []string{}},
		{"newline in unknown path", []string{"--changed"}, "source\nname\x00", []string{"cluster", "lifecycle"}},
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
	t.Run("main", func(t *testing.T) {
		testCoverage(t, []string{"--ci", "--main"}, "cmd/katlctl/management_rotation.go\x00")
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
			"TestThreeNodeGeneration0InstallSmoke",
			"TestInstallerISOFirstInstallStorageAuthority",
			"TestInstallerISOBootSmoke",
			"TestInstallerPXEBootSmoke",
		},
		"./internal/vmtest": {
			"TestFirstInstallTargetDiskBootSmoke",
			"TestFirstInstallTargetDiskLocalHandoffSmoke",
			"TestInstalledRuntimeConfigApplyModesSmoke",
			"TestInstalledRuntimeSysupdateRootUKITransfer",
			"TestInstalledRuntimeManagementRotationSmoke",
			"TestInstalledRuntimePowerButton",
		},
	}
	if len(plan.Include) != 2 {
		t.Fatalf("expected two VM lanes, got %s", &output)
	}
	for _, lane := range plan.Include {
		tests := strings.Split(lane.Tests, "|")
		slices.Sort(tests)
		expected, ok := want[lane.Package]
		slices.Sort(expected)
		if !ok || !slices.Equal(tests, expected) {
			t.Fatalf("%s tests = %v, want %v", lane.Package, tests, expected)
		}
		delete(want, lane.Package)
	}
	if len(want) != 0 {
		t.Fatalf("missing packages: %v", want)
	}
}

func TestCIScenarios(t *testing.T) {
	var output bytes.Buffer
	if err := run([]string{"--ci"}, strings.NewReader("cmd/katlctl/management_rotation.go\x00"), &output); err != nil {
		t.Fatal(err)
	}
	var plan ciPlan
	if err := json.Unmarshal(output.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	want := []group{{"lifecycle", "./internal/vmtest", "TestInstalledRuntimeManagementRotationSmoke"}}
	if !slices.Equal(plan.Groups, want) {
		t.Fatalf("scenarios = %v, want %v", plan.Groups, want)
	}
}

func TestCIPlan(t *testing.T) {
	both := []string{"standard", "lts"}
	for _, tc := range []struct {
		name, paths  string
		main         bool
		vms, drivers []string
	}{
		{name: "empty"},
		{name: "documentation", paths: "docs/development/ci.md"},
		{name: "unit only", paths: "internal/installer/run_test.go"},
		{name: "release tooling", paths: "cmd/katl-release/main.go"},
		{name: "capture tooling", paths: "cmd/katl-kernel-release/candidate.go"},
		{name: "proposal workflow", paths: ".github/workflows/kernel-updates.yml"},
		{name: "single proposal workflow", paths: ".github/workflows/kernel-update.yml"},
		{name: "product", paths: "cmd/katlctl/main.go", vms: []string{"standard"}},
		{name: "main product", paths: "cmd/katlctl/main.go", main: true, vms: both},
		{name: "main docs", paths: "README.md", main: true},
		{name: "standard lock", paths: "kernels/fedora-44/x86_64/standard.json", drivers: []string{"standard"}},
		{name: "LTS lock", paths: "kernels/fedora-44/x86_64/lts.json", drivers: []string{"lts"}},
		{name: "main LTS lock", paths: "kernels/fedora-44/x86_64/lts.json", main: true, vms: both},
		{name: "main shared ABI", paths: "extensions/drbd9/build.sh", main: true, vms: both},
		{name: "both locks", paths: "kernels/fedora-44/x86_64/standard.json\x00kernels/fedora-44/x86_64/lts.json", drivers: both},
		{name: "DRBD recipe", paths: "extensions/drbd9/build.sh", drivers: both},
		{name: "driver builder", paths: "scripts/build-kernel-extension", drivers: both},
		{name: "DRBD tools", paths: "mkosi.profiles/vmtest-drbd-tools/mkosi.conf", drivers: both},
		{name: "DRBD journey", paths: "internal/vmtest/drbd_smoke_test.go", drivers: both},
		{name: "power button qualification", paths: "internal/vmtest/kernel_shutdown_vmtest_test.go", drivers: both},
		{name: "module source", paths: "internal/kernelmodule/compose.go", drivers: both},
		{name: "runtime source", paths: "packages/containerd/source.json", vms: both},
		{name: "runtime spec", paths: "packages/crun/crun.spec", vms: both},
		{name: "runtime builder", paths: "cmd/katl-runtime-rpms/main.go", vms: both},
		{name: "runtime hook", paths: "mkosi.profiles/runtime/mkosi.postinst", drivers: both},
		{name: "shared config", paths: "mkosi.conf.d/packages.conf", drivers: both},
		{name: "Go dependency", paths: "go.sum", drivers: both},
		{name: "builder", paths: "scripts/mkosi", drivers: both},
		{name: "VM test", paths: "internal/vmtest/installed_smoke_test.go", vms: both},
		{name: "VM workflow", paths: ".github/workflows/hosted-vmtests.yml", vms: both},
		{name: "CI workflow", paths: ".github/workflows/ci.yml", drivers: both},
		{name: "planner", paths: "cmd/katl-vmtest-plan/main.go", drivers: both},
		{name: "directory boundary", paths: "kernels-not-related/file", vms: []string{"standard"}},
		{name: "renamed module", paths: "docs/moved.go\x00internal/kernelmodule/old.go", drivers: both},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := tc.paths
			if input != "" {
				input += "\x00"
			}
			args := []string{"--ci"}
			if tc.main {
				args = append(args, "--main")
			}
			var output bytes.Buffer
			if err := run(args, strings.NewReader(input), &output); err != nil {
				t.Fatal(err)
			}
			var got ciPlan
			if err := json.Unmarshal(output.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got.VMFlavours, tc.vms) || !slices.Equal(got.KernelFlavours, tc.drivers) || strings.Contains(output.String(), "null") {
				t.Fatalf("CI plan = %s, want VMs %v and drivers %v", &output, tc.vms, tc.drivers)
			}
		})
	}
}

func TestKernelQualification(t *testing.T) {
	var output bytes.Buffer
	input := "kernels/fedora-44/x86_64/lts.json\x00"
	if err := run([]string{"--ci"}, strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	var plan ciPlan
	if err := json.Unmarshal(output.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if len(plan.VMFlavours) != 0 || !slices.Equal(plan.KernelFlavours, []string{"lts"}) {
		t.Fatalf("qualification ownership = %s", output.Bytes())
	}
	var boot, shutdown bool
	for _, group := range plan.Groups {
		boot = boot || strings.Contains(group.Tests, "TestInstallerISOBootSmoke")
		shutdown = shutdown || strings.Contains(group.Tests, "TestInstalledRuntimePowerButton")
	}
	if !boot || !shutdown {
		t.Fatalf("kernel qualification omits boot or native shutdown: %s", output.Bytes())
	}
}
