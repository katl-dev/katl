package scriptstest

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/workstationtest"
)

func TestVMTestArtifactOwnership(t *testing.T) {
	for _, tc := range []struct {
		name, directory, override, artifactSet string
		rejected                               bool
	}{
		{name: "default"},
		{name: "canonical", directory: "canonical"},
		{name: "relative canonical", directory: "_build/mkosi"},
		{name: "canonical alias", directory: "alias"},
		{name: "different build root", directory: "alternate", rejected: true},
		{name: "runtime build root", directory: "alternate", artifactSet: "runtime", rejected: true},
		{name: "installer output", override: "KATL_INSTALLER_ISO", rejected: true},
		{name: "runtime boot output", override: "KATL_RUNTIME_UKI", rejected: true},
		{name: "runtime index", artifactSet: "runtime", override: "KATL_MKOSI_ARTIFACT_INDEX", rejected: true},
		{name: "package evidence", override: "KATL_RUNTIME_PACKAGE_SET", rejected: true},
		{name: "install payload", override: "KATL_KATLOS_IMAGE", rejected: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			canonical := filepath.Join(root, "_build/mkosi")
			if err := os.MkdirAll(canonical, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(canonical, "katl-runtime-root.squashfs"), []byte("stale"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o755); err != nil {
				t.Fatal(err)
			}
			writeExecutable(t, filepath.Join(root, "scripts/mkosi"), `#!/usr/bin/env bash
if [[ "$1" == builder-version ]]; then echo 26; exit 0; fi
printf 'build\n' >> "$KATL_SCRIPTTEST_LOG"
output="${KATL_MKOSI_BUILD_DIR:-$KATL_REPO_ROOT/_build/mkosi}"
mkdir -p "$output"
printf current > "$output/katl-runtime-root.squashfs"
`)
			if err := os.Symlink("mkosi", filepath.Join(root, "scripts/build-kubernetes-sysext")); err != nil {
				t.Fatal(err)
			}
			writeExecutable(t, filepath.Join(root, "resource-lock"), `#!/usr/bin/env bash
printf 'record ' >> "$KATL_SCRIPTTEST_LOG"
cat "$KATL_REPO_ROOT/_build/mkosi/katl-runtime-root.squashfs" >> "$KATL_SCRIPTTEST_LOG"
printf '\n' >> "$KATL_SCRIPTTEST_LOG"
exit 17
`)
			directory := tc.directory
			switch directory {
			case "canonical":
				directory = canonical
			case "alternate":
				directory = filepath.Join(root, "alternate")
			case "alias":
				directory = filepath.Join(root, "alias")
				if err := os.Symlink(canonical, directory); err != nil {
					t.Fatal(err)
				}
			}
			artifactSet := tc.artifactSet
			if artifactSet == "" {
				artifactSet = "install"
			}
			cmd := exec.Command(filepath.Join(repoRoot(t), "scripts/vmtest-run"), "--artifact-set="+artifactSet, "./internal/vmtest", "-run", "TestFixture")
			cmd.Dir = root
			cmd.Env = append(os.Environ(),
				"KATL_REPO_ROOT="+root,
				"KATL_MKOSI_BUILD_DIR="+directory,
				"KATL_VMTEST_RESOURCE_LOCK="+filepath.Join(root, "resource-lock"),
				"KATL_VMTEST_RUN_DIR="+filepath.Join(root, "run"),
				"KATL_VMTEST_CACHE_DIR="+filepath.Join(root, "cache"),
				"KATL_SCRIPTTEST_LOG="+filepath.Join(root, "commands.log"),
			)
			if tc.override != "" {
				cmd.Env = append(cmd.Env, tc.override+"="+filepath.Join(root, "alternate-output"))
			}
			output, err := cmd.CombinedOutput()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
				t.Fatalf("exit = %v, want setup failure: %s", err, output)
			}
			log, logErr := os.ReadFile(filepath.Join(root, "commands.log"))
			if tc.rejected {
				if !os.IsNotExist(logErr) {
					t.Fatalf("unsupported input reached build or provenance recording: %q, %v", log, logErr)
				}
				if !strings.Contains(string(output), "not supported") && !strings.Contains(string(output), "requires the canonical") {
					t.Fatalf("missing input diagnostic: %s", output)
				}
			} else if logErr != nil || !strings.HasSuffix(string(log), "record current\n") {
				t.Fatalf("canonical build and provenance did not agree: %q, %v: %s", log, logErr, output)
			}
		})
	}
}

func TestVMTestRunLeavesDomainsAlone(t *testing.T) {
	repo := repoRoot(t)
	tmp := workstationtest.WorktreeTempDir(t, repo)
	logPath := filepath.Join(tmp, "commands.log")
	binDir := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	writeExecutable(t, filepath.Join(binDir, "fake-go"), `#!/usr/bin/env bash
printf 'go %s\n' "$*" >>"$KATL_SCRIPTTEST_LOG"
printf -- '--- PASS: TestDoesNotMatter (0.00s)\n'
exit 0
`)
	writeExecutable(t, filepath.Join(binDir, "fake-qemu-img"), `#!/usr/bin/env bash
exit 0
`)
	writeExecutable(t, filepath.Join(binDir, "fake-kubectl"), `#!/usr/bin/env bash
exit 0
`)
	writeExecutable(t, filepath.Join(binDir, "fake-virsh"), `#!/usr/bin/env bash
printf 'virsh %s\n' "$*" >>"$KATL_SCRIPTTEST_LOG"
if [[ "$1" != "-c" ]]; then
  exit 2
fi
shift 2
case "$1" in
  uri)
    printf 'qemu:///system\n'
    ;;
  net-info)
    printf 'Name: default\nActive: yes\n'
    ;;
  net-dumpxml)
    printf "<network><ip address='10.77.0.1' prefix='24'/></network>\n"
    ;;
  pool-info)
    printf 'Name: default\nState: running\n'
    ;;
  pool-dumpxml)
    printf '<pool><target><path>%s</path></target></pool>\n' "$KATL_SCRIPTTEST_STORAGE"
    ;;
  list)
    printf 'katl-old\nother-domain\nkatl-current\n'
    ;;
  metadata)
    case "$2" in
      katl-old|katl-current)
        printf '<vmtest xmlns:vmtest="https://katlos.io/xmlns/vmtest/1">katl/vmtest</vmtest>\n'
        ;;
      *)
        printf '\n'
        ;;
    esac
    ;;
  domstate)
    printf 'running\n'
    ;;
  destroy|undefine)
    ;;
  *)
    exit 2
    ;;
esac
`)

	command := func(runID string) *exec.Cmd {
		cmd := exec.Command(filepath.Join(repo, "scripts", "vmtest-run"), "--artifact-set=none", "./internal/vmtest", "-run", "^TestDoesNotMatter$", "-count=1")
		cmd.Dir = repo
		cmd.Env = append(
			os.Environ(),
			"KATL_SCRIPTTEST_LOG="+logPath,
			"KATL_SCRIPTTEST_STORAGE="+tmp,
			"KATL_VMTEST_GO="+filepath.Join(binDir, "fake-go"),
			"KATL_VMTEST_IMAGE_TOOL="+filepath.Join(binDir, "fake-qemu-img"),
			"KATL_VMTEST_KUBECTL="+filepath.Join(binDir, "fake-kubectl"),
			"KATL_VMTEST_VIRSH="+filepath.Join(binDir, "fake-virsh"),
			"KATL_VMTEST_RUN_ID="+runID,
			"KATL_VMTEST_RUN_DIR="+filepath.Join(tmp, runID),
			"KATL_VMTEST_CACHE_DIR="+filepath.Join(tmp, "cache"),
			"KATL_VMTEST_KEEP=always",
			"KATL_VMTEST_REQUIRE_SCENARIO_RESULT=0",
		)
		return cmd
	}

	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := command("scripttest")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("scripts/vmtest-run failed: %v\n%s", err, output)
	}

	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read command log: %v", err)
	}
	log := string(logData)
	if !strings.Contains(log, "go test -exec ") {
		t.Fatalf("go test did not run:\n%s", log)
	}
	// Libvirt objects of other runs belong to the runs that hold their leases,
	// so the runner leaves every domain, including vmtest ones, alone.
	for _, unwanted := range []string{" list ", " metadata ", " destroy ", " undefine "} {
		if strings.Contains(log, unwanted) {
			t.Fatalf("runner touched libvirt domains (%q) in:\n%s", unwanted, log)
		}
	}
}

func writeExecutable(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestVMTestRunKubernetesUpgradeSource(t *testing.T) {
	const upgradeSource = "_build/mkosi/katl-kubernetes-upgrade-source.raw"
	for _, tc := range []struct {
		name, run   string
		wantSource  bool
		wantChecked []string
	}{
		{
			name:        "upgrade with bootstrap",
			run:         "TestKubeadmUpgradeOperationSmoke|TestInstalledRuntimeTwoNodeOperationBackedBootstrapSmoke",
			wantSource:  true,
			wantChecked: []string{upgradeSource, "_build/mkosi/katl-kubernetes-upgrade.raw"},
		},
		{name: "bootstrap only", run: "TestInstalledRuntimeTwoNodeOperationBackedBootstrapSmoke"},
		{
			name:        "upgrade only",
			run:         "TestKubeadmUpgradeOperationSmoke",
			wantSource:  true,
			wantChecked: []string{upgradeSource, "_build/mkosi/katl-kubernetes-upgrade.raw"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			scripts := filepath.Join(root, "scripts")
			if err := os.MkdirAll(scripts, 0o755); err != nil {
				t.Fatal(err)
			}
			writeExecutable(t, filepath.Join(scripts, "mkosi"), `#!/usr/bin/env bash
if [[ "$1" == builder-version ]]; then echo 26; exit 0; fi
mkdir -p "$KATL_REPO_ROOT/_build/mkosi"
printf current > "$KATL_REPO_ROOT/_build/mkosi/katl-runtime-root.squashfs"
`)
			// The fake records the payload version it was asked to build into the
			// artifact metadata, as the real builder does.
			writeExecutable(t, filepath.Join(scripts, "build-kubernetes-sysext"), `#!/usr/bin/env bash
name=katl-kubernetes
[[ "$1" == --output ]] && name="$2"
mkdir -p "$KATL_REPO_ROOT/_build/mkosi"
printf sysext > "$KATL_REPO_ROOT/_build/mkosi/$name.raw"
printf '{"payloadVersion":"%s"}\n' "${KATL_KUBERNETES_PAYLOAD_VERSION:-default}" > "$KATL_REPO_ROOT/_build/mkosi/$name.raw.json"
`)
			writeExecutable(t, filepath.Join(scripts, "check-kubernetes-sysext"), `#!/usr/bin/env bash
printf '%s\n' "${1#"$KATL_REPO_ROOT/"}" >> "$KATL_SCRIPTTEST_CHECKED"
`)
			writeExecutable(t, filepath.Join(root, "resource-lock"), "#!/usr/bin/env bash\nexit 17\n")

			checked := filepath.Join(root, "checked.log")
			cmd := exec.Command(filepath.Join(repoRoot(t), "scripts/vmtest-run"), "./internal/vmtest/scenarios", "-run", tc.run)
			cmd.Dir = root
			cmd.Env = append(os.Environ(),
				"KATL_REPO_ROOT="+root,
				"KATL_MKOSI_BUILD_DIR="+filepath.Join(root, "_build/mkosi"),
				"KATL_VMTEST_RESOURCE_LOCK="+filepath.Join(root, "resource-lock"),
				"KATL_VMTEST_RUN_DIR="+filepath.Join(root, "run"),
				"KATL_VMTEST_CACHE_DIR="+filepath.Join(root, "cache"),
				"KATL_SCRIPTTEST_CHECKED="+checked,
				"KATL_KUBERNETES_PAYLOAD_VERSION=",
				"KATL_VMTEST_KUBERNETES_BUNDLE=",
				"KATL_VMTEST_KUBERNETES_UPGRADE_BUNDLE=",
			)
			// The fake resource lock stops the run once artifacts are built.
			output, err := cmd.CombinedOutput()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
				t.Fatalf("exit = %v, want setup failure after the build: %s", err, output)
			}

			defaultMetadata, err := os.ReadFile(filepath.Join(root, "_build/mkosi/katl-kubernetes.raw.json"))
			if err != nil || !strings.Contains(string(defaultMetadata), `"default"`) {
				t.Fatalf("default payload metadata = %q, %v; want the default payload: %s", defaultMetadata, err, output)
			}
			sourceMetadata, err := os.ReadFile(filepath.Join(root, upgradeSource+".json"))
			if !tc.wantSource {
				if err == nil {
					t.Fatalf("upgrade source built for a run that does not need it: %s", sourceMetadata)
				}
			} else if err != nil || !strings.Contains(string(sourceMetadata), `"v1.36.0"`) {
				t.Fatalf("upgrade source metadata = %q, %v; want v1.36.0 at its own path: %s", sourceMetadata, err, output)
			}

			got, err := os.ReadFile(checked)
			if tc.wantChecked == nil {
				if err == nil {
					t.Fatalf("verified Kubernetes artifacts for a run without upgrade tests: %q", got)
				}
				return
			}
			if err != nil || strings.Join(strings.Fields(string(got)), " ") != strings.Join(tc.wantChecked, " ") {
				t.Fatalf("verified artifacts = %q, %v; want %q", got, err, tc.wantChecked)
			}
		})
	}
}

func TestVMTestRunRefusesPackageParallelism(t *testing.T) {
	repo := repoRoot(t)
	for _, tc := range []struct {
		name     string
		args     []string
		rejected bool
	}{
		{name: "separate value", args: []string{"-p", "2"}, rejected: true},
		{name: "equals value", args: []string{"-p=4"}, rejected: true},
		{name: "double dash", args: []string{"--p", "3"}, rejected: true},
		{name: "one", args: []string{"-p", "1"}},
		{name: "after args", args: []string{"-args", "-p", "8"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := workstationtest.WorktreeTempDir(t, repo)
			logPath := filepath.Join(tmp, "commands.log")
			if err := os.WriteFile(logPath, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			fakeGo := filepath.Join(tmp, "fake-go")
			writeExecutable(t, fakeGo, "#!/usr/bin/env bash\nprintf 'go %s\\n' \"$*\" >>\"$KATL_SCRIPTTEST_LOG\"\nexit 0\n")
			args := append([]string{"--artifact-set=none", "./internal/vmtest", "-run", "^TestDoesNotMatter$", "-count=1"}, tc.args...)
			cmd := exec.Command(filepath.Join(repo, "scripts", "vmtest-run"), args...)
			cmd.Dir = repo
			cmd.Env = append(os.Environ(),
				"KATL_SCRIPTTEST_LOG="+logPath,
				"KATL_VMTEST_GO="+fakeGo,
				"KATL_VMTEST_RUN_ID=scripttest-p",
				"KATL_VMTEST_RUN_DIR="+filepath.Join(tmp, "run"),
				"KATL_VMTEST_CACHE_DIR="+filepath.Join(tmp, "cache"),
				"KATL_VMTEST_REQUIRE_SCENARIO_RESULT=0",
			)

			output, err := cmd.CombinedOutput()

			logData, _ := os.ReadFile(logPath)
			ran := strings.Contains(string(logData), "go test")
			if tc.rejected {
				if err == nil || ran || !strings.Contains(string(output), "-p") || !strings.Contains(string(output), "one at a time") {
					t.Fatalf("vmtest-run %v: err = %v, go ran = %v, want a refusal that explains sequential packages\n%s", tc.args, err, ran, output)
				}
				return
			}
			if strings.Contains(string(output), "one at a time") {
				t.Fatalf("vmtest-run %v was refused:\n%s", tc.args, output)
			}
		})
	}
}
