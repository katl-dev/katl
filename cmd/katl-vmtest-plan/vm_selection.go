package main

import (
	"slices"
	"strings"
)

type vmMode string

const (
	affectedVMs   vmMode = "affected"
	fullVMs       vmMode = "full"
	mergeGroupVMs vmMode = "merge-group"
	alwaysVMs     vmMode = "always"
	noVMs         vmMode = "off"

	// Enable after collecting the shadow evidence described in docs/development/ci.md.
	selectAffectedMergeGroupVMs = false
)

// These fixtures are read at scenario runtime and are absent from the shared
// producer and the Go build and test inputs. Other fixtures default to full coverage.
var fixtureSuites = map[string][]string{
	// ConfigApplyModesSmoke submits this fixture and reads it to build unit
	// configuration. Its other reader is the fast ConfigApplyNetworkFixtures test.
	"internal/vmtest/testdata/config-apply/live-udev.yaml": {"config"},
	// TwoNodeOperationBackedBootstrapSmoke stages this manifest.
	// ExternalWorkerJoin and WorkerReaddress apply it directly. The local
	// wipe/reinstall scenarios also stage it through bootstrapFixtureInputsForRun.
	"internal/vmtest/scenarios/testdata/bootstrap/cross-node-workload.yaml": {"two-node", "worker-join", "readdress"},
}

type vmSelection struct {
	groups []group
	reason string
	paths  []string
}

type vmSelectionSummary struct {
	Suites []string `json:"suites"`
	Reason string   `json:"reason"`
	Paths  []string `json:"paths,omitempty"`
}

type vmShadow struct {
	vmSelectionSummary
	RunnerMinutesSaved float64 `json:"runnerMinutesSaved"`
}

func (s vmSelection) summary() vmSelectionSummary {
	suites := make([]string, 0, len(s.groups))
	for _, g := range s.groups {
		suites = append(suites, g.Name)
	}
	return vmSelectionSummary{Suites: suites, Reason: s.reason, Paths: s.paths}
}

func planVMs(paths []string, mode vmMode, loadInputs func() (vmInputs, error)) (vmSelection, *vmSelection) {
	switch mode {
	case alwaysVMs:
		return vmSelection{groups: groups, reason: "scheduled or manual run"}, nil
	case noVMs:
		return vmSelection{reason: "event did not request hosted VM coverage"}, nil
	}

	affected := affectedSelection(paths, loadInputs)
	if len(affected.groups) == 0 {
		return affected, nil
	}
	if mode == mergeGroupVMs && !selectAffectedMergeGroupVMs {
		full := affected
		if len(affected.groups) < len(groups) {
			full = vmSelection{groups: groups, reason: "merge-group selection is in shadow mode"}
		}
		return full, &affected
	}
	if mode == fullVMs && len(affected.groups) < len(groups) {
		return vmSelection{groups: groups, reason: "full coverage required for this pull request", paths: affected.paths}, nil
	}
	return affected, nil
}

func affectedSelection(paths []string, loadInputs func() (vmInputs, error)) vmSelection {
	// A path that already requires the full plan makes inventory discovery
	// unnecessary. For a possible reduction, check the input inventory before
	// honoring either the documentation skip or a fixture rule.
	for _, name := range paths {
		if sharedVMPath(name) {
			return vmSelection{groups, "shared VM input", []string{name}}
		}
	}
	for _, name := range paths {
		if _, mapped := fixtureSuites[name]; !mapped && !documentationPath(name) {
			return vmSelection{groups, "unmapped path", []string{name}}
		}
	}
	inputs, err := loadInputs()
	if err != nil {
		return vmSelection{groups: groups, reason: "could not inventory shared VM inputs: " + err.Error()}
	}
	if len(paths) == 0 {
		return vmSelection{reason: "empty diff"}
	}
	selected := map[string]bool{}
	var mappedPaths []string
	for _, name := range paths {
		if inputs.goFiles[name] {
			return vmSelection{groups, "shared Go build or test input", []string{name}}
		}
		if inputs.hasLinkTarget(name) {
			return vmSelection{groups, "VM input symlink target", []string{name}}
		}
		if suites, mapped := fixtureSuites[name]; mapped {
			mappedPaths = append(mappedPaths, name)
			for _, suite := range suites {
				selected[suite] = true
			}
		}
	}
	if len(mappedPaths) == 0 {
		return vmSelection{reason: "docs-only diff"}
	}
	var result []group
	for _, g := range groups {
		if selected[g.Name] {
			result = append(result, g)
		}
	}
	return vmSelection{result, "mapped fixture paths", mappedPaths}
}

// ciShellInputs define the flake's ci shell and how jobs enter it. The VM
// lanes and driver qualification take their tools from that shell, so a change
// to any of these files is an input to both.
var ciShellInputs = []string{
	".github/actions/setup-tools/action.yml",
	".github/scripts/ci-shell",
	"flake.nix",
	"flake.lock",
	"scripts/dev-shell-env",
}

func sharedVMPath(name string) bool {
	for _, prefix := range []string{
		"mkosi.", "mkosi/", "kernels/", "packages/",
		".github/actions/hosted-vmtest/", ".github/actions/image-caches/", ".github/actions/mkosi-builder/", ".github/scripts/hosted-vmtest-",
		"scripts/vmtest-", "cmd/katl-vmtest-plan/",
	} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	switch name {
	case "go.mod", "go.sum", "Containerfile.mkosi", "mkosi-builder.lock", "containers-policy.json",
		".github/scripts/hosted-apt-install", ".github/scripts/hosted-hermetic-setup",
		".github/workflows/ci.yml", ".github/workflows/hosted-vmtests.yml",
		".github/workflows/production-build.yml", ".github/workflows/production-vmtests.yml",
		".github/workflows/kernel-qualification.yml", ".github/workflows/release-artifacts.yml",
		"scripts/mkosi", "scripts/mkosi-builder", "scripts/retry", "scripts/fedora-release", "scripts/build-installer-iso", "scripts/build-katlos-install-image",
		"scripts/build-endpoint-advertiser-sysext", "scripts/bind-install-manifest-image", "scripts/build-kubernetes-sysext",
		"scripts/check-kubernetes-sysext", "scripts/kubernetes-repoquery",
		"scripts/hermetic", "scripts/job-environment":
		return true
	}
	if slices.Contains(ciShellInputs, name) {
		return true
	}
	_, dedicated := fixtureSuites[name]
	return strings.HasPrefix(name, "internal/vmtest/") && !dedicated
}

func documentationPath(name string) bool {
	for _, part := range strings.Split(name, "/") {
		if part == "testdata" || part == "fixtures" {
			return false
		}
	}
	return strings.HasPrefix(name, "docs/") || strings.HasSuffix(name, ".md")
}

func suiteSeconds(suites []group) int {
	seconds := 0
	for _, g := range suites {
		seconds += g.Seconds
	}
	return seconds
}
