package imagemount

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type CommandRunner interface {
	Run(context.Context, string, ...string) error
}

// Unmount proves an ordinary image mount is absent before its owner removes
// backing data. The caller must exclusively own the mountpoint through deletion
// and retain its backing resources on any error.
func Unmount(commands CommandRunner, mountPoint string) error {
	return release(commands, mountPoint, "umount", mountPoint)
}

// DissectUnmount releases systemd-dissect's image hierarchy and loop devices,
// with the same absence and caller-ownership requirements as Unmount.
// The supported acquisition mounts the root before descendants on a fresh
// mountpoint. Callers must not lazily detach that root before this proof.
func DissectUnmount(commands CommandRunner, mountPoint string) error {
	return release(commands, mountPoint, "systemd-dissect", "--umount", mountPoint)
}

func release(commands CommandRunner, mountPoint, name string, args ...string) (resultErr error) {
	defer func() {
		if resultErr != nil {
			resultErr = &retainedError{cause: resultErr}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	absent, err := mountAbsent(ctx, commands, mountPoint)
	if err != nil || absent {
		return err
	}
	if err := commands.Run(ctx, name, args...); err != nil {
		return err
	}
	absent, err = mountAbsent(ctx, commands, mountPoint)
	if err != nil {
		return err
	}
	if !absent {
		return fmt.Errorf("image remains mounted at %s after unmount", mountPoint)
	}
	return nil
}

// Retained reports uncertain native release. Enclosing owners must preserve
// backing inputs when a constructor returns this error without a usable result.
func Retained(err error) bool {
	var retained *retainedError
	return errors.As(err, &retained)
}

type retainedError struct{ cause error }

func (e *retainedError) Error() string { return e.cause.Error() }
func (e *retainedError) Unwrap() error { return e.cause }

func mountAbsent(ctx context.Context, commands CommandRunner, mountPoint string) (bool, error) {
	err := commands.Run(ctx, "mountpoint", "--quiet", "--", mountPoint)
	if err == nil {
		return false, nil
	}
	var exit interface{ ExitCode() int }
	if errors.As(err, &exit) && exit.ExitCode() == 32 {
		return true, nil
	}
	return false, fmt.Errorf("inspect image mount %s: %w", mountPoint, err)
}
