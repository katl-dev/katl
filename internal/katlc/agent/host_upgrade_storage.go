package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/katl-dev/katl/internal/imagemount"
)

const (
	maxHostUpgradePreparationInput = uint64(2 << 30)
	hostUpgradeStorageReserve      = uint64(512 << 20)
	hostUpgradeArtifactRetention   = time.Hour
	hostUpgradeOperationTimeout    = 25 * time.Minute
	hostUpgradePreparationTimeout  = 20 * time.Minute
)

type availableStorageFunc func(string) (uint64, error)

func filesystemAvailable(path string) (uint64, error) {
	for {
		var stat syscall.Statfs_t
		if err := syscall.Statfs(path, &stat); err == nil {
			return stat.Bavail * uint64(stat.Bsize), nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return 0, err
		}
		next := filepath.Dir(path)
		if next == path {
			return 0, fmt.Errorf("find existing parent for %s", path)
		}
		path = next
	}
}

func requireHostUpgradeStorage(available availableStorageFunc, path string, writeBytes uint64, purpose string) error {
	if available == nil {
		available = filesystemAvailable
	}
	free, err := available(path)
	if err != nil {
		return fmt.Errorf("inspect free storage for %s: %w", purpose, err)
	}
	required := writeBytes + hostUpgradeStorageReserve
	if required < writeBytes || free < required {
		return fmt.Errorf("insufficient state storage for %s: %d bytes available, need %d bytes including %d bytes reserved for the running node", purpose, free, required, hostUpgradeStorageReserve)
	}
	return nil
}

func hostUpgradeArtifactRoot(root string) string {
	return filepath.Join(runtimeRoot(root), "var/lib/katl/artifacts/host-upgrade")
}

// cleanupHostUpgradeStorage removes only workspace-owned transient data. The
// generation and operation stores remain the durable rollback and evidence owners.
func cleanupHostUpgradeStorage(ctx context.Context, root, preservedLocalRef string, run ToolRunner, now time.Time) error {
	base := hostUpgradeArtifactRoot(root)
	entries, err := os.ReadDir(base)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var cleanupErr error
	// A private target namespace can still hold an image whose outer mount is
	// releasable. Prove all preparation units stopped before touching any image.
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "prepare-") {
			cleanupErr = errors.Join(cleanupErr, stopUpgradePreparation(upgradePreparationUnit(entry.Name()), run))
		}
	}
	if cleanupErr != nil {
		return cleanupErr
	}
	// Release every possible backing-file consumer before any sweep of downloads
	// or uploads. A failed unmount keeps all backing data available for recovery.
	// Module loop devices hold files inside the outer image mounts, so release
	// those consumers before attempting to unmount their source filesystems.
	for _, modulePhase := range []bool{true, false} {
		for _, entry := range entries {
			path := filepath.Join(base, entry.Name())
			moduleImages := strings.HasPrefix(entry.Name(), "kernel-images-")
			if moduleImages != modulePhase || entry.Name() != "mounts" && entry.Name() != "acquisitions" && !moduleImages {
				continue
			}
			mounts, readErr := os.ReadDir(path)
			if readErr != nil {
				cleanupErr = errors.Join(cleanupErr, readErr)
				continue
			}
			for _, mount := range mounts {
				mountPath := filepath.Join(path, mount.Name())
				if moduleImages && !strings.HasPrefix(mount.Name(), "mount-") {
					continue
				}
				if entry.Name() == "acquisitions" {
					mountPath = filepath.Join(mountPath, "mount")
					if _, err := os.Stat(mountPath); errors.Is(err, os.ErrNotExist) {
						continue
					} else if err != nil {
						cleanupErr = errors.Join(cleanupErr, err)
						continue
					}
				}
				if run == nil {
					cleanupErr = errors.Join(cleanupErr, fmt.Errorf("cannot release stale image mount %s without a command runner", mountPath))
					continue
				}
				release := imagemount.Unmount
				if moduleImages {
					release = imagemount.DissectUnmount
				}
				if err := release(hostUpgradeCommands{run: run}, mountPath); err != nil {
					cleanupErr = errors.Join(cleanupErr, fmt.Errorf("unmount stale host upgrade image %s: %w", mountPath, err))
				}
			}
		}
		if cleanupErr != nil {
			return cleanupErr
		}
	}
	for _, entry := range entries {
		path := filepath.Join(base, entry.Name())
		switch entry.Name() {
		case "uploads":
			cleanupErr = errors.Join(cleanupErr, cleanupUpgradeUploads(path, preservedLocalRef, now))
		default:
			cleanupErr = errors.Join(cleanupErr, os.RemoveAll(path))
		}
	}
	return cleanupErr
}

func cleanupUpgradeUploads(directory, preservedLocalRef string, now time.Time) error {
	preserved := ""
	if strings.TrimSpace(preservedLocalRef) != "" {
		preserved = filepath.Base(filepath.FromSlash(preservedLocalRef))
	}
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var cleanupErr error
	for _, entry := range entries {
		if entry.Name() == preserved {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".partial") && now.Sub(info.ModTime()) < hostUpgradeArtifactRetention {
			continue
		}
		cleanupErr = errors.Join(cleanupErr, os.RemoveAll(filepath.Join(directory, entry.Name())))
	}
	return cleanupErr
}
