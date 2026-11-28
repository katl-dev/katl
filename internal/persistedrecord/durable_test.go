package persistedrecord

import (
	"bytes"
	"errors"
	"fmt"
	iofs "io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
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

// A tree built with ordinary writes, as a generation's content is, is wholly
// durable once SyncTree returns. The root's own entry belongs to its parent,
// which the caller syncs.
func TestSyncTreeCrash(t *testing.T) {
	root := "/var/lib/katl/generations/candidate"
	files := map[string][]byte{
		root + "/kernel.cmdline":                 []byte("root=PARTUUID=x\n"),
		root + "/confext/etc/sysctl.d/katl.conf": []byte("vm.swappiness = 1\n"),
		root + "/sysext/kubernetes.raw":          []byte("hsqs kubernetes\n"),
	}
	crashEachStep(t, func(fs *crashFS) {
		fs.dir(root)
		for _, dir := range []string{"/confext", "/confext/etc", "/confext/etc/sysctl.d", "/sysext"} {
			fs.seedVolatileDir(root + dir)
		}
		for path, data := range files {
			fs.seedVolatile(path, data)
		}
	}, func() error {
		return SyncTree(root)
	}, func(t *testing.T, fs *crashFS, returned bool) {
		if !returned {
			return
		}
		for path, want := range files {
			if got, ok := fs.read(path); !bytes.Equal(got, want) {
				t.Errorf("after a successful return %s is %q (present %v), want %q", path, got, ok, want)
			}
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
// that step, and checks the state that survives, under each filesystem model.
// The last run completes without a crash and then loses power, so check also
// sees what a successful return leaves durable.
func crashEachStep(t *testing.T, seed func(*crashFS), operation func() error, check func(t *testing.T, fs *crashFS, returned bool)) {
	t.Helper()
	for _, model := range []crashModel{journaled, fat} {
		t.Run(model.String(), func(t *testing.T) {
			for step := 1; ; step++ {
				fs := newCrashFS(model, step)
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
		})
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

type crashModel int

const (
	// journaled models ext4: content survives once the file is synced, and a
	// name, including a rename, once its directory is synced.
	journaled crashModel = iota
	// fat models vfat, which holds the ESP. A file's size and first cluster
	// live in its directory slot, which a file sync writes. A rename gives the
	// new name a fresh slot (size 0) or keeps the replaced file's slot, and the
	// moved file's metadata reaches that slot only when the file is written
	// back again. A directory sync writes the slots as they are.
	fat
)

func (m crashModel) String() string {
	if m == fat {
		return "vfat"
	}
	return "ext4"
}

// crashFS models what survives a power loss under one crashModel.
// Directories are durable once created, except those added with
// seedVolatileDir, which survive only once their parent directory is synced.
type crashFS struct {
	model   crashModel
	crashAt int
	steps   int
	crashed bool
	temps   int
	dirs    map[string]*crashDir
}

type crashDir struct {
	// names is the live namespace.
	names map[string]*crashInode
	// durable is the journaled model's on-disk namespace.
	durable map[string]*crashInode
	// slots and durableSlots are the fat model's directory slots.
	slots        map[string]crashSlot
	durableSlots map[string]crashSlot
}

// crashSlot is a vfat directory slot: the file its cluster chain points at and
// the size it records. A nil inode is a slot with no clusters.
type crashSlot struct {
	inode *crashInode
	size  int
}

type crashInode struct {
	data    []byte
	durable []byte
	path    string
	// dir marks the entry of a directory added with seedVolatileDir.
	dir bool
}

func newCrashFS(model crashModel, crashAt int) *crashFS {
	return &crashFS{model: model, crashAt: crashAt, dirs: map[string]*crashDir{}}
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
		dir = &crashDir{
			names:        map[string]*crashInode{},
			durable:      map[string]*crashInode{},
			slots:        map[string]crashSlot{},
			durableSlots: map[string]crashSlot{},
		}
		fs.dirs[path] = dir
	}
	return dir
}

// seedVolatile adds a file another program wrote without syncing it.
func (fs *crashFS) seedVolatile(path string, data []byte) {
	inode := &crashInode{data: bytes.Clone(data), path: path}
	dir := fs.dir(filepath.Dir(path))
	dir.names[filepath.Base(path)] = inode
	dir.slots[filepath.Base(path)] = crashSlot{inode: inode, size: len(data)}
}

// seedVolatileDir adds a directory another program created without syncing
// its parent, as a materializer does with os.MkdirAll.
func (fs *crashFS) seedVolatileDir(path string) {
	fs.dir(path)
	inode := &crashInode{path: path, dir: true}
	parent := fs.dir(filepath.Dir(path))
	parent.names[filepath.Base(path)] = inode
	parent.slots[filepath.Base(path)] = crashSlot{inode: inode}
}

func (fs *crashFS) seed(path string, data []byte) {
	inode := &crashInode{data: bytes.Clone(data), durable: bytes.Clone(data), path: path}
	dir := fs.dir(filepath.Dir(path))
	base := filepath.Base(path)
	dir.names[base] = inode
	dir.durable[base] = inode
	dir.slots[base] = crashSlot{inode: inode, size: len(data)}
	dir.durableSlots[base] = dir.slots[base]
}

func (fs *crashFS) powerLoss() {
	var lost []string
	for path, dir := range fs.dirs {
		for name, inode := range dir.names {
			if !inode.dir {
				continue
			}
			_, durable := dir.durable[name]
			if fs.model == fat {
				_, durable = dir.durableSlots[name]
			}
			if !durable {
				lost = append(lost, filepath.Join(path, name))
			}
		}
	}
	for _, gone := range lost {
		for path := range fs.dirs {
			if path == gone || strings.HasPrefix(path, gone+"/") {
				delete(fs.dirs, path)
			}
		}
	}

	for path, dir := range fs.dirs {
		names := map[string]*crashInode{}
		if fs.model == fat {
			for name, slot := range dir.durableSlots {
				if slot.inode != nil && slot.inode.dir {
					names[name] = slot.inode
					continue
				}
				var content []byte
				if slot.inode != nil {
					content = bytes.Clone(slot.inode.durable[:min(slot.size, len(slot.inode.durable))])
				}
				names[name] = &crashInode{data: content, durable: bytes.Clone(content), path: filepath.Join(path, name)}
			}
		} else {
			for name, inode := range dir.durable {
				inode.data = bytes.Clone(inode.durable)
				names[name] = inode
			}
		}
		dir.names = names
	}
}

func (fs *crashFS) read(path string) ([]byte, bool) {
	dir, ok := fs.dirs[filepath.Dir(path)]
	if !ok {
		return nil, false
	}
	inode, ok := dir.names[filepath.Base(path)]
	if !ok {
		return nil, false
	}
	return inode.data, true
}

func (fs *crashFS) list(path string) []string {
	var names []string
	for name := range fs.dir(path).names {
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

func (fs *crashFS) create(path string) *crashFile {
	inode := &crashInode{path: path}
	dir := fs.dir(filepath.Dir(path))
	dir.names[filepath.Base(path)] = inode
	dir.slots[filepath.Base(path)] = crashSlot{}
	return &crashFile{fs: fs, inode: inode}
}

func (fs *crashFS) CreateTemp(dir, pattern string) (file, error) {
	if err := fs.step(); err != nil {
		return nil, err
	}
	fs.temps++
	return fs.create(filepath.Join(dir, fmt.Sprintf("%s%d", pattern, fs.temps))), nil
}

func (fs *crashFS) OpenFile(name string, flag int, _ os.FileMode) (file, error) {
	if err := fs.step(); err != nil {
		return nil, err
	}
	existing, ok := fs.dir(filepath.Dir(name)).names[filepath.Base(name)]
	switch {
	case ok && flag&os.O_EXCL != 0:
		return nil, os.ErrExist
	case ok && flag&os.O_CREATE == 0:
		return &crashFile{fs: fs, inode: existing}, nil
	case !ok && flag&os.O_CREATE == 0:
		return nil, os.ErrNotExist
	}
	return fs.create(name), nil
}

func (fs *crashFS) Rename(oldpath, newpath string) error {
	if err := fs.step(); err != nil {
		return err
	}
	from := fs.dir(filepath.Dir(oldpath))
	inode, ok := from.names[filepath.Base(oldpath)]
	if !ok {
		return os.ErrNotExist
	}
	to := fs.dir(filepath.Dir(newpath))
	delete(from.names, filepath.Base(oldpath))
	to.names[filepath.Base(newpath)] = inode
	delete(from.slots, filepath.Base(oldpath))
	if _, replaced := to.slots[filepath.Base(newpath)]; !replaced {
		to.slots[filepath.Base(newpath)] = crashSlot{}
	}
	inode.path = newpath
	return nil
}

func (fs *crashFS) Remove(name string) error {
	if err := fs.step(); err != nil {
		return err
	}
	dir := fs.dir(filepath.Dir(name))
	if _, ok := dir.names[filepath.Base(name)]; !ok {
		return os.ErrNotExist
	}
	delete(dir.names, filepath.Base(name))
	delete(dir.slots, filepath.Base(name))
	return nil
}

func (fs *crashFS) SyncDir(path string) error {
	if err := fs.step(); err != nil {
		return err
	}
	dir := fs.dir(path)
	dir.durable = maps.Clone(dir.names)
	dir.durableSlots = maps.Clone(dir.slots)
	return nil
}

// WalkDir visits root and everything below it in lexical order. Reading the
// namespace changes nothing durable, so it is not a crash step.
func (fs *crashFS) WalkDir(root string, fn iofs.WalkDirFunc) error {
	if _, ok := fs.dirs[root]; !ok {
		return fn(root, nil, os.ErrNotExist)
	}
	return fs.walk(root, fn)
}

func (fs *crashFS) walk(path string, fn iofs.WalkDirFunc) error {
	if err := fn(path, crashEntry{name: filepath.Base(path), dir: true}, nil); err != nil {
		return err
	}
	var children []crashEntry
	for name, inode := range fs.dirs[path].names {
		if !inode.dir {
			children = append(children, crashEntry{name: name})
		}
	}
	for child := range fs.dirs {
		if child != path && filepath.Dir(child) == path {
			children = append(children, crashEntry{name: filepath.Base(child), dir: true})
		}
	}
	slices.SortFunc(children, func(a, b crashEntry) int { return strings.Compare(a.name, b.name) })
	for _, child := range children {
		childPath := filepath.Join(path, child.name)
		var err error
		if child.dir {
			err = fs.walk(childPath, fn)
		} else {
			err = fn(childPath, child, nil)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

type crashEntry struct {
	name string
	dir  bool
}

func (e crashEntry) Name() string { return e.name }

func (e crashEntry) IsDir() bool { return e.dir }

func (e crashEntry) Type() iofs.FileMode {
	if e.dir {
		return iofs.ModeDir
	}
	return 0
}

func (e crashEntry) Info() (iofs.FileInfo, error) { return nil, iofs.ErrInvalid }

type crashFile struct {
	fs    *crashFS
	inode *crashInode
}

func (f *crashFile) Name() string { return f.inode.path }

func (f *crashFile) Write(data []byte) (int, error) {
	if err := f.fs.step(); err != nil {
		return 0, err
	}
	f.inode.data = append(f.inode.data, data...)
	return len(data), nil
}

func (f *crashFile) Chmod(os.FileMode) error { return f.fs.step() }

// Sync makes the content durable and, on vfat, writes the file's metadata
// into the slot at its current name.
func (f *crashFile) Sync() error {
	if err := f.fs.step(); err != nil {
		return err
	}
	f.inode.durable = bytes.Clone(f.inode.data)
	dir := f.fs.dir(filepath.Dir(f.inode.path))
	slot := crashSlot{inode: f.inode, size: len(f.inode.data)}
	dir.slots[filepath.Base(f.inode.path)] = slot
	dir.durableSlots[filepath.Base(f.inode.path)] = slot
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
