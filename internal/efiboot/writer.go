package efiboot

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// bootctlTimeout bounds a firmware variable write, which can stall on a
// firmware that is slow to commit NVRAM.
const bootctlTimeout = 30 * time.Second

// SetDefault points LoaderEntryDefault at the loader entry's basename.
func SetDefault(root, entry string) error {
	if entryID(entry) == "" {
		return fmt.Errorf("set EFI default: loader entry is required")
	}
	return bootctl(root, "set-default", entryID(entry))
}

// SetOneShot points LoaderEntryOneShot at the loader entry's basename. An
// empty entry clears the variable.
func SetOneShot(root, entry string) error {
	return bootctl(root, "set-oneshot", entryID(entry))
}

func entryID(entry string) string {
	if entry == "" {
		return ""
	}
	return filepath.Base(entry)
}

func bootctl(root, verb, entry string) error {
	args := []string{}
	if root != "" && filepath.Clean(root) != "/" {
		args = append(args, "--esp-path="+filepath.Join(root, "efi"))
	}
	args = append(args, verb, entry)
	ctx, cancel := context.WithTimeout(context.Background(), bootctlTimeout)
	defer cancel()
	output, err := exec.CommandContext(ctx, "bootctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("bootctl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}
