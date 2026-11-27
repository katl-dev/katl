// Package deadline connects cancellable operations to systemd expiry units.
package deadline

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type Kind string

const (
	ManagementLink      Kind = "management-link"
	KubeletConfigHealth Kind = "kubelet-config-health"
	ExtensionActivation Kind = "extension-activation"
)

// expiryGrace is how long a supervised command has to exit after SIGTERM
// before it is killed.
const expiryGrace = 10 * time.Second

// markerPoll is how often an armed invocation checks for its expiry. The
// timers fire with one-second accuracy, so this adds no material delay.
const markerPoll = 250 * time.Millisecond

type scheduler struct {
	dir       string
	systemctl func(context.Context, string, string) error
	stderr    io.Writer
}

var production = scheduler{dir: "/run/katl/deadlines", systemctl: systemctl, stderr: os.Stderr}

// Arm starts the kind's timer for a new instance. The returned context ends
// with context.DeadlineExceeded when that instance expires.
func Arm(parent context.Context, kind Kind, subject string) (context.Context, context.CancelFunc, error) {
	ctx, stop, _, err := production.start(parent, kind, subject)
	return ctx, stop, err
}

func (s scheduler) start(parent context.Context, kind Kind, subject string) (context.Context, context.CancelFunc, string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, nil, "", err
	}
	id := hex.EncodeToString(nonce[:])
	ctx, stop, err := s.arm(parent, kind, id)
	if err != nil {
		return nil, nil, "", err
	}
	if subject != "" {
		subject = " " + subject
	}
	fmt.Fprintf(s.stderr, "armed %s deadline %s%s\n", kind, id, subject)
	return ctx, stop, id, nil
}

func (s scheduler) arm(parent context.Context, kind Kind, id string) (context.Context, context.CancelFunc, error) {
	if err := validateInstance(id); err != nil {
		return nil, nil, err
	}
	// The parent stays traversable: other /run/katl state is read by
	// unprivileged services. Only the marker directory is private.
	if err := os.MkdirAll(filepath.Dir(s.dir), 0o755); err != nil {
		return nil, nil, err
	}
	if err := os.Mkdir(s.dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, nil, err
	}
	path := filepath.Join(s.dir, id)
	if err := os.Mkdir(path, 0o700); err != nil {
		return nil, nil, err
	}

	ctx, cancel := context.WithCancelCause(parent)
	stopWatch := watchRemoval(path, cancel)
	timer := fmt.Sprintf("katl-deadline-%s@%s.timer", kind, id)
	stop := func() {
		cancel(nil)
		stopWatch()
		// Remove the address before stopping the timer. An expiry job that
		// already started can only affect this invocation's marker.
		_ = os.Remove(path)
		if err := s.systemctl(context.Background(), "stop", timer); err != nil {
			log.Printf("disarm %s deadline %s: %v", kind, id, err)
		}
	}
	if err := s.systemctl(ctx, "start", timer); err != nil {
		stop()
		return nil, nil, fmt.Errorf("arm %s deadline: %w", kind, err)
	}
	return ctx, stop, nil
}

// Expire delivers expiry to one invocation. A late job is harmless once its
// waiter has completed and removed the marker.
func Expire(id string) error {
	return production.expire(id)
}

func (s scheduler) expire(id string) error {
	if err := validateInstance(id); err != nil {
		return err
	}
	err := os.Remove(filepath.Join(s.dir, id))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func validateInstance(id string) error {
	if _, err := hex.DecodeString(id); len(id) != 32 || err != nil {
		return fmt.Errorf("invalid deadline instance %q", id)
	}
	return nil
}

// Run supervises a command under a new deadline instance, so expiry fails
// the command and its enclosing service.
func Run(ctx context.Context, kind Kind, argv []string, stdout, stderr io.Writer) error {
	return production.run(ctx, kind, argv, stdout, stderr)
}

func (s scheduler) run(ctx context.Context, kind Kind, argv []string, stdout, stderr io.Writer) error {
	// Failing to arm must not stop the command from running: the enclosing
	// service's own timeout still bounds it.
	if armed, stop, _, err := s.start(ctx, kind, ""); err != nil {
		fmt.Fprintf(s.stderr, "run without %s deadline: %v\n", kind, err)
	} else {
		ctx = armed
		defer stop()
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	// Give the command a chance to clean up; WaitDelay then kills it.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = expiryGrace
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", kind, errors.Join(context.Cause(ctx), err))
	}
	return nil
}

// watchRemoval cancels with context.DeadlineExceeded once the marker at path
// is removed. Polling holds no file descriptor or inotify instance, so the
// number of armed invocations is not bounded by per-user kernel limits.
func watchRemoval(path string, cancel context.CancelCauseFunc) func() {
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(markerPoll)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
			}
			if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
				cancel(context.DeadlineExceeded)
				return
			} else if err != nil {
				cancel(fmt.Errorf("watch deadline marker: %w", err))
				return
			}
		}
	}()
	return func() {
		close(done)
		<-stopped
	}
}

func systemctl(ctx context.Context, action, unit string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "/usr/bin/systemctl", action, unit).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s %s: %w: %s", action, unit, err, strings.TrimSpace(string(output)))
	}
	return nil
}
