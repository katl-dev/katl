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
const driverDigestVersion = "v5"

var kernelFlavours = flavour.Supported()

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
	"cmd/katl-kernel-release/main.go",
	"cmd/katl-mkosi-artifacts/main.go",
	"cmd/katl-mkosi-artifacts/compile_extension.go",
	"cmd/katl-mkosi-artifacts/kernel_extension.go",
	"cmd/katl-mkosi-artifacts/kernel_inputs.go",
	"cmd/katl-mkosi-artifacts/kernel_lock.go",
	"cmd/katl-mkosi-artifacts/release_extension_inventory.go",
	"cmd/katl-mkosi-artifacts/release_extensions.go",
	"internal/releaseextensions/**",
	"internal/kernelrelease/**",
	"internal/kernelmodule/**",
	"internal/extensionrelease/**",
	"internal/retry/retry.go",

	// Kernel export, the mkosi toolchain, and install and upgrade image assembly.
	"Containerfile.mkosi",
	"containers-policy.json",
	"scripts/fedora-release",
	"scripts/mkosi",
	"scripts/build-katlos-install-image",
	".github/actions/mkosi-builder/**",
	"mkosi.conf",
	"mkosi.conf.d/**",
	"mkosi.profiles/installer-image/mkosi.conf",
	"mkosi.profiles/runtime/mkosi.build",
	"mkosi.profiles/runtime/mkosi.conf",
	"mkosi.profiles/runtime/mkosi.finalize",
	"go.mod",
	"go.sum",

	// Release identity, module layout and loader settings.
	"mkosi.profiles/runtime/os-release.in",
	"mkosi.profiles/runtime/mkosi.extra/etc/os-release",
	"mkosi.profiles/runtime/mkosi.extra/usr/lib/os-release",
	"mkosi.profiles/runtime/mkosi.extra/lib/modules/**",
	"mkosi.profiles/runtime/mkosi.extra/usr/lib/modules/**",
	"mkosi.profiles/runtime/mkosi.extra/usr/lib/modules-load.d/**",
	"mkosi.profiles/runtime/mkosi.extra/usr/lib/depmod.d/**",
	"mkosi.profiles/runtime/mkosi.extra/usr/lib/modprobe.d/**",
	"mkosi.profiles/runtime/mkosi.extra/etc/modules-load.d/**",
	"mkosi.profiles/runtime/mkosi.extra/etc/depmod.d/**",
	"mkosi.profiles/runtime/mkosi.extra/etc/modprobe.d/**",
	"mkosi.profiles/runtime/mkosi.extra/usr/lib/extension-release.d/**",

	// Driver selection, staging, module indexing, upgrade, and sysext activation.
	"cmd/katl-generation-activate/**",
	"internal/generation/activation.go",
	"internal/generation/module_indexes.go",
	"internal/generation/state.go",
	"internal/deadline/**",
	"internal/systemdunit/units/katl-system-extensions-activate.service",
	"internal/systemdunit/units/katl-deadline-extension-activation@.timer",
	"internal/systemdunit/units/katl-deadline-expire@.service",
	"internal/installer/systemextensionbundle/**",
	"internal/installer/nodeextensionbundle/**",
	"internal/installer/katlosimage/**",
	"internal/installer/configapply/extension_resolution.go",
	"internal/installer/configapply/module_indexes.go",
	"internal/installer/configapply/runtime_apply.go",
	"internal/installer/configapply/system_extensions.go",
	"mkosi.profiles/runtime/mkosi.extra/usr/lib/systemd/system/katl-system-extensions-*/**",
	"mkosi.profiles/runtime/mkosi.extra/usr/lib/systemd/system/katl-system-extensions-*.service",
	"mkosi.profiles/runtime/mkosi.extra/usr/lib/systemd/system/systemd-sysext.*/**",
	"mkosi.profiles/runtime/mkosi.extra/usr/lib/systemd/system/systemd-sysext.service",
	"mkosi.profiles/runtime/mkosi.extra/usr/lib/systemd/system/katl-generation-activate.service",
	"mkosi.profiles/runtime/mkosi.extra/usr/lib/systemd/system/katl-generation-activate.service.d/**",
	"mkosi.profiles/runtime/mkosi.extra/usr/lib/katl/systemd/activation-security.conf",
	"mkosi.profiles/runtime/mkosi.extra/usr/lib/katl/configuration-capabilities-v1",

	// Hosted provisioning and the entrypoints that launch the DRBD test and
	// propagate its exit status to the qualification job.
	qualificationWorkflow,
	".github/actions/setup-go/action.yml",
	".github/actions/hosted-vmtest/action.yml",
	".github/scripts/hosted-apt-install",
	".github/scripts/hosted-hermetic-setup",
	".github/scripts/hosted-vmtest-go",
	".github/scripts/hosted-vmtest-measure",
	".github/scripts/hosted-vmtest-run",
	"scripts/vmtest-run",
	"scripts/hermetic",
	"scripts/job-environment",
	"scripts/vmtest-exec",

	// The DRBD workload and the helpers it uses to install fixtures, control
	// guests and observe the generation and disk state.
	"mkosi.profiles/vmtest-drbd-tools/**",
	"internal/vmtest/admission.go",
	"internal/vmtest/drbd_smoke_test.go",
	"internal/vmtest/agent_client.go",
	"internal/vmtest/config_apply_smoke_test.go",
	"internal/vmtest/disk.go",
	"internal/vmtest/first_install.go",
	"internal/vmtest/first_install_runtime_fixture.go",
	"internal/vmtest/first_install_world.go",
	"internal/vmtest/guest.go",
	"internal/vmtest/guest_io_test.go",
	"internal/vmtest/guest_records_test.go",
	"internal/vmtest/installed_node.go",
	"internal/vmtest/installed_smoke_test.go",
	"internal/vmtest/installed_world.go",
	"internal/vmtest/installed_world_test.go",
	"internal/vmtest/katlctl.go",
	"internal/vmtest/management_identity.go",
	"internal/vmtest/vmtest.go",
	"internal/vmtest/world.go",
	"internal/vmtest/world_fixtures.go",
	"internal/vmtest/world_network.go",
	"internal/vmtest/world_scenario.go",
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
	files := map[string]input{}
	var selected []string
	locks := map[string]input{}
	for line := range bytes.SplitSeq(bytes.TrimSuffix(tree, []byte{0}), []byte{0}) {
		metadata, name, ok := strings.Cut(string(line), "\t")
		fields := strings.Fields(metadata)
		if !ok || len(fields) != 3 {
			return nil, fmt.Errorf("invalid git tree entry %q", line)
		}
		file := input{name: name, mode: fields[0], content: fields[2]}
		files[name] = file
		if flavour := kernelLockFlavour(name); flavour != "" && name == lockDir+flavour+".json" {
			locks[flavour] = file
		} else if sharedDriverInput(name) {
			selected = append(selected, name)
		}
	}

	var shared []input
	// Follow selected links through the committed tree. Keep link blobs as
	// inputs too, so changing the destination is itself observable.
	seen := map[string]bool{}
	for i := 0; i < len(selected); i++ {
		name := selected[i]
		if seen[name] {
			continue
		}
		seen[name] = true
		file := files[name]
		switch file.mode {
		case "120000":
			link, err := gitOutput("cat-file", "blob", file.content)
			if err != nil {
				return nil, err
			}
			target := path.Join(path.Dir(name), string(link))
			if !path.IsAbs(string(link)) && target != ".." && !strings.HasPrefix(target, "../") {
				if _, ok := files[target]; ok {
					selected = append(selected, target)
				} else {
					for child := range files {
						if target == "." || strings.HasPrefix(child, target+"/") {
							selected = append(selected, child)
						}
					}
				}
			}
		case "100644", "100755":
			if project := driverInputProjection(name); project != nil {
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
		default:
			return nil, fmt.Errorf("driver input %q is not a Git blob", name)
		}
		shared = append(shared, file)
	}

	digests := map[string]string{}
	for _, flavour := range kernelFlavours {
		lock, ok := locks[flavour]
		if !ok || (lock.mode != "100644" && lock.mode != "100755") {
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
	case name == "mkosi.conf", name == "mkosi.profiles/installer-image/mkosi.conf":
		return mkosiBuildInputs
	case name == "go.mod":
		return driverGoMod
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
	delete(job, "needs")
	delete(job, "if")
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
	return json.Marshal([]any{workflow["env"], workflow["defaults"], job})
}

func gitOutput(args ...string) ([]byte, error) {
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}
