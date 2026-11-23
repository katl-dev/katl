package main

import "testing"

// A Git worktree nested inside a jj workspace, and a jj workspace nested inside
// a Git checkout, each own their own _build state.
func TestNearestRoot(t *testing.T) {
	for _, roots := range [][]string{
		{"/src/katl", "/src/katl/.claude/worktrees/agent"},
		{"/src/katl/.claude/worktrees/agent", "/src/katl"},
	} {
		got, err := nearestRoot(roots)
		if err != nil {
			t.Fatal(err)
		}
		if got != "/src/katl/.claude/worktrees/agent" {
			t.Errorf("nearestRoot(%q) = %q, want the nested checkout", roots, got)
		}
	}
}
