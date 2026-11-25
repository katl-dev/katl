package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"slices"
	"strings"

	"github.com/katl-dev/katl/internal/flavour"
	"gopkg.in/yaml.v3"
)

// Bump when changing the digest format or the interpretation of its inputs.
const driverDigestVersion = "v1"

var kernelFlavours = []string{flavour.Standard, flavour.LTS}

const qualificationWorkflow = ".github/workflows/kernel-qualification.yml"

// driverInputs lists the shared inputs to the qualification workflow. The
// active Fedora/x86_64 lock is added separately for each flavour. See
// docs/development/driver-qualification.md for the coverage boundary.
var driverInputs = []string{
	// Recipe discovery, compilation, and verification of the combined module tree.
	"extensions/*/recipe.json",
	"mkosi.profiles/kernel-extension-*/**",
	"scripts/build-release-extensions",
	"scripts/assemble-release-extensions",
	"cmd/katl-mkosi-artifacts/**",
	"cmd/katl-kernel-release/main.go",
	"internal/extensionrelease/**",
	"internal/kernelmodule/**",
	"internal/kernelrelease/**",
	"internal/releaseextensions/**",
	"internal/imagemount/**",
	"internal/flavour/**",

	// Builder, runtime and installer kernel preparation, and both image roles.
	"Containerfile.mkosi",
	"containers-policy.json",
	"scripts/fedora-release",
	"scripts/mkosi",
	"scripts/build-katlos-install-image",
	"scripts/build-endpoint-advertiser-sysext",
	".github/actions/mkosi-builder/**",
	"mkosi.conf",
	"mkosi.conf.d/**",
	"mkosi.profiles/installer-image/**",
	"mkosi.profiles/endpoint-advertiser-sysext/**",
	"cmd/katl-endpoint-advertiser/**",
	"mkosi.profiles/runtime/components.lock",
	"mkosi.profiles/runtime/mkosi.*",
	"mkosi.profiles/runtime/mkosi.conf.d/**",
	"mkosi.profiles/runtime/mkosi.repart/**",
	"mkosi.profiles/runtime/mkosi.sandbox/**",
	"mkosi.profiles/runtime/os-release.in",
	"mkosi.profiles/runtime/mkosi.extra/etc/os-release",
	"mkosi.profiles/runtime/mkosi.extra/usr/lib/os-release",
	"mkosi.profiles/runtime/mkosi.extra/lib/modules/**",
	"mkosi.profiles/runtime/mkosi.extra/usr/lib/modules/**",
	"mkosi.profiles/runtime/mkosi.extra/usr/lib/modules-load.d/**",
	"mkosi.profiles/runtime/mkosi.extra/usr/lib/depmod.d/**",
	"mkosi.profiles/runtime/mkosi.extra/usr/lib/modprobe.d/**",
	"mkosi.profiles/runtime/mkosi.extra/etc/depmod.d/**",
	"mkosi.profiles/runtime/mkosi.extra/etc/modprobe.d/**",
	"mkosi.profiles/runtime/mkosi.extra/usr/lib/extension-release.d/**",
	"packages/**",
	"cmd/katl-runtime-components/**",
	"cmd/katl-runtime-rpms/**",
	"go.mod",
	"go.sum",

	// Driver selection, staging, module indexing, upgrade, and sysext activation.
	"cmd/katlctl/**",
	"internal/katlctl/**",
	"cmd/katlc/**",
	"internal/katlc/agent/**",
	"internal/katlc/agentapi/**",
	"internal/katlc/config/**",
	"internal/generation/**",
	"cmd/katl-generation-activate/**",
	"cmd/katl-host-config-activate/**",
	"internal/installer/configapply/**",
	"internal/installer/configbundle/**",
	"internal/installer/configdomain/**",
	"internal/installer/katlosimage/**",
	"internal/installer/manifest/**",
	"internal/installer/payloadbundle/**",
	"internal/installer/systemextensionbundle/**",
	"mkosi.profiles/runtime/mkosi.extra/usr/lib/systemd/system/katl-system-extensions-*/**",
	"mkosi.profiles/runtime/mkosi.extra/usr/lib/systemd/system/katl-system-extensions-*.service",
	"mkosi.profiles/runtime/mkosi.extra/usr/lib/systemd/system/systemd-sysext.*/**",
	"mkosi.profiles/runtime/mkosi.extra/usr/lib/systemd/system/systemd-sysext.service",
	"mkosi.profiles/runtime/mkosi.extra/usr/lib/systemd/system/katl-generation-activate.service",
	"mkosi.profiles/runtime/mkosi.extra/usr/lib/systemd/system/katl-generation-activate.service.d/**",
	"mkosi.profiles/runtime/mkosi.extra/usr/lib/katl/systemd/activation-security.conf",
	"mkosi.profiles/runtime/mkosi.extra/usr/lib/katl/configuration-capabilities-v1",

	// Hosted runner, generated install fixture and DRBD workload.
	qualificationWorkflow,
	".github/actions/hosted-vmtest/action.yml",
	".github/scripts/hosted-apt-install",
	".github/scripts/hosted-vmtest-go",
	".github/scripts/hosted-vmtest-measure",
	".github/scripts/hosted-vmtest-run",
	"scripts/vmtest-run",
	"scripts/vmtest-exec",
	"cmd/katl-vmtest-agent/**",
	"cmd/katl-vmtest-ran/**",
	"cmd/katl-resource-lock/**",
	"internal/resourcetest/**",
	"mkosi.profiles/vmtest-drbd-tools/**",
	"internal/vmtest/*.go",
	"internal/vmtest/image/**",
	"internal/vmtest/proto/**",
	"internal/vmtest/drbd_smoke_test.go",
	"internal/vmtest/config_apply_smoke_test.go",
	"internal/vmtest/installed_smoke_test.go",
	"internal/vmtest/installed_world_test.go",
	"internal/vmtest/sysupdate_smoke_test.go",
	"internal/vmtest/world_test_helpers_test.go",
}

func kernelLockFlavour(name string) string {
	for _, flavour := range kernelFlavours {
		if matched, _ := path.Match("kernels/fedora-*/*/"+flavour+".json", name); matched {
			return flavour
		}
	}
	return ""
}

func sharedDriverInput(name string) bool {
	if strings.HasSuffix(name, ".md") {
		return false
	}
	for _, pattern := range driverInputs {
		if pattern == name {
			return true
		}
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		// A terminal /** includes the whole subtree. path.Match handles the
		// remaining pattern, including the recipe and profile directory names.
		candidate := name
		if base, recursive := strings.CutSuffix(pattern, "/**"); recursive {
			pattern = base
			depth := strings.Count(pattern, "/") + 1
			parts := strings.SplitN(name, "/", depth+1)
			if len(parts) <= depth {
				continue
			}
			candidate = strings.Join(parts[:depth], "/")
		}
		if matched, _ := path.Match(pattern, candidate); matched {
			return true
		}
	}
	return false
}

func committedDriverDigests() (map[string]string, error) {
	tree, err := gitOutput("ls-tree", "-rz", "--full-tree", "HEAD")
	if err != nil {
		return nil, err
	}
	// Use the release selector used by the builder. CI checks out the candidate
	// before planning, so the active Fedora release comes from that same tree.
	root, err := gitOutput("rev-parse", "--show-toplevel")
	if err != nil {
		return nil, err
	}
	fedoraRelease := exec.Command("./scripts/fedora-release")
	fedoraRelease.Dir = strings.TrimSpace(string(root))
	fedoraRelease.Env = append(os.Environ(), "KATL_REPO_ROOT="+fedoraRelease.Dir)
	release, err := fedoraRelease.Output()
	if err != nil {
		return nil, fmt.Errorf("select Fedora release: %w", err)
	}
	lockDir := "kernels/fedora-" + strings.TrimSpace(string(release)) + "/x86_64/"

	type input struct{ name, mode, content string }
	var shared []input
	locks := map[string]input{}
	for line := range bytes.SplitSeq(bytes.TrimSuffix(tree, []byte{0}), []byte{0}) {
		metadata, name, ok := strings.Cut(string(line), "\t")
		fields := strings.Fields(metadata)
		if !ok || len(fields) != 3 {
			return nil, fmt.Errorf("invalid git tree entry %q", line)
		}
		flavour := kernelLockFlavour(name)
		activeLock := flavour != "" && name == lockDir+flavour+".json"
		if !activeLock && !sharedDriverInput(name) {
			continue
		}
		if fields[1] != "blob" {
			return nil, fmt.Errorf("driver input %q is not a Git blob", name)
		}
		// Git's blob ID hashes the committed bytes, including symlink targets.
		// Project mixed-purpose inputs before hashing their content.
		file := input{name: name, mode: fields[0], content: fields[2]}
		if project := driverInputProjection(name); project != nil {
			if file.mode != "100644" && file.mode != "100755" {
				return nil, fmt.Errorf("projected driver input %q is not a regular file", name)
			}
			data, err := gitOutput("cat-file", "blob", file.content)
			if err != nil {
				return nil, err
			}
			projected, err := project(data)
			if err != nil {
				return nil, fmt.Errorf("project driver input %q: %w", name, err)
			}
			file.content = fmt.Sprintf("%x", sha256.Sum256(projected))
		}
		if activeLock {
			locks[flavour] = file
		} else {
			shared = append(shared, file)
		}
	}

	digests := map[string]string{}
	for _, flavour := range kernelFlavours {
		lock, ok := locks[flavour]
		if !ok || lock.mode == "120000" {
			continue
		}
		inputs := append(slices.Clone(shared), lock)
		slices.SortFunc(inputs, func(a, b input) int { return strings.Compare(a.name, b.name) })
		hash := sha256.New()
		fmt.Fprintf(hash, "katl-driver-qualification\x00%s\x00%s\x00", driverDigestVersion, flavour)
		for _, input := range inputs {
			fmt.Fprintf(hash, "%s\x00%s\x00%s\x00", input.name, input.mode, input.content)
		}
		digests[flavour] = fmt.Sprintf("%s:%x", driverDigestVersion, hash.Sum(nil))
	}
	return digests, nil
}

func driverInputProjection(name string) func([]byte) ([]byte, error) {
	switch {
	case name == qualificationWorkflow:
		return qualificationSteps
	case name == "mkosi.conf":
		return mkosiBuildInputs
	case path.Base(name) == "os-release", path.Base(name) == "os-release.in",
		strings.HasPrefix(path.Base(name), "extension-release."):
		return extensionIdentity
	default:
		return nil
	}
}

func extensionIdentity(data []byte) ([]byte, error) {
	var identity []string
	for line := range strings.SplitSeq(string(data), "\n") {
		key, _, _ := strings.Cut(strings.TrimSpace(line), "=")
		switch key {
		case "ID", "VERSION_ID", "ARCHITECTURE", "SYSEXT_LEVEL", "SYSEXT_SCOPE", "SYSEXT_ID", "CONFEXT_LEVEL":
			identity = append(identity, strings.TrimSpace(line))
		default:
			if strings.HasPrefix(key, "KATL_") {
				identity = append(identity, strings.TrimSpace(line))
			}
		}
	}
	return []byte(strings.Join(identity, "\n")), nil
}

func mkosiBuildInputs(data []byte) ([]byte, error) {
	var inputs []string
	var section string
	for raw := range strings.SplitSeq(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			section = line
		}
		if section == "[Build]" {
			key, _, _ := strings.Cut(line, "=")
			switch strings.TrimSpace(key) {
			case "Incremental", "WorkspaceDirectory", "CacheDirectory", "PackageCacheDirectory", "BuildDirectory":
				continue
			}
		}
		inputs = append(inputs, raw)
	}
	return []byte(strings.Join(inputs, "\n")), nil
}

func qualificationSteps(data []byte) ([]byte, error) {
	var workflow map[string]any
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		return nil, err
	}
	jobs, _ := workflow["jobs"].(map[string]any)
	job, _ := jobs["drivers"].(map[string]any)
	steps, ok := job["steps"].([]any)
	if !ok {
		return nil, fmt.Errorf("drivers job has no steps")
	}
	job["steps"] = slices.DeleteFunc(steps, func(step any) bool {
		fields, _ := step.(map[string]any)
		switch fields["id"] {
		case "record-qualification", "diagnostics", "upload-diagnostics":
			return true
		}
		return false
	})
	if env, ok := job["env"].(map[string]any); ok {
		delete(env, "KATL_VMTEST_KEEP")
		delete(env, "KATL_VMTEST_DEBUG_ON_FAILURE")
	}
	return json.Marshal(workflow)
}

func gitOutput(args ...string) ([]byte, error) {
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}
