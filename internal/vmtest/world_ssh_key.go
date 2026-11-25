package vmtest

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// EnsureWorldSSHKey returns the world's shared SSH key pair, generating it on
// first use. Runs share the fixture cache, so generation happens under a cache
// lock, and the pair appears under its final names only once both files are
// complete: a concurrent run sees no key or a whole key.
func EnsureWorldSSHKey(world World) (privatePath, authorizedKey string, err error) {
	cacheDir := WorldFixtureCacheDir(world)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	unlock, err := lockFile(ctx, filepath.Join(cacheDir, "locks", "ssh"))
	if err != nil {
		return "", "", err
	}
	defer unlock()

	dir := filepath.Join(cacheDir, "ssh")
	privatePath = filepath.Join(dir, "id_ed25519")
	publicPath := privatePath + ".pub"
	if data, err := os.ReadFile(publicPath); err == nil && strings.TrimSpace(string(data)) != "" {
		return privatePath, strings.TrimSpace(string(data)), nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	staging, err := os.MkdirTemp(dir, "keygen-")
	if err != nil {
		return "", "", err
	}
	defer os.RemoveAll(staging)
	stagedKey := filepath.Join(staging, "id_ed25519")
	if output, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "katl-vmtest", "-f", stagedKey).CombinedOutput(); err != nil {
		return "", "", fmt.Errorf("generate VM SSH key: %w: %s", err, strings.TrimSpace(string(output)))
	}
	data, err := os.ReadFile(stagedKey + ".pub")
	if err != nil {
		return "", "", err
	}
	// The public key is the presence marker, so it moves last.
	if err := os.Rename(stagedKey, privatePath); err != nil {
		return "", "", err
	}
	if err := os.Rename(stagedKey+".pub", publicPath); err != nil {
		return "", "", err
	}
	return privatePath, strings.TrimSpace(string(data)), nil
}
