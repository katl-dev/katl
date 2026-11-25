// Package devclean decides whether a developer's scratch state may be deleted.
package devclean

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// RunDirParent is the directory name vmtest run directories live under.
const RunDirParent = "katl-vmtest"

// RunDirMarker is the file vmtest-run writes into every run directory it
// creates, holding the run ID.
const RunDirMarker = ".katl-vmtest-run-dir"

// RunDir is a vmtest run directory that may be cleaned.
type RunDir struct {
	// Path is the absolute path of the run directory, with symbolic links
	// in its ancestors resolved.
	Path string
	// Present is false when the directory is already gone.
	Present bool

	// root is the path that was verified; Remove deletes nothing else.
	root     string
	verified bool
}

// CheckRunDir returns the run directory at path, or an error when path is not
// a vmtest run directory. A run directory is a direct, non-symlink child of a
// directory named katl-vmtest, once symbolic links in its ancestors are
// resolved, that holds the marker vmtest-run writes. A path that no longer
// exists is accepted when its location is that of a run directory, so
// repeating a cleanup succeeds.
func CheckRunDir(path string) (RunDir, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return RunDir{}, err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if errors.Is(err, fs.ErrNotExist) {
		parent, err = filepath.Dir(abs), nil
	}
	if err != nil {
		return RunDir{}, err
	}
	if filepath.Base(parent) != RunDirParent {
		return RunDir{}, fmt.Errorf("%s is not a vmtest run directory: its parent directory is not named %s", abs, RunDirParent)
	}
	abs = filepath.Join(parent, filepath.Base(abs))

	info, err := os.Lstat(abs)
	if errors.Is(err, fs.ErrNotExist) {
		return RunDir{Path: abs, root: abs, verified: true}, nil
	}
	if err != nil {
		return RunDir{}, err
	}
	if !info.IsDir() {
		return RunDir{}, fmt.Errorf("%s is not a vmtest run directory: it is not a directory (symbolic links are refused)", abs)
	}
	if marker, err := os.Lstat(filepath.Join(abs, RunDirMarker)); err != nil || !marker.Mode().IsRegular() {
		return RunDir{}, fmt.Errorf("%s is not a vmtest run directory: it has no %s marker", abs, RunDirMarker)
	}
	return RunDir{Path: abs, Present: true, root: abs, verified: true}, nil
}

// CheckOwnedRunDir returns the run directory at path when its marker holds
// runID, which is how vmtest-run recognises a directory it created wherever
// the operator placed it.
func CheckOwnedRunDir(path, runID string) (RunDir, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return RunDir{}, err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return RunDir{}, err
	}
	abs = filepath.Join(parent, filepath.Base(abs))

	info, err := os.Lstat(abs)
	if err != nil {
		return RunDir{}, err
	}
	if !info.IsDir() {
		return RunDir{}, fmt.Errorf("%s is not a vmtest run directory: it is not a directory (symbolic links are refused)", abs)
	}
	got, err := os.ReadFile(filepath.Join(abs, RunDirMarker))
	if err != nil || runID == "" || strings.TrimSpace(string(got)) != runID {
		return RunDir{}, fmt.Errorf("%s is not the run directory of run %q: its %s marker does not name that run", abs, runID, RunDirMarker)
	}
	return RunDir{Path: abs, Present: true, root: abs, verified: true}, nil
}
