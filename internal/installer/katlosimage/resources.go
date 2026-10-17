package katlosimage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/katl-dev/katl/internal/imagemount"
	"github.com/katl-dev/katl/internal/installer/manifest"
)

type payloadResources struct {
	mu             sync.Mutex
	directory      string
	mountPoint     string
	commands       CommandRunner
	mountAttempted bool
}

// Close releases this acquisition's mount and downloaded image. Copies of a
// Payload share ownership; borrowed local images and directory payloads survive.
func (p Payload) Close() error {
	if p.resources == nil {
		return nil
	}
	return p.resources.close()
}

func newPayloadResources(workDir string, commands CommandRunner) (*payloadResources, error) {
	if commands == nil {
		return nil, fmt.Errorf("mount command runner is required")
	}
	if workDir == "" {
		workDir = filepath.Join(os.TempDir(), "katlos-image")
	}
	parent := filepath.Join(workDir, "acquisitions")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return nil, fmt.Errorf("create KatlOS image workspace: %w", err)
	}
	directory, err := os.MkdirTemp(parent, "image-")
	if err != nil {
		return nil, fmt.Errorf("create KatlOS image acquisition: %w", err)
	}
	return &payloadResources{directory: directory, mountPoint: filepath.Join(directory, "mount"), commands: commands}, nil
}

func (r *payloadResources) close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.mountAttempted {
		// Cancellation of the consuming operation must not strand a loop mount.
		// Keep its backing files intact if ordinary unmount cannot finish.
		if err := imagemount.Unmount(r.commands, r.mountPoint); err != nil {
			return fmt.Errorf("release KatlOS image mount %s: %w", r.mountPoint, err)
		}
		r.mountAttempted = false
	}
	if r.directory != "" {
		if err := os.RemoveAll(r.directory); err != nil {
			return fmt.Errorf("remove KatlOS image acquisition %s: %w", r.directory, err)
		}
		r.directory = ""
	}
	return nil
}

func resolveMountedImage(ctx context.Context, expected manifest.KatlosImage, imagePath, digest string, size uint64, resources *payloadResources, opaque bool) (payload Payload, err error) {
	defer func() {
		if err != nil {
			err = errors.Join(err, resources.close())
		}
	}()
	if err := os.MkdirAll(resources.mountPoint, 0o700); err != nil {
		return Payload{}, fmt.Errorf("create KatlOS image mountpoint: %w", err)
	}
	resources.mountAttempted = true
	if err := resources.commands.Run(ctx, "mount", "-o", "ro,loop", imagePath, resources.mountPoint); err != nil {
		return Payload{}, fmt.Errorf("mount KatlOS image: %w", err)
	}
	resolve := ResolveDirectory
	if opaque {
		resolve = ResolveStagingDirectory
	}
	payload, err = resolve(ctx, resources.mountPoint, expected)
	if err != nil {
		return Payload{}, err
	}
	payload.ImageSHA256, payload.ImageSizeBytes, payload.ImagePath = digest, size, imagePath
	payload.resources = resources
	return payload, nil
}
