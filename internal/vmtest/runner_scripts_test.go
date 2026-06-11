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
	runDir := filepath.Join(tmp, "run")
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
	cmd.Env = appendHostEnv(os.Environ(), host,
		"KATL_VMTEST_GO="+fakeGo,
		"KATL_FAKE_GO_ARGS="+goArgsPath,
		"KATL_FAKE_CHILD="+fakeChild,
		"KATL_FAKE_CHILD_ARGS="+childArgsPath,
		"KATL_FAKE_CHILD_ENV="+childEnvPath,
		"KATL_VMTEST_RUN_ID=run-1",
		"KATL_VMTEST_RUN_DIR="+runDir,
		"TMPDIR="+tmp,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("vmtest-run failed: %v\n%s", err, output)
	}
	if strings.Contains(string(output), `{"Action":"run"`) {
		t.Fatalf("vmtest-run emitted JSON without caller -json:\n%s", output)
	}

	world, err := LoadWorld(filepath.Join(runDir, "world.json"))
	if err != nil {
		t.Fatalf("LoadWorld() error = %v", err)
	}
	if world.RunID != "run-1" || world.RunDir != runDir {
		t.Fatalf("world = %#v", world)
	}
	if world.RunIndex != filepath.Join(runDir, "run.json") {
		t.Fatalf("world run index = %q", world.RunIndex)
	}
	if world.Libvirt.URI != "qemu:///system" || world.Libvirt.Network != "default" || world.Libvirt.StoragePool != "default" {
		t.Fatalf("world libvirt = %#v", world.Libvirt)
	}
	if world.Network.Backend != NetworkLibvirt || world.Network.Name != "default" || world.Network.CIDR != "192.168.122.0/24" || world.Network.Gateway != "192.168.122.1" || world.Network.LeaseFile != filepath.Join(runDir, "network", "leases.json") {
		t.Fatalf("world network = %#v", world.Network)
	}
	for _, capability := range []string{"image-tool", "libvirt", "libvirt-network", "libvirt-storage-pool", "ovmf", "kvm", "vsock", "mtools", "sfdisk", "sha256sum", "awk", "realpath", "kubectl", "systemd-nspawn"} {
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
	if childEnv["KATL_VMTEST_LIBVIRT_URI"] != "qemu:///system" || childEnv["KATL_VMTEST_LIBVIRT_NETWORK"] != "default" {
		t.Fatalf("child libvirt env = %#v", childEnv)
	}
	if childEnv["KATL_VMTEST_RUN"] != "1" || childEnv["KATL_VMTEST_WORLD_STRICT"] != "1" {
		t.Fatalf("child strict env = %#v", childEnv)
	}

	runIndex := readRunIndex(t, filepath.Join(runDir, "run.json"))
	if runIndex.Kind != "VMTestRun" || runIndex.Status != "go-test" {
		t.Fatalf("run index = %#v", runIndex)
	}
	if runIndex.RunID != "run-1" || runIndex.WorldManifest != filepath.Join(runDir, "world.json") || runIndex.HostCapabilities != filepath.Join(runDir, "host-capabilities.json") {
		t.Fatalf("run index paths = %#v", runIndex)
	}
	if !reflect.DeepEqual(runIndex.GoTestArgs, []string{"./internal/vmtest/scenarios", "-run", "^TestTwoNode$", "-count=99", "-timeout", "2m"}) {
		t.Fatalf("run index go test args = %#v", runIndex.GoTestArgs)
	}

	if _, err := os.Stat(filepath.Join(runDir, "summary.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("summary.json exists unexpectedly: %v", err)
	}
	if _, err := os.Stat(filepath.Join(runDir, "go-test.log")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("go-test.log exists unexpectedly: %v", err)
	}
	assertJSONEmptyObject(t, filepath.Join(runDir, "network", "leases.json"))
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
	if !reflect.DeepEqual(mkosiArgs, []string{"build-installer", "build-katlos-install-image"}) {
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
	if runIndex.Status != "go-test" {
		t.Fatalf("run index status = %q", runIndex.Status)
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
	if !strings.Contains(string(output), `{"Action":"run","Package":"fake/vmtest"}`) {
		t.Fatalf("output missing JSON events:\n%s", output)
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
	if _, err := os.Stat(filepath.Join(runDir, "go-test.log")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("go-test.log exists unexpectedly: %v", err)
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
			if !contains(runIndex.MissingCapabilities, tt.capability) || runIndex.Status != "go-test" {
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
	if !strings.Contains(string(output), "ok  \tfake/vmtest") {
		t.Fatalf("output missing go test success:\n%s", output)
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
    printf 'KATL_VMTEST_LIBVIRT_URI=%s\n' "${KATL_VMTEST_LIBVIRT_URI:-}"
    printf 'KATL_VMTEST_LIBVIRT_NETWORK=%s\n' "${KATL_VMTEST_LIBVIRT_NETWORK:-}"
    printf 'KATL_VMTEST_LIBVIRT_STORAGE_POOL=%s\n' "${KATL_VMTEST_LIBVIRT_STORAGE_POOL:-}"
    printf 'KATL_VMTEST_RUN=%s\n' "${KATL_VMTEST_RUN:-}"
    printf 'KATL_VMTEST_WORLD_STRICT=%s\n' "${KATL_VMTEST_WORLD_STRICT:-}"
} > "$KATL_FAKE_CHILD_ENV"
[[ -f "${KATL_VMTEST_WORLD_MANIFEST:-}" ]] || exit 41
if [[ -n "${KATL_FAKE_CHILD_WORLD_SCENARIO:-}" ]]; then
    scenario_name="$KATL_FAKE_CHILD_WORLD_SCENARIO"
    scenario_id="${KATL_FAKE_CHILD_WORLD_SCENARIO_ID:-fake-scenario}"
    scenario_dir="$(jq -r '.scenarioDir' "$KATL_VMTEST_WORLD_MANIFEST")/$scenario_id"
    scenario_run_dir="$scenario_dir"
    if [[ "${KATL_FAKE_CHILD_WORLD_RESULT_LAYOUT:-}" == "nested" ]]; then
        scenario_run_dir="$scenario_dir/vm-runs/fake-run"
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
                } '"$missing_filter" > "$result_path"
            ;;
    esac
fi
exit "${KATL_FAKE_CHILD_EXIT:-0}"
`)
	return fakeGo, fakeChild
}

type fakeHostTools struct {
	imageTool string
	virsh     string
	ovmfCode  string
	ovmfVars  string
	kvm       string
	vsock     string
	kubectl   string
}

func writeFakeHostTools(t *testing.T, dir string, _ bool) fakeHostTools {
	t.Helper()
	tools := fakeHostTools{
		imageTool: filepath.Join(dir, "qemu-img"),
		virsh:     filepath.Join(dir, "virsh"),
		ovmfCode:  filepath.Join(dir, "OVMF_CODE.fd"),
		ovmfVars:  filepath.Join(dir, "OVMF_VARS.fd"),
		kvm:       filepath.Join(dir, "kvm"),
		vsock:     filepath.Join(dir, "vhost-vsock"),
		kubectl:   filepath.Join(dir, "kubectl"),
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
	writeExecutable(t, filepath.Join(dir, "systemd-nspawn"), "#!/usr/bin/env bash\nexit 0\n")
	writeExecutable(t, tools.kubectl, "#!/usr/bin/env bash\nexit 0\n")
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
		"KATL_MKOSI_ARTIFACT_INDEX="+filepath.Join(filepath.Dir(tools.imageTool), "prebuilt-artifacts.json"),
		"KATL_NSPAWN_ALLOW_UNPRIVILEGED=1",
		"KATL_FAKE_CHILD_WORLD_SCENARIO=fake vm scenario",
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
	Kind                string   `json:"kind"`
	RunID               string   `json:"runID"`
	WorldManifest       string   `json:"worldManifest"`
	HostCapabilities    string   `json:"hostCapabilities"`
	Status              string   `json:"status"`
	MissingCapabilities []string `json:"missingCapabilities"`
	GoTestArgs          []string `json:"goTestArgs"`
	SetupFailures       []string `json:"setupFailures"`
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
