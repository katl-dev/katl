package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

type group struct {
	Name    string `json:"name"`
	Package string `json:"package"`
	Tests   string `json:"tests"`
}

var groups = []group{
	{"two-node", "./internal/vmtest/scenarios", "TestInstalledRuntimeTwoNodeOperationBackedBootstrapSmoke"},
	{"three-node-install", "./internal/vmtest/scenarios", "TestThreeNodeGeneration0InstallSmoke"},
	{"install", "./internal/vmtest", "TestFirstInstallTargetDiskBootSmoke|TestFirstInstallTargetDiskLocalHandoffSmoke"},
	{"config", "./internal/vmtest", "TestInstalledRuntimeConfigApplyModesSmoke"},
	{"update", "./internal/vmtest", "TestInstalledRuntimeSysupdateRootUKITransfer"},
	{"management", "./internal/vmtest", "TestInstalledRuntimeManagementRotationSmoke"},
	{"storage", "./internal/vmtest/scenarios", "TestInstallerISOFirstInstallStorageAuthority"},
	{"boot", "./internal/vmtest/scenarios", "TestInstallerISOBootSmoke|TestInstallerPXEBootSmoke"},
}

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, input io.Reader, output io.Writer) error {
	flags := flag.NewFlagSet("katl-vmtest-plan", flag.ContinueOnError)
	changed := flags.Bool("changed", false, "select groups from NUL-delimited changed paths on stdin; default: all groups")
	ci := flags.Bool("ci", false, "plan VM and driver flavours from NUL-delimited changed paths")
	main := flags.Bool("main", false, "run both regression flavours after merge; driver qualification belongs to PRs")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || (*main && !*ci) || (*changed && *ci) {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	selected := groups
	if *changed || *ci {
		data, err := io.ReadAll(input)
		if err != nil {
			return err
		}
		var paths []string
		if len(data) > 0 {
			if data[len(data)-1] != 0 {
				return fmt.Errorf("changed paths must be NUL-terminated; use git diff --name-only --no-renames -z")
			}
			paths = strings.Split(string(bytes.TrimSuffix(data, []byte{0})), "\x00")
		}
		for _, name := range paths {
			if name == "." || strings.HasPrefix(name, "/") || strings.HasPrefix(name, "../") || path.Clean(name) != name {
				return fmt.Errorf("changed path must be repository-relative: %q", name)
			}
		}
		if *ci {
			return json.NewEncoder(output).Encode(planCI(paths, *main))
		}
		selected = selectGroups(paths)
	}
	return json.NewEncoder(output).Encode(struct {
		Include []group `json:"include"`
	}{batchGroups(selected)})
}

func driverChange(name string) bool {
	for _, prefix := range []string{
		"kernels/", "extensions/", "internal/kernelrelease/", "internal/kernelmodule/",
		"internal/extensionrelease/", "internal/releaseextensions/", "cmd/katl-kernel-release/",
		"cmd/katl-mkosi-artifacts/", "cmd/katl-vmtest-plan/", "mkosi.conf", "mkosi.conf.d/", "mkosi.profiles/runtime/",
		"mkosi.profiles/installer-image/", "mkosi.profiles/kernel-extension-", ".github/actions/mkosi-builder/",
		"mkosi.profiles/vmtest-drbd-tools/", ".github/actions/hosted-vmtest/", ".github/scripts/hosted-vmtest-",
	} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	switch name {
	case "go.mod", "go.sum", "Containerfile.mkosi", "scripts/mkosi", "scripts/fedora-release",
		"scripts/build-kernel-extension", "scripts/build-release-extensions", "scripts/assemble-release-extensions",
		"internal/vmtest/drbd_smoke_test.go", ".github/workflows/kernel-qualification.yml",
		".github/workflows/ci.yml":
		return true
	}
	return false
}

type ciPlan struct {
	VMFlavours     []string `json:"vmFlavours"`
	DriverFlavours []string `json:"driverFlavours"`
}

func planCI(paths []string, main bool) ciPlan {
	vms, drivers := map[string]bool{}, map[string]bool{}
	for _, name := range paths {
		if len(selectGroups([]string{name})) == 0 {
			continue
		}
		if strings.HasPrefix(name, "kernels/") {
			if strings.HasSuffix(name, "/standard.json") {
				vms["standard"], drivers["standard"] = true, true
				continue
			}
			if strings.HasSuffix(name, "/lts.json") {
				vms["lts"], drivers["lts"] = true, true
				continue
			}
		}
		if driverChange(name) {
			vms["standard"], vms["lts"] = true, true
			drivers["standard"], drivers["lts"] = true, true
		} else if strings.HasPrefix(name, "packages/") || strings.HasPrefix(name, "cmd/katl-runtime-rpms/") ||
			strings.HasPrefix(name, "internal/vmtest/") || strings.HasPrefix(name, ".github/") {
			vms["standard"], vms["lts"] = true, true
		} else {
			vms["standard"] = true
		}
	}
	if main && len(vms) != 0 {
		vms["standard"], vms["lts"] = true, true
	}
	if main {
		// Strict branch protection already requires driver qualification before merge.
		drivers = map[string]bool{}
	}
	plan := ciPlan{VMFlavours: []string{}, DriverFlavours: []string{}}
	for _, flavour := range []string{"standard", "lts"} {
		if vms[flavour] {
			plan.VMFlavours = append(plan.VMFlavours, flavour)
		}
		if drivers[flavour] {
			plan.DriverFlavours = append(plan.DriverFlavours, flavour)
		}
	}
	return plan
}

func batchGroups(selected []group) []group {
	lanes := []group{
		{Name: "cluster", Package: "./internal/vmtest/scenarios"},
		{Name: "lifecycle", Package: "./internal/vmtest"},
	}
	result := []group{}
	for _, lane := range lanes {
		var tests []string
		for _, scenario := range selected {
			if scenario.Package == lane.Package {
				tests = append(tests, scenario.Tests)
			}
		}
		if len(tests) != 0 {
			lane.Tests = strings.Join(tests, "|")
			result = append(result, lane)
		}
	}
	return result
}

func selectGroups(paths []string) []group {
	selected := map[string]bool{}
	for _, name := range paths {
		switch {
		case strings.HasPrefix(name, "docs/"), name == "README.md", name == "AGENTS.md", name == ".gitignore",
			strings.HasPrefix(name, ".github/ISSUE_TEMPLATE/"), name == ".github/dependabot.yml",
			strings.HasPrefix(name, "cmd/katl-release/"), name == "scripts/katl-release",
			strings.HasSuffix(name, "_test.go") && !strings.HasPrefix(name, "internal/vmtest/"):
			continue
		case name == "cmd/katl-kernel-release/candidate.go", name == "cmd/katl-kernel-release/propose.go",
			name == "cmd/katl-kernel-release/publish.go", name == "cmd/katl-kernel-release/update-commit.txt",
			name == ".github/workflows/kernel-updates.yml", name == ".github/workflows/kernel-automerge.yml":
			continue
		case name == "cmd/katlctl/config_compatibility.go", name == "cmd/katlctl/config_inspect.go":
			selected["config"], selected["management"], selected["two-node"] = true, true, true
		case name == "cmd/katlctl/management_rotation.go":
			selected["management"] = true
		default:
			// Unknown and shared changes receive the full suite: missing coverage must not look green.
			return groups
		}
	}
	result := []group{}
	for _, candidate := range groups {
		if selected[candidate.Name] {
			result = append(result, candidate)
		}
	}
	return result
}
