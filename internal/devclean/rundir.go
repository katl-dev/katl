// Package devclean decides whether a developer's scratch state may be deleted.
package devclean

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// RunDirParent is the directory name vmtest run directories live under.
const RunDirParent = "katl-vmtest"

// RunDir is a vmtest run directory that may be cleaned.
type RunDir struct {
	// Path is the absolute path of the run directory.
	Path string
	// Present is false when the directory is already gone.
	Present bool

	verified bool
}

// CheckRunDir returns the run directory at path, or an error when path is not
// a vmtest run directory. A run directory is a direct, non-symlink child of a
// directory named katl-vmtest that records a result.json at or below it. A
// path that no longer exists is accepted when its location is that of a run
// directory, so repeating a cleanup succeeds.
func CheckRunDir(path string) (RunDir, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return RunDir{}, err
	}
	if filepath.Base(filepath.Dir(abs)) != RunDirParent {
		return RunDir{}, fmt.Errorf("%s is not a vmtest run directory: its parent directory is not named %s", abs, RunDirParent)
	}

	info, err := os.Lstat(abs)
	if errors.Is(err, fs.ErrNotExist) {
		return RunDir{Path: abs, verified: true}, nil
	}
	if err != nil {
		return RunDir{}, err
	}
	if !info.IsDir() {
		return RunDir{}, fmt.Errorf("%s is not a vmtest run directory: it is not a directory (symbolic links are refused)", abs)
	}

	found, err := containsResult(abs)
	if err != nil {
		return RunDir{}, err
	}
	if !found {
		return RunDir{}, fmt.Errorf("%s is not a vmtest run directory: it contains no result.json", abs)
	}
	return RunDir{Path: abs, Present: true, verified: true}, nil
}

func containsResult(dir string) (bool, error) {
	found := false
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			// Directories left unreadable by mkosi or libvirt are made
			// readable before deletion; an unreadable one cannot hide the
			// result.json that identifies the run, which vmtest writes at
			// the top or in readable node directories.
			if entry != nil && entry.IsDir() && errors.Is(err, fs.ErrPermission) {
				return fs.SkipDir
			}
			return err
		}
		if entry.Type().IsRegular() && entry.Name() == "result.json" {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	return found, err
}
