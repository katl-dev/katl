package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/katlosimage"
	"github.com/katl-dev/katl/internal/installer/operation"
)

func TestStaleUpgradeCleanupRequiresMountAbsence(t *testing.T) {
	for _, mode := range []string{"busy", "probe failed", "still mounted", "absent", "released"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			base := hostUpgradeArtifactRoot(root)
			paths := []string{
				filepath.Join(base, "downloads", "old.squashfs"),
				filepath.Join(base, "uploads", strings.Repeat("a", 64)+".squashfs"),
				filepath.Join(base, "acquisitions/image-crash/downloads", "target.squashfs"),
				filepath.Join(base, "module-inputs-crash", "module.raw"),
			}
			mounts := []string{filepath.Join(base, "mounts/legacy"), filepath.Join(base, "acquisitions/image-crash/mount"), filepath.Join(base, "kernel-images-crash/mount-0")}
			for _, path := range mounts {
				if err := os.MkdirAll(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			for _, path := range paths {
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("backing image"), 0o600); err != nil {
					t.Fatal(err)
				}
				old := time.Now().Add(-2 * hostUpgradeArtifactRetention)
				if err := os.Chtimes(path, old, old); err != nil {
					t.Fatal(err)
				}
			}
			mounted := map[string]bool{mounts[0]: mode != "absent", mounts[1]: mode != "absent", mounts[2]: mode != "absent"}
			dissectReleased := false
			run := func(ctx context.Context, argv []string, _ func(int)) ToolResult {
				if err := ctx.Err(); err != nil {
					t.Fatalf("cleanup inherited cancellation: %v", err)
				}
				path := argv[len(argv)-1]
				if argv[0] == "mountpoint" {
					if mode == "probe failed" {
						return ToolResult{ExitStatus: 1, Err: errors.New("probe failed")}
					}
					if !mounted[path] {
						return ToolResult{ExitStatus: 32, Err: errors.New("ordinary directory")}
					}
					return ToolResult{}
				}
				if mode == "busy" {
					return ToolResult{ExitStatus: 32, Err: errors.New("busy"), Stderr: []byte("not mounted is not proof")}
				}
				if mode != "still mounted" {
					mounted[path] = false
				}
				if path == mounts[2] {
					if argv[0] != "systemd-dissect" || argv[1] != "--umount" {
						t.Fatalf("module hierarchy released with %v", argv)
					}
					dissectReleased = true
				}
				return ToolResult{}
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			err := cleanupHostUpgradeStorage(ctx, root, "", run, time.Now())
			preserved := mode != "absent" && mode != "released"
			if (err != nil) != preserved {
				t.Fatalf("cleanup error = %v, retain = %v", err, preserved)
			}
			if mode == "released" && !dissectReleased {
				t.Fatal("stale module mount was not released before backing deletion")
			}
			for _, path := range paths {
				data, err := os.ReadFile(path)
				if preserved && (err != nil || string(data) != "backing image") || !preserved && !os.IsNotExist(err) {
					t.Fatalf("backing resource %s = %q, %v", path, data, err)
				}
			}
		})
	}
}

func TestStaleUpgradeCleanupReleasesModuleConsumers(t *testing.T) {
	root := t.TempDir()
	base := hostUpgradeArtifactRoot(root)
	outer := filepath.Join(base, "acquisitions/image-crash/mount")
	inner := filepath.Join(base, "kernel-images-crash/mount-0")
	backing := filepath.Join(base, "acquisitions/image-crash/downloads/target.squashfs")
	for _, path := range []string{outer, inner, filepath.Dir(backing)} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{backing, filepath.Join(outer, "runtime.raw")} {
		if err := os.WriteFile(path, []byte("image bytes"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mounted := map[string]bool{inner: true, outer: true}
	run := func(_ context.Context, argv []string, _ func(int)) ToolResult {
		path := argv[len(argv)-1]
		if argv[0] == "mountpoint" {
			if mounted[path] {
				return ToolResult{}
			}
			return ToolResult{ExitStatus: 32}
		}
		if path == outer && mounted[inner] {
			// The inner image's loop device holds runtime.raw on the outer
			// filesystem open, even though its mountpoint is a sibling.
			return ToolResult{ExitStatus: 32, Err: syscall.EBUSY}
		}
		if path == inner && (argv[0] != "systemd-dissect" || argv[1] != "--umount") {
			t.Fatalf("module consumer released without its loop device: %v", argv)
		}
		mounted[path] = false
		return ToolResult{}
	}

	if err := cleanupHostUpgradeStorage(context.Background(), root, "", run, time.Now()); err != nil {
		t.Fatalf("one cleanup attempt could not release dependent images: %v", err)
	}
	if mounted[inner] || mounted[outer] {
		t.Fatal("cleanup left a module consumer or its source image mounted")
	}
	if _, err := os.Stat(backing); !os.IsNotExist(err) {
		t.Fatalf("released download still consumes capacity: %v", err)
	}
}

func TestHostUpgradeAcquisitionFailureCleanup(t *testing.T) {
	for _, brokenStore := range []bool{false, true} {
		t.Run(map[bool]string{false: "resolver error", true: "persistence error"}[brokenStore], func(t *testing.T) {
			root := t.TempDir()
			writeResetGenerationZero(t, root)
			if err := generation.WriteBootSelection(root, generation.BootSelectionRecord{APIVersion: generation.APIVersion, Kind: "BootSelection", ActiveGenerationID: "0", DefaultGenerationID: "0", TargetBootGenerationID: "0", UpdatedAt: time.Now()}); err != nil {
				t.Fatal(err)
			}
			store, err := operation.NewStore(filepath.Join(root, "var/lib/katl/operations"))
			if err != nil {
				t.Fatal(err)
			}
			ref := hostUpgradeUploadDirectory + "/" + strings.Repeat("a", 64) + ".squashfs"
			path := filepath.Join(root, "var/lib/katl/artifacts", ref)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("managed upload"), 0o600); err != nil {
				t.Fatal(err)
			}
			record, err := store.Create(operation.OperationRecord{OperationID: "upgrade", OperationKind: OperationKindHostUpgrade, Scope: "host-generation", RequestDigest: strings.Repeat("d", 64), Phase: "accepted", ExpectedCurrentGenerationID: "0", CandidateGenerationID: "next", HostUpgradeRequest: &operation.HostUpgrade{ImageLocalRef: ref, CandidateGenerationID: "next"}}, "accepted", time.Now())
			if err != nil {
				t.Fatal(err)
			}
			resolveErr := errors.New("resolution failed with retained mount evidence")
			executor := NewExecutor(root, store, "test")
			executor.ResolveHostUpgrade = func(context.Context, operation.HostUpgrade) (katlosimage.Payload, error) {
				if !brokenStore {
					return katlosimage.Payload{}, resolveErr
				}
				directory := filepath.Join(store.Root, record.OperationID)
				if err := os.Rename(directory, directory+"-saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(directory, []byte("store unavailable"), 0o600); err != nil {
					t.Fatal(err)
				}
				return katlosimage.Payload{ImagePath: path, ImageSHA256: strings.Repeat("a", 64), ImageSizeBytes: 14}, nil
			}
			err = executor.executeHostUpgrade(context.Background(), record)
			if !brokenStore && !errors.Is(err, resolveErr) || brokenStore && !errors.Is(err, syscall.ENOTDIR) {
				t.Fatalf("host upgrade lost acquisition/persistence error: %v", err)
			}
			_, statErr := os.Stat(path)
			if brokenStore && !os.IsNotExist(statErr) || !brokenStore && statErr != nil {
				t.Fatalf("managed upload ownership was lost: %v", statErr)
			}
		})
	}
}
