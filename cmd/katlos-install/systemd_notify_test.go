package main

import (
	"bytes"
	"errors"
	"testing"
)

func TestReportInstallerProgressMarksServiceReady(t *testing.T) {
	previous := notifySystemd
	t.Cleanup(func() { notifySystemd = previous })
	var notification string
	notifySystemd = func(payload string) error {
		notification = payload
		return nil
	}
	var stdout bytes.Buffer
	reportInstallerProgress(&stdout, "  configuration\n handoff   ready ", true)
	if notification != "READY=1\nSTATUS=configuration handoff ready" {
		t.Fatalf("notification = %q", notification)
	}
	if got := stdout.String(); got != "katlos-install progress: configuration handoff ready\n" {
		t.Fatalf("stdout = %q", got)
	}
}

func TestReportInstallerFailureLeavesStatus(t *testing.T) {
	previous := notifySystemd
	t.Cleanup(func() { notifySystemd = previous })
	var notification string
	notifySystemd = func(payload string) error {
		notification = payload
		return nil
	}
	reportInstallerFailure(errors.New("fetch manifest:\n  connection refused"))
	if want := "STATUS=installer stopped: fetch manifest: connection refused"; notification != want {
		t.Fatalf("notification = %q, want %q", notification, want)
	}
}
