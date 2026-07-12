package vmtest

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestVMTestRunInjectsWorld(t *testing.T) {
	repo := scriptTestRepoRoot(t)
	tmp := t.TempDir()
	fakeGo, fakeChild := writeFakeGoTools(t, tmp)
	host := writeFakeHostTools(t, tmp, true)
	buildDir := filepath.Join(repo, "_build")
	if err := os.MkdirAll(buildDir, 0o755); err != nil {
		t.Fatalf("create build dir: %v", err)
	}
	runRoot, err := os.MkdirTemp(buildDir, "vmtest-relative-run-")
	if err != nil {
		t.Fatalf("create relative run root: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(runRoot); err != nil {
			t.Errorf("remove relative run root: %v", err)
		}
	})
	runDir := filepath.Join(runRoot, "run")
	relativeRunDir, err := filepath.Rel(repo, runDir)
	if err != nil {
		t.Fatalf("relative run dir: %v", err)
	}
	goArgsPath := filepath.Join(tmp, "go-args.txt")
	childArgsPath := filepath.Join(tmp, "child-args.txt")
	childEnvPath := filepath.Join(tmp, "child-env.txt")

	cmd := exec.Command(filepath.Join(repo, "scripts", "vmtest-run"),
		"./internal/vmtest/scenarios",
		"-run", "^TestTwoNode$",
		"-count=99",
		"-timeout", "2m",
	)
	cmd.Dir = repo
	cmd.Env = appendHostEnv(removeEnv(removeEnv(os.Environ(), "CI"), "KATL_VMTEST_DEBUG_ON_FAILURE"), host,
		"KATL_VMTEST_GO="+fakeGo,
		"KATL_FAKE_GO_ARGS="+goArgsPath,
		"KATL_FAKE_CHILD="+fakeChild,
		"KATL_FAKE_CHILD_ARGS="+childArgsPath,
		"KATL_FAKE_CHILD_ENV="+childEnvPath,
		"KATL_VMTEST_RUN_ID=run-1",
		"KATL_VMTEST_RUN_DIR="+relativeRunDir,
		"TMPDIR="+tmp,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("vmtest-run failed: %v\n%s", err, output)
	}
	if strings.Contains(string(output), `{"Action":"run"`) {
		t.Fatalf("vmtest-run emitted JSON without caller -json:\n%s", output)
	}
	if strings.Contains(string(output), "=== RUN") || strings.Contains(string(output), "ok  \tfake/vmtest") {
		t.Fatalf("vmtest-run emitted go test output instead of logging it:\n%s", output)
	}
	if !strings.Contains(string(output), "vmtest go test log: "+filepath.Join(runDir, "go-test.log")) {
		t.Fatalf("output missing go test log path:\n%s", output)
	}

	world, err := LoadWorld(filepath.Join(runDir, "world.json"))
	if err != nil {
		t.Fatalf("LoadWorld() error = %v", err)
	}
	if world.RunID != "run-1" || world.RunDir != runDir {
		t.Fatalf("world = %#v", world)
	}
	if world.CacheDir != filepath.Join(repo, "_build", "vmtest") {
		t.Fatalf("world cache dir = %q", world.CacheDir)
	}
	if world.RunIndex != filepath.Join(runDir, "run.json") {
		t.Fatalf("world run index = %q", world.RunIndex)
	}
	if world.GoTestLog != filepath.Join(runDir, "go-test.log") || world.ResourceManifest != filepath.Join(runDir, "resource-test-manifest.json") || world.ResourceDigest != strings.Repeat("a", 64) || world.PackageLock != filepath.Join(repo, "mkosi.profiles", "resource-package-lock.json") || world.PackageLockDigest != strings.Repeat("a", 64) || world.AutoRebuild || world.ArtifactSet != "default" {
		t.Fatalf("world log/rebuild fields = %#v", world)
	}
	if len(world.Artifacts) != 1 {
		t.Fatalf("world artifacts = %#v", world.Artifacts)
	}
	if artifact := world.Artifacts[0]; artifact.Name != "installer-uki" || artifact.Path != "/tmp/katl-installer.efi" || artifact.RepoPath != "/tmp/katl-installer.efi" || artifact.Digest != strings.Repeat("c", 64) || artifact.Source != "resource-test-manifest" || artifact.Action != "validated" {
		t.Fatalf("world artifact = %#v", artifact)
	}
	if world.ArtifactInputs == nil {
		t.Fatal("world artifact inputs missing")
	}
	if len(world.ArtifactInputs.MkosiProfiles) != 1 || world.ArtifactInputs.MkosiProfiles[0].Name != "installer-image" || world.ArtifactInputs.MkosiProfiles[0].ConfigSHA256 != strings.Repeat("a", 64) {
		t.Fatalf("world artifact input profiles = %#v", world.ArtifactInputs.MkosiProfiles)
	}
	if len(world.ArtifactInputs.PackageSets) != 1 || world.ArtifactInputs.PackageSets[0].Name != "installer-image" || world.ArtifactInputs.PackageSets[0].LockSHA256 != strings.Repeat("b", 64) || world.ArtifactInputs.PackageSets[0].PackageCount != 1 {
		t.Fatalf("world artifact input package sets = %#v", world.ArtifactInputs.PackageSets)
	}
	if world.Libvirt.URI != "qemu:///system" || world.Libvirt.Network != "default" || world.Libvirt.StoragePool != "default" {
		t.Fatalf("world libvirt = %#v", world.Libvirt)
	}
	if world.Network.Backend != NetworkLibvirt || world.Network.Name != "default" || world.Network.CIDR != "192.168.122.0/24" || world.Network.Gateway != "192.168.122.1" || world.Network.LeaseFile != filepath.Join(runDir, "network", "leases.json") {
		t.Fatalf("world network = %#v", world.Network)
	}
	for _, capability := range []string{"image-tool", "libvirt", "libvirt-network", "libvirt-storage-pool", "ovmf", "kvm", "vsock", "mtools", "sfdisk", "sha256sum", "awk", "realpath", "kubectl"} {
		if world.Capabilities[capability] != WorldStatusPassed {
			t.Fatalf("capability %s = %q", capability, world.Capabilities[capability])
		}
	}

	goArgs := readLines(t, goArgsPath)
	wantGoArgs := []string{
		"test",
		"-exec",
		filepath.Join(repo, "scripts", "vmtest-exec"),
		"./internal/vmtest/scenarios",
		"-run",
		"^TestTwoNode$",
		"-count=99",
		"-timeout",
		"2m",
	}
	if !reflect.DeepEqual(goArgs, wantGoArgs) {
		t.Fatalf("go args = %#v, want %#v", goArgs, wantGoArgs)
	}

	childArgs := readLines(t, childArgsPath)
	wantChildArgs := []string{"-test.run=^Forwarded$", "-test.v", "child-extra"}
	if !reflect.DeepEqual(childArgs, wantChildArgs) {
		t.Fatalf("child args = %#v, want %#v", childArgs, wantChildArgs)
	}

	childEnv := readKeyValues(t, childEnvPath)
	if childEnv["KATL_VMTEST_WORLD_MANIFEST"] != filepath.Join(runDir, "world.json") {
		t.Fatalf("child manifest env = %q", childEnv["KATL_VMTEST_WORLD_MANIFEST"])
	}
	if childEnv["KATL_VMTEST_CACHE_DIR"] != filepath.Join(repo, "_build", "vmtest") {
		t.Fatalf("child cache dir env = %q", childEnv["KATL_VMTEST_CACHE_DIR"])
	}
	if childEnv["KATL_VMTEST_LIBVIRT_URI"] != "qemu:///system" || childEnv["KATL_VMTEST_LIBVIRT_NETWORK"] != "default" {
		t.Fatalf("child libvirt env = %#v", childEnv)
	}
	if childEnv["KATL_VMTEST_RUN"] != "1" || childEnv["KATL_VMTEST_WORLD_STRICT"] != "1" {
		t.Fatalf("child strict env = %#v", childEnv)
	}
	if childEnv["KATL_VMTEST_DEBUG_ON_FAILURE"] != "1" || !world.DebugOnFailure || !world.DebugShell {
		t.Fatalf("child debug env/world = %#v %#v", childEnv, world)
	}
	if childEnv["KATL_VMTEST_RESOURCE_MANIFEST"] != filepath.Join(runDir, "resource-test-manifest.json") {
		t.Fatalf("child resource manifest env = %q", childEnv["KATL_VMTEST_RESOURCE_MANIFEST"])
	}

	runIndex := readRunIndex(t, filepath.Join(runDir, "run.json"))
	if runIndex.Kind != "VMTestRun" || runIndex.Status != "passed" {
		t.Fatalf("run index = %#v", runIndex)
	}
	if runIndex.RunID != "run-1" || runIndex.WorldManifest != filepath.Join(runDir, "world.json") || runIndex.HostCapabilities != filepath.Join(runDir, "host-capabilities.json") {
		t.Fatalf("run index paths = %#v", runIndex)
	}
	if runIndex.CacheDir != filepath.Join(repo, "_build", "vmtest") {
		t.Fatalf("run index cache dir = %q", runIndex.CacheDir)
	}
	if runIndex.GoTestLog != filepath.Join(runDir, "go-test.log") || runIndex.ResourceManifest != filepath.Join(runDir, "resource-test-manifest.json") || runIndex.ResourceDigest != strings.Repeat("a", 64) || runIndex.PackageLock != filepath.Join(repo, "mkosi.profiles", "resource-package-lock.json") || runIndex.PackageLockDigest != strings.Repeat("a", 64) || runIndex.AutoRebuild || runIndex.ArtifactSet != "default" {
		t.Fatalf("run index log/rebuild fields = %#v", runIndex)
	}
	if len(runIndex.Artifacts) != 1 || runIndex.Artifacts[0].Name != "installer-uki" || runIndex.Artifacts[0].Action != "validated" {
		t.Fatalf("run index artifacts = %#v", runIndex.Artifacts)
	}
	if runIndex.ArtifactInputs == nil || len(runIndex.ArtifactInputs.PackageSets) != 1 || runIndex.ArtifactInputs.PackageSets[0].PackageCount != 1 {
		t.Fatalf("run index artifact inputs = %#v", runIndex.ArtifactInputs)
	}
	if _, err := os.Stat(runIndex.ResourceManifest); err != nil {
		t.Fatalf("resource manifest missing: %v", err)
	}
	if !reflect.DeepEqual(runIndex.GoTestArgs, []string{"./internal/vmtest/scenarios", "-run", "^TestTwoNode$", "-count=99", "-timeout", "2m"}) {
		t.Fatalf("run index go test args = %#v", runIndex.GoTestArgs)
	}
	if !runIndex.DebugOnFailure || !runIndex.DebugShell {
		t.Fatalf("run index debug policy = %#v", runIndex)
	}

	if _, err := os.Stat(filepath.Join(runDir, "summary.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("summary.json exists unexpectedly: %v", err)
	}
	goLog := readFile(t, filepath.Join(runDir, "go-test.log"))
	if !strings.Contains(goLog, "=== RUN   TestForwarded") || !strings.Contains(goLog, "ok  \tfake/vmtest") {
		t.Fatalf("go-test.log missing go test output:\n%s", goLog)
	}
	assertJSONEmptyObject(t, filepath.Join(runDir, "network", "leases.json"))
}

func TestVMTestRunDebugOnFailurePolicy(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		env       []string
		want      string
		wantExit  int
		wantError string
	}{
		{name: "CI default disables", env: []string{"CI=true"}, want: "0"},
		{name: "env enables in CI", env: []string{"CI=true", "KATL_VMTEST_DEBUG_ON_FAILURE=1"}, want: "1"},
		{name: "env disables local", env: []string{"KATL_VMTEST_DEBUG_ON_FAILURE=0"}, want: "0"},
		{name: "flag enables", args: []string{"--debug-on-failure"}, env: []string{"CI=true"}, want: "1"},
		{name: "flag disables", args: []string{"--no-debug-on-failure"}, want: "0"},
		{name: "invalid env", env: []string{"KATL_VMTEST_DEBUG_ON_FAILURE=maybe"}, wantExit: 2, wantError: "invalid vmtest debug-on-failure policy"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := scriptTestRepoRoot(t)
			tmp := t.TempDir()
			fakeGo, fakeChild := writeFakeGoTools(t, tmp)
			host := writeFakeHostTools(t, tmp, true)
			runDir := filepath.Join(tmp, "run")
			childEnvPath := filepath.Join(tmp, "child-env.txt")

			args := append([]string{}, tt.args...)
			args = append(args, "./internal/vmtest")
			cmd := exec.Command(filepath.Join(repo, "scripts", "vmtest-run"), args...)
			cmd.Dir = repo
			env := appendHostEnv(removeEnv(os.Environ(), "KATL_VMTEST_DEBUG_ON_FAILURE"), host,
				"KATL_VMTEST_GO="+fakeGo,
				"KATL_FAKE_GO_ARGS="+filepath.Join(tmp, "go-args.txt"),
				"KATL_FAKE_CHILD="+fakeChild,
				"KATL_FAKE_CHILD_ARGS="+filepath.Join(tmp, "child-args.txt"),
				"KATL_FAKE_CHILD_ENV="+childEnvPath,
				"KATL_VMTEST_RUN_ID=run-debug-policy-"+strings.ReplaceAll(tt.name, " ", "-"),
				"KATL_VMTEST_RUN_DIR="+runDir,
				"TMPDIR="+tmp,
			)
			env = append(env, tt.env...)
			cmd.Env = env
			output, err := cmd.CombinedOutput()
			if tt.wantExit != 0 {
				if exitCode(err) != tt.wantExit {
					t.Fatalf("vmtest-run exit = %v, want %d\n%s", err, tt.wantExit, output)
				}
				if !strings.Contains(string(output), tt.wantError) {
					t.Fatalf("output missing %q:\n%s", tt.wantError, output)
				}
				return
			}
			if err != nil {
				t.Fatalf("vmtest-run failed: %v\n%s", err, output)
			}
			childEnv := readKeyValues(t, childEnvPath)
			if childEnv["KATL_VMTEST_DEBUG_ON_FAILURE"] != tt.want {
				t.Fatalf("debug env = %q, want %q; output:\n%s", childEnv["KATL_VMTEST_DEBUG_ON_FAILURE"], tt.want, output)
			}
			world, err := LoadWorld(filepath.Join(runDir, "world.json"))
			if err != nil {
				t.Fatalf("LoadWorld() error = %v", err)
			}
			if world.DebugOnFailure != (tt.want == "1") || world.DebugShell != (tt.want == "1") {
				t.Fatalf("world debug policy = %#v, want %s", world, tt.want)
			}
		})
	}
}

func TestVMTestRunRejectsStaleInstallerArtifactBypass(t *testing.T) {
	repo := scriptTestRepoRoot(t)
	tmp := t.TempDir()
	fakeGo, fakeChild := writeFakeGoTools(t, tmp)
	host := writeFakeHostTools(t, tmp, true)
	runDir := filepath.Join(tmp, "run-stale-installer")
	goArgsPath := filepath.Join(tmp, "go-args.txt")

	cmd := exec.Command(filepath.Join(repo, "scripts", "vmtest-run"), "./internal/vmtest")
	cmd.Dir = repo
	cmd.Env = appendHostEnv(os.Environ(), host,
		"KATL_VMTEST_GO="+fakeGo,
		"KATL_FAKE_GO_ARGS="+goArgsPath,
		"KATL_FAKE_CHILD="+fakeChild,
		"KATL_FAKE_CHILD_ARGS="+filepath.Join(tmp, "child-args.txt"),
		"KATL_FAKE_CHILD_ENV="+filepath.Join(tmp, "child-env.txt"),
		"KATL_ALLOW_STALE_INSTALLER_ARTIFACTS=1",
		"KATL_VMTEST_RUN_ID=run-stale-installer",
		"KATL_VMTEST_RUN_DIR="+runDir,
		"TMPDIR="+tmp,
	)
	output, err := cmd.CombinedOutput()
	if exitCode(err) != 2 {
		t.Fatalf("vmtest-run exit = %v, want 2\n%s", err, output)
	}
	if _, err := os.Stat(goArgsPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("go test ran for stale installer setup failure, stat err = %v", err)
	}
	runIndex := readRunIndex(t, filepath.Join(runDir, "run.json"))
	if runIndex.Status != "setup-failed" {
		t.Fatalf("run index status = %q", runIndex.Status)
	}
	if len(runIndex.SetupFailures) != 1 || !strings.Contains(runIndex.SetupFailures[0], "KATL_ALLOW_STALE_INSTALLER_ARTIFACTS") {
		t.Fatalf("run index setup failures = %#v", runIndex.SetupFailures)
	}
}

func TestVMTestRunRejectsExplicitInstallerArtifacts(t *testing.T) {
	tests := []struct {
		name string
		env  string
	}{
		{name: "artifact index", env: "KATL_MKOSI_ARTIFACT_INDEX=/tmp/stale-artifacts.json"},
		{name: "installer UKI", env: "KATL_INSTALLER_UKI=/tmp/stale-installer.efi"},
		{name: "install manifest", env: "KATL_INSTALL_MANIFEST=/tmp/stale-install.yaml"},
		{name: "runtime artifact", env: "KATL_RUNTIME_ARTIFACT=/tmp/stale-runtime.squashfs"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := scriptTestRepoRoot(t)
			tmp := t.TempDir()
			fakeGo, fakeChild := writeFakeGoTools(t, tmp)
			host := writeFakeHostTools(t, tmp, true)
			runDir := filepath.Join(tmp, "run-explicit-"+strings.ReplaceAll(tt.name, " ", "-"))
			goArgsPath := filepath.Join(tmp, "go-args.txt")

			cmd := exec.Command(filepath.Join(repo, "scripts", "vmtest-run"), "./internal/vmtest")
			cmd.Dir = repo
			cmd.Env = appendHostEnv(os.Environ(), host,
				"KATL_VMTEST_GO="+fakeGo,
				"KATL_FAKE_GO_ARGS="+goArgsPath,
				"KATL_FAKE_CHILD="+fakeChild,
				"KATL_FAKE_CHILD_ARGS="+filepath.Join(tmp, "child-args.txt"),
				"KATL_FAKE_CHILD_ENV="+filepath.Join(tmp, "child-env.txt"),
				"KATL_VMTEST_RUN_ID=run-explicit-"+strings.ReplaceAll(tt.name, " ", "-"),
				"KATL_VMTEST_RUN_DIR="+runDir,
				"TMPDIR="+tmp,
				tt.env,
			)
			output, err := cmd.CombinedOutput()
			if exitCode(err) != 2 {
				t.Fatalf("vmtest-run exit = %v, want 2\n%s", err, output)
			}
			if _, err := os.Stat(goArgsPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("go test ran for explicit artifact setup failure, stat err = %v", err)
			}
			runIndex := readRunIndex(t, filepath.Join(runDir, "run.json"))
			if runIndex.Status != "setup-failed" {
				t.Fatalf("run index status = %q", runIndex.Status)
			}
			if len(runIndex.SetupFailures) != 1 || !strings.Contains(runIndex.SetupFailures[0], "explicit installer artifact inputs") {
				t.Fatalf("run index setup failures = %#v", runIndex.SetupFailures)
			}
		})
	}
}

func TestVMTestRunBuildsDefaultArtifacts(t *testing.T) {
	realRepo := scriptTestRepoRoot(t)
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "scripts"), 0o755); err != nil {
		t.Fatalf("MkdirAll(scripts) error = %v", err)
	}
	copyScript(t, filepath.Join(realRepo, "scripts", "vmtest-run"), filepath.Join(repo, "scripts", "vmtest-run"))
	writeExecutable(t, filepath.Join(repo, "scripts", "mkosi"), `#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$KATL_FAKE_MKOSI_ARGS"
`)
	writeExecutable(t, filepath.Join(repo, "scripts", "vmtest-exec"), `#!/usr/bin/env bash
set -euo pipefail
export KATL_VMTEST_RUN=1
export KATL_VMTEST_WORLD_STRICT=1
exec "$@"
`)

	tmp := t.TempDir()
	fakeGo, fakeChild := writeFakeGoTools(t, tmp)
	host := writeFakeHostTools(t, tmp, true)
	runDir := filepath.Join(tmp, "run")
	goArgsPath := filepath.Join(tmp, "go-args.txt")
	mkosiArgsPath := filepath.Join(tmp, "mkosi-args.txt")
	env := appendHostEnv(os.Environ(), host,
		"KATL_VMTEST_GO="+fakeGo,
		"KATL_FAKE_GO_ARGS="+goArgsPath,
		"KATL_FAKE_CHILD="+fakeChild,
		"KATL_FAKE_CHILD_ARGS="+filepath.Join(tmp, "child-args.txt"),
		"KATL_FAKE_CHILD_ENV="+filepath.Join(tmp, "child-env.txt"),
		"KATL_FAKE_MKOSI_ARGS="+mkosiArgsPath,
		"KATL_VMTEST_RUN_ID=run-build-default",
		"KATL_VMTEST_RUN_DIR="+runDir,
		"TMPDIR="+tmp,
	)
	for _, name := range []string{
		"KATL_MKOSI_ARTIFACT_INDEX",
		"KATL_INSTALLER_UKI",
		"KATL_INSTALLER_KERNEL",
		"KATL_INSTALLER_INITRD",
		"KATL_RUNTIME_ARTIFACT",
		"KATL_INSTALL_MANIFEST",
		"KATL_VMTEST_AUTO_REBUILD",
	} {
		env = removeEnv(env, name)
	}

	cmd := exec.Command(filepath.Join(repo, "scripts", "vmtest-run"), "./internal/vmtest", "-run", "NeedsArtifacts")
	cmd.Dir = repo
	cmd.Env = env
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("vmtest-run failed: %v\n%s", err, output)
	}

	mkosiArgs := readLines(t, mkosiArgsPath)
	if !reflect.DeepEqual(mkosiArgs, []string{"build-katlos-install-image", "build-katlos-upgrade-image", "build-kubernetes-sysext", "build-installer-iso"}) {
		t.Fatalf("mkosi args = %#v", mkosiArgs)
	}
	goArgs := readLines(t, goArgsPath)
	if !reflect.DeepEqual(goArgs, []string{
		"test",
		"-exec",
		filepath.Join(repo, "scripts", "vmtest-exec"),
		"-timeout",
		"90m",
		"./internal/vmtest",
		"-run",
		"NeedsArtifacts",
	}) {
		t.Fatalf("go args = %#v", goArgs)
	}
	runIndex := readRunIndex(t, filepath.Join(runDir, "run.json"))
	if runIndex.Status != "passed" {
		t.Fatalf("run index status = %q", runIndex.Status)
	}
	if len(runIndex.Artifacts) != 1 || runIndex.Artifacts[0].Name != "installer-uki" || runIndex.Artifacts[0].Action != "built" {
		t.Fatalf("run index artifacts = %#v", runIndex.Artifacts)
	}
}

func TestVMTestRunRecordsCacheResolvedArtifacts(t *testing.T) {
	realRepo := scriptTestRepoRoot(t)
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "scripts"), 0o755); err != nil {
		t.Fatalf("MkdirAll(scripts) error = %v", err)
	}
	copyScript(t, filepath.Join(realRepo, "scripts", "vmtest-run"), filepath.Join(repo, "scripts", "vmtest-run"))
	writeExecutable(t, filepath.Join(repo, "scripts", "mkosi"), `#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$KATL_FAKE_MKOSI_ARGS"
case "$*" in
  build-installer-iso)
    printf 'mkosi cache hit: installer-iso artifacts match the current repo\n'
    ;;
  build-katlos-install-image)
    printf 'mkosi cache hit: katlos-install-image artifacts match the current repo\n'
    ;;
esac
`)
	writeExecutable(t, filepath.Join(repo, "scripts", "vmtest-exec"), `#!/usr/bin/env bash
set -euo pipefail
export KATL_VMTEST_RUN=1
export KATL_VMTEST_WORLD_STRICT=1
exec "$@"
`)

	tmp := t.TempDir()
	fakeGo, fakeChild := writeFakeGoTools(t, tmp)
	host := writeFakeHostTools(t, tmp, true)
	runDir := filepath.Join(tmp, "run")
	mkosiArgsPath := filepath.Join(tmp, "mkosi-args.txt")
	env := appendHostEnv(os.Environ(), host,
		"KATL_VMTEST_GO="+fakeGo,
		"KATL_FAKE_GO_ARGS="+filepath.Join(tmp, "go-args.txt"),
		"KATL_FAKE_CHILD="+fakeChild,
		"KATL_FAKE_CHILD_ARGS="+filepath.Join(tmp, "child-args.txt"),
		"KATL_FAKE_CHILD_ENV="+filepath.Join(tmp, "child-env.txt"),
		"KATL_FAKE_MKOSI_ARGS="+mkosiArgsPath,
		"KATL_VMTEST_RUN_ID=run-cache-resolved",
		"KATL_VMTEST_RUN_DIR="+runDir,
		"TMPDIR="+tmp,
	)
	for _, name := range []string{
		"KATL_MKOSI_ARTIFACT_INDEX",
		"KATL_INSTALLER_UKI",
		"KATL_INSTALLER_KERNEL",
		"KATL_INSTALLER_INITRD",
		"KATL_RUNTIME_ARTIFACT",
		"KATL_INSTALL_MANIFEST",
		"KATL_VMTEST_AUTO_REBUILD",
	} {
		env = removeEnv(env, name)
	}

	cmd := exec.Command(filepath.Join(repo, "scripts", "vmtest-run"), "./internal/vmtest", "-run", "NeedsArtifacts")
	cmd.Dir = repo
	cmd.Env = env
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("vmtest-run failed: %v\n%s", err, output)
	}

	runIndex := readRunIndex(t, filepath.Join(runDir, "run.json"))
	if len(runIndex.Artifacts) != 1 || runIndex.Artifacts[0].Name != "installer-uki" || runIndex.Artifacts[0].Action != "cache-resolved" {
		t.Fatalf("run index artifacts = %#v", runIndex.Artifacts)
	}
	world, err := LoadWorld(filepath.Join(runDir, "world.json"))
	if err != nil {
		t.Fatalf("LoadWorld() error = %v", err)
	}
	if len(world.Artifacts) != 1 || world.Artifacts[0].Action != "cache-resolved" {
		t.Fatalf("world artifacts = %#v", world.Artifacts)
	}
}

func TestVMTestRunRecordsRuntimeOnlyCacheProvenance(t *testing.T) {
	realRepo := scriptTestRepoRoot(t)
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "scripts"), 0o755); err != nil {
		t.Fatalf("MkdirAll(scripts) error = %v", err)
	}
	copyScript(t, filepath.Join(realRepo, "scripts", "vmtest-run"), filepath.Join(repo, "scripts", "vmtest-run"))
	writeExecutable(t, filepath.Join(repo, "scripts", "mkosi"), `#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$KATL_FAKE_MKOSI_ARGS"
printf 'mkosi cache hit: runtime artifacts match the current repo\n'
`)
	writeExecutable(t, filepath.Join(repo, "scripts", "vmtest-exec"), `#!/usr/bin/env bash
set -euo pipefail
export KATL_VMTEST_RUN=1
export KATL_VMTEST_WORLD_STRICT=1
exec "$@"
`)
	runtimePath := filepath.Join(repo, "_build", "mkosi", "katl-runtime-root.squashfs")
	if err := os.MkdirAll(filepath.Dir(runtimePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runtimePath, []byte("current runtime"), 0o644); err != nil {
		t.Fatal(err)
	}
	runtimeSHA := strings.Repeat("a", 64)
	artifactIndex := map[string]any{
		"schemaVersion": 1,
		"artifacts": []map[string]any{{
			"kind": "runtime-root", "path": "_build/mkosi/katl-runtime-root.squashfs",
			"sha256": runtimeSHA, "sizeBytes": len("current runtime"),
		}},
	}
	indexData, err := json.Marshal(artifactIndex)
	if err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(repo, "_build", "mkosi", "artifacts.json")
	if err := os.WriteFile(indexPath, indexData, 0o644); err != nil {
		t.Fatal(err)
	}

	tmp := t.TempDir()
	fakeGo, fakeChild := writeFakeGoTools(t, tmp)
	host := writeFakeHostTools(t, tmp, true)
	runDir := filepath.Join(tmp, "run")
	goArgsPath := filepath.Join(tmp, "go-args.txt")
	mkosiArgsPath := filepath.Join(tmp, "mkosi-args.txt")
	env := appendHostEnv(os.Environ(), host,
		"KATL_VMTEST_GO="+fakeGo,
		"KATL_FAKE_GO_ARGS="+goArgsPath,
		"KATL_FAKE_CHILD="+fakeChild,
		"KATL_FAKE_CHILD_ARGS="+filepath.Join(tmp, "child-args.txt"),
		"KATL_FAKE_CHILD_ENV="+filepath.Join(tmp, "child-env.txt"),
		"KATL_FAKE_MKOSI_ARGS="+mkosiArgsPath,
		"KATL_VMTEST_RUN_ID=run-runtime-only",
		"KATL_VMTEST_RUN_DIR="+runDir,
		"TMPDIR="+tmp,
	)
	for _, name := range []string{
		"KATL_MKOSI_ARTIFACT_INDEX",
		"KATL_INSTALLER_UKI",
		"KATL_INSTALLER_KERNEL",
		"KATL_INSTALLER_INITRD",
		"KATL_RUNTIME_ARTIFACT",
		"KATL_INSTALL_MANIFEST",
		"KATL_VMTEST_AUTO_REBUILD",
	} {
		env = removeEnv(env, name)
	}

	cmd := exec.Command(filepath.Join(repo, "scripts", "vmtest-run"), "--artifact-set=runtime", "./internal/vmtest", "-run", "NeedsRuntimeOnly")
	cmd.Dir = repo
	cmd.Env = env
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("vmtest-run failed: %v\n%s", err, output)
	}

	mkosiArgs := readLines(t, mkosiArgsPath)
	if !reflect.DeepEqual(mkosiArgs, []string{"build-runtime"}) {
		t.Fatalf("mkosi args = %#v", mkosiArgs)
	}
	world, err := LoadWorld(filepath.Join(runDir, "world.json"))
	if err != nil {
		t.Fatalf("LoadWorld() error = %v", err)
	}
	if world.ArtifactSet != "runtime" {
		t.Fatalf("world artifact set = %q", world.ArtifactSet)
	}
	if len(world.Artifacts) != 1 || world.Artifacts[0].Kind != "runtime-root" || world.Artifacts[0].Digest != runtimeSHA || world.Artifacts[0].Action != "cache-resolved" {
		t.Fatalf("world artifacts = %#v", world.Artifacts)
	}
	if world.ArtifactInputs == nil || len(world.ArtifactInputs.Tools) != 1 || world.ArtifactInputs.Tools[0].Name != "mkosi-artifact-index" || world.ArtifactInputs.Tools[0].SHA256 == "" {
		t.Fatalf("world artifact inputs = %#v", world.ArtifactInputs)
	}
	runIndex := readRunIndex(t, filepath.Join(runDir, "run.json"))
	if runIndex.ArtifactSet != "runtime" {
		t.Fatalf("run index artifact set = %q", runIndex.ArtifactSet)
	}
	if len(runIndex.Artifacts) != 1 || runIndex.Artifacts[0].Action != "cache-resolved" || runIndex.ArtifactInputs == nil {
		t.Fatalf("run index provenance = artifacts=%#v inputs=%#v", runIndex.Artifacts, runIndex.ArtifactInputs)
	}
}

func TestVMTestRunCanDisableDefaultArtifactBuilds(t *testing.T) {
	realRepo := scriptTestRepoRoot(t)
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "scripts"), 0o755); err != nil {
		t.Fatalf("MkdirAll(scripts) error = %v", err)
	}
	copyScript(t, filepath.Join(realRepo, "scripts", "vmtest-run"), filepath.Join(repo, "scripts", "vmtest-run"))
	writeExecutable(t, filepath.Join(repo, "scripts", "mkosi"), `#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$KATL_FAKE_MKOSI_ARGS"
`)
	writeExecutable(t, filepath.Join(repo, "scripts", "vmtest-exec"), `#!/usr/bin/env bash
set -euo pipefail
export KATL_VMTEST_RUN=1
export KATL_VMTEST_WORLD_STRICT=1
exec "$@"
`)

	tmp := t.TempDir()
	fakeGo, fakeChild := writeFakeGoTools(t, tmp)
	host := writeFakeHostTools(t, tmp, true)
	runDir := filepath.Join(tmp, "run")
	goArgsPath := filepath.Join(tmp, "go-args.txt")
	mkosiArgsPath := filepath.Join(tmp, "mkosi-args.txt")
	env := appendHostEnv(os.Environ(), host,
		"KATL_VMTEST_GO="+fakeGo,
		"KATL_FAKE_GO_ARGS="+goArgsPath,
		"KATL_FAKE_CHILD="+fakeChild,
		"KATL_FAKE_CHILD_ARGS="+filepath.Join(tmp, "child-args.txt"),
		"KATL_FAKE_CHILD_ENV="+filepath.Join(tmp, "child-env.txt"),
		"KATL_FAKE_MKOSI_ARGS="+mkosiArgsPath,
		"KATL_VMTEST_RUN_ID=run-no-rebuild",
		"KATL_VMTEST_RUN_DIR="+runDir,
		"TMPDIR="+tmp,
	)
	for _, name := range []string{
		"KATL_MKOSI_ARTIFACT_INDEX",
		"KATL_INSTALLER_UKI",
		"KATL_INSTALLER_KERNEL",
		"KATL_INSTALLER_INITRD",
		"KATL_RUNTIME_ARTIFACT",
		"KATL_INSTALL_MANIFEST",
		"KATL_VMTEST_AUTO_REBUILD",
	} {
		env = removeEnv(env, name)
	}

	cmd := exec.Command(filepath.Join(repo, "scripts", "vmtest-run"), "--no-rebuild", "./internal/vmtest", "-run", "NeedsArtifacts")
	cmd.Dir = repo
	cmd.Env = env
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("vmtest-run failed: %v\n%s", err, output)
	}

	if _, err := os.Stat(mkosiArgsPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("mkosi ran with --no-rebuild, stat err = %v", err)
	}
	goArgs := readLines(t, goArgsPath)
	if !reflect.DeepEqual(goArgs, []string{
		"test",
		"-exec",
		filepath.Join(repo, "scripts", "vmtest-exec"),
		"-timeout",
		"90m",
		"./internal/vmtest",
		"-run",
		"NeedsArtifacts",
	}) {
		t.Fatalf("go args = %#v", goArgs)
	}
	runIndex := readRunIndex(t, filepath.Join(runDir, "run.json"))
	if runIndex.AutoRebuild {
		t.Fatalf("run index autoRebuild = true, want false")
	}
}

func TestVMTestRunForwardsJSONFlag(t *testing.T) {
	repo := scriptTestRepoRoot(t)
	tmp := t.TempDir()
	fakeGo, fakeChild := writeFakeGoTools(t, tmp)
	host := writeFakeHostTools(t, tmp, true)
	runDir := filepath.Join(tmp, "run")
	goArgsPath := filepath.Join(tmp, "go-args.txt")

	cmd := exec.Command(filepath.Join(repo, "scripts", "vmtest-run"),
		"-json",
		"./internal/vmtest/scenarios",
		"-run", "^TestTwoNode$",
	)
	cmd.Dir = repo
	cmd.Env = appendHostEnv(os.Environ(), host,
		"KATL_VMTEST_GO="+fakeGo,
		"KATL_FAKE_GO_ARGS="+goArgsPath,
		"KATL_FAKE_CHILD="+fakeChild,
		"KATL_FAKE_CHILD_ARGS="+filepath.Join(tmp, "child-args.txt"),
		"KATL_FAKE_CHILD_ENV="+filepath.Join(tmp, "child-env.txt"),
		"KATL_VMTEST_RUN_ID=run-json",
		"KATL_VMTEST_RUN_DIR="+runDir,
		"TMPDIR="+tmp,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("vmtest-run -json failed: %v\n%s", err, output)
	}

	goArgs := readLines(t, goArgsPath)
	if !contains(goArgs, "-json") {
		t.Fatalf("go args missing caller -json: %#v", goArgs)
	}
	if strings.Contains(string(output), `{"Action":"run","Package":"fake/vmtest"}`) {
		t.Fatalf("output included JSON events instead of logging them:\n%s", output)
	}
	goLog := readFile(t, filepath.Join(runDir, "go-test.log"))
	if !strings.Contains(goLog, `{"Action":"run","Package":"fake/vmtest"}`) {
		t.Fatalf("go-test.log missing JSON events:\n%s", goLog)
	}
	if _, err := os.Stat(filepath.Join(runDir, "summary.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("summary.json exists unexpectedly: %v", err)
	}
}

func TestVMTestRunPreservesArbitraryGoTestArgs(t *testing.T) {
	repo := scriptTestRepoRoot(t)
	tmp := t.TempDir()
	fakeGo, fakeChild := writeFakeGoTools(t, tmp)
	host := writeFakeHostTools(t, tmp, true)
	runDir := filepath.Join(tmp, "run")
	goArgsPath := filepath.Join(tmp, "go-args.txt")
	childArgsPath := filepath.Join(tmp, "child-args.txt")

	cmd := exec.Command(filepath.Join(repo, "scripts", "vmtest-run"),
		"-json",
		"-count=99",
		"-run", "^TestTwoNode$",
		"-args",
		"-test.v",
		"literal",
		"./internal/vmtest/scenarios",
	)
	cmd.Dir = repo
	cmd.Env = appendHostEnv(os.Environ(), host,
		"KATL_VMTEST_GO="+fakeGo,
		"KATL_FAKE_GO_ARGS="+goArgsPath,
		"KATL_FAKE_CHILD="+fakeChild,
		"KATL_FAKE_CHILD_ARGS="+childArgsPath,
		"KATL_FAKE_CHILD_ENV="+filepath.Join(tmp, "child-env.txt"),
		"KATL_VMTEST_RUN_ID=run-preserve-args",
		"KATL_VMTEST_RUN_DIR="+runDir,
		"TMPDIR="+tmp,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("vmtest-run failed: %v\n%s", err, output)
	}

	goArgs := readLines(t, goArgsPath)
	wantGoArgs := []string{
		"test",
		"-exec",
		filepath.Join(repo, "scripts", "vmtest-exec"),
		"-timeout",
		"90m",
		"-json",
		"-count=99",
		"-run",
		"^TestTwoNode$",
		"-args",
		"-test.v",
		"literal",
		"./internal/vmtest/scenarios",
	}
	if !reflect.DeepEqual(goArgs, wantGoArgs) {
		t.Fatalf("go args = %#v, want %#v", goArgs, wantGoArgs)
	}
	if _, err := os.Stat(filepath.Join(runDir, "summary.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("summary.json exists unexpectedly: %v", err)
	}
	if _, err := os.Stat(filepath.Join(runDir, "go-test.log")); err != nil {
		t.Fatalf("go-test.log missing: %v", err)
	}
}

func TestVMTestExecRequiresManifest(t *testing.T) {
	repo := scriptTestRepoRoot(t)
	cmd := exec.Command(filepath.Join(repo, "scripts", "vmtest-exec"), filepath.Join(repo, "scripts", "vmtest-run"))
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "KATL_VMTEST_WORLD_MANIFEST=", "KATL_VMTEST_RUN_DIR=")

	output, err := cmd.CombinedOutput()
	if exitCode(err) != 2 {
		t.Fatalf("vmtest-exec exit = %v, want 2\n%s", err, output)
	}
	if !strings.Contains(string(output), "scripts/vmtest-run") {
		t.Fatalf("output missing runner hint:\n%s", output)
	}
}

func TestVMTestRunHelpDoesNotCreateRunDir(t *testing.T) {
	repo := scriptTestRepoRoot(t)
	tmp := t.TempDir()
	runDir := filepath.Join(tmp, "run")
	fakeGo := filepath.Join(tmp, "go")
	writeExecutable(t, fakeGo, "#!/usr/bin/env bash\nexit 99\n")

	cmd := exec.Command(filepath.Join(repo, "scripts", "vmtest-run"), "--help")
	cmd.Dir = repo
	cmd.Env = append(os.Environ(),
		"KATL_VMTEST_GO="+fakeGo,
		"KATL_VMTEST_RUN_DIR="+runDir,
		"TMPDIR="+tmp,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("vmtest-run --help failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "usage: scripts/vmtest-run") {
		t.Fatalf("help output missing usage:\n%s", output)
	}
	if !strings.Contains(string(output), "--no-rebuild") {
		t.Fatalf("help output missing runner options:\n%s", output)
	}
	if _, err := os.Stat(runDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("run dir exists after --help: %v", err)
	}
}

func TestVMTestRunRecordsLibvirtHostGapsAndExecsGo(t *testing.T) {
	tests := []struct {
		name       string
		extraEnv   []string
		capability string
		notMissing []string
	}{
		{
			name:       "connection",
			extraEnv:   []string{"KATL_VMTEST_VIRSH=/tmp/missing-virsh"},
			capability: "libvirt",
			notMissing: []string{"libvirt-network", "libvirt-storage-pool"},
		},
		{
			name:       "network",
			extraEnv:   []string{"KATL_FAKE_VIRSH_NET_INACTIVE=1"},
			capability: "libvirt-network",
		},
		{
			name:       "storage pool",
			extraEnv:   []string{"KATL_FAKE_VIRSH_POOL_INACTIVE=1"},
			capability: "libvirt-storage-pool",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := scriptTestRepoRoot(t)
			tmp := t.TempDir()
			fakeGo, fakeChild := writeFakeGoTools(t, tmp)
			host := writeFakeHostTools(t, tmp, true)
			runDir := filepath.Join(tmp, "run")
			goArgsPath := filepath.Join(tmp, "go-args.txt")

			env := appendHostEnv(os.Environ(), host,
				"KATL_VMTEST_GO="+fakeGo,
				"KATL_FAKE_GO_ARGS="+goArgsPath,
				"KATL_FAKE_CHILD="+fakeChild,
				"KATL_FAKE_CHILD_ARGS="+filepath.Join(tmp, "child-args.txt"),
				"KATL_FAKE_CHILD_ENV="+filepath.Join(tmp, "child-env.txt"),
				"KATL_VMTEST_RUN_ID=run-host-gap-"+tt.name,
				"KATL_VMTEST_RUN_DIR="+runDir,
				"TMPDIR="+tmp,
			)
			env = append(env, tt.extraEnv...)
			cmd := exec.Command(filepath.Join(repo, "scripts", "vmtest-run"), "./internal/vmtest", "-run", "NeedsLibvirt")
			cmd.Dir = repo
			cmd.Env = env
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("vmtest-run failed: %v\n%s", err, output)
			}
			if !strings.Contains(string(output), "vmtest world has missing host capabilities") {
				t.Fatalf("output missing host gap heading:\n%s", output)
			}
			if !strings.Contains(string(output), "  - "+tt.capability+":") {
				t.Fatalf("output missing %s diagnostic:\n%s", tt.capability, output)
			}
			goArgs := readLines(t, goArgsPath)
			if !reflect.DeepEqual(goArgs, []string{
				"test",
				"-exec",
				filepath.Join(repo, "scripts", "vmtest-exec"),
				"-timeout",
				"90m",
				"./internal/vmtest",
				"-run",
				"NeedsLibvirt",
			}) {
				t.Fatalf("go args = %#v", goArgs)
			}
			world, err := LoadWorld(filepath.Join(runDir, "world.json"))
			if err != nil {
				t.Fatalf("LoadWorld() error = %v", err)
			}
			if world.Capabilities[tt.capability] != WorldStatusFailed {
				t.Fatalf("%s capability = %q", tt.capability, world.Capabilities[tt.capability])
			}
			caps := readCapabilities(t, filepath.Join(runDir, "host-capabilities.json"))
			if !contains(caps.Missing, tt.capability) {
				t.Fatalf("missing capabilities = %#v", caps.Missing)
			}
			for _, unexpected := range tt.notMissing {
				if contains(caps.Missing, unexpected) {
					t.Fatalf("missing capabilities include dependent %s: %#v", unexpected, caps.Missing)
				}
			}
			runIndex := readRunIndex(t, filepath.Join(runDir, "run.json"))
			if !contains(runIndex.MissingCapabilities, tt.capability) || runIndex.Status != "passed" {
				t.Fatalf("run index = %#v", runIndex)
			}
			for _, unexpected := range tt.notMissing {
				if contains(runIndex.MissingCapabilities, unexpected) {
					t.Fatalf("run index includes dependent %s: %#v", unexpected, runIndex.MissingCapabilities)
				}
			}
			if _, err := os.Stat(filepath.Join(runDir, "summary.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("summary.json exists unexpectedly: %v", err)
			}
		})
	}
}

func TestVMTestRunDoesNotDefaultFlagOnlyArgs(t *testing.T) {
	repo := scriptTestRepoRoot(t)
	tmp := t.TempDir()
	fakeGo, fakeChild := writeFakeGoTools(t, tmp)
	host := writeFakeHostTools(t, tmp, true)
	runDir := filepath.Join(tmp, "run")
	goArgsPath := filepath.Join(tmp, "go-args.txt")

	cmd := exec.Command(filepath.Join(repo, "scripts", "vmtest-run"), "-run", "^TestDoesNotNeedLibvirt$", "-timeout", "2m")
	cmd.Dir = repo
	cmd.Env = appendHostEnv(os.Environ(), host,
		"KATL_VMTEST_GO="+fakeGo,
		"KATL_FAKE_GO_ARGS="+goArgsPath,
		"KATL_FAKE_CHILD="+fakeChild,
		"KATL_FAKE_CHILD_ARGS="+filepath.Join(tmp, "child-args.txt"),
		"KATL_FAKE_CHILD_ENV="+filepath.Join(tmp, "child-env.txt"),
		"KATL_VMTEST_RUN_ID=run-flag-only",
		"KATL_VMTEST_RUN_DIR="+runDir,
		"TMPDIR="+tmp,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("vmtest-run failed: %v\n%s", err, output)
	}
	goArgs := readLines(t, goArgsPath)
	if !reflect.DeepEqual(goArgs, []string{
		"test",
		"-exec",
		filepath.Join(repo, "scripts", "vmtest-exec"),
		"-run",
		"^TestDoesNotNeedLibvirt$",
		"-timeout",
		"2m",
	}) {
		t.Fatalf("go args = %#v", goArgs)
	}
}

func TestVMTestRunFailsWhenNoScenarioResultIsWritten(t *testing.T) {
	repo := scriptTestRepoRoot(t)
	tmp := t.TempDir()
	fakeGo, fakeChild := writeFakeGoTools(t, tmp)
	host := writeFakeHostTools(t, tmp, true)
	runDir := filepath.Join(tmp, "run")

	cmd := exec.Command(filepath.Join(repo, "scripts", "vmtest-run"), "./internal/vmtest", "-run", "^TestUnitOnly$")
	cmd.Dir = repo
	cmd.Env = appendHostEnv(os.Environ(), host,
		"KATL_VMTEST_GO="+fakeGo,
		"KATL_FAKE_GO_ARGS="+filepath.Join(tmp, "go-args.txt"),
		"KATL_FAKE_CHILD="+fakeChild,
		"KATL_FAKE_CHILD_ARGS="+filepath.Join(tmp, "child-args.txt"),
		"KATL_FAKE_CHILD_ENV="+filepath.Join(tmp, "child-env.txt"),
		"KATL_FAKE_CHILD_WORLD_SCENARIO=",
		"KATL_VMTEST_RUN_ID=run-no-scenario",
		"KATL_VMTEST_RUN_DIR="+runDir,
		"TMPDIR="+tmp,
	)
	output, err := cmd.CombinedOutput()
	if exitCode(err) != 3 {
		t.Fatalf("vmtest-run exit = %v, want 3\n%s", err, output)
	}
	if !strings.Contains(string(output), "did not execute any world scenario") {
		t.Fatalf("output missing no-scenario diagnostic:\n%s", output)
	}
	runIndex := readRunIndex(t, filepath.Join(runDir, "run.json"))
	if runIndex.Status != "no-scenario-result" {
		t.Fatalf("run index status = %q", runIndex.Status)
	}
}

func TestVMTestRunRemovesPassedRunDirByDefault(t *testing.T) {
	repo := scriptTestRepoRoot(t)
	tmp := t.TempDir()
	fakeGo, fakeChild := writeFakeGoTools(t, tmp)
	host := writeFakeHostTools(t, tmp, true)
	runDir := filepath.Join(tmp, "run-default-cleanup")

	env := appendHostEnv(os.Environ(), host,
		"KATL_VMTEST_GO="+fakeGo,
		"KATL_FAKE_GO_ARGS="+filepath.Join(tmp, "go-args.txt"),
		"KATL_FAKE_CHILD="+fakeChild,
		"KATL_FAKE_CHILD_ARGS="+filepath.Join(tmp, "child-args.txt"),
		"KATL_FAKE_CHILD_ENV="+filepath.Join(tmp, "child-env.txt"),
		"KATL_VMTEST_RUN_ID=run-default-cleanup",
		"KATL_VMTEST_RUN_DIR="+runDir,
		"TMPDIR="+tmp,
	)
	env = removeEnv(env, "KATL_VMTEST_KEEP")
	cmd := exec.Command(filepath.Join(repo, "scripts", "vmtest-run"), "./internal/vmtest")
	cmd.Dir = repo
	cmd.Env = env
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("vmtest-run failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "vmtest run dir removed: "+runDir) {
		t.Fatalf("output missing cleanup message:\n%s", output)
	}
	if _, err := os.Stat(runDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("run dir exists after passed cleanup: %v", err)
	}
}

func TestVMTestRunKeepNeverRemovesFailedRunDir(t *testing.T) {
	repo := scriptTestRepoRoot(t)
	tmp := t.TempDir()
	fakeGo, fakeChild := writeFakeGoTools(t, tmp)
	host := writeFakeHostTools(t, tmp, true)
	runDir := filepath.Join(tmp, "run-failed-cleanup")

	cmd := exec.Command(filepath.Join(repo, "scripts", "vmtest-run"), "./internal/vmtest")
	cmd.Dir = repo
	cmd.Env = appendHostEnv(os.Environ(), host,
		"KATL_VMTEST_GO="+fakeGo,
		"KATL_FAKE_GO_ARGS="+filepath.Join(tmp, "go-args.txt"),
		"KATL_FAKE_CHILD="+fakeChild,
		"KATL_FAKE_CHILD_ARGS="+filepath.Join(tmp, "child-args.txt"),
		"KATL_FAKE_CHILD_ENV="+filepath.Join(tmp, "child-env.txt"),
		"KATL_FAKE_CHILD_EXIT=7",
		"KATL_VMTEST_KEEP=never",
		"KATL_VMTEST_RUN_ID=run-failed-cleanup",
		"KATL_VMTEST_RUN_DIR="+runDir,
		"TMPDIR="+tmp,
	)
	output, err := cmd.CombinedOutput()
	if exitCode(err) != 7 {
		t.Fatalf("vmtest-run exit = %v, want 7\n%s", err, output)
	}
	if !strings.Contains(string(output), "vmtest run dir removed: "+runDir) {
		t.Fatalf("output missing cleanup message:\n%s", output)
	}
	if _, err := os.Stat(runDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("run dir exists after failed cleanup: %v", err)
	}
}

func TestVMTestRunKeepsFailedRunDirByDefault(t *testing.T) {
	repo := scriptTestRepoRoot(t)
	tmp := t.TempDir()
	fakeGo, fakeChild := writeFakeGoTools(t, tmp)
	host := writeFakeHostTools(t, tmp, true)
	runDir := filepath.Join(tmp, "run-failed-kept")

	env := appendHostEnv(os.Environ(), host,
		"KATL_VMTEST_GO="+fakeGo,
		"KATL_FAKE_GO_ARGS="+filepath.Join(tmp, "go-args.txt"),
		"KATL_FAKE_CHILD="+fakeChild,
		"KATL_FAKE_CHILD_ARGS="+filepath.Join(tmp, "child-args.txt"),
		"KATL_FAKE_CHILD_ENV="+filepath.Join(tmp, "child-env.txt"),
		"KATL_FAKE_CHILD_EXIT=7",
		"KATL_VMTEST_RUN_ID=run-failed-kept",
		"KATL_VMTEST_RUN_DIR="+runDir,
		"TMPDIR="+tmp,
	)
	env = removeEnv(env, "KATL_VMTEST_KEEP")
	cmd := exec.Command(filepath.Join(repo, "scripts", "vmtest-run"), "./internal/vmtest")
	cmd.Dir = repo
	cmd.Env = env
	output, err := cmd.CombinedOutput()
	if exitCode(err) != 7 {
		t.Fatalf("vmtest-run exit = %v, want 7\n%s", err, output)
	}
	runIndex := readRunIndex(t, filepath.Join(runDir, "run.json"))
	if runIndex.Status != "failed" {
		t.Fatalf("run index status = %q", runIndex.Status)
	}
}

func TestVMTestRunPrintsPreservedDebugTargets(t *testing.T) {
	repo := scriptTestRepoRoot(t)
	tmp := t.TempDir()
	fakeGo, fakeChild := writeFakeGoTools(t, tmp)
	host := writeFakeHostTools(t, tmp, true)
	runDir := filepath.Join(tmp, "run-debug-output")

	cmd := exec.Command(filepath.Join(repo, "scripts", "vmtest-run"), "./internal/vmtest")
	cmd.Dir = repo
	cmd.Env = appendHostEnv(os.Environ(), host,
		"KATL_VMTEST_GO="+fakeGo,
		"KATL_FAKE_GO_ARGS="+filepath.Join(tmp, "go-args.txt"),
		"KATL_FAKE_CHILD="+fakeChild,
		"KATL_FAKE_CHILD_ARGS="+filepath.Join(tmp, "child-args.txt"),
		"KATL_FAKE_CHILD_ENV="+filepath.Join(tmp, "child-env.txt"),
		"KATL_FAKE_CHILD_EXIT=7",
		"KATL_FAKE_CHILD_WORLD_RESULT=failed",
		"KATL_FAKE_CHILD_WORLD_RESULT_LAYOUT=node",
		"KATL_FAKE_CHILD_WORLD_DEBUG_PRESERVED=1",
		"KATL_VMTEST_RUN_ID=run-debug-output",
		"KATL_VMTEST_RUN_DIR="+runDir,
		"TMPDIR="+tmp,
	)
	output, err := cmd.CombinedOutput()
	if exitCode(err) != 7 {
		t.Fatalf("vmtest-run exit = %v, want 7\n%s", err, output)
	}
	for _, want := range []string{
		"preserved VM debug targets",
		"domain: katl-debug-run",
		"console: 'virsh' '-c' 'qemu:///system' 'console' 'katl-debug-run' '--force'",
		"serial log: /tmp/katl-debug/serial.log",
		"vsock: cid=2048 port=10240",
		"cleanup: 'scripts/vmtest-clean' '/tmp/katl-debug/result.json'",
		"Run the cleanup command when you are done debugging.",
	} {
		if !strings.Contains(string(output), want) {
			t.Fatalf("output missing %q:\n%s", want, output)
		}
	}
	artifact := readFile(t, filepath.Join(runDir, "preserved-vm-debug.txt"))
	if !strings.Contains(artifact, "katl-debug-run") || !strings.Contains(artifact, "vmtest-clean") || !strings.Contains(artifact, "nodes/cp-1/result.json") {
		t.Fatalf("debug artifact missing target info:\n%s", artifact)
	}
}

func TestVMTestDebugPrintsLiveTargets(t *testing.T) {
	repo := scriptTestRepoRoot(t)
	tmp := t.TempDir()
	result := filepath.Join(tmp, "run", "result.json")
	serial := filepath.Join(tmp, "run", "vm", "runtime-serial.log")
	if err := os.MkdirAll(filepath.Dir(serial), 0o755); err != nil {
		t.Fatalf("MkdirAll(serial dir) error = %v", err)
	}
	jq := exec.Command("jq", "-n",
		"--arg", "serial", serial,
		`{
		  status: "passed",
		  domainName: "katl-live-debug",
		  artifacts: {runtimeSerial: $serial},
		  vsock: {enabled: true, guestCid: 2048, port: 10240},
		  debug: {onFailure: true, shell: true}
		}`,
	)
	data, err := jq.Output()
	if err != nil {
		t.Fatalf("jq result fixture: %v", err)
	}
	if err := os.WriteFile(result, data, 0o644); err != nil {
		t.Fatalf("WriteFile(result) error = %v", err)
	}

	cmd := exec.Command(filepath.Join(repo, "scripts", "vmtest-debug"), filepath.Dir(result))
	cmd.Dir = repo
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("vmtest-debug failed: %v\n%s", err, output)
	}
	for _, want := range []string{
		"vmtest debug targets",
		"domain: katl-live-debug",
		"reason: vmtest domain recorded in result; it may no longer be running",
		"source: result",
		"serial tail: 'tail' '-f' '" + serial + "'",
		"domstate: 'virsh' '-c' 'qemu:///system' 'domstate' 'katl-live-debug'",
		"console (invasive): 'virsh' '-c' 'qemu:///system' 'console' 'katl-live-debug' '--force'",
		"vsock: cid=2048 port=10240",
		"shell: serial-root",
		"cleanup after preservation: 'scripts/vmtest-clean' '" + result + "'",
	} {
		if !strings.Contains(string(output), want) {
			t.Fatalf("output missing %q:\n%s", want, output)
		}
	}
}

func TestVMTestCleanDestroysRecordedDomains(t *testing.T) {
	repo := scriptTestRepoRoot(t)
	tmp := t.TempDir()
	result := filepath.Join(tmp, "run", "result.json")
	if err := os.MkdirAll(filepath.Dir(result), 0o755); err != nil {
		t.Fatalf("MkdirAll(result dir) error = %v", err)
	}
	if err := os.WriteFile(result, []byte(`{
  "debug": {
    "targets": [{
      "preserved": true,
      "domainName": "katl-clean-me",
      "libvirtURI": "qemu:///system"
    }]
  }
}`), 0o644); err != nil {
		t.Fatalf("WriteFile(result) error = %v", err)
	}
	virshLog := filepath.Join(tmp, "virsh.log")
	virsh := filepath.Join(tmp, "virsh")
	writeExecutable(t, virsh, `#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$KATL_FAKE_VIRSH_LOG"
exit 0
`)

	cmd := exec.Command(filepath.Join(repo, "scripts", "vmtest-clean"), filepath.Dir(result))
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "KATL_VMTEST_VIRSH="+virsh, "KATL_FAKE_VIRSH_LOG="+virshLog)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("vmtest-clean failed: %v\n%s", err, output)
	}
	log := readFile(t, virshLog)
	for _, want := range []string{
		"-c qemu:///system destroy katl-clean-me",
		"-c qemu:///system undefine katl-clean-me --nvram",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("virsh log missing %q:\n%s", want, log)
		}
	}
}

func TestVMTestCleanKillsOrphanQEMUProcess(t *testing.T) {
	repo := scriptTestRepoRoot(t)
	tmp := t.TempDir()
	result := filepath.Join(tmp, "result.json")
	if err := os.WriteFile(result, []byte(`{
  "debug": {
    "targets": [{
      "preserved": true,
      "domainName": "katl-orphaned-domain",
      "libvirtURI": "qemu:///system"
    }]
  }
}`), 0o644); err != nil {
		t.Fatalf("WriteFile(result) error = %v", err)
	}
	virsh := filepath.Join(tmp, "virsh")
	writeExecutable(t, virsh, `#!/usr/bin/env bash
exit 1
`)
	ps := filepath.Join(tmp, "ps")
	writeExecutable(t, ps, `#!/usr/bin/env bash
printf '123 /run/libvirt/nix-emulators/qemu-system-x86_64 -name guest=katl-orphaned-domain,debug-threads=on\n'
printf '456 /run/libvirt/nix-emulators/qemu-system-x86_64 -name guest=katl-other-domain,debug-threads=on\n'
`)
	killLog := filepath.Join(tmp, "kill.log")
	kill := filepath.Join(tmp, "kill")
	writeExecutable(t, kill, `#!/usr/bin/env bash
printf '%s\n' "$*" >> "$KATL_FAKE_KILL_LOG"
exit 0
`)

	cmd := exec.Command(filepath.Join(repo, "scripts", "vmtest-clean"), result)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(),
		"KATL_VMTEST_VIRSH="+virsh,
		"KATL_VMTEST_PS="+ps,
		"KATL_VMTEST_KILL="+kill,
		"KATL_FAKE_KILL_LOG="+killLog,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("vmtest-clean failed: %v\n%s", err, output)
	}
	log := readFile(t, killLog)
	if strings.TrimSpace(log) != "123" {
		t.Fatalf("kill log = %q, want only orphan pid 123", log)
	}
	if !strings.Contains(string(output), "checking for orphan qemu process") {
		t.Fatalf("output missing orphan fallback diagnostic:\n%s", output)
	}
}

func TestVMTestCleanFailsWhenOrphanQEMUKillFails(t *testing.T) {
	repo := scriptTestRepoRoot(t)
	tmp := t.TempDir()
	result := filepath.Join(tmp, "result.json")
	if err := os.WriteFile(result, []byte(`{
  "debug": {
    "targets": [{
      "preserved": true,
      "domainName": "katl-stuck-domain",
      "libvirtURI": "qemu:///system"
    }]
  }
}`), 0o644); err != nil {
		t.Fatalf("WriteFile(result) error = %v", err)
	}
	virsh := filepath.Join(tmp, "virsh")
	writeExecutable(t, virsh, `#!/usr/bin/env bash
exit 1
`)
	ps := filepath.Join(tmp, "ps")
	writeExecutable(t, ps, `#!/usr/bin/env bash
printf '123 /run/libvirt/nix-emulators/qemu-system-x86_64 -name guest=katl-stuck-domain,debug-threads=on\n'
`)
	kill := filepath.Join(tmp, "kill")
	writeExecutable(t, kill, `#!/usr/bin/env bash
exit 1
`)
	sudo := filepath.Join(tmp, "sudo")
	writeExecutable(t, sudo, `#!/usr/bin/env bash
exit 1
`)

	cmd := exec.Command(filepath.Join(repo, "scripts", "vmtest-clean"), result)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(),
		"KATL_VMTEST_VIRSH="+virsh,
		"KATL_VMTEST_PS="+ps,
		"KATL_VMTEST_KILL="+kill,
		"KATL_VMTEST_SUDO="+sudo,
	)
	output, err := cmd.CombinedOutput()
	if exitCode(err) == 0 {
		t.Fatalf("vmtest-clean unexpectedly succeeded:\n%s", output)
	}
	if !strings.Contains(string(output), "failed to kill orphan qemu pid 123") {
		t.Fatalf("output missing kill failure diagnostic:\n%s", output)
	}
}

func TestVMTestCleanRefusesUnsafeDomain(t *testing.T) {
	repo := scriptTestRepoRoot(t)
	tmp := t.TempDir()
	result := filepath.Join(tmp, "result.json")
	if err := os.WriteFile(result, []byte(`{
  "debug": {
    "targets": [{
      "preserved": true,
      "domainName": "../bad",
      "libvirtURI": "qemu:///system"
    }]
  }
}`), 0o644); err != nil {
		t.Fatalf("WriteFile(result) error = %v", err)
	}

	cmd := exec.Command(filepath.Join(repo, "scripts", "vmtest-clean"), result)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "KATL_VMTEST_VIRSH=/tmp/should-not-run")
	output, err := cmd.CombinedOutput()
	if exitCode(err) != 2 {
		t.Fatalf("vmtest-clean exit = %v, want 2\n%s", err, output)
	}
	if !strings.Contains(string(output), "refusing unsafe domain name") {
		t.Fatalf("output missing unsafe-domain diagnostic:\n%s", output)
	}
}

func TestVMTestRunAcceptsNestedWorldScenarioResult(t *testing.T) {
	repo := scriptTestRepoRoot(t)
	tmp := t.TempDir()
	fakeGo, fakeChild := writeFakeGoTools(t, tmp)
	host := writeFakeHostTools(t, tmp, true)
	runDir := filepath.Join(tmp, "run")

	cmd := exec.Command(filepath.Join(repo, "scripts", "vmtest-run"), "./internal/vmtest", "-run", "^TestWorldScenario$")
	cmd.Dir = repo
	cmd.Env = appendHostEnv(os.Environ(), host,
		"KATL_VMTEST_GO="+fakeGo,
		"KATL_FAKE_GO_ARGS="+filepath.Join(tmp, "go-args.txt"),
		"KATL_FAKE_CHILD="+fakeChild,
		"KATL_FAKE_CHILD_ARGS="+filepath.Join(tmp, "child-args.txt"),
		"KATL_FAKE_CHILD_ENV="+filepath.Join(tmp, "child-env.txt"),
		"KATL_FAKE_CHILD_WORLD_SCENARIO=world-smoke",
		"KATL_FAKE_CHILD_WORLD_RESULT_LAYOUT=nested",
		"KATL_VMTEST_RUN_ID=run-nested-result",
		"KATL_VMTEST_RUN_DIR="+runDir,
		"TMPDIR="+tmp,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("vmtest-run failed: %v\n%s", err, output)
	}
	if strings.Contains(string(output), "ok  \tfake/vmtest") {
		t.Fatalf("output included go test success instead of logging it:\n%s", output)
	}
	goLog := readFile(t, filepath.Join(runDir, "go-test.log"))
	if !strings.Contains(goLog, "ok  \tfake/vmtest") {
		t.Fatalf("go-test.log missing go test success:\n%s", goLog)
	}
}

func TestVMTestRunInvalidCIDRSetupFailed(t *testing.T) {
	repo := scriptTestRepoRoot(t)
	tmp := t.TempDir()
	fakeGo, fakeChild := writeFakeGoTools(t, tmp)
	host := writeFakeHostTools(t, tmp, true)
	runDir := filepath.Join(tmp, "run")
	goArgsPath := filepath.Join(tmp, "go-args.txt")

	cmd := exec.Command(filepath.Join(repo, "scripts", "vmtest-run"), "./internal/vmtest")
	cmd.Dir = repo
	cmd.Env = appendHostEnv(os.Environ(), host,
		"KATL_VMTEST_GO="+fakeGo,
		"KATL_FAKE_GO_ARGS="+goArgsPath,
		"KATL_FAKE_CHILD="+fakeChild,
		"KATL_FAKE_CHILD_ARGS="+filepath.Join(tmp, "child-args.txt"),
		"KATL_FAKE_CHILD_ENV="+filepath.Join(tmp, "child-env.txt"),
		"KATL_VMTEST_CIDR=10.77.0.0/33",
		"KATL_VMTEST_RUN_ID=run-invalid-cidr",
		"KATL_VMTEST_RUN_DIR="+runDir,
		"TMPDIR="+tmp,
	)
	output, err := cmd.CombinedOutput()
	if exitCode(err) != 2 {
		t.Fatalf("vmtest-run exit = %v, want 2\n%s", err, output)
	}
	if _, err := os.Stat(goArgsPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("go test ran for setup-failed world, stat err = %v", err)
	}
	runIndex := readRunIndex(t, filepath.Join(runDir, "run.json"))
	if runIndex.Status != "setup-failed" {
		t.Fatalf("run index status = %q, want setup-failed", runIndex.Status)
	}
	if !reflect.DeepEqual(runIndex.GoTestArgs, []string{"./internal/vmtest"}) {
		t.Fatalf("run index go test args = %#v", runIndex.GoTestArgs)
	}
	if len(runIndex.SetupFailures) != 1 || !strings.Contains(runIndex.SetupFailures[0], "invalid CIDR prefix") {
		t.Fatalf("run index setup failures = %#v", runIndex.SetupFailures)
	}
	if len(runIndex.MissingCapabilities) != 0 {
		t.Fatalf("run index missing capabilities = %#v", runIndex.MissingCapabilities)
	}
	if _, err := os.Stat(filepath.Join(runDir, "summary.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("summary.json exists unexpectedly: %v", err)
	}
}

func writeFakeGoTools(t *testing.T, dir string) (string, string) {
	t.Helper()
	fakeGo := filepath.Join(dir, "go")
	fakeChild := filepath.Join(dir, "fake-test-binary")
	writeExecutable(t, fakeGo, `#!/usr/bin/env bash
set -euo pipefail
if [[ -n "${KATL_FAKE_GO_PID:-}" ]]; then
    printf '%s\n' "$$" > "$KATL_FAKE_GO_PID"
fi
printf '%s\n' "$@" > "$KATL_FAKE_GO_ARGS"
args=("$@")
if [[ "${args[0]:-}" != "test" ]]; then
    echo "unexpected go command: $*" >&2
    exit 91
fi
exec_wrapper=""
for ((i = 0; i < ${#args[@]}; i++)); do
    if [[ "${args[$i]}" == "-exec" ]]; then
        exec_wrapper="${args[$((i + 1))]:-}"
    fi
done
if [[ -z "$exec_wrapper" ]]; then
    echo "missing -exec" >&2
    exit 92
fi
json_output=0
for arg in "${args[@]}"; do
    if [[ "$arg" == "-json" ]]; then
        json_output=1
    fi
done
if [[ "$json_output" == 1 ]]; then
    printf '{"Action":"run","Package":"fake/vmtest"}\n'
else
    printf '=== RUN   TestForwarded\n'
fi
set +e
"$exec_wrapper" "$KATL_FAKE_CHILD" "-test.run=^Forwarded$" "-test.v" "child-extra"
exit_code=$?
set -e
if [[ "$json_output" == 1 ]]; then
    if [[ "${KATL_FAKE_GO_JSON_MALFORMED:-0}" == "1" ]]; then
        printf '{not json\n'
        exit "$exit_code"
    fi
    if [[ "${KATL_FAKE_GO_JSON_NO_TERMINAL:-0}" == "1" ]]; then
        exit "$exit_code"
    fi
    json_action="${KATL_FAKE_GO_JSON_ACTION:-pass}"
    printf '{"Action":"%s","Package":"fake/vmtest"}\n' "$json_action"
    if [[ "${KATL_FAKE_GO_JSON_EXTRA_SKIP:-0}" == "1" ]]; then
        printf '{"Action":"skip","Package":"fake/vmtest","Test":"TestUnexpectedSkip"}\n'
    fi
else
    printf -- '--- PASS: TestForwarded (0.00s)\n'
    printf 'ok  \tfake/vmtest\t0.001s\n'
fi
exit "$exit_code"
`)
	writeExecutable(t, fakeChild, `#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$@" > "$KATL_FAKE_CHILD_ARGS"
{
    printf 'KATL_VMTEST_WORLD_MANIFEST=%s\n' "${KATL_VMTEST_WORLD_MANIFEST:-}"
    printf 'KATL_VMTEST_CACHE_DIR=%s\n' "${KATL_VMTEST_CACHE_DIR:-}"
    printf 'KATL_VMTEST_LIBVIRT_URI=%s\n' "${KATL_VMTEST_LIBVIRT_URI:-}"
    printf 'KATL_VMTEST_LIBVIRT_NETWORK=%s\n' "${KATL_VMTEST_LIBVIRT_NETWORK:-}"
    printf 'KATL_VMTEST_LIBVIRT_STORAGE_POOL=%s\n' "${KATL_VMTEST_LIBVIRT_STORAGE_POOL:-}"
    printf 'KATL_VMTEST_RUN=%s\n' "${KATL_VMTEST_RUN:-}"
    printf 'KATL_VMTEST_WORLD_STRICT=%s\n' "${KATL_VMTEST_WORLD_STRICT:-}"
    printf 'KATL_VMTEST_DEBUG_ON_FAILURE=%s\n' "${KATL_VMTEST_DEBUG_ON_FAILURE:-}"
    printf 'KATL_VMTEST_RESOURCE_MANIFEST=%s\n' "${KATL_VMTEST_RESOURCE_MANIFEST:-}"
} > "$KATL_FAKE_CHILD_ENV"
[[ -f "${KATL_VMTEST_WORLD_MANIFEST:-}" ]] || exit 41
if [[ -n "${KATL_FAKE_CHILD_WORLD_SCENARIO:-}" ]]; then
    scenario_name="$KATL_FAKE_CHILD_WORLD_SCENARIO"
    scenario_id="${KATL_FAKE_CHILD_WORLD_SCENARIO_ID:-fake-scenario}"
    scenario_dir="$(jq -r '.scenarioDir' "$KATL_VMTEST_WORLD_MANIFEST")/$scenario_id"
    scenario_run_dir="$scenario_dir"
    if [[ "${KATL_FAKE_CHILD_WORLD_RESULT_LAYOUT:-}" == "nested" ]]; then
        scenario_run_dir="$scenario_dir/vm-runs/fake-run"
    elif [[ "${KATL_FAKE_CHILD_WORLD_RESULT_LAYOUT:-}" == "node" ]]; then
        scenario_run_dir="$scenario_dir/vm-runs/fake-run/nodes/cp-1"
    fi
    run_id="$(jq -r '.runID' "$KATL_VMTEST_WORLD_MANIFEST")"
    mkdir -p "$scenario_run_dir"
    result_path="$scenario_run_dir/result.json"
    if [[ "${KATL_FAKE_CHILD_WORLD_MANIFEST:-}" == "malformed" ]]; then
        printf '{' > "$scenario_dir/scenario.json"
        exit "${KATL_FAKE_CHILD_EXIT:-0}"
    fi
    jq -n \
        --arg name "$scenario_name" \
        --arg id "$scenario_id" \
        --arg dir "$scenario_run_dir" \
        --arg runID "$run_id" \
        --arg resultPath "$result_path" \
        '{
          apiVersion: "katl.dev/v1alpha1",
          kind: "VMTestScenario",
          worldRunID: $runID,
          name: $name,
          id: $id,
          dir: $dir,
          resultPath: $resultPath
        }' > "$scenario_dir/scenario.json"
    case "${KATL_FAKE_CHILD_WORLD_RESULT:-passed}" in
        missing)
            ;;
        malformed)
            printf '{' > "$result_path"
            ;;
        *)
            result_run_id="${KATL_FAKE_CHILD_WORLD_RESULT_RUN_ID:-$run_id}"
            result_name="${KATL_FAKE_CHILD_WORLD_RESULT_SCENARIO:-$scenario_name}"
            result_status="${KATL_FAKE_CHILD_WORLD_RESULT:-passed}"
            if [[ -n "${KATL_FAKE_CHILD_WORLD_MISSING:-}" ]]; then
                missing_filter='| .missing = [{name: $missing, detail: "synthetic missing capability"}]'
            else
                missing_filter=''
            fi
            if [[ "${KATL_FAKE_CHILD_WORLD_DEBUG_PRESERVED:-0}" == "1" ]]; then
                debug_filter='| .debug = {
                    onFailure: true,
                    shell: true,
                    targets: [{
                        preserved: true,
                        domainName: "katl-debug-run",
                        libvirtURI: "qemu:///system",
                        serialLog: "/tmp/katl-debug/serial.log",
                        consoleCommand: "'\''virsh'\'' '\''-c'\'' '\''qemu:///system'\'' '\''console'\'' '\''katl-debug-run'\'' '\''--force'\''",
                        cleanupCommand: "'\''scripts/vmtest-clean'\'' '\''/tmp/katl-debug/result.json'\''",
                        shellMode: "serial-root",
                        vsock: {enabled: true, guestCid: 2048, port: 10240}
                    }]
                }'
            else
                debug_filter=''
            fi
            jq -n \
                --arg scenarioName "$result_name" \
                --arg status "$result_status" \
                --arg runID "$result_run_id" \
                --arg runDir "$scenario_dir" \
                --arg manifestPath "$scenario_dir/scenario.json" \
                --arg missing "${KATL_FAKE_CHILD_WORLD_MISSING:-}" \
                --arg resultPath "$result_path" \
                '{
                  apiVersion: "katl.dev/v1alpha1",
                  kind: "VMTestScenarioResult",
                  scenarioName: $scenarioName,
                  status: $status,
                  runId: $runID,
                  runDir: $runDir,
                  manifestPath: $manifestPath,
                  resultPath: $resultPath
                } '"$missing_filter"' '"$debug_filter" > "$result_path"
            ;;
    esac
fi
exit "${KATL_FAKE_CHILD_EXIT:-0}"
`)
	return fakeGo, fakeChild
}

type fakeHostTools struct {
	imageTool    string
	virsh        string
	ovmfCode     string
	ovmfVars     string
	kvm          string
	vsock        string
	kubectl      string
	resourceLock string
}

func writeFakeHostTools(t *testing.T, dir string, _ bool) fakeHostTools {
	t.Helper()
	tools := fakeHostTools{
		imageTool:    filepath.Join(dir, "qemu-img"),
		virsh:        filepath.Join(dir, "virsh"),
		ovmfCode:     filepath.Join(dir, "OVMF_CODE.fd"),
		ovmfVars:     filepath.Join(dir, "OVMF_VARS.fd"),
		kvm:          filepath.Join(dir, "kvm"),
		vsock:        filepath.Join(dir, "vhost-vsock"),
		kubectl:      filepath.Join(dir, "kubectl"),
		resourceLock: filepath.Join(dir, "katl-resource-lock"),
	}
	writeExecutable(t, tools.imageTool, "#!/usr/bin/env bash\nexit 0\n")
	writeExecutable(t, tools.virsh, `#!/usr/bin/env bash
set -euo pipefail
if [[ "${1:-}" == "-c" ]]; then
    shift 2
fi
case "${1:-}" in
    uri)
        printf 'qemu:///system\n'
        ;;
    net-info)
        if [[ "${KATL_FAKE_VIRSH_NET_INACTIVE:-0}" == "1" ]]; then
            printf 'Name: default\nActive: no\n'
        else
            printf 'Name: default\nActive: yes\n'
        fi
        ;;
    net-dumpxml)
        printf '<network><name>default</name><ip address="192.168.122.1" netmask="255.255.255.0"/></network>\n'
        ;;
    pool-info)
        if [[ "${KATL_FAKE_VIRSH_POOL_INACTIVE:-0}" == "1" ]]; then
            printf 'Name: default\nState: inactive\n'
        else
            printf 'Name: default\nState: running\n'
        fi
        ;;
    pool-dumpxml)
        printf '<pool><target><path>%s</path></target></pool>\n' "${KATL_FAKE_VIRSH_POOL_PATH:-/tmp/libvirt-images}"
        ;;
    *)
        echo "unexpected virsh args: $*" >&2
        exit 40
        ;;
esac
`)
	writeExecutable(t, filepath.Join(dir, "mformat"), "#!/usr/bin/env bash\nexit 0\n")
	writeExecutable(t, filepath.Join(dir, "mcopy"), "#!/usr/bin/env bash\nexit 0\n")
	writeExecutable(t, filepath.Join(dir, "truncate"), "#!/usr/bin/env bash\nexit 0\n")
	writeExecutable(t, filepath.Join(dir, "sfdisk"), "#!/usr/bin/env bash\nexit 0\n")
	writeExecutable(t, filepath.Join(dir, "sha256sum"), "#!/usr/bin/env bash\nprintf 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa  %s\\n' \"${1:-}\"\n")
	writeExecutable(t, filepath.Join(dir, "awk"), "#!/usr/bin/env bash\nexit 0\n")
	writeExecutable(t, filepath.Join(dir, "realpath"), "#!/usr/bin/env bash\nprintf '%s\\n' \"${@: -1}\"\n")
	writeExecutable(t, tools.kubectl, "#!/usr/bin/env bash\nexit 0\n")
	writeExecutable(t, tools.resourceLock, `#!/usr/bin/env bash
set -euo pipefail
if [[ "${1:-}" != "prepare-mkosi" ]]; then
    echo "unexpected katl-resource-lock command: $*" >&2
    exit 43
fi
manifest=""
lock=""
mode=""
run_id="resource-test"
git_revision="unknown"
while (($# > 0)); do
    case "$1" in
        -manifest)
            manifest="$2"
            shift 2
            ;;
        -lock)
            lock="$2"
            shift 2
            ;;
        -mode)
            mode="$2"
            shift 2
            ;;
        -run-id)
            run_id="$2"
            shift 2
            ;;
        -git-revision)
            git_revision="$2"
            shift 2
            ;;
        *)
            shift
            ;;
    esac
done
if [[ -z "$manifest" || "$mode" != "strict" ]]; then
    echo "invalid fake katl-resource-lock invocation" >&2
    exit 44
fi
mkdir -p "$(dirname "$manifest")"
jq -n \
    --arg runID "$run_id" \
    --arg gitRevision "$git_revision" \
    --arg lock "$lock" \
    '{
      apiVersion: "katl.dev/v1alpha1",
      kind: "ResourceTestManifest",
      runID: $runID,
      created: "2026-06-18T00:00:00Z",
      git: {revision: $gitRevision},
      tools: [{name: "mkosi", version: "fake"}],
      mkosiProfiles: [{name: "installer-image", path: "mkosi.profiles/installer-image", configSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", packageSetRef: "installer-image"}],
      packageSets: [{name: "installer-image", source: "mkosi.profiles/installer-image", lockSHA256: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", packages: [{name: "systemd", nevra: "systemd-1.x86_64"}]}],
      artifacts: [{name: "installer-uki", kind: "uki", path: "/tmp/katl-installer.efi", sha256: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", sizeBytes: 1}],
      fixtures: [{name: "package-lock", kind: "resource-package-lock", path: $lock}]
    }' > "$manifest"
printf 'manifest: %s\nmode: strict\nartifacts: 1\npackageSets: 1\nlockSHA256: %s\n' "$manifest" "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
`)
	if err := os.WriteFile(tools.ovmfCode, []byte("code"), 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", tools.ovmfCode, err)
	}
	if err := os.WriteFile(tools.ovmfVars, []byte("vars"), 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", tools.ovmfVars, err)
	}
	if err := os.WriteFile(tools.kvm, []byte{}, 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", tools.kvm, err)
	}
	if err := os.WriteFile(tools.vsock, []byte{}, 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", tools.vsock, err)
	}
	return tools
}

func appendHostEnv(env []string, tools fakeHostTools, extra ...string) []string {
	env = append(env,
		"KATL_VMTEST_IMAGE_TOOL="+tools.imageTool,
		"KATL_VMTEST_VIRSH="+tools.virsh,
		"KATL_VMTEST_LIBVIRT_URI=qemu:///system",
		"KATL_VMTEST_LIBVIRT_NETWORK=default",
		"KATL_VMTEST_LIBVIRT_STORAGE_POOL=default",
		"KATL_OVMF_CODE="+tools.ovmfCode,
		"KATL_OVMF_VARS="+tools.ovmfVars,
		"KATL_VMTEST_KVM_DEVICE="+tools.kvm,
		"KATL_VMTEST_VSOCK_DEVICE="+tools.vsock,
		"KATL_VMTEST_KUBECTL="+tools.kubectl,
		"KATL_VMTEST_RESOURCE_LOCK="+tools.resourceLock,
		"KATL_VMTEST_AUTO_REBUILD=0",
		"KATL_FAKE_CHILD_WORLD_SCENARIO=fake vm scenario",
		"KATL_VMTEST_KEEP=always",
		"PATH="+filepath.Dir(tools.imageTool)+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	return append(env, extra...)
}

func removeEnv(env []string, name string) []string {
	prefix := name + "="
	var kept []string
	for _, value := range env {
		if !strings.HasPrefix(value, prefix) {
			kept = append(kept, value)
		}
	}
	return kept
}

func copyScript(t *testing.T, src string, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", src, err)
	}
	if err := os.WriteFile(dst, data, 0o755); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", dst, err)
	}
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
}

func scriptTestRepoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("Abs(repo root) error = %v", err)
	}
	return root
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, err)
	}
	text := strings.TrimSuffix(string(data), "\n")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

func readKeyValues(t *testing.T, path string) map[string]string {
	t.Helper()
	values := make(map[string]string)
	for _, line := range readLines(t, path) {
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("malformed env line %q", line)
		}
		values[name] = value
	}
	return values
}

type vmtestHostCapabilities struct {
	Missing []string `json:"missing"`
}

type vmtestRunIndex struct {
	Kind                string               `json:"kind"`
	RunID               string               `json:"runID"`
	CacheDir            string               `json:"cacheDir"`
	WorldManifest       string               `json:"worldManifest"`
	HostCapabilities    string               `json:"hostCapabilities"`
	GoTestLog           string               `json:"goTestLog"`
	ResourceManifest    string               `json:"resourceManifest"`
	ResourceDigest      string               `json:"resourceManifestSHA256"`
	PackageLock         string               `json:"packageLock"`
	PackageLockDigest   string               `json:"packageLockSHA256"`
	AutoRebuild         bool                 `json:"autoRebuild"`
	ArtifactSet         string               `json:"artifactSet"`
	DebugOnFailure      bool                 `json:"debugOnFailure"`
	DebugShell          bool                 `json:"debugShell"`
	Status              string               `json:"status"`
	MissingCapabilities []string             `json:"missingCapabilities"`
	GoTestArgs          []string             `json:"goTestArgs"`
	SetupFailures       []string             `json:"setupFailures"`
	Artifacts           []WorldArtifact      `json:"vmtestArtifacts"`
	ArtifactInputs      *WorldArtifactInputs `json:"vmtestArtifactInputs"`
}

func readRunIndex(t *testing.T, path string) vmtestRunIndex {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, err)
	}
	var index vmtestRunIndex
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatalf("Unmarshal(%s) error = %v", path, err)
	}
	return index
}

func readCapabilities(t *testing.T, path string) vmtestHostCapabilities {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, err)
	}
	var capabilities vmtestHostCapabilities
	if err := json.Unmarshal(data, &capabilities); err != nil {
		t.Fatalf("Unmarshal(%s) error = %v", path, err)
	}
	return capabilities
}

func assertJSONEmptyObject(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, err)
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatalf("Unmarshal(%s) error = %v", path, err)
	}
	if len(value) != 0 {
		t.Fatalf("%s = %#v, want empty JSON object", path, value)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}
