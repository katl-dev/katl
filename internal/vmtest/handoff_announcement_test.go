package vmtest

import (
	"context"
	"path/filepath"
	"testing"
)

const testAnnouncementLine = "katlos-install waiting for config at http://192.168.122.44:8080/v1/config-bundle\n"

// handoffHookRun drives the installer-guest-handoff hook through the serial
// dispatcher and records where it posted.
type handoffHookRun struct {
	posted   []string
	hooksRun []bool
	run      func(serial string) error
}

func newHandoffHookRun(t *testing.T) *handoffHookRun {
	t.Helper()
	h := &handoffHookRun{hooksRun: make([]bool, 1)}
	dir := t.TempDir()
	result := Result{Artifacts: ArtifactPaths{HandoffRequest: filepath.Join(dir, "req.json"), HandoffResponse: filepath.Join(dir, "resp.json")}}
	config := FirstInstallConfig{
		HandoffPoster: func(_ context.Context, url string, _ []byte) (int, string, error) {
			h.posted = append(h.posted, url)
			return 200, "{}", nil
		},
	}
	vm, err := configureGuestHandoff(result, config, nil)
	if err != nil {
		t.Fatal(err)
	}
	hooks := vm.Installer.VM.SerialHooks
	h.hooksRun = make([]bool, len(hooks))
	h.run = func(serial string) error {
		return VMRunner{}.runSerialHooks(context.Background(), result, VMConfig{SerialHooks: hooks}, VMPlan{}, serial, h.hooksRun)
	}
	return h
}

func (h *handoffHookRun) fired() bool {
	for _, ran := range h.hooksRun {
		if ran {
			return true
		}
	}
	return false
}

func TestGuestHandoffHookIgnoresUnterminatedLine(t *testing.T) {
	h := newHandoffHookRun(t)

	// A chunk ending mid-port must not produce a handoff attempt.
	if err := h.run("booting\nkatlos-install waiting for config at http://192.168.122.44:8"); err != nil {
		t.Fatalf("partial line: %v", err)
	}
	if h.fired() || len(h.posted) != 0 {
		t.Fatalf("hook acted on a partial line: posted %v", h.posted)
	}

	if err := h.run("booting\n" + testAnnouncementLine); err != nil {
		t.Fatalf("complete line: %v", err)
	}
	if len(h.posted) != 1 || h.posted[0] != "http://192.168.122.44:8080/v1/install" {
		t.Fatalf("posted = %v, want one post to http://192.168.122.44:8080/v1/install", h.posted)
	}
}

func TestGuestHandoffHookAtEveryByteOffset(t *testing.T) {
	serial := "booting\n" + testAnnouncementLine
	for split := 0; split < len(serial); split++ {
		h := newHandoffHookRun(t)
		if err := h.run(serial[:split]); err != nil {
			t.Fatalf("first %d bytes: %v", split, err)
		}
		if h.fired() {
			t.Fatalf("hook fired on the first %d of %d bytes", split, len(serial))
		}
	}

	h := newHandoffHookRun(t)
	if err := h.run(serial[:len(serial)-1]); err != nil {
		t.Fatal(err)
	}
	if err := h.run(serial); err != nil {
		t.Fatal(err)
	}
	if len(h.posted) != 1 || h.posted[0] != "http://192.168.122.44:8080/v1/install" {
		t.Fatalf("posted = %v", h.posted)
	}
}

func TestGuestHandoffHookUsesCompleteLineBeforePartial(t *testing.T) {
	h := newHandoffHookRun(t)

	serial := testAnnouncementLine + "katlos-install waiting for config at http://10.0.0.1:9"
	if err := h.run(serial); err != nil {
		t.Fatal(err)
	}
	if len(h.posted) != 1 || h.posted[0] != "http://192.168.122.44:8080/v1/install" {
		t.Fatalf("posted = %v", h.posted)
	}
}

func TestLastHandoffAnnouncement(t *testing.T) {
	tests := []struct {
		name     string
		serial   string
		wantBase string
		wantErr  bool
	}{
		{name: "none", serial: "booting\n"},
		{name: "ipv4", serial: testAnnouncementLine, wantBase: "http://192.168.122.44:8080"},
		{name: "ipv6", serial: "katlos-install waiting for config at http://[fd00::44]:8080/v1/config-bundle\n", wantBase: "http://[fd00::44]:8080"},
		{name: "last wins", serial: "katlos-install waiting for config at http://10.0.0.1:8080/v1/config-bundle\n" + testAnnouncementLine, wantBase: "http://192.168.122.44:8080"},
		{name: "carriage return", serial: "katlos-install waiting for config at http://10.0.0.1:8080/v1/config-bundle\r\n", wantBase: "http://10.0.0.1:8080"},
		{name: "wrong path", serial: "katlos-install waiting for config at http://192.168.122.44:8080/config-bundle\n", wantErr: true},
		{name: "no path", serial: "katlos-install waiting for config at http://192.168.122.44:8080\n", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok, err := LastHandoffAnnouncement(test.serial)
			if (err != nil) != test.wantErr {
				t.Fatalf("error = %v, want error %v", err, test.wantErr)
			}
			if ok != (test.wantBase != "") || got.Base != test.wantBase {
				t.Fatalf("ok = %v, base = %q, want %q", ok, got.Base, test.wantBase)
			}
		})
	}
}
