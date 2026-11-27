package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestReleaseRefusesBindMount(t *testing.T) {
	const child = "KATL_EVIDENCE_BIND_MOUNT_CHILD"
	if os.Getenv(child) != "1" {
		binary, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(binary, "-test.run=^TestReleaseRefusesBindMount$", "-test.v")
		cmd.Env = append(os.Environ(), child+"=1")
		uid, gid := os.Geteuid(), os.Getegid()
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Cloneflags: unix.CLONE_NEWUSER | unix.CLONE_NEWNS,
			// Root in the child namespace retains its mount capability across exec.
			UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: uid, Size: 1}},
			GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: gid, Size: 1}},
		}
		output, err := cmd.CombinedOutput()
		if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) || errors.Is(err, unix.ENOSPC) {
			t.Skipf("unprivileged user namespaces are disabled on this host: %v", err)
		}
		if err != nil {
			t.Fatalf("bind-mount test in private namespaces: %v\n%s", err, output)
		}
		return
	}

	fixture := newEvidenceFixture(t)
	entry := fixture.runOK(t, testDate, "new", "822/entry")
	source := filepath.Join(filepath.Dir(fixture.location), "source")
	target := filepath.Join(entry, "mount")
	makeDir(t, source)
	makeDir(t, target)
	payload := writePayload(t, source, "mounted")
	if err := unix.Mount(source, target, "", unix.MS_BIND, ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := unix.Unmount(target, 0); err != nil {
			t.Error(err)
		}
	})

	var parent, mounted unix.Stat_t
	if err := unix.Stat(entry, &parent); err != nil {
		t.Fatal(err)
	}
	if err := unix.Stat(target, &mounted); err != nil {
		t.Fatal(err)
	}
	if parent.Dev != mounted.Dev {
		t.Fatal("test requires a same-device bind mount")
	}
	checkPayload(t, filepath.Join(target, filepath.Base(payload)), "mounted")

	err := fixture.run([]string{"release", entry}, &bytes.Buffer{}, testDate)
	if err == nil || !strings.Contains(err.Error(), "refusing to cross mount") || !strings.Contains(err.Error(), target) {
		t.Errorf("release with a bind mount: %v", err)
	}
	checkPayload(t, payload, "mounted")
}
