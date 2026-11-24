package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// hermeticGitEnv confines git to dir: no system or global config, no
// repository discovery above it, and a fixed identity.
func hermeticGitEnv(dir string) []string {
	var env []string
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "GIT_") {
			env = append(env, value)
		}
	}
	return append(
		env,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CEILING_DIRECTORIES="+filepath.Dir(dir),
		"GIT_AUTHOR_NAME=Katl Test",
		"GIT_AUTHOR_EMAIL=test@katl.dev",
		"GIT_COMMITTER_NAME=Katl Test",
		"GIT_COMMITTER_EMAIL=test@katl.dev",
	)
}

type fixtureRepo struct {
	t   *testing.T
	dir string
}

func (r fixtureRepo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = r.dir
	cmd.Env = hermeticGitEnv(r.dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// commit records files on top of parent and returns the new commit, without
// moving any branch.
func (r fixtureRepo) commit(parent string, files map[string]string) string {
	r.t.Helper()
	r.git("checkout", "--quiet", "--detach", parent)
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(r.dir, name), []byte(content), 0o644); err != nil {
			r.t.Fatal(err)
		}
	}
	r.git("add", "--all")
	r.git("commit", "--quiet", "-m", "change")
	return r.git("rev-parse", "HEAD")
}

func TestSimulate(t *testing.T) {
	repo := fixtureRepo{t: t, dir: t.TempDir()}
	repo.git("init", "--quiet", "--initial-branch=main")
	repo.git("commit", "--quiet", "--allow-empty", "-m", "root")
	root := repo.git("rev-parse", "HEAD")
	base := repo.commit(root, map[string]string{"a": "a\n", "b": "b\n", "c": "c\n"})

	first := queuedPR{Number: 1, Head: repo.commit(base, map[string]string{"a": "first\n"})}
	second := queuedPR{Number: 2, Head: repo.commit(base, map[string]string{"b": "second\n"})}
	clashesWithFirst := queuedPR{Number: 3, Head: repo.commit(base, map[string]string{"a": "third\n"})}
	touchesC := queuedPR{Number: 4, Head: repo.commit(base, map[string]string{"c": "fourth\n"})}
	clashesWithFourth := queuedPR{Number: 5, Head: repo.commit(base, map[string]string{"c": "fifth\n"})}
	movedBase := repo.commit(base, map[string]string{"c": "main\n"})

	tests := []struct {
		name      string
		base      string
		queue     []queuedPR
		candidate queuedPR
		want      simulation
	}{
		{
			name:      "clean",
			base:      base,
			queue:     []queuedPR{first, second},
			candidate: touchesC,
			want:      simulation{Applied: []int{1, 2}},
		},
		{
			name:      "conflicts with the first queued PR",
			base:      base,
			queue:     []queuedPR{first, second},
			candidate: clashesWithFirst,
			want:      simulation{Applied: []int{1, 2}, ConflictWith: 1, Conflicts: []string{"a"}},
		},
		{
			name:      "conflicts only with a later queued PR",
			base:      base,
			queue:     []queuedPR{first, touchesC},
			candidate: clashesWithFourth,
			want:      simulation{Applied: []int{1, 4}, ConflictWith: 4, Conflicts: []string{"c"}},
		},
		{
			name:      "conflicts with the base",
			base:      movedBase,
			queue:     []queuedPR{first},
			candidate: touchesC,
			want:      simulation{Applied: []int{1}, Conflicts: []string{"c"}},
		},
		{
			// The candidate is skipped, and the entry behind it, which
			// conflicts with it, is the queue's problem rather than the
			// candidate's.
			name:      "already queued",
			base:      base,
			queue:     []queuedPR{first, touchesC, clashesWithFourth},
			candidate: touchesC,
			want:      simulation{Applied: []int{1}},
		},
		{
			name:      "queued PR conflicting with an entry ahead is left out",
			base:      base,
			queue:     []queuedPR{first, clashesWithFirst, second},
			candidate: touchesC,
			want:      simulation{Applied: []int{1, 2}, Evicted: []int{3}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			refsBefore := repo.git("for-each-ref")
			headBefore := repo.git("rev-parse", "HEAD")
			objectsBefore := repo.git("count-objects")

			objects, err := newScratchObjects(repo.dir, hermeticGitEnv(repo.dir))
			if err != nil {
				t.Fatal(err)
			}
			got, err := simulate(context.Background(), objects, tt.base, tt.queue, tt.candidate)
			if err != nil {
				t.Fatal(err)
			}
			if err := objects.Close(); err != nil {
				t.Fatal(err)
			}

			if !slices.Equal(got.Applied, tt.want.Applied) || !slices.Equal(got.Evicted, tt.want.Evicted) ||
				got.ConflictWith != tt.want.ConflictWith || !slices.Equal(got.Conflicts, tt.want.Conflicts) {
				t.Errorf("simulate() = %+v, want %+v", got, tt.want)
			}

			if refs := repo.git("for-each-ref"); refs != refsBefore {
				t.Errorf("refs changed:\n%s\nwant:\n%s", refs, refsBefore)
			}
			if head := repo.git("rev-parse", "HEAD"); head != headBefore {
				t.Errorf("HEAD moved to %s, want %s", head, headBefore)
			}
			if status := repo.git("status", "--porcelain"); status != "" {
				t.Errorf("working tree changed:\n%s", status)
			}
			if count := repo.git("count-objects"); count != objectsBefore {
				t.Errorf("repository objects changed: %s, want %s", count, objectsBefore)
			}
		})
	}
}
