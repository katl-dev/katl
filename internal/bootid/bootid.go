// Package bootid reads the kernel's identity of the current boot.
package bootid

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const path = "/proc/sys/kernel/random/boot_id"

// Read returns the current boot's ID from the proc filesystem under root; an
// empty root or "/" reads the host's.
func Read(root string) (string, error) {
	data, err := os.ReadFile(filepath.Join(root, path))
	if err != nil {
		return "", fmt.Errorf("read kernel boot ID: %w", err)
	}
	id := strings.TrimSpace(string(data))
	if id == "" {
		return "", errors.New("kernel boot ID is empty")
	}
	return id, nil
}
