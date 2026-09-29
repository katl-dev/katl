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
	for _, entry := range entries {
		path := filepath.Join(base, entry.Name())
		switch entry.Name() {
		case "uploads":
			cleanupErr = errors.Join(cleanupErr, cleanupUpgradeUploads(path, preservedLocalRef, now))
		case "mounts":
			mounts, readErr := os.ReadDir(path)
			if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
				cleanupErr = errors.Join(cleanupErr, readErr)
				continue
			}
			for _, mount := range mounts {
				mountPath := filepath.Join(path, mount.Name())
				if run != nil {
					result := run(ctx, []string{"umount", mountPath}, nil)
					if result.Err != nil && !strings.Contains(string(result.Stderr), "not mounted") {
						cleanupErr = errors.Join(cleanupErr, fmt.Errorf("unmount stale host upgrade image %s: %s", mount.Name(), toolFailure(result)))
						continue
					}
				}
				cleanupErr = errors.Join(cleanupErr, os.RemoveAll(mountPath))
			}
		case "downloads":
			cleanupErr = errors.Join(cleanupErr, os.RemoveAll(path))
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
