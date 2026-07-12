package main

import (
	"bytes"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestSendSystemdNotification(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notify.sock")
	listener, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	t.Setenv("NOTIFY_SOCKET", path)
	if err := sendSystemdNotification("READY=1\nSTATUS=waiting for configuration"); err != nil {
		t.Fatal(err)
	}
	if err := listener.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 256)
	n, _, err := listener.ReadFromUnix(buffer)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(buffer[:n]); !strings.Contains(got, "READY=1") || !strings.Contains(got, "STATUS=waiting for configuration") {
		t.Fatalf("notification = %q", got)
	}
}
