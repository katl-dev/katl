// Command katl-devclean decides whether scripts/vmtest-clean and
// scripts/worktree-clean may delete the path they were given, and deletes it.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/katl-dev/katl/internal/devclean"
)

const usage = "usage: katl-devclean run-dir <path> | remove run-dir <path> | remove owned-run-dir <run-id> <path> | remove worktree <repo> <path>"

// Exit statuses: a refusal changed nothing; an incomplete removal changed
// something, and repeating the command may finish it.
const (
	exitIncomplete = 1
	exitRefused    = 2
)

func main() {
	err := run(context.Background(), os.Args[1:])
	if err == nil {
		return
	}
	var left *devclean.LeftoverError
	if errors.As(err, &left) {
		fmt.Fprintln(os.Stderr, "incomplete:", err)
		os.Exit(exitIncomplete)
	}
	fmt.Fprintln(os.Stderr, "refused:", err)
	os.Exit(exitRefused)
}

// run prints key=value lines describing what the caller may delete, or what
// remove deleted.
func run(ctx context.Context, args []string) error {
	switch {
	case len(args) == 2 && args[0] == "run-dir":
		dir, err := devclean.CheckRunDir(args[1])
		if err != nil {
			return err
		}
		fmt.Printf("path=%s\npresent=%t\n", dir.Path, dir.Present)
	case len(args) == 3 && args[0] == "remove" && args[1] == "run-dir":
		dir, err := devclean.CheckRunDir(args[2])
		if err != nil {
			return err
		}
		if err := dir.Remove(ctx, devclean.PodmanUserNS{}); err != nil {
			return err
		}
		fmt.Printf("path=%s\n", dir.Path)
	case len(args) == 4 && args[0] == "remove" && args[1] == "owned-run-dir":
		dir, err := devclean.CheckOwnedRunDir(args[3], args[2])
		if err != nil {
			return err
		}
		if err := dir.Remove(ctx, devclean.PodmanUserNS{}); err != nil {
			return err
		}
		fmt.Printf("path=%s\n", dir.Path)
	case len(args) == 4 && args[0] == "remove" && args[1] == "worktree":
		lookup := devclean.GitHubLookup{Environ: os.Environ(), Repo: args[2]}
		wt, err := devclean.CheckWorktree(ctx, os.Environ(), lookup, args[2], args[3])
		if err != nil {
			return err
		}
		if err := wt.Remove(ctx, devclean.PodmanUserNS{}); err != nil {
			return err
		}
		fmt.Printf("path=%s\nbranch=%s\ndelete-branch=%t\nkeep-reason=%s\nmerged-pr=%d\ntip=%s\n", wt.Path, wt.Branch, wt.DeleteBranch, wt.KeepReason, wt.MergedPR, wt.Tip)
	default:
		return fmt.Errorf("%s", usage)
	}
	return nil
}
