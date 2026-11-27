package workstationtest

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestWorktreeTempDirRemovesAbandonedFixtures(t *testing.T) {
	worktree := t.TempDir()
	live := WorktreeTempDir(t, worktree)
	// A killed test leaves its directory and an owner lock that nobody holds.
	abandoned := filepath.Join(worktree, "_build", "test-fixtures", "test-killed")
	if err := os.MkdirAll(abandoned, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(abandoned, ".owner"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	next := WorktreeTempDir(t, worktree)

	if _, err := os.Stat(abandoned); !os.IsNotExist(err) {
		t.Errorf("abandoned fixture directory survived: %v", err)
	}
	for _, dir := range []string{live, next} {
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("fixture directory of a live test was removed: %v", err)
		}
	}
}

// failureRecorder records the failures that the code under test reports, so a
// test can assert that it reported none.
type failureRecorder struct {
	testing.TB
	failures []string
}

func (r *failureRecorder) Fatal(args ...any) { r.failures = append(r.failures, fmt.Sprint(args...)) }
func (r *failureRecorder) Error(args ...any) { r.failures = append(r.failures, fmt.Sprint(args...)) }

func TestWorktreeTempDirIgnoresUnremovableLeftovers(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can open the unreadable owner lock")
	}
	worktree := t.TempDir()
	// Another user's leftover, whose owner lock this test cannot open.
	foreign := filepath.Join(worktree, "_build", "test-fixtures", "test-foreign")
	if err := os.MkdirAll(foreign, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(foreign, ".owner"), nil, 0); err != nil {
		t.Fatal(err)
	}

	recorder := &failureRecorder{TB: t}
	dir := WorktreeTempDir(recorder, worktree)

	if len(recorder.failures) != 0 {
		t.Errorf("an unremovable leftover failed the test: %v", recorder.failures)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("no fixture directory: %v", err)
	}
}
