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
	{"boot", "./internal/vmtest/scenarios", "TestInstallerISOBootSmoke|TestInstallerPXEBootSmoke"},
	{"install", "./internal/vmtest", "TestFirstInstallTargetDiskBootSmoke|TestFirstInstallTargetDiskLocalHandoffSmoke"},
	{"storage", "./internal/vmtest/scenarios", "TestInstallerISOFirstInstallStorageAuthority"},
	{"config", "./internal/vmtest", "TestInstalledRuntimeConfigApplyModesSmoke"},
	{"management", "./internal/vmtest", "TestInstalledRuntimeManagementRotationSmoke"},
	{"update", "./internal/vmtest", "TestInstalledRuntimeSysupdateRootUKITransfer"},
	{"two-node", "./internal/vmtest/scenarios", "TestInstalledRuntimeTwoNodeOperationBackedBootstrapSmoke"},
	{"three-node-install", "./internal/vmtest/scenarios", "TestThreeNodeGeneration0InstallSmoke"},
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
	kernel := flags.Bool("kernel-changes", false, "print whether NUL-delimited changed paths require kernel qualification")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	selected := groups
	if *changed || *kernel {
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
		if *kernel {
			return json.NewEncoder(output).Encode(kernelChanges(paths))
		}
		selected = selectGroups(paths)
	}
	return json.NewEncoder(output).Encode(struct {
		Include []group `json:"include"`
	}{selected})
}

func kernelChanges(paths []string) bool {
	for _, name := range paths {
		for _, prefix := range []string{
			"kernels/", "extensions/", "packages/", "cmd/katl-runtime-rpms/", "internal/kernelrelease/", "internal/kernelmodule/",
			"internal/extensionrelease/", "internal/releaseextensions/", "cmd/katl-kernel-release/",
			"cmd/katl-mkosi-artifacts/", "cmd/katl-vmtest-plan/", "mkosi.conf", "mkosi.profiles/runtime/",
			"mkosi.profiles/installer-image/", "mkosi.profiles/kernel-extension-", ".github/actions/mkosi-builder/",
		} {
			if strings.HasPrefix(name, prefix) {
				return true
			}
		}
		switch name {
		case "go.mod", "go.sum", "Containerfile.mkosi", "scripts/mkosi", "scripts/fedora-release",
			"scripts/build-release-extensions", "scripts/assemble-release-extensions", ".github/workflows/kernel-updates.yml":
			return true
		}
	}
	return false
}

func selectGroups(paths []string) []group {
	selected := map[string]bool{}
	for _, name := range paths {
		switch {
		case strings.HasPrefix(name, "docs/"), name == "README.md", name == "AGENTS.md", name == ".gitignore",
			strings.HasPrefix(name, ".github/ISSUE_TEMPLATE/"), name == ".github/dependabot.yml",
			name == ".github/workflows/fast-checks.yml", strings.HasPrefix(name, "cmd/katl-release/"), name == "scripts/katl-release",
			strings.HasSuffix(name, "_test.go") && !strings.HasPrefix(name, "internal/vmtest/"):
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
