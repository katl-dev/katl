package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var testDate = time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)

func TestKeep(t *testing.T) {
	root := evidenceFixture(t)
	path := runOK(t, testDate, "new", "822/logs")
	payload := writePayload(t, path, "logs")
	future := testDate.AddDate(0, 0, 1)

	pinned := runOK(t, testDate, "keep", path, "--until", future.Format(time.DateOnly))
	if !strings.HasPrefix(pinned, filepath.Join(root, ".pinned")+string(filepath.Separator)) {
		t.Fatalf("keep returned path outside the tmpfiles exclusion: %q", pinned)
	}
	if again := runOK(t, testDate, "keep", "822/logs", "--until", future.Format(time.DateOnly)); again != pinned {
		t.Fatalf("repeated keep returned %q, want %q", again, pinned)
	}
	assertMissing(t, path)
	checkPayload(t, filepath.Join(pinned, filepath.Base(payload)), "logs")

	list := runOK(t, testDate, "list")
	_, row, _ := strings.Cut(list, "\n")
	fields := strings.Fields(row)
	if len(fields) != 6 || fields[0] != "822" || fields[1] != "logs" || fields[2] != "4" ||
		fields[4] != future.Format(time.DateOnly) || fields[5] != pinned {
		t.Errorf("unexpected list output: %s", list)
	}

	runOK(t, future.AddDate(0, 0, 1), "prune")
	assertMissing(t, pinned)
	checkPayload(t, payload, "logs")
	runOK(t, future.AddDate(0, 0, 1), "release", "822/logs")
	assertMissing(t, path)
}

func TestPrune(t *testing.T) {
	root := evidenceFixture(t)
	pinned := runOK(t, testDate, "new", "822/pinned")
	writePayload(t, pinned, "pin")
	setModified(t, pinned, testDate.Add(-72*time.Hour))
	pinned = runOK(t, testDate, "keep", pinned, "--until", testDate.Format(time.DateOnly))

	// Lexical order differs from age order, and deleting the oldest entry
	// alone suffices to meet the budget.
	newer := runOK(t, testDate, "new", "822/aaa-newer")
	newerFile := writePayload(t, newer, "abcd")
	setModified(t, newer, testDate.Add(-time.Hour))
	oldest := runOK(t, testDate, "new", "822/zzz-oldest")
	writePayload(t, oldest, "fg")
	setModified(t, oldest, testDate.Add(-48*time.Hour))

	if output := runOK(t, testDate, "prune", "--budget", "7B"); output != "822/zzz-oldest" {
		t.Fatalf("prune removed %q, want only the oldest entry", output)
	}
	assertMissing(t, oldest)
	checkPayload(t, newerFile, "abcd")
	pinnedFile := filepath.Join(pinned, "payload")
	checkPayload(t, pinnedFile, "pin")

	var out bytes.Buffer
	err := run([]string{"prune", "--budget", "2"}, &out, testDate)
	if err == nil || !strings.Contains(err.Error(), "over budget") {
		t.Fatalf("prune with a pin above budget: %v", err)
	}
	assertMissing(t, newer)
	checkPayload(t, pinnedFile, "pin")

	// The same entry becomes eligible when its pin expires.
	runOK(t, testDate.AddDate(0, 0, 1), "prune", "--budget", "2")
	assertMissing(t, pinned)
	assertMissing(t, filepath.Join(root, "822", "pinned"))
}

func TestNewOverBudget(t *testing.T) {
	root := evidenceFixture(t)
	large := runOK(t, testDate, "new", "822/large")
	payload := writePayload(t, large, "")
	if err := os.Truncate(payload, 50<<30+1); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	err := run([]string{"new", "822/blocked"}, &out, testDate)
	if err == nil || !strings.Contains(err.Error(), "katl-dev-evidence list") || !strings.Contains(err.Error(), "release") {
		t.Fatalf("new over budget did not explain how to free space: %v", err)
	}
	assertMissing(t, filepath.Join(root, "822", "blocked"))

	if err := os.Truncate(payload, 50<<30); err != nil {
		t.Fatal(err)
	}
	runOK(t, testDate, "new", "822/at-budget")
}

func TestReleaseConfinesPaths(t *testing.T) {
	root := evidenceFixture(t)
	outside := t.TempDir()
	outsideEntry := filepath.Join(outside, "entry")
	if err := os.Mkdir(outsideEntry, 0o700); err != nil {
		t.Fatal(err)
	}
	outsideFile := writePayload(t, outsideEntry, "outside")
	inside := runOK(t, testDate, "new", "822/inside")
	insideFile := writePayload(t, inside, "inside")

	for name, target := range map[string]string{
		"owner-link":              outside,
		"822/leaf-link":           outsideEntry,
		"822/inside/payload-link": outsideEntry,
	} {
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}

	for _, operand := range []string{
		outsideEntry, "822/../822/inside", root + "/822/../822/inside",
		root, "owner-link/entry", filepath.Join(root, "owner-link", "entry"), "822/leaf-link",
	} {
		t.Run(operand, func(t *testing.T) {
			if err := run([]string{"release", operand}, &bytes.Buffer{}, testDate); err == nil {
				t.Errorf("release accepted %q", operand)
			}
			checkPayload(t, outsideFile, "outside")
			checkPayload(t, insideFile, "inside")
		})
	}
	runOK(t, testDate, "release", inside)
	assertMissing(t, inside)
	checkPayload(t, outsideFile, "outside")
}

func evidenceFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("KATL_DEV_EVIDENCE_ROOT", root)
	return root
}

func runOK(t *testing.T, now time.Time, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	if err := run(args, &out, now); err != nil {
		t.Fatalf("%v: %v", args, err)
	}
	return strings.TrimSpace(out.String())
}

func writePayload(t *testing.T, entryPath, content string) string {
	t.Helper()
	path := filepath.Join(entryPath, "payload")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func setModified(t *testing.T, path string, at time.Time) {
	t.Helper()
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

func assertMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%q should be absent, stat returned %v", path, err)
	}
}

func checkPayload(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || string(got) != want {
		t.Fatalf("read %q: got %q, %v; want %q", path, got, err, want)
	}
}
