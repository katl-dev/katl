package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/katl-dev/katl/internal/gittest"
	"github.com/katl-dev/katl/internal/reviewrecord"
)

func TestReviewAcrossRebases(t *testing.T) {
	repo := fixtureRepo{t: t, dir: t.TempDir()}
	repo.git("init", "--quiet", "--initial-branch=main")
	repo.git("commit", "--quiet", "--allow-empty", "-m", "root")
	base := repo.commit(repo.git("rev-parse", "HEAD"), map[string]string{"policy": "original\n"})
	reviewed := repo.commit(base, map[string]string{"policy": "reviewed\n"})
	cleanBase := repo.commit(base, map[string]string{"unrelated": "new main content\n"})
	conflictBase := repo.commit(base, map[string]string{"policy": "new main policy\n"})

	repo.git("checkout", "--quiet", "--detach", reviewed)
	repo.git("rebase", cleanBase)
	rebased := repo.git("rev-parse", "HEAD")
	advancedBase := repo.commit(cleanBase, map[string]string{"another": "later main change\n"})
	extra := repo.commit(rebased, map[string]string{"extra": "unreviewed\n"})

	repo.git("checkout", "--quiet", "--detach", reviewed)
	cmd := exec.Command("git", "rebase", conflictBase)
	cmd.Dir, cmd.Env = repo.dir, gittest.Env(os.Environ(), repo.dir)
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("expected conflict, got %s", out)
	}
	if got := repo.git("diff", "--name-only", "--diff-filter=U"); got != "policy" {
		t.Fatalf("conflicted paths = %q, want policy", got)
	}
	if err := os.WriteFile(filepath.Join(repo.dir, "policy"), []byte("combined policy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	repo.git("add", "policy")
	repo.git("-c", "core.editor=true", "rebase", "--continue")
	resolved := repo.git("rev-parse", "HEAD")

	objects, err := newScratchObjects(repo.dir, gittest.Env(os.Environ(), repo.dir))
	if err != nil {
		t.Fatal(err)
	}
	defer objects.Close()
	comments := []reviewrecord.Comment{{
		ID: 1, Member: true,
		Body: fmt.Sprintf("<!-- katl-queue:review approved:%s:%s -->", reviewed, base),
	}}

	for _, test := range []struct {
		name       string
		base, head string
		want       reviewrecord.Match
	}{
		{"identical patch", cleanBase, rebased, reviewrecord.EquivalentPatch},
		{"identical patch after main advances", advancedBase, rebased, reviewrecord.EquivalentPatch},
		{"resolved conflict", conflictBase, resolved, reviewrecord.Stale},
		{"extra change", cleanBase, extra, reviewrecord.Stale},
	} {
		t.Run(test.name, func(t *testing.T) {
			record, match, err := reviewrecord.Select(comments, test.head, test.base, func(base, head string) (string, error) {
				return objects.patchID(t.Context(), base, head)
			})
			if err != nil || match != test.want || record.Verdict != reviewrecord.Approved || record.Head != reviewed {
				t.Errorf("Select = %+v, %v, %v; want original approval with match %v", record, match, err, test.want)
			}
		})
	}
}
