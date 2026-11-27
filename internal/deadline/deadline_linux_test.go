package deadline

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/systemdunit"
)

func TestExpiryIsScopedToArmedInvocation(t *testing.T) {
	const first = "00000000000000000000000000000001"
	const second = "00000000000000000000000000000002"
	s := scheduler{dir: t.TempDir(), systemctl: func(context.Context, string, string) error { return nil }}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	a, stopA, err := s.arm(ctx, KubeletConfigHealth, first)
	if err != nil {
		t.Fatal(err)
	}
	defer stopA()
	b, stopB, err := s.arm(ctx, KubeletConfigHealth, second)
	if err != nil {
		t.Fatal(err)
	}
	defer stopB()

	if err := s.expire(first); err != nil {
		t.Fatal(err)
	}
	<-a.Done()
	if ctx.Err() != nil || context.Cause(a) != context.DeadlineExceeded {
		t.Fatalf("expiry cause = %v, parent error = %v", context.Cause(a), ctx.Err())
	}
	if err := s.expire(first); err != nil || b.Err() != nil {
		t.Fatalf("late expiry = %v, other invocation = %v", err, b.Err())
	}
	if _, err := os.Stat(filepath.Join(s.dir, second)); err != nil {
		t.Fatalf("other invocation's marker: %v", err)
	}
}

// Each kind must start the timer that the image ships for it, and that timer
// must hand expiry to the shared expiry service for the same instance.
func TestArmStartsShippedTimer(t *testing.T) {
	for kind, timer := range map[Kind]string{
		ManagementLink:      "katl-deadline-management-link@.timer",
		KubeletConfigHealth: "katl-deadline-kubelet-config-health@.timer",
		ExtensionActivation: "katl-deadline-extension-activation@.timer",
		ManagementCarrier:   "katl-deadline-management-carrier@.timer",
	} {
		t.Run(string(kind), func(t *testing.T) {
			unit, err := systemdunit.RuntimeUnits.ReadFile("units/" + timer)
			if err != nil {
				t.Fatalf("no shipped timer for %s: %v", kind, err)
			}
			if !strings.Contains(string(unit), "\nUnit=katl-deadline-expire@%i.service\n") {
				t.Fatalf("%s does not activate the instance's expiry service:\n%s", timer, unit)
			}

			var calls []string
			var log strings.Builder
			s := scheduler{
				dir:    t.TempDir(),
				stderr: &log,
				systemctl: func(_ context.Context, action, unit string) error {
					calls = append(calls, action+" "+unit)
					return nil
				},
			}
			ctx, stop, id, err := s.start(t.Context(), kind, "")
			if err != nil {
				t.Fatal(err)
			}
			instance := strings.Replace(timer, "@.", "@"+id+".", 1)
			if len(calls) != 1 || calls[0] != "start "+instance {
				t.Fatalf("arming ran %q, want start %s", calls, instance)
			}
			if want := "armed " + string(kind) + " deadline " + id + "\n"; log.String() != want {
				t.Fatalf("arming logged %q, want %q", log.String(), want)
			}

			stop()
			if context.Cause(ctx) != context.Canceled {
				t.Fatalf("completion cause = %v, want cancellation", context.Cause(ctx))
			}
			if _, err := os.Stat(filepath.Join(s.dir, id)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("completed marker stat = %v, want absent", err)
			}
			if len(calls) != 2 || calls[1] != "stop "+instance {
				t.Fatalf("completion ran %q, want stop %s", calls, instance)
			}
		})
	}
}

// Each kind's period is its shipped timer's, including spans in minutes.
func TestPeriod(t *testing.T) {
	for kind, want := range map[Kind]time.Duration{
		ManagementLink:      90 * time.Second,
		ManagementCarrier:   20 * time.Second,
		KubeletConfigHealth: 2 * time.Minute,
		ExtensionActivation: 3 * time.Minute,
	} {
		if got, err := Period(kind); err != nil || got != want {
			t.Errorf("Period(%s) = %v, %v, want %v", kind, got, err, want)
		}
	}
	if _, err := Period("unshipped"); err == nil {
		t.Error("Period of a kind without a timer succeeded")
	}
}

// The supervised command's instance is the one the supervisor logs as armed,
// and the started timer, the expiry address and that instance must all agree.
func TestRunExpiryTerminatesCommand(t *testing.T) {
	dir := t.TempDir()
	startedFile := filepath.Join(dir, "started")
	termFile := filepath.Join(dir, "terminated")
	var mu sync.Mutex
	var started string
	var log syncBuffer
	s := scheduler{
		dir:    filepath.Join(dir, "deadlines"),
		stderr: &log,
		systemctl: func(_ context.Context, action, unit string) error {
			mu.Lock()
			defer mu.Unlock()
			if action == "start" {
				started = unit
			}
			return nil
		},
	}
	script := `trap 'kill $!; echo term >"$2"; exit 0' TERM
sleep 60 & : >"$1"; wait`

	done := make(chan error, 1)
	go func() {
		done <- s.run(t.Context(), ExtensionActivation, []string{"/bin/sh", "-c", script, "sh", startedFile, termFile}, nil, nil)
	}()
	for {
		if _, err := os.Stat(startedFile); err == nil {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("command ended before expiry: %v", err)
		case <-time.After(10 * time.Millisecond):
		}
	}
	instance, ok := strings.CutPrefix(strings.TrimSpace(log.String()), "armed extension-activation deadline ")
	if !ok {
		t.Fatalf("supervisor logged %q, want an armed extension-activation deadline", log.String())
	}
	mu.Lock()
	timer := started
	mu.Unlock()
	if want := "katl-deadline-extension-activation@" + instance + ".timer"; timer != want {
		t.Fatalf("started %q, supervisor armed instance %q", timer, instance)
	}

	if err := s.expire(instance); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("run error = %v, want deadline expiry", err)
		}
	case <-time.After(expiryGrace / 2):
		t.Fatal("expired command still running")
	}
	if data, err := os.ReadFile(termFile); err != nil || strings.TrimSpace(string(data)) != "term" {
		t.Fatalf("command was not sent SIGTERM: %q, %v", data, err)
	}
}

// A deadline that cannot be armed must not block the work it would bound:
// the enclosing service's timeout remains the backstop.
func TestRunWithoutDeadlineWhenArmingFails(t *testing.T) {
	dir := t.TempDir()
	ran := filepath.Join(dir, "ran")
	var log syncBuffer
	s := scheduler{
		dir:       filepath.Join(dir, "deadlines"),
		stderr:    &log,
		systemctl: func(context.Context, string, string) error { return errors.New("bus unavailable") },
	}

	if err := s.run(t.Context(), ExtensionActivation, []string{"/bin/sh", "-c", `: >"$1"`, "sh", ran}, nil, nil); err != nil {
		t.Fatalf("run = %v, want the command's own result", err)
	}
	if _, err := os.Stat(ran); err != nil {
		t.Fatalf("command did not run: %v", err)
	}
	if !strings.Contains(log.String(), "bus unavailable") {
		t.Fatalf("arming failure was not logged: %q", log.String())
	}

	if err := s.run(t.Context(), ExtensionActivation, []string{"/bin/false"}, nil, nil); err == nil {
		t.Fatal("run without a deadline hid the command's failure")
	}
}

// Each armed invocation must not hold a kernel object such as an inotify
// instance, whose per-user limit would otherwise fail later arms.
func TestArmHoldsNoDescriptor(t *testing.T) {
	const count = 64
	s := scheduler{dir: t.TempDir(), systemctl: func(context.Context, string, string) error { return nil }}
	before := openDescriptors(t)

	var stops []context.CancelFunc
	defer func() {
		for _, stop := range stops {
			stop()
		}
	}()
	for i := range count {
		_, stop, err := s.arm(t.Context(), KubeletConfigHealth, fmt.Sprintf("%032x", i+1))
		if err != nil {
			t.Fatalf("arm %d: %v", i, err)
		}
		stops = append(stops, stop)
	}
	if grown := openDescriptors(t) - before; grown >= count/2 {
		t.Fatalf("%d armed deadlines opened %d descriptors", count, grown)
	}
}

func openDescriptors(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Arming may create the shared /run/katl parent, which unprivileged services
// must still traverse; only the marker directory is private.
func TestArmKeepsParentTraversable(t *testing.T) {
	defer syscall.Umask(syscall.Umask(0o022))
	root := filepath.Join(t.TempDir(), "katl")
	s := scheduler{dir: filepath.Join(root, "deadlines"), systemctl: func(context.Context, string, string) error { return nil }}

	_, stop, err := s.arm(t.Context(), KubeletConfigHealth, "00000000000000000000000000000003")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	for path, want := range map[string]fs.FileMode{root: 0o755, s.dir: 0o700} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s mode = %o, want %o", path, got, want)
		}
	}
}
