package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

var testDate = time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)

func TestKeep(t *testing.T) {
	fixture := newEvidenceFixture(t)
	root := fixture.location
	path := fixture.runOK(t, testDate, "new", "822/logs")
	payload := writePayload(t, path, "logs")
	future := testDate.AddDate(0, 0, 1)

	pinned := fixture.runOK(t, testDate, "keep", path, "--until", future.Format(time.DateOnly))
	if !strings.HasPrefix(pinned, filepath.Join(root, ".pinned")+string(filepath.Separator)) {
		t.Fatalf("keep returned path outside the tmpfiles exclusion: %q", pinned)
	}
	if again := fixture.runOK(t, testDate, "keep", "822/logs", "--until", future.Format(time.DateOnly)); again != pinned {
		t.Fatalf("repeated keep returned %q, want %q", again, pinned)
	}
	assertMissing(t, path)
	checkPayload(t, filepath.Join(pinned, filepath.Base(payload)), "logs")

	list := fixture.runOK(t, testDate, "list")
	_, row, _ := strings.Cut(list, "\n")
	fields := strings.Fields(row)
	if len(fields) != 6 || fields[0] != "822" || fields[1] != "logs" || fields[2] != "4" ||
		fields[4] != future.Format(time.DateOnly) || fields[5] != pinned {
		t.Errorf("unexpected list output: %s", list)
	}

	fixture.runOK(t, future.AddDate(0, 0, 1), "prune")
	assertMissing(t, pinned)
	checkPayload(t, payload, "logs")
	fixture.runOK(t, future.AddDate(0, 0, 1), "release", "822/logs")
	assertMissing(t, path)
}

func TestPrune(t *testing.T) {
	fixture := newEvidenceFixture(t)
	root := fixture.location
	pinned := fixture.runOK(t, testDate, "new", "822/pinned")
	writePayload(t, pinned, "pin")
	setModified(t, pinned, testDate.Add(-72*time.Hour))
	pinned = fixture.runOK(t, testDate, "keep", pinned, "--until", testDate.Format(time.DateOnly))

	// Lexical order differs from age order, and deleting the oldest entry
	// alone suffices to meet the budget.
	newer := fixture.runOK(t, testDate, "new", "822/aaa-newer")
	newerFile := writePayload(t, newer, "abcd")
	setModified(t, newer, testDate.Add(-time.Hour))
	oldest := fixture.runOK(t, testDate, "new", "822/zzz-oldest")
	writePayload(t, oldest, "fg")
	setModified(t, oldest, testDate.Add(-48*time.Hour))

	if output := fixture.runOK(t, testDate, "prune", "--budget", "7B"); output != "822/zzz-oldest" {
		t.Fatalf("prune removed %q, want only the oldest entry", output)
	}
	assertMissing(t, oldest)
	checkPayload(t, newerFile, "abcd")
	pinnedFile := filepath.Join(pinned, "payload")
	checkPayload(t, pinnedFile, "pin")

	var out bytes.Buffer
	err := fixture.run([]string{"prune", "--budget", "2"}, &out, testDate)
	if err == nil || !strings.Contains(err.Error(), "over budget") {
		t.Fatalf("prune with a pin above budget: %v", err)
	}
	assertMissing(t, newer)
	checkPayload(t, pinnedFile, "pin")

	// The same entry becomes eligible when its pin expires.
	fixture.runOK(t, testDate.AddDate(0, 0, 1), "prune", "--budget", "2")
	assertMissing(t, pinned)
	assertMissing(t, filepath.Join(root, "822", "pinned"))
}

func TestNewOverBudget(t *testing.T) {
	fixture := newEvidenceFixture(t)
	root := fixture.location
	large := fixture.runOK(t, testDate, "new", "822/large")
	payload := writePayload(t, large, "")
	if err := os.Truncate(payload, 50<<30+1); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	err := fixture.run([]string{"new", "822/blocked"}, &out, testDate)
	if err == nil || !strings.Contains(err.Error(), "katl-dev-evidence list") || !strings.Contains(err.Error(), "release") {
		t.Fatalf("new over budget did not explain how to free space: %v", err)
	}
	assertMissing(t, filepath.Join(root, "822", "blocked"))

	if err := os.Truncate(payload, 50<<30); err != nil {
		t.Fatal(err)
	}
	fixture.runOK(t, testDate, "new", "822/at-budget")
}

func TestSizeWalkFailure(t *testing.T) {
	fixture := newEvidenceFixture(t)
	first := fixture.runOK(t, testDate, "new", "822/a-good")
	bad := fixture.runOK(t, testDate, "new", "822/b-unreadable")
	last := fixture.runOK(t, testDate, "new", "822/c-good")
	writePayload(t, first, "first")
	lastFile := writePayload(t, last, "last")
	unreadable := filepath.Join(bad, "nested")
	makeDir(t, unreadable)
	if err := os.Chmod(unreadable, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(unreadable, 0o700); err != nil {
			t.Error(err)
		}
	})
	if _, err := os.ReadDir(unreadable); err == nil {
		t.Skip("test requires the caller to be denied access to a mode 000 directory")
	} else if !errors.Is(err, os.ErrPermission) {
		t.Fatal(err)
	}

	for _, args := range [][]string{{"list"}, {"new", "822/created"}} {
		t.Run(args[0], func(t *testing.T) {
			var out, diagnostics bytes.Buffer
			if err := fixture.runWithDiagnostics(args, &out, &diagnostics, testDate); err != nil {
				t.Fatal(err)
			}
			if message := diagnostics.String(); !strings.Contains(message, bad) || !strings.Contains(message, os.ErrPermission.Error()) {
				t.Fatalf("expected an entry-specific permission error, got %q", message)
			}
			switch args[0] {
			case "list":
				rows := make(map[string]string)
				for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n")[1:] {
					if fields := strings.Fields(line); len(fields) >= 3 {
						rows[fields[0]+"/"+fields[1]] = fields[2]
					}
				}
				for id, size := range map[string]string{"822/a-good": "5", "822/b-unreadable": "unknown", "822/c-good": "4"} {
					if rows[id] != size {
						t.Errorf("want %s to report %s bytes, got:\n%s", id, size, &out)
					}
				}
			case "new":
				created := filepath.Join(fixture.location, "822", "created")
				if got := strings.TrimSpace(out.String()); got != created {
					t.Fatalf("new printed %q, want %q", got, created)
				}
				if info, err := os.Stat(created); err != nil || !info.IsDir() {
					t.Fatalf("new did not create the entry directory: %v", err)
				}

				// The failed entry must not hide measured bytes later in the scan.
				if err := os.Truncate(lastFile, 50<<30+1); err != nil {
					t.Fatal(err)
				}
				err := fixture.run([]string{"new", "822/blocked"}, &out, testDate)
				if err == nil || !strings.Contains(err.Error(), "over budget") {
					t.Fatalf("new with measured bytes over budget: %v", err)
				}
				assertMissing(t, filepath.Join(fixture.location, "822", "blocked"))
			}
		})
	}
}

func TestReleasePin(t *testing.T) {
	fixture := newEvidenceFixture(t)
	const until = "2026-11-26"
	deadline := time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC)
	entry := fixture.runOK(t, testDate, "new", "822/pinned")
	writePayload(t, entry, "pinned")
	pinned := fixture.runOK(t, testDate, "keep", entry, "--until", until)
	entry = fixture.runOK(t, testDate, "new", "822/expiring")
	expiring := fixture.runOK(t, testDate, "keep", entry, "--until", until)

	beforeExpiry := deadline.Add(-time.Nanosecond)
	err := fixture.run([]string{"release", "822/pinned"}, &bytes.Buffer{}, beforeExpiry)
	if err == nil || !strings.Contains(err.Error(), "822/pinned") ||
		!strings.Contains(err.Error(), deadline.Format(time.RFC3339)) || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("release of an active pin: %v", err)
	}
	checkPayload(t, filepath.Join(pinned, "payload"), "pinned")

	fixture.runOK(t, beforeExpiry, "release", pinned, "--force")
	assertMissing(t, pinned)
	fixture.runOK(t, deadline, "release", "822/expiring")
	assertMissing(t, expiring)
	ordinary := fixture.runOK(t, testDate, "new", "822/ordinary")
	fixture.runOK(t, testDate, "release", "822/ordinary")
	assertMissing(t, ordinary)
}

// removalCommands are the commands that remove entries; both must apply the
// same removal policy.
var removalCommands = map[string][]string{
	"release": {"release", "822/entry"},
	"prune":   {"prune", "--budget", "0"},
}

func TestRemoveRestrictedTree(t *testing.T) {
	for command, args := range removalCommands {
		t.Run(command, func(t *testing.T) {
			fixture := newEvidenceFixture(t)
			entry := fixture.runOK(t, testDate, "new", "822/entry")
			outside := t.TempDir()
			target := writePayload(t, outside, "outside")
			if err := os.Symlink(outside, filepath.Join(entry, "symlink")); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(target, filepath.Join(entry, "hard-link")); err != nil {
				t.Fatal(err)
			}
			readOnly := writePayload(t, entry, "read-only")
			modes := []struct {
				path string
				mode os.FileMode
			}{
				{target, 0o444},
				{outside, 0o555},
				{readOnly, 0o444},
			}
			// Deepest first, so each directory can still be populated.
			for _, dir := range []struct {
				name string
				mode os.FileMode
			}{
				{"a/b/c", 0o500},
				{"a/b", 0o700},
				{"a", 0o555},
				{"private", 0o700},
				{".", 0o555},
			} {
				path := filepath.Join(entry, dir.name)
				makeDir(t, path)
				if dir.name != "." {
					writePayload(t, path, dir.name)
				}
				modes = append(modes, struct {
					path string
					mode os.FileMode
				}{path, dir.mode})
			}
			for _, m := range modes {
				if err := os.Chmod(m.path, m.mode); err != nil {
					t.Fatal(err)
				}
			}
			// Leave anything a failed removal keeps removable by t.TempDir.
			t.Cleanup(func() {
				for _, m := range modes {
					if err := os.Chmod(m.path, 0o700); err != nil && !errors.Is(err, os.ErrNotExist) {
						t.Error(err)
					}
				}
			})

			if err := fixture.run(args, &bytes.Buffer{}, testDate); err != nil {
				t.Fatalf("%v: %v", args, err)
			}
			assertMissing(t, entry)
			checkPayload(t, target, "outside")
			for _, m := range modes[:2] {
				info, err := os.Stat(m.path)
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm() != m.mode {
					t.Errorf("%s changed mode of %q to %o, want %o", command, m.path, info.Mode().Perm(), m.mode)
				}
			}
		})
	}
}

func TestRemoveRefusesForeignOwner(t *testing.T) {
	const foreignUID = 4242
	for _, kind := range []string{"directory", "file"} {
		for command, args := range removalCommands {
			t.Run(kind+"/"+command, func(t *testing.T) {
				fixture := newEvidenceFixture(t)
				entry := fixture.runOK(t, testDate, "new", "822/entry")
				foreign := filepath.Join(entry, "nested", "foreign")
				makeDir(t, filepath.Dir(foreign))
				if kind == "directory" {
					makeDir(t, foreign)
					writePayload(t, foreign, "inside")
				} else if err := os.WriteFile(foreign, []byte("foreign"), 0o600); err != nil {
					t.Fatal(err)
				}
				var stat unix.Stat_t
				if err := unix.Lstat(foreign, &stat); err != nil {
					t.Fatal(err)
				}
				fixture.ownerOf = func(s unix.Stat_t) uint32 {
					if identity(s) == identity(stat) {
						return foreignUID
					}
					return s.Uid
				}

				err := fixture.run(args, &bytes.Buffer{}, testDate)
				if err == nil {
					t.Fatalf("%v removed a path owned by uid %d", args, foreignUID)
				}
				for _, want := range []string{foreign, "uid 4242", "run the command again"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not mention %q", err, want)
					}
				}
				if _, err := os.Lstat(foreign); err != nil {
					t.Errorf("%s did not keep the foreign path: %v", command, err)
				}
			})
		}
	}
}

func TestReleaseConfinesPaths(t *testing.T) {
	fixture := newEvidenceFixture(t)
	root := fixture.location
	outside := t.TempDir()
	outsideEntry := filepath.Join(outside, "entry")
	if err := os.Mkdir(outsideEntry, 0o700); err != nil {
		t.Fatal(err)
	}
	outsideFile := writePayload(t, outsideEntry, "outside")
	inside := fixture.runOK(t, testDate, "new", "822/inside")
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
			if err := fixture.run([]string{"release", operand}, &bytes.Buffer{}, testDate); err == nil {
				t.Errorf("release accepted %q", operand)
			}
			checkPayload(t, outsideFile, "outside")
			checkPayload(t, insideFile, "inside")
		})
	}
	fixture.runOK(t, testDate, "release", inside)
	assertMissing(t, inside)
	checkPayload(t, outsideFile, "outside")
}

type evidenceFixture struct {
	location     string
	beforeRemove func()
	deviceOf     func(unix.Stat_t) uint64
	ownerOf      func(unix.Stat_t) uint32
}

func newEvidenceFixture(t *testing.T) evidenceFixture {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return evidenceFixture{location: filepath.Join(dir, "evidence")}
}

func (f evidenceFixture) run(args []string, out *bytes.Buffer, now time.Time) error {
	return f.runWithDiagnostics(args, out, io.Discard, now)
}

func (f evidenceFixture) runWithDiagnostics(args []string, out, diagnostics io.Writer, now time.Time) error {
	return runWithStore(args, out, diagnostics, now, func() (*store, error) {
		s, err := openStore(f.location)
		if err != nil {
			return nil, err
		}
		s.beforeRemove = f.beforeRemove
		if f.deviceOf != nil {
			s.deviceOf = f.deviceOf
		}
		if f.ownerOf != nil {
			s.ownerOf = f.ownerOf
		}
		return s, nil
	})
}

func (f evidenceFixture) runOK(t *testing.T, now time.Time, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	if err := f.run(args, &out, now); err != nil {
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

func TestReleaseRejectsSymlinkedRootParent(t *testing.T) {
	fixture := newEvidenceFixture(t)
	entry := fixture.runOK(t, testDate, "new", "822/entry")
	payload := writePayload(t, entry, "evidence")
	parent := filepath.Dir(fixture.location)
	link := filepath.Join(parent, "linked-parent")
	if err := os.Symlink(parent, link); err != nil {
		t.Fatal(err)
	}
	fixture.location = filepath.Join(link, "evidence")

	err := fixture.run([]string{"release", "822/entry"}, &bytes.Buffer{}, testDate)
	if err == nil || !strings.Contains(err.Error(), filepath.Dir(fixture.location)) {
		t.Fatalf("release through symlinked root parent: %v", err)
	}
	checkPayload(t, payload, "evidence")
}

func TestReleaseRejectsReplacement(t *testing.T) {
	for _, change := range []string{"owner", "entry"} {
		t.Run(change, func(t *testing.T) {
			fixture := newEvidenceFixture(t)
			entry := fixture.runOK(t, testDate, "new", "822/entry")
			payload := writePayload(t, entry, "evidence")
			owner := filepath.Dir(entry)
			saved := filepath.Join(filepath.Dir(fixture.location), "saved")
			target := writePayload(t, t.TempDir(), "outside")
			link := filepath.Join(entry, "payload-link")
			fixture.beforeRemove = func() {
				from := entry
				if change == "owner" {
					from = owner
				}
				mustRename(t, from, saved)
				makeDir(t, entry)
				if err := os.Symlink(target, link); err != nil {
					t.Fatal(err)
				}
			}

			err := fixture.run([]string{"release", entry}, &bytes.Buffer{}, testDate)
			if err == nil || !strings.Contains(err.Error(), "changed during cleanup") {
				t.Fatalf("release after replacing %s: %v", change, err)
			}
			if change == "owner" {
				saved = filepath.Join(saved, filepath.Base(entry))
			}
			checkPayload(t, filepath.Join(saved, filepath.Base(payload)), "evidence")
			checkPayload(t, link, "outside")
		})
	}
}

func TestPruneProtectsPinDuringOwnerReplacement(t *testing.T) {
	fixture := newEvidenceFixture(t)
	pinned := fixture.runOK(t, testDate, "new", "822/entry")
	writePayload(t, pinned, "pinned")
	pinned = fixture.runOK(t, testDate, "keep", pinned, "--until", testDate.Format(time.DateOnly))
	ordinary := filepath.Join(fixture.location, "822", "entry")
	makeDir(t, ordinary)
	writePayload(t, ordinary, "ordinary")
	owner := filepath.Dir(ordinary)
	saved := filepath.Join(filepath.Dir(fixture.location), "saved")
	pinOwner, err := filepath.Rel(fixture.location, filepath.Dir(pinned))
	if err != nil {
		t.Fatal(err)
	}
	fixture.beforeRemove = func() {
		mustRename(t, owner, saved)
		if err := os.Symlink(pinOwner, owner); err != nil {
			t.Fatal(err)
		}
	}

	var out bytes.Buffer
	err = fixture.run([]string{"prune", "--budget", "0"}, &out, testDate)
	if err == nil || out.Len() != 0 {
		t.Fatalf("prune after redirecting owner into pin tree printed %q and returned %v", &out, err)
	}
	checkPayload(t, filepath.Join(pinned, "payload"), "pinned")
	checkPayload(t, filepath.Join(saved, "entry", "payload"), "ordinary")
}

func TestReleaseRefusesForeignDevice(t *testing.T) {
	fixture := newEvidenceFixture(t)
	entry := fixture.runOK(t, testDate, "new", "822/entry")
	foreign := filepath.Join(entry, "nested", "mount")
	makeDir(t, foreign)
	payload := writePayload(t, foreign, "mounted")
	var mounted unix.Stat_t
	if err := unix.Stat(foreign, &mounted); err != nil {
		t.Fatal(err)
	}
	fixture.deviceOf = func(stat unix.Stat_t) uint64 {
		if identity(stat) == identity(mounted) {
			return uint64(stat.Dev) + 1
		}
		return uint64(stat.Dev)
	}

	err := fixture.run([]string{"release", entry}, &bytes.Buffer{}, testDate)
	if err == nil || !strings.Contains(err.Error(), "mount") || !strings.Contains(err.Error(), foreign) {
		t.Fatalf("release with a foreign mount: %v", err)
	}
	checkPayload(t, payload, "mounted")
}

func TestPruneContinuesAfterPinCollision(t *testing.T) {
	fixture := newEvidenceFixture(t)
	original := fixture.runOK(t, testDate, "new", "822/aaa-collision")
	writePayload(t, original, "pinned")
	collision := fixture.runOK(t, testDate, "keep", original, "--until", testDate.Format(time.DateOnly))
	makeDir(t, original)
	recreated := writePayload(t, original, "recreated")
	unrelated := fixture.runOK(t, testDate, "new", "822/zzz-unrelated")
	writePayload(t, unrelated, "unrelated")
	laterPin := fixture.runOK(t, testDate, "keep", unrelated, "--until", testDate.Format(time.DateOnly))

	var out bytes.Buffer
	err := fixture.run([]string{"prune", "--budget", "0"}, &out, testDate.AddDate(0, 0, 1))
	if !errors.Is(err, os.ErrExist) || !strings.Contains(err.Error(), collision) || !strings.Contains(err.Error(), original) {
		t.Fatalf("prune did not report both conflicting paths: %v", err)
	}
	if strings.TrimSpace(out.String()) != "822/zzz-unrelated" {
		t.Fatalf("prune printed %q, want only the unrelated entry", &out)
	}
	checkPayload(t, recreated, "recreated")
	checkPayload(t, filepath.Join(collision, "payload"), "pinned")
	assertMissing(t, laterPin)
	assertMissing(t, unrelated)
}

func makeDir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

func mustRename(t *testing.T, from, to string) {
	t.Helper()
	if err := os.Rename(from, to); err != nil {
		t.Fatal(err)
	}
}
