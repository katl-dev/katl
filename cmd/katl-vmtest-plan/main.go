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

// group is a selectable set of tests. Lane names the CI job that runs it and
// Seconds is the measured wall time (median of the standard and LTS flavours
// over recent merge-group runs), which balances the lanes.
type group struct {
	Name    string `json:"name"`
	Package string `json:"package"`
	Tests   string `json:"tests"`
	Lane    string `json:"-"`
	Seconds int    `json:"-"`
}

type lane struct{ Name, Package string }

// lanes are the parallel VM jobs per kernel flavour. laneBudget bounds the
// summed Seconds of a lane's groups so no lane becomes the serial tail of a
// merge group.
var lanes = []lane{
	{"readdress", "./internal/vmtest/scenarios"},
	{"two-node", "./internal/vmtest/scenarios"},
	{"cluster", "./internal/vmtest/scenarios"},
	{"config", "./internal/vmtest"},
	{"lifecycle", "./internal/vmtest"},
}

const laneBudget = 900

var groups = []group{
	{"two-node", "./internal/vmtest/scenarios", "TestInstalledRuntimeTwoNodeOperationBackedBootstrapSmoke", "two-node", 660},
	{"worker-join", "./internal/vmtest/scenarios", "TestExternalWorkerJoin", "cluster", 215},
	{"readdress", "./internal/vmtest/scenarios", "TestWorkerReaddress", "readdress", 710},
	{"three-node-install", "./internal/vmtest/scenarios", "TestThreeNodeGeneration0InstallSmoke", "cluster", 270},
	{"install", "./internal/vmtest", "TestFirstInstallTargetDiskBootSmoke|TestFirstInstallTargetDiskLocalHandoffSmoke", "lifecycle", 75},
	{"config", "./internal/vmtest", "TestInstalledRuntimeRequiredLinkHealth|TestInstalledRuntimeConfigApplyModesSmoke|TestInstalledRuntimeServiceSecuritySmoke|TestInstalledRuntimeCarrierRecovery", "config", 570},
	{"update", "./internal/vmtest", "TestInstalledRuntimeSysupdateRootUKITransfer", "lifecycle", 330},
	{"management", "./internal/vmtest", "TestInstalledRuntimeManagementRotationSmoke|TestManagementIdentityRecovery", "lifecycle", 140},
	{"enable-mtls", "./internal/vmtest/scenarios", "TestManagementEnableMTLS", "cluster", 190},
	{"storage", "./internal/vmtest/scenarios", "TestInstallerISOFirstInstallStorageAuthority", "two-node", 32},
	{"volume-binding", "./internal/vmtest/scenarios", "TestVolumeBindingGate", "cluster", 150},
	{"boot", "./internal/vmtest/scenarios", "TestInstallerISOBootSmoke|TestInstallerPXEBootSmoke|TestInstallerESPSameDiskInstall|TestInstallerReinstallBesideOldKatlDisk", "two-node", 180},
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
	ci := flags.Bool("ci", false, "plan VM, driver, and production flavours from NUL-delimited changed paths")
	regression := flags.Bool("regression", false, "with --ci, use both regression flavours when VM suites are selected")
	production := flags.String("production", "off", "with --ci, select production flavours: off, non-prose, or always")
	vmModeFlag := flags.String("vm-mode", string(affectedVMs), "with --ci, select affected, full, merge-group, always, or off")
	base := flags.String("base", "", "tested base revision required for reduced VM coverage")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || (!*ci && (*regression || *production != "off")) || (*changed && *ci) {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	switch *production {
	case "off", "non-prose", "always":
	default:
		return fmt.Errorf("unknown production selection %q", *production)
	}
	mode := vmMode(*vmModeFlag)
	switch mode {
	case affectedVMs, fullVMs, mergeGroupVMs, alwaysVMs, noVMs:
	default:
		return fmt.Errorf("unknown VM selection mode %q", mode)
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
		decision, shadow := planVMs(paths, mode, func() (vmInputs, error) {
			return sharedVMInputs(*base)
		})
		if *ci {
			plan := planCI(paths, *regression || mode == alwaysVMs, decision, shadow)
			plan.ProductionFlavours = productionFlavours(paths, *production)
			plan.selectDrivers(paths, os.Getenv("GITHUB_EVENT_NAME"), os.Getenv("GITHUB_REF"), committedDriverDigests, mainDriverQualifications)
			return json.NewEncoder(output).Encode(plan)
		}
		selected = decision.groups
	}
	return json.NewEncoder(output).Encode(struct {
		Include []group `json:"include"`
	}{batchGroups(selected)})
}

// bothVMFlavours preserves the path policy for general VM coverage. Driver
// qualification is planned independently from its committed inputs.
func bothVMFlavours(name string) bool {
	for _, prefix := range []string{
		"kernels/", "extensions/", "internal/kernelrelease/", "internal/kernelmodule/",
		"internal/extensionrelease/", "internal/releaseextensions/", "cmd/katl-kernel-release/",
		"cmd/katl-mkosi-artifacts/", "cmd/katl-vmtest-plan/", "mkosi.conf", "mkosi.profiles/runtime/",
		"mkosi.profiles/installer-image/", "mkosi.profiles/kernel-extension-", "mkosi.profiles/vmtest-drbd-tools/",
		"packages/", "cmd/katl-runtime-rpms/", "internal/vmtest/", ".github/",
	} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	switch name {
	case "go.mod", "go.sum", "Containerfile.mkosi", "scripts/mkosi", "scripts/fedora-release",
		"scripts/build-kernel-extension", "scripts/build-release-extensions", "scripts/assemble-release-extensions":
		return true
	}
	return false
}

type ciPlan struct {
	VMFlavours         []string           `json:"vmFlavours"`
	DriverFlavours     []string           `json:"driverFlavours"`
	DriverDigests      map[string]string  `json:"driverDigests"`
	QualifiedDrivers   map[string]string  `json:"qualifiedDrivers"`
	DriverDecisions    []string           `json:"driverDecisions"`
	ProductionFlavours []string           `json:"productionFlavours"`
	Groups             []group            `json:"groups"`
	VMSelection        vmSelectionSummary `json:"vmSelection"`
	VMShadow           *vmShadow          `json:"vmShadow,omitempty"`
}

func productionFlavours(paths []string, mode string) []string {
	both := []string{"standard", "lts"}
	switch mode {
	case "always":
		return both
	case "non-prose":
		for _, name := range paths {
			// Production builds read no Markdown except the release staging
			// inputs docs/support.md and docs/releases/ (scripts/katl-release-artifacts).
			prose := name == "README.md" || name == "AGENTS.md" ||
				strings.HasPrefix(name, "docs/") && strings.HasSuffix(name, ".md") &&
					name != "docs/support.md" && !strings.HasPrefix(name, "docs/releases/")
			if !prose {
				return both
			}
		}
	}
	return []string{}
}

func planCI(paths []string, regression bool, selected vmSelection, shadow *vmSelection) ciPlan {
	vms := map[string]bool{}
	for _, name := range paths {
		if skipsLegacyQualification(name) {
			continue
		}
		if strings.HasPrefix(name, "kernels/") {
			if strings.HasSuffix(name, "/standard.json") {
				vms["standard"] = true
				continue
			}
			if strings.HasSuffix(name, "/lts.json") {
				vms["lts"] = true
				continue
			}
		}
		if bothVMFlavours(name) {
			vms["standard"], vms["lts"] = true, true
		} else {
			vms["standard"] = true
		}
	}
	if len(selected.groups) == 0 {
		clear(vms)
	} else if regression {
		vms["standard"], vms["lts"] = true, true
	} else if len(vms) == 0 {
		vms["standard"] = true
	}
	plan := ciPlan{VMFlavours: []string{}, Groups: batchGroups(selected.groups), VMSelection: selected.summary()}
	for _, flavour := range kernelFlavours {
		if vms[flavour] {
			plan.VMFlavours = append(plan.VMFlavours, flavour)
		}
	}
	if shadow != nil {
		plan.VMShadow = &vmShadow{
			vmSelectionSummary: shadow.summary(),
			RunnerMinutesSaved: float64((suiteSeconds(selected.groups)-suiteSeconds(shadow.groups))*len(plan.VMFlavours)) / 60,
		}
	}
	return plan
}

func batchGroups(selected []group) []group {
	result := []group{}
	for _, l := range lanes {
		var tests []string
		for _, scenario := range selected {
			if scenario.Lane == l.Name {
				tests = append(tests, scenario.Tests)
			}
		}
		if len(tests) != 0 {
			result = append(result, group{Name: l.Name, Package: l.Package, Tests: strings.Join(tests, "|")})
		}
	}
	return result
}

// Preserve the path exclusions that applied to flavour selection before VM
// suites gained their own selection policy.
func skipsLegacyQualification(name string) bool {
	switch {
	case strings.HasPrefix(name, "docs/"), name == "README.md", name == "AGENTS.md", name == ".gitignore",
		strings.HasPrefix(name, ".github/ISSUE_TEMPLATE/"), name == ".github/dependabot.yml",
		strings.HasPrefix(name, "cmd/katl-release/"), name == "scripts/katl-release",
		strings.HasSuffix(name, "_test.go") && !strings.HasPrefix(name, "internal/vmtest/"):
		return true
	case name == "cmd/katl-kernel-release/candidate.go", name == "cmd/katl-kernel-release/propose.go",
		name == "cmd/katl-kernel-release/publish.go", name == "cmd/katl-kernel-release/update-commit.txt",
		name == ".github/workflows/kernel-updates.yml", name == ".github/workflows/kernel-automerge.yml":
		return true
	}
	return false
}
