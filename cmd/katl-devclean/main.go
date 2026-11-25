// Command katl-devclean decides whether scripts/vmtest-clean and
// scripts/worktree-clean may delete the path they were given. It deletes
// nothing itself.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/katl-dev/katl/internal/devclean"
)

const usage = "usage: katl-devclean run-dir <path> | worktree <repo> <path>"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "refused:", err)
		os.Exit(1)
	}
}

// run prints key=value lines describing what the caller may delete.
func run(args []string) error {
	switch {
	case len(args) == 2 && args[0] == "run-dir":
		dir, err := devclean.CheckRunDir(args[1])
		if err != nil {
			return err
		}
		fmt.Printf("path=%s\npresent=%t\n", dir.Path, dir.Present)
	case len(args) == 3 && args[0] == "worktree":
		wt, err := devclean.CheckWorktree(context.Background(), os.Environ(), args[1], args[2])
		if err != nil {
			return err
		}
		fmt.Printf("path=%s\nbranch=%s\ndelete-branch=%t\nkeep-reason=%s\n", wt.Path, wt.Branch, wt.DeleteBranch, wt.KeepReason)
	default:
		return fmt.Errorf("%s", usage)
	}
	return nil
}
