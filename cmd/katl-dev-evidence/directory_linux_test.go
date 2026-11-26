package main

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestReleaseRefusesBindMount(t *testing.T) {
	fixture := newEvidenceFixture(t)
	entry := fixture.runOK(t, testDate, "new", "822/entry")
	source := filepath.Join(filepath.Dir(fixture.location), "source")
	target := filepath.Join(entry, "mount")
	makeDir(t, source)
	makeDir(t, target)
	payload := writePayload(t, source, "mounted")
	if err := unix.Mount(source, target, "", unix.MS_BIND, ""); err != nil {
		if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) {
			t.Skip("bind mount requires a private mount namespace with CAP_SYS_ADMIN")
		}
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := unix.Unmount(target, 0); err != nil {
			t.Error(err)
		}
	})

	err := fixture.run([]string{"release", entry}, &bytes.Buffer{}, testDate)
	if err == nil || !strings.Contains(err.Error(), "refusing to cross mount") || !strings.Contains(err.Error(), target) {
		t.Errorf("release with a bind mount: %v", err)
	}
	checkPayload(t, payload, "mounted")
}
