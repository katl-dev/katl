package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestJournalRingIsBounded(t *testing.T) {
	ring := newJournalRing(2)
	ring.Add([]byte("one"))
	ring.Add([]byte("two"))
	ring.Add([]byte("three"))
	got, rows := ring.AppendTail(make([]byte, 0, 32), 2, 80)
	if rows != 2 || string(got) != "two\nthree\n" {
		t.Fatalf("AppendTail() = %q, %d rows", got, rows)
	}
}

func TestJournalRingReusesPreallocatedLines(t *testing.T) {
	ring := newJournalRing(2)
	line := []byte(strings.Repeat("x", journalLineCapacity))
	buffer := make([]byte, 0, 4096)
	allocations := testing.AllocsPerRun(1000, func() {
		ring.Add(line)
		buffer, _ = ring.AppendTail(buffer[:0], 2, 80)
	})
	if allocations != 0 {
		t.Fatalf("journal add/render allocations = %v, want 0", allocations)
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

	buffer := make([]byte, 0, 4096)
	for {
		var rows int
		buffer, rows = ring.AppendTail(buffer[:0], 8, 80)
		if rows > 8 {
			t.Fatalf("AppendTail() rows = %d, want at most 8", rows)
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
}
