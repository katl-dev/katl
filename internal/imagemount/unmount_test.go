package imagemount

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestReleaseRequiresAbsence(t *testing.T) {
	for _, dissect := range []bool{false, true} {
		for _, mode := range []string{"absent", "probe failed", "busy", "still mounted", "released"} {
			t.Run(fmt.Sprintf("dissect=%v/%s", dissect, mode), func(t *testing.T) {
				cause := errors.New("native cleanup failed")
				commands := &mountCommands{mounted: mode != "absent", stillMounted: mode == "still mounted"}
				if mode == "probe failed" {
					commands.probeErr = exitCode(1)
				}
				if mode == "busy" {
					commands.releaseErr = cause
				}
				release := Unmount
				wantTool := "umount"
				if dissect {
					release, wantTool = DissectUnmount, "systemd-dissect"
				}
				err := release(commands, "/private/image")
				wantError := mode != "absent" && mode != "released"
				if (err != nil) != wantError || mode == "busy" && !errors.Is(err, cause) {
					t.Fatalf("release error = %v", err)
				}
				if Retained(errors.Join(errors.New("construction failed"), err)) != wantError {
					t.Fatalf("uncertain release lost its backing retention contract: %v", err)
				}
				if mode == "absent" || mode == "probe failed" {
					if commands.releaseTool != "" {
						t.Fatalf("unproven mount triggered %s", commands.releaseTool)
					}
				} else if commands.releaseTool != wantTool {
					t.Fatalf("release used %q, want %s", commands.releaseTool, wantTool)
				}
			})
		}
	}
}

func TestNativeMountpointAbsence(t *testing.T) {
	if _, err := exec.LookPath("mountpoint"); err != nil {
		t.Skip("native absence regression requires mountpoint")
	}
	commands := nativeProbeCommands{t: t}
	root := t.TempDir()
	if err := Unmount(commands, root); err != nil {
		t.Fatalf("ordinary directory was not confirmed absent: %v", err)
	}
	if err := Unmount(commands, filepath.Join(root, "missing")); err == nil {
		t.Fatal("missing path probe was incorrectly treated as absence")
	}
}

type mountCommands struct {
	mounted, stillMounted bool
	probeErr, releaseErr  error
	releaseTool           string
}

func (c *mountCommands) Run(ctx context.Context, name string, args ...string) error {
	if _, ok := ctx.Deadline(); !ok {
		return errors.New("cleanup lacks a deadline")
	}
	if name == "mountpoint" {
		if c.probeErr != nil {
			return c.probeErr
		}
		if c.mounted {
			return nil
		}
		return exitCode(32)
	}
	c.releaseTool = name
	if c.releaseErr == nil && !c.stillMounted {
		c.mounted = false
	}
	return c.releaseErr
}

type nativeProbeCommands struct{ t *testing.T }

func (c nativeProbeCommands) Run(ctx context.Context, name string, args ...string) error {
	if name != "mountpoint" {
		c.t.Fatalf("attempted native %s for an unmounted directory", name)
	}
	return exec.CommandContext(ctx, name, args...).Run()
}

type exitCode int

func (code exitCode) Error() string { return fmt.Sprintf("native exit %d", code) }
func (code exitCode) ExitCode() int { return int(code) }
