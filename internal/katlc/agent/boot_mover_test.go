package agent

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/efiboot"
	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/operation"
)

// fakeEFI stands in for the firmware boot variables. Until the first access
// it reports the published default entry as the firmware default, as a node
// whose firmware agrees with its selection would.
type fakeEFI struct {
	root string

	mu      sync.Mutex
	seeded  bool
	entries efiboot.Entries
	// defaultErr and oneShotErr fail the next write of that variable.
	defaultErr, oneShotErr error
	// defaultHook runs before each default write and may fail or panic.
	defaultHook        func(entry string) error
	defaults, oneShots []string
}

func (f *fakeEFI) seedLocked() {
	if f.seeded {
		return
	}
	selection, err := generation.ReadBootSelection(f.root)
	if err != nil {
		return
	}
	f.seeded = true
	f.entries.Default = filepath.Base(selection.DefaultBootEntry)
}

func (f *fakeEFI) read(string) (efiboot.Entries, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seedLocked()
	return f.entries, nil
}

func (f *fakeEFI) setDefault(_, entry string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seedLocked()
	if hook := f.defaultHook; hook != nil {
		if err := hook(entry); err != nil {
			return err
		}
	}
	if err := f.defaultErr; err != nil {
		f.defaultErr = nil
		return err
	}
	f.entries.Default = filepath.Base(entry)
	f.defaults = append(f.defaults, f.entries.Default)
	return nil
}

func (f *fakeEFI) setOneShot(_, entry string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seedLocked()
	if err := f.oneShotErr; err != nil {
		f.oneShotErr = nil
		return err
	}
	f.entries.OneShot = ""
	if entry != "" {
		f.entries.OneShot = filepath.Base(entry)
	}
	f.oneShots = append(f.oneShots, f.entries.OneShot)
	return nil
}

func (f *fakeEFI) observed() efiboot.Entries {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seedLocked()
	return f.entries
}

// newBootMover returns the boot-state owner for a boot of the running
// generation on root, writing a fake firmware.
func newBootMover(t *testing.T, root, bootID, running string) (*generation.BootMover, *fakeEFI) {
	t.Helper()
	efi := &fakeEFI{root: root}
	return newBootMoverOn(t, root, bootID, running, efi), efi
}

// newBootMoverOn returns another owner over the same firmware, as a restarted
// agent on the same boot would hold.
func newBootMoverOn(t *testing.T, root, bootID, running string, efi *fakeEFI) *generation.BootMover {
	t.Helper()
	return newBootMoverCommandLine(t, root, bootID, "root=PARTUUID=11111111-1111-1111-1111-111111111111 rootfstype=squashfs ro katl.generation="+running, efi)
}

// newBootMoverCommandLine is newBootMoverOn for a boot with the given kernel
// command line.
func newBootMoverCommandLine(t *testing.T, root, bootID, commandLine string, efi *fakeEFI) *generation.BootMover {
	t.Helper()
	mover, err := generation.NewBootMover(root, generation.BootMoverOptions{
		BootID:      bootID,
		CommandLine: commandLine,
		Now:         func() time.Time { return time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC) },
		ReadEFI:     efi.read,
		SetDefault:  efi.setDefault,
		SetOneShot:  efi.setOneShot,
	})
	if err != nil {
		t.Fatal(err)
	}
	return mover
}

// written reports whether any firmware variable was written.
func (f *fakeEFI) written() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.defaults)+len(f.oneShots) != 0
}

// newExecutorFor returns an executor whose boot moves go through mover.
func newExecutorFor(mover *generation.BootMover, root string, store operation.Store, agentStartID string) *Executor {
	executor := NewExecutor(root, store, agentStartID)
	executor.BootMover = mover
	return executor
}

// defaultRootPartUUID is the root partition of the fixtures' first generation.
const defaultRootPartUUID = "11111111-1111-1111-1111-111111111111"

func bootCommandLine(rootPartUUID, running string) string {
	return "root=PARTUUID=" + rootPartUUID + " rootfstype=squashfs ro katl.generation=" + running
}

// bootAs makes the server's boot-state owner observe a boot of the running
// generation, whose root partition is rootPartUUID. A repeated boot ID is the
// same boot with a restarted agent.
func (s *testServer) bootAs(t *testing.T, bootID, rootPartUUID, running string) *generation.BootMover {
	t.Helper()
	s.bootID, s.rootPartUUID, s.running = bootID, rootPartUUID, running
	s.BootMover = newBootMoverCommandLine(t, s.Root, bootID, bootCommandLine(rootPartUUID, running), s.efi)
	return s.BootMover
}

// seedBootstrapRuntime seeds a node running its first generation.
func (s *testServer) seedBootstrapRuntime(t *testing.T, role string) {
	t.Helper()
	seedBootstrapRuntimeRootForRole(t, s.Root, role)
	s.bootAs(t, "boot-1", "11111111-2222-3333-4444-555555555555", "0")
}

// restartAgent returns an owner for the same boot, as a restarted agent holds.
func (s *testServer) restartAgent(t *testing.T) *generation.BootMover {
	t.Helper()
	return s.bootAs(t, s.bootID, s.rootPartUUID, s.running)
}

// recordActivation records that this boot's activation applied generation id.
func recordActivation(t *testing.T, root, id string) {
	t.Helper()
	writeTestFile(t, filepath.Join(root, "run/katl/boot-health/current"), `{"generationID":"`+id+`"}`+"\n")
}
