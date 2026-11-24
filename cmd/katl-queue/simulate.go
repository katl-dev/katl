package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// scratchObjects runs git against a repository with a throwaway object
// directory layered over the repository's own. Fetched objects, merge
// results and simulation commits land only in the throwaway directory, and no
// command it runs names a ref, so branches, the index and the working tree
// are never touched.
type scratchObjects struct {
	repo string
	dir  string
	env  []string
}

func newScratchObjects(repo string, env []string) (*scratchObjects, error) {
	cmd := exec.Command("git", "rev-parse", "--path-format=absolute", "--git-path", "objects")
	cmd.Dir = repo
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("locate repository objects: %w", err)
	}
	objects := strings.TrimSpace(string(out))

	dir, err := os.MkdirTemp("", "katl-queue-objects-")
	if err != nil {
		return nil, err
	}
	return &scratchObjects{
		repo: repo,
		dir:  dir,
		env: append(
			append([]string(nil), env...),
			"GIT_OBJECT_DIRECTORY="+dir,
			"GIT_ALTERNATE_OBJECT_DIRECTORIES="+objects,
			"GIT_AUTHOR_NAME=katl-queue",
			"GIT_AUTHOR_EMAIL=katl-queue@invalid",
			"GIT_COMMITTER_NAME=katl-queue",
			"GIT_COMMITTER_EMAIL=katl-queue@invalid",
		),
	}, nil
}

func (s *scratchObjects) Close() error {
	return os.RemoveAll(s.dir)
}

func (s *scratchObjects) git(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "gc.auto=0", "-c", "maintenance.auto=false"}, args...)...)
	cmd.Dir = s.repo
	cmd.Env = s.env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return string(out), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

// fetch copies the named commits from remote without writing any ref or
// FETCH_HEAD.
func (s *scratchObjects) fetch(ctx context.Context, remote string, oids ...string) error {
	args := append([]string{"fetch", "--quiet", "--no-tags", "--no-write-fetch-head", remote}, oids...)
	_, err := s.git(ctx, args...)
	return err
}

// merge merges two commits and returns the merged tree, or the conflicted
// paths when they do not merge cleanly.
func (s *scratchObjects) merge(ctx context.Context, ours, theirs string) (tree string, conflicts []string, err error) {
	out, err := s.git(ctx, "merge-tree", "--write-tree", "--name-only", "--no-messages", ours, theirs)
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	var exit *exec.ExitError
	switch {
	case err == nil:
		return lines[0], nil, nil
	case errors.As(err, &exit) && exit.ExitCode() == 1 && len(lines) > 1:
		return "", dedupe(lines[1:]), nil
	default:
		return "", nil, err
	}
}

func (s *scratchObjects) commit(ctx context.Context, tree string, parents ...string) (string, error) {
	args := []string{"commit-tree", tree, "-m", "katl-queue simulation"}
	for _, p := range parents {
		args = append(args, "-p", p)
	}
	out, err := s.git(ctx, args...)
	return strings.TrimSpace(out), err
}

func dedupe(paths []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range paths {
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

type queuedPR struct {
	Number int
	Head   string
}

type simulation struct {
	// Applied lists the queued pull requests the candidate was merged
	// over, in queue order.
	Applied []int
	// Evicted lists queued pull requests that conflict with an entry ahead
	// of them; the queue will remove them, so they are left out.
	Evicted []int
	// ConflictWith is the queued pull request whose addition first makes
	// the candidate conflict, or 0 when it conflicts with the base itself.
	ConflictWith int
	Conflicts    []string
}

func (s simulation) clean() bool { return len(s.Conflicts) == 0 }

// simulate merges candidate onto base plus the queue, in queue order, the way
// the merge queue would build its merge group. A candidate already in the
// queue is merged over only the entries ahead of it, since entries behind it
// are built on top of it.
func simulate(ctx context.Context, objects *scratchObjects, base string, queue []queuedPR, candidate queuedPR) (simulation, error) {
	var sim simulation
	for i, pr := range queue {
		if pr.Number == candidate.Number {
			queue = queue[:i]
			break
		}
	}

	// states[i] is base with the first i applied entries merged.
	states := []string{base}
	for _, pr := range queue {
		tip := states[len(states)-1]
		tree, conflicts, err := objects.merge(ctx, tip, pr.Head)
		if err != nil {
			return sim, err
		}
		if conflicts != nil {
			sim.Evicted = append(sim.Evicted, pr.Number)
			continue
		}
		next, err := objects.commit(ctx, tree, tip, pr.Head)
		if err != nil {
			return sim, err
		}
		states = append(states, next)
		sim.Applied = append(sim.Applied, pr.Number)
	}

	for i, state := range states {
		_, conflicts, err := objects.merge(ctx, state, candidate.Head)
		if err != nil {
			return sim, err
		}
		if conflicts != nil {
			if i > 0 {
				sim.ConflictWith = sim.Applied[i-1]
			}
			sim.Conflicts = conflicts
			return sim, nil
		}
	}
	return sim, nil
}
