package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/operatorconsole"
	"golang.org/x/sys/unix"
)

// Run inside a throwaway VM with katl-console stopped and tty1 foregrounded.
func TestKernelLogRouting(t *testing.T) {
	if os.Getenv("KATL_TEST_KERNEL_LOG_ROUTING") != "1" {
		t.Skip("requires a throwaway Linux VM virtual console")
	}
	active, err := os.ReadFile("/sys/class/tty/tty0/active")
	if err != nil || strings.TrimSpace(string(active)) != "tty1" {
		t.Fatalf("foreground console = %q, err=%v; select tty1 before this test", active, err)
	}
	tty, err := os.OpenFile("/dev/tty1", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer tty.Close()

	restore, err := redirectKernelLogs(tty)
	if err != nil {
		t.Fatal(err)
	}
	defer restore()
	before, err := os.ReadFile("/dev/vcs1")
	if err != nil {
		t.Fatal(err)
	}
	kmsg, err := os.OpenFile("/dev/kmsg", os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer kmsg.Close()
	marker := fmt.Sprintf("katl-console-routing-%d", time.Now().UnixNano())
	if _, err := fmt.Fprintf(kmsg, "<0>%s\n", marker); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(time.Second)
	for {
		logs, err := os.ReadFile("/dev/vcs3")
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(logs, []byte(marker)) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("kernel message did not reach the log console")
		}
		time.Sleep(10 * time.Millisecond)
	}
	after, err := os.ReadFile("/dev/vcs1")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("kernel message changed the dashboard console")
	}
}

func TestDisplayTermiosDisablesTerminalSignals(t *testing.T) {
	original := unix.Termios{
		Iflag:  0x1234,
		Oflag:  0x5678,
		Cflag:  0x9abc,
		Lflag:  unix.ISIG | unix.ICANON | unix.ECHO,
		Line:   7,
		Ispeed: 115200,
		Ospeed: 115200,
	}
	original.Cc[unix.VINTR] = 3
	original.Cc[unix.VQUIT] = 28
	original.Cc[unix.VSUSP] = 26
	got := displayTermios(original)
	if got.Lflag&unix.ISIG != 0 {
		t.Fatalf("display termios Lflag = %#x, ISIG remains enabled", got.Lflag)
	}
	got.Lflag |= unix.ISIG
	if got != original {
		t.Fatalf("display termios changed settings other than ISIG:\ngot  %#v\nwant %#v", got, original)
	}
}

func TestJournalRingIsBounded(t *testing.T) {
	ring := newJournalRing(2)
	ring.Add([]byte("one"))
	ring.Add([]byte("two"))
	ring.Add([]byte("three"))
	target := operatorconsole.NewRenderTarget(make([]byte, 256), 80, 2)
	writer := operatorconsole.NewJournalWriter(target)
	ring.WriteTail(&writer)
	got, rows := writer.Bytes(), writer.RowsWritten()
	if rows != 2 || string(got) != "two\nthree\n" {
		t.Fatalf("WriteTail() = %q, %d rows", got, rows)
	}
}

func TestJournalRingTruncatesOversizedEntries(t *testing.T) {
	ring := newJournalRing(1)
	ring.Add([]byte(strings.Repeat("x", journalLineCapacity) + "SECRET-TAIL"))
	if got := len(ring.lines[0]); got != journalLineCapacity {
		t.Fatalf("journal slot length = %d", got)
	}
	writer := operatorconsole.NewJournalWriter(operatorconsole.NewRenderTarget(make([]byte, 4096), 128, 10))
	ring.WriteTail(&writer)
	got := string(writer.Bytes())
	if !strings.Contains(got, journalTruncated) || strings.Contains(got, "SECRET-TAIL") {
		t.Fatalf("truncated journal entry = %q", got)
	}
}

func TestStreamJournalCancelsFollowerAfterOversizedLine(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	command := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestJournalOversizeHelper")
		cmd.Env = append(os.Environ(), "KATL_JOURNAL_OVERSIZE_HELPER=1")
		return cmd
	}
	err := streamJournal(ctx, newJournalRing(1), command)
	if err == nil || !strings.Contains(err.Error(), "token too long") {
		t.Fatalf("streamJournal() error = %v", err)
	}
	if ctx.Err() != nil {
		t.Fatalf("streamJournal() waited for the context deadline: %v", ctx.Err())
	}
}

func TestJournalOversizeHelper(t *testing.T) {
	if os.Getenv("KATL_JOURNAL_OVERSIZE_HELPER") != "1" {
		return
	}
	_, _ = fmt.Fprint(os.Stdout, strings.Repeat("x", journalScanCapacity+4096))
	select {}
}

func TestJournalUsesDateTimeTimestamps(t *testing.T) {
	if !slices.Contains(journalctlArgs, "--output=short-iso") || slices.Contains(journalctlArgs, "--output=short-monotonic") {
		t.Fatalf("journalctlArgs = %#v", journalctlArgs)
	}
}

func TestJournalRingCompactsDateTimeTimestamps(t *testing.T) {
	tests := []struct {
		name string
		line string
		want string
	}{
		{
			name: "seconds",
			line: "2026-07-17T18:02:03+0100 host service[1]: ready",
			want: "2026-07-17 18:02:03 host service[1]: ready\n",
		},
		{
			name: "milliseconds",
			line: "2026-07-17T18:02:03.123456+01:00 host service[1]: ready",
			want: "2026-07-17 18:02:03.123 host service[1]: ready\n",
		},
		{
			name: "short fraction",
			line: "2026-07-17T18:02:03.1Z host service[1]: ready",
			want: "2026-07-17 18:02:03.1 host service[1]: ready\n",
		},
		{
			name: "non timestamp",
			line: "journal stream unavailable: disconnected",
			want: "journal stream unavailable: disconnected\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ring := newJournalRing(1)
			ring.Add([]byte(test.line))
			target := operatorconsole.NewRenderTarget(make([]byte, 256), 128, 1)
			writer := operatorconsole.NewJournalWriter(target)
			ring.WriteTail(&writer)
			got := writer.Bytes()
			if string(got) != test.want {
				t.Fatalf("journal line = %q, want %q", got, test.want)
			}
		})
	}
}

func TestJournalRingReusesBoundedSlots(t *testing.T) {
	ring := newJournalRing(2)
	line := []byte("2026-07-17T18:02:03.123456+01:00 " + strings.Repeat("x", journalLineCapacity-39))
	storage := make([]byte, 4096)
	for range 1000 {
		ring.Add(line)
		writer := operatorconsole.NewJournalWriter(operatorconsole.NewRenderTarget(storage, 80, 2))
		ring.WriteTail(&writer)
		if len(writer.Bytes()) == 0 {
			t.Fatal("journal slot rendered empty content")
		}
	}
	for index, slot := range ring.lines {
		if cap(slot) > journalLineCapacity {
			t.Fatalf("slot %d capacity = %d", index, cap(slot))
		}
	}
}

func TestJournalRingSPSC(t *testing.T) {
	ring := newJournalRing(32)
	done := make(chan struct{})
	go func() {
		line := []byte("journal line")
		for range 10_000 {
			ring.Add(line)
		}
		close(done)
	}()

	storage := make([]byte, 4096)
	for {
		writer := operatorconsole.NewJournalWriter(operatorconsole.NewRenderTarget(storage, 80, 8))
		ring.WriteTail(&writer)
		rows := writer.RowsWritten()
		if rows > 8 {
			t.Fatalf("WriteTail() rows = %d, want at most 8", rows)
		}
		select {
		case <-done:
			return
		default:
		}
	}
}

func TestWriteSnapshotReplacesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "console", "rendered.txt")
	if err := writeSnapshot(path, []byte("first\n")); err != nil {
		t.Fatal(err)
	}
	if err := writeSnapshot(path, []byte("second\n")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "second\n" {
		t.Fatalf("snapshot = %q", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("snapshot permissions = %o", got)
	}
}
