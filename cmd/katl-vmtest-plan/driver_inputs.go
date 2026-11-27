package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"slices"
	"strings"

	"github.com/katl-dev/katl/internal/buildinputs"
	"github.com/katl-dev/katl/internal/flavour"
	"gopkg.in/yaml.v3"
)

// Bump when changing the digest format or the interpretation of its inputs.
const driverDigestVersion = "v5"

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

func committedDriverDigests() (map[string]string, error) {
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

	locks := map[string]string{}
	for _, flavour := range kernelFlavours {
		locks[flavour] = lockDir + flavour + ".json"
	}
	return buildinputs.Digests(fedoraRelease.Dir, "HEAD", buildinputs.Spec{
		Domain:  "katl-driver-qualification",
		Version: driverDigestVersion,
		Include: driverInputs,
		Exclude: []string{".md"},
		Salts:   locks,
		Project: driverInputProjection,
	})
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
