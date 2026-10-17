package kubeletstartup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const testInvocation = "0123456789abcdef0123456789abcdef"

func TestPrepare(t *testing.T) {
	root := t.TempDir()
	writeStartupInputs(t, root)
	before := time.Now()
	record, err := Prepare(context.Background(), root, testInvocation)
	if err != nil {
		t.Fatal(err)
	}
	after := time.Now()
	if record.NotBefore.Nanosecond() != 0 || !record.NotBefore.After(before) || record.NotBefore.After(after) {
		t.Fatalf("startup boundary %s does not separate old reports at %s from the new process at %s", record.NotBefore, before, after)
	}
	if record.InvocationID != testInvocation || record.BootID != "boot-1" || record.ConfextTarget != "/var/lib/katl/generations/1/confext" {
		t.Fatalf("startup identity = %+v", record)
	}
	persisted, err := Read(root)
	if err != nil || persisted != record {
		t.Fatalf("persisted startup identity = %+v, %v", persisted, err)
	}
	if err := persisted.Verify(root, testInvocation, "boot-1", record.NotBefore, after); err != nil {
		t.Fatalf("first report in the startup second was rejected: %v", err)
	}
}

func TestPrepareCancellation(t *testing.T) {
	root := t.TempDir()
	writeStartupInputs(t, root)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Prepare(ctx, root, testInvocation); !errors.Is(err, context.Canceled) {
		t.Fatalf("Prepare error = %v, want cancellation", err)
	}
	if _, err := os.Stat(rooted(root, recordPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled startup published evidence: %v", err)
	}
}

func TestVerify(t *testing.T) {
	boundary := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)
	for _, tc := range []struct {
		name, invocation, boot string
		heartbeat, now         time.Time
		wantError              bool
	}{
		{name: "first same-second report", invocation: testInvocation, boot: "boot-1", heartbeat: boundary, now: boundary.Add(50 * time.Millisecond)},
		{name: "later report", invocation: testInvocation, boot: "boot-1", heartbeat: boundary.Add(time.Hour), now: boundary.Add(time.Hour + time.Second)},
		{name: "previous process report", invocation: testInvocation, boot: "boot-1", heartbeat: boundary.Add(-time.Second), now: boundary.Add(time.Second), wantError: true},
		{name: "automatic restart", invocation: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", boot: "boot-1", heartbeat: boundary, now: boundary.Add(time.Second), wantError: true},
		{name: "previous boot", invocation: testInvocation, boot: "boot-0", heartbeat: boundary, now: boundary.Add(time.Second), wantError: true},
		{name: "missing heartbeat", invocation: testInvocation, boot: "boot-1", now: boundary.Add(time.Second), wantError: true},
		{name: "future heartbeat", invocation: testInvocation, boot: "boot-1", heartbeat: boundary.Add(time.Second), now: boundary, wantError: true},
		{name: "clock before startup boundary", invocation: testInvocation, boot: "boot-1", heartbeat: boundary, now: boundary.Add(-time.Second), wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeStartupInputs(t, root)
			record := Record{InvocationID: testInvocation, BootID: "boot-1", ConfextTarget: "/var/lib/katl/generations/1/confext", NotBefore: boundary}
			if err := record.Verify(root, tc.invocation, tc.boot, tc.heartbeat, tc.now); (err != nil) != tc.wantError {
				t.Fatalf("Verify error = %v, want error %v", err, tc.wantError)
			}
		})
	}
}

func TestConfextChanged(t *testing.T) {
	root := t.TempDir()
	writeStartupInputs(t, root)
	record := Record{ConfextTarget: "/var/lib/katl/generations/1/confext"}
	if changed, err := record.ConfextChanged(root); changed || err != nil {
		t.Fatalf("unchanged configuration = %v, %v", changed, err)
	}
	link := rooted(root, "/run/confexts/zzzz-katl-node")
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/var/lib/katl/generations/2/confext", link); err != nil {
		t.Fatal(err)
	}
	if changed, err := record.ConfextChanged(root); !changed || err != nil {
		t.Fatalf("configuration changed = %v, %v", changed, err)
	}
}

func TestReadRefusesInvalidEvidence(t *testing.T) {
	for _, content := range []string{
		`{`,
		`{}`,
		`{"invocationID":"0123456789abcdef0123456789abcdef","bootID":"boot-1","confextTarget":"/generation/1","notBefore":"2026-01-01T00:00:01.5Z"}`,
		`{"invocationID":"not-a-service-invocation","bootID":"boot-1","confextTarget":"/generation/1","notBefore":"2026-01-01T00:00:01Z"}`,
	} {
		t.Run(content, func(t *testing.T) {
			root := t.TempDir()
			path := rooted(root, recordPath)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Read(root); err == nil {
				t.Fatal("invalid startup evidence was accepted")
			}
		})
	}
}

func writeStartupInputs(t *testing.T, root string) {
	t.Helper()
	boot := rooted(root, "/proc/sys/kernel/random/boot_id")
	if err := os.MkdirAll(filepath.Dir(boot), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(boot, []byte("boot-1\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	link := rooted(root, "/run/confexts/zzzz-katl-node")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/var/lib/katl/generations/1/confext", link); err != nil {
		t.Fatal(err)
	}
}
