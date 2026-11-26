//go:build (cgo || darwin) && !osusergo

package main

import (
	"os/user"
	"path/filepath"
	"testing"
)

func TestEvidenceRootIgnoresEnvironment(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("KATL_DEV_EVIDENCE_ROOT", t.TempDir())
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}

	got, err := evidenceRoot()
	want := filepath.Join(account.HomeDir, ".local", "state", "katl-dev", "evidence")
	if err != nil || got != want {
		t.Fatalf("evidence root = %q, %v; want %q", got, err, want)
	}
}
