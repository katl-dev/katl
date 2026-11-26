package systemdunit

import (
	"net"
	"path/filepath"
	"testing"
	"time"
)

func TestNotify(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notify.sock")
	listener, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	t.Setenv("NOTIFY_SOCKET", path)

	if err := Notify("READY=1\nSTATUS=waiting for configuration"); err != nil {
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
	if got, want := string(buffer[:n]), "READY=1\nSTATUS=waiting for configuration"; got != want {
		t.Fatalf("notification = %q, want %q", got, want)
	}
}

func TestNotifyWithoutSocket(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")

	if err := Notify("READY=1"); err != nil {
		t.Fatalf("Notify() without a socket = %v, want nil", err)
	}
}
