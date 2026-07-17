package scriptstest

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestVMTestRunRemovesExistingDomainsBeforeGoTest(t *testing.T) {
	repo := repoRoot(t)
	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "commands.log")
	binDir := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	writeExecutable(t, filepath.Join(binDir, "fake-go"), `#!/usr/bin/env bash
printf 'go %s\n' "$*" >>"$KATL_SCRIPTTEST_LOG"
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

	hostLock := filepath.Join(tmp, "host.lock")
	command := func(runID string) *exec.Cmd {
		cmd := exec.Command(filepath.Join(repo, "scripts", "vmtest-run"), "--artifact-set=none", "./internal/vmtest", "-run", "^TestDoesNotMatter$", "-count=1")
		cmd.Dir = repo
		cmd.Env = append(os.Environ(),
			"KATL_SCRIPTTEST_LOG="+logPath,
			"KATL_SCRIPTTEST_STORAGE="+tmp,
			"KATL_VMTEST_GO="+filepath.Join(binDir, "fake-go"),
			"KATL_VMTEST_IMAGE_TOOL="+filepath.Join(binDir, "fake-qemu-img"),
			"KATL_VMTEST_KUBECTL="+filepath.Join(binDir, "fake-kubectl"),
			"KATL_VMTEST_VIRSH="+filepath.Join(binDir, "fake-virsh"),
			"KATL_VMTEST_RUN_ID="+runID,
			"KATL_VMTEST_RUN_DIR="+filepath.Join(tmp, runID),
			"KATL_VMTEST_CACHE_DIR="+filepath.Join(tmp, "cache"),
			"KATL_VMTEST_HOST_LOCK="+hostLock,
			"KATL_VMTEST_KEEP=always",
			"KATL_VMTEST_REQUIRE_SCENARIO_RESULT=0",
		)
		return cmd
	}

	lockFile, err := os.OpenFile(hostLock, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	blocked := command("scripttest-blocked")
	blocked.Env = append(blocked.Env, "KATL_VMTEST_HOST_LOCK_TIMEOUT=0")
	blockedOutput, blockedErr := blocked.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(blockedErr, &exitErr) || exitErr.ExitCode() != 2 {
		t.Fatalf("scripts/vmtest-run under host lock exit = %v, want 2\n%s", blockedErr, blockedOutput)
	}
	if !strings.Contains(string(blockedOutput), "timed out waiting for another libvirt-backed VM run") {
		t.Fatalf("host lock failure missing actionable diagnostic:\n%s", blockedOutput)
	}
	blockedLog, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read blocked command log: %v", err)
	}
	if strings.Contains(string(blockedLog), " list --all --name") || strings.Contains(string(blockedLog), "go test ") {
		t.Fatalf("domain cleanup or go test ran without the host lock:\n%s", blockedLog)
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if err := lockFile.Close(); err != nil {
		t.Fatal(err)
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
	for _, want := range []string{
		"virsh -c qemu:///system list --all --name",
		"virsh -c qemu:///system metadata katl-old --uri https://katlos.io/xmlns/vmtest/1",
		"virsh -c qemu:///system metadata other-domain --uri https://katlos.io/xmlns/vmtest/1",
		"virsh -c qemu:///system metadata katl-current --uri https://katlos.io/xmlns/vmtest/1",
		"virsh -c qemu:///system destroy katl-old",
		"virsh -c qemu:///system undefine katl-old --nvram",
		"virsh -c qemu:///system destroy katl-current",
		"virsh -c qemu:///system undefine katl-current --nvram",
		"go test -exec ",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("command log missing %q:\n%s", want, log)
		}
	}
	for _, unwanted := range []string{
		"virsh -c qemu:///system destroy other-domain",
		"virsh -c qemu:///system undefine other-domain --nvram",
	} {
		if strings.Contains(log, unwanted) {
			t.Fatalf("unowned domain command %q found in log:\n%s", unwanted, log)
		}
	}
	if strings.Index(log, "virsh -c qemu:///system undefine katl-current --nvram") > strings.Index(log, "go test -exec ") {
		t.Fatalf("go test started before vm cleanup completed:\n%s", log)
	}
}

func writeExecutable(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}
