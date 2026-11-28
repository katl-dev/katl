package persistedrecord

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const entryPath = "/efi/loader/entries/katl-candidate.conf"

func TestWriteFileAtomicCrash(t *testing.T) {
	for _, tt := range []struct {
		name string
		old  []byte
	}{
		{name: "replace", old: []byte("title old\n")},
		{name: "create"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			want := []byte("title new\n")
			crashEachStep(t, func(fs *crashFS) {
				if tt.old != nil {
					fs.seed(entryPath, tt.old)
				}
			}, func() error {
				return WriteFileAtomic(entryPath, want, 0o644)
			}, func(t *testing.T, fs *crashFS, returned bool) {
				got, ok := fs.read(entryPath)
				switch {
				case returned && !bytes.Equal(got, want):
					t.Fatalf("after a successful return the entry is %q (present %v), want %q", got, ok, want)
				case ok && !bytes.Equal(got, want) && !bytes.Equal(got, tt.old):
					t.Fatalf("entry is %q, want the old %q or the new %q", got, tt.old, want)
				case !ok && tt.old != nil:
					t.Fatalf("entry vanished; want the old or the new content")
				}
				assertOnlyHiddenTemporaries(t, fs, filepath.Dir(entryPath), filepath.Base(entryPath))
			})
		})
	}
}

func TestWriteFileNoReplaceCrash(t *testing.T) {
	want := []byte("record\n")
	path := "/var/lib/katl/boot/record.json"
	crashEachStep(t, func(*crashFS) {}, func() error {
		return WriteFileNoReplace(path, want, 0o600, 0o700)
	}, func(t *testing.T, fs *crashFS, returned bool) {
		got, ok := fs.read(path)
		if returned && !bytes.Equal(got, want) {
			t.Fatalf("after a successful return the record is %q (present %v), want %q", got, ok, want)
		}
		if ok && !bytes.Equal(got, want) {
			t.Fatalf("record is %q, want absent or %q", got, want)
		}
	})
}

func TestRemoveFileCrash(t *testing.T) {
	old := []byte("intent\n")
	path := "/var/lib/katl/boot/move.json"
	crashEachStep(t, func(fs *crashFS) { fs.seed(path, old) }, func() error {
		return RemoveFile(path)
	}, func(t *testing.T, fs *crashFS, returned bool) {
		got, ok := fs.read(path)
		if returned && ok {
			t.Fatalf("after a successful return the record is still present")
		}
		if ok && !bytes.Equal(got, old) {
			t.Fatalf("record is %q, want absent or %q", got, old)
		}
	})
}

func TestSyncFileCrash(t *testing.T) {
	want := []byte("MZ uki\n")
	path := "/efi/EFI/katl/katl_1.efi"
	crashEachStep(t, func(fs *crashFS) { fs.seedVolatile(path, want) }, func() error {
		return SyncFile(path)
	}, func(t *testing.T, fs *crashFS, returned bool) {
		got, ok := fs.read(path)
		if returned && !bytes.Equal(got, want) {
			t.Fatalf("after a successful return the UKI is %q (present %v), want %q", got, ok, want)
		}
	})
}

// A reference written after its target never names a target that a crash
// can lose, which is the ordering boot selection relies on.
func TestReferenceAfterTargetCrash(t *testing.T) {
	target := []byte("title candidate\n")
	reference := "/sys/firmware/efi/efivars/LoaderEntryOneShot"
	crashEachStep(t, func(*crashFS) {}, func() error {
		if err := WriteFileAtomic(entryPath, target, 0o644); err != nil {
			return err
		}
		return WriteFileAtomic(reference, []byte(filepath.Base(entryPath)), 0o644)
	}, func(t *testing.T, fs *crashFS, _ bool) {
		named, ok := fs.read(reference)
		if !ok {
			return
		}
		got, present := fs.read(filepath.Join(filepath.Dir(entryPath), string(named)))
		if !present || !bytes.Equal(got, target) {
			t.Fatalf("reference names %s but the entry is %q (present %v)", named, got, present)
		}
	})
}

// crashEachStep runs operation once per filesystem step, losing power at
// that step, and checks the state that survives. The last run completes
// without a crash and then loses power, so check also sees what a successful
// return leaves durable.
func crashEachStep(t *testing.T, seed func(*crashFS), operation func() error, check func(t *testing.T, fs *crashFS, returned bool)) {
	t.Helper()
	for step := 1; ; step++ {
		fs := newCrashFS(step)
		seed(fs)
		previous := fsys
		fsys = fs
		err := operation()
		fsys = previous

		completed := !fs.crashed
		if completed && err != nil {
			t.Fatalf("step %d: operation failed without a crash: %v", step, err)
		}
		if !completed && !errors.Is(err, errPowerLoss) {
			t.Fatalf("step %d: operation error = %v, want power loss", step, err)
		}
		fs.powerLoss()
		t.Run(fmt.Sprintf("crash at step %d", step), func(t *testing.T) {
			check(t, fs, completed)
		})
		if completed {
			if step < 3 {
				t.Fatalf("operation finished after %d steps; the crash model is not exercised", step)
			}
			return
		}
	}
}

func assertOnlyHiddenTemporaries(t *testing.T, fs *crashFS, dir string, name string) {
	t.Helper()
	for _, entry := range fs.list(dir) {
		if entry == name {
			continue
		}
		if matched, _ := filepath.Match("*.conf", entry); matched || !strings.HasPrefix(entry, ".") {
			t.Fatalf("leftover %q would be read as a boot entry or record", entry)
		}
	}
}

var errPowerLoss = errors.New("power lost")

// crashFS models what survives a power loss: file content only after the file
// is synced, and directory entries only after the directory is synced.
// Directories themselves are durable once created.
type crashFS struct {
	crashAt int
	steps   int
	crashed bool
	temps   int
	dirs    map[string]*crashDir
}

type crashDir struct {
	entries map[string]*crashInode
	durable map[string]*crashInode
}

type crashInode struct {
	data    []byte
	durable []byte
}

func newCrashFS(crashAt int) *crashFS {
	return &crashFS{crashAt: crashAt, dirs: map[string]*crashDir{}}
}

func (fs *crashFS) step() error {
	if fs.crashed {
		return errPowerLoss
	}
	fs.steps++
	if fs.steps == fs.crashAt {
		fs.crashed = true
		return errPowerLoss
	}
	return nil
}

func (fs *crashFS) dir(path string) *crashDir {
	dir, ok := fs.dirs[path]
	if !ok {
		dir = &crashDir{entries: map[string]*crashInode{}, durable: map[string]*crashInode{}}
		fs.dirs[path] = dir
	}
	return dir
}

// seedVolatile adds a file another program wrote without syncing it.
func (fs *crashFS) seedVolatile(path string, data []byte) {
	fs.dir(filepath.Dir(path)).entries[filepath.Base(path)] = &crashInode{data: bytes.Clone(data)}
}

func (fs *crashFS) seed(path string, data []byte) {
	inode := &crashInode{data: bytes.Clone(data), durable: bytes.Clone(data)}
	dir := fs.dir(filepath.Dir(path))
	dir.entries[filepath.Base(path)] = inode
	dir.durable[filepath.Base(path)] = inode
}

func (fs *crashFS) powerLoss() {
	for _, dir := range fs.dirs {
		dir.entries = map[string]*crashInode{}
		for name, inode := range dir.durable {
			inode.data = bytes.Clone(inode.durable)
			dir.entries[name] = inode
		}
	}
}

func (fs *crashFS) read(path string) ([]byte, bool) {
	dir, ok := fs.dirs[filepath.Dir(path)]
	if !ok {
		return nil, false
	}
	inode, ok := dir.entries[filepath.Base(path)]
	if !ok {
		return nil, false
	}
	return inode.data, true
}

func (fs *crashFS) list(path string) []string {
	var names []string
	for name := range fs.dir(path).entries {
		names = append(names, name)
	}
	return names
}

func (fs *crashFS) MkdirAll(path string, _ os.FileMode) error {
	if err := fs.step(); err != nil {
		return err
	}
	fs.dir(path)
	return nil
}

func (fs *crashFS) CreateTemp(dir, pattern string) (file, error) {
	if err := fs.step(); err != nil {
		return nil, err
	}
	fs.temps++
	name := filepath.Join(dir, fmt.Sprintf("%s%d", pattern, fs.temps))
	inode := &crashInode{}
	fs.dir(dir).entries[filepath.Base(name)] = inode
	return &crashFile{fs: fs, name: name, inode: inode}, nil
}

func (fs *crashFS) OpenFile(name string, flag int, _ os.FileMode) (file, error) {
	if err := fs.step(); err != nil {
		return nil, err
	}
	dir := fs.dir(filepath.Dir(name))
	existing, ok := dir.entries[filepath.Base(name)]
	switch {
	case ok && flag&os.O_EXCL != 0:
		return nil, os.ErrExist
	case ok && flag&os.O_CREATE == 0:
		return &crashFile{fs: fs, name: name, inode: existing}, nil
	case !ok && flag&os.O_CREATE == 0:
		return nil, os.ErrNotExist
	}
	inode := &crashInode{}
	dir.entries[filepath.Base(name)] = inode
	return &crashFile{fs: fs, name: name, inode: inode}, nil
}

func (fs *crashFS) Rename(oldpath, newpath string) error {
	if err := fs.step(); err != nil {
		return err
	}
	from := fs.dir(filepath.Dir(oldpath))
	inode, ok := from.entries[filepath.Base(oldpath)]
	if !ok {
		return os.ErrNotExist
	}
	delete(from.entries, filepath.Base(oldpath))
	fs.dir(filepath.Dir(newpath)).entries[filepath.Base(newpath)] = inode
	return nil
}

func (fs *crashFS) Remove(name string) error {
	if err := fs.step(); err != nil {
		return err
	}
	dir := fs.dir(filepath.Dir(name))
	if _, ok := dir.entries[filepath.Base(name)]; !ok {
		return os.ErrNotExist
	}
	delete(dir.entries, filepath.Base(name))
	return nil
}

func (fs *crashFS) SyncDir(path string) error {
	if err := fs.step(); err != nil {
		return err
	}
	dir := fs.dir(path)
	dir.durable = map[string]*crashInode{}
	for name, inode := range dir.entries {
		dir.durable[name] = inode
	}
	return nil
}

type crashFile struct {
	fs    *crashFS
	name  string
	inode *crashInode
}

func (f *crashFile) Name() string { return f.name }

func (f *crashFile) Write(data []byte) (int, error) {
	if err := f.fs.step(); err != nil {
		return 0, err
	}
	f.inode.data = append(f.inode.data, data...)
	return len(data), nil
}

func (f *crashFile) Chmod(os.FileMode) error { return f.fs.step() }

func (f *crashFile) Sync() error {
	if err := f.fs.step(); err != nil {
		return err
	}
	f.inode.durable = bytes.Clone(f.inode.data)
	return nil
}

func (f *crashFile) Close() error { return f.fs.step() }

func TestSyncTreeAcceptsGenerationContent(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "confext", "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "confext", "etc", "sysctl.conf"), []byte("vm.swappiness = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing-target", filepath.Join(root, "confext", "dangling")); err != nil {
		t.Fatal(err)
	}

	if err := SyncTree(root); err != nil {
		t.Fatalf("SyncTree() error = %v", err)
	}
}
