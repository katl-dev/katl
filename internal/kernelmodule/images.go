package kernelmodule

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const IndexExtensionName = "zzzz-katl-module-indexes"

type Image struct {
	Name   string
	Path   string
	SHA256 string
}

type BundleImages struct {
	Name     string
	Images   []Image
	Contract *Contract
}

type ImageRequest struct {
	Target           Target
	Runtime          Image
	RuntimeInterface string
	RuntimeVersion   string
	Architecture     string
	Bundles          []BundleImages
	WorkDir          string
}

// PreparedIndexes owns a temporary, immutable image. Close it only after the
// generation has copied and verified the image into its retained assets.
type PreparedIndexes struct {
	Path      string
	SHA256    string
	directory string
}

func (p PreparedIndexes) Close() error {
	if p.directory == "" {
		return nil
	}
	return os.RemoveAll(p.directory)
}

// PrepareImages inspects read-only source images, composes the full target
// module tree, and packages only its generated indexes. It requires mount
// privileges, but never merges extensions or loads modules into the host.
func PrepareImages(ctx context.Context, request ImageRequest) (prepared PreparedIndexes, resultErr error) {
	if err := request.Target.Validate(); err != nil {
		return PreparedIndexes{}, err
	}
	for name, value := range map[string]string{"runtime interface": request.RuntimeInterface, "architecture": request.Architecture} {
		if value == "" || strings.ContainsAny(value, "\n\r\x00\"'\\ \t") {
			return PreparedIndexes{}, fmt.Errorf("invalid %s for module index layer", name)
		}
	}
	architecture := ""
	switch request.Architecture {
	case "x86_64":
		architecture = "x86-64"
	case "aarch64":
		architecture = "arm64"
	default:
		return PreparedIndexes{}, fmt.Errorf("unsupported module image architecture %q", request.Architecture)
	}
	work, err := os.MkdirTemp(request.WorkDir, "kernel-images-")
	if err != nil {
		return PreparedIndexes{}, err
	}
	var mounts []string
	defer func() {
		// Unmount in reverse order, even after cancellation. A failed unmount
		// keeps its scratch directory intact rather than traversing a mount.
		unmounted := true
		for i := len(mounts) - 1; i >= 0; i-- {
			if output, err := exec.Command("systemd-dissect", "--umount", mounts[i]).CombinedOutput(); err != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("unmount module input %s: %w: %s", mounts[i], err, output))
				unmounted = false
			}
		}
		if !unmounted {
			prepared = PreparedIndexes{}
			return
		}
		if resultErr != nil || prepared.Path == "" {
			_ = os.RemoveAll(work)
		}
	}()
	mount := func(image Image, runtime bool) (string, error) {
		if err := verifyImage(image, runtime); err != nil {
			return "", err
		}
		path := filepath.Join(work, fmt.Sprintf("mount-%d", len(mounts)))
		command := exec.CommandContext(ctx, "systemd-dissect", "--read-only", "--fsck=no", "--mount", "--mkdir", image.Path, path)
		if output, err := command.CombinedOutput(); err != nil {
			return "", fmt.Errorf("mount module input %s: %w: %s", image.Path, err, output)
		}
		mounts = append(mounts, path)
		return path, nil
	}
	var selections []mountedBundle
	hasModules := false
	for _, bundle := range request.Bundles {
		selection := mountedBundle{
			Name:     bundle.Name,
			Contract: bundle.Contract,
		}
		hasModules = hasModules || bundle.Contract != nil
		for _, image := range bundle.Images {
			root, err := mount(image, false)
			if err != nil {
				return PreparedIndexes{}, err
			}
			if err := validateExtensionRelease(root, image.Name, request.RuntimeVersion, request.RuntimeInterface, architecture); err != nil {
				return PreparedIndexes{}, err
			}
			selection.Roots = append(selection.Roots, root)
		}
		selections = append(selections, selection)
	}
	base := ""
	if hasModules {
		if request.Runtime.SHA256 != request.Target.RuntimeSHA256 {
			return PreparedIndexes{}, fmt.Errorf("runtime image does not match the kernel target")
		}
		base, err = mount(request.Runtime, true)
		if err != nil {
			return PreparedIndexes{}, err
		}
	}
	layer, err := compose(ctx, composeRequest{
		Target:     request.Target,
		BaseRoot:   base,
		Selections: selections,
		WorkDir:    work,
	})
	if err != nil || layer == "" {
		return PreparedIndexes{}, err
	}
	releaseDir := filepath.Join(layer, "usr/lib/extension-release.d")
	if err := os.MkdirAll(releaseDir, 0o755); err != nil {
		return PreparedIndexes{}, err
	}
	release := fmt.Sprintf("ID=katlos\nSYSEXT_LEVEL=%s\nARCHITECTURE=%s\n", request.RuntimeInterface, architecture)
	if err := os.WriteFile(filepath.Join(releaseDir, "extension-release."+IndexExtensionName), []byte(release), 0o644); err != nil {
		return PreparedIndexes{}, err
	}
	if err := os.Remove(filepath.Join(layer, "lib")); err != nil {
		return PreparedIndexes{}, err
	}
	path := filepath.Join(work, IndexExtensionName+".raw")
	command := exec.CommandContext(ctx, "mksquashfs", layer, path, "-noappend", "-all-root", "-no-xattrs", "-processors", "1", "-mkfs-time", "0", "-all-time", "0")
	if output, err := command.CombinedOutput(); err != nil {
		return PreparedIndexes{}, fmt.Errorf("package module indexes: %w: %s", err, output)
	}
	digest, err := fileDigest(path)
	if err != nil {
		return PreparedIndexes{}, err
	}
	return PreparedIndexes{
		Path:      path,
		SHA256:    digest,
		directory: work,
	}, nil
}

func verifyImage(image Image, runtime bool) error {
	if !filepath.IsAbs(image.Path) {
		return fmt.Errorf("module input image path must be absolute")
	}
	if err := validateDigest(image.SHA256); err != nil {
		return err
	}
	info, err := os.Stat(image.Path)
	if err != nil {
		return err
	}
	// A recorded root partition contains padding beyond the runtime artifact.
	// Its identity is established by the generation and immutable root slot;
	// callers must hold the node mutation lock while reading it.
	if runtime && info.Mode()&os.ModeDevice != 0 && info.Mode()&os.ModeCharDevice == 0 {
		return nil
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("module input %s is not a regular image or root block device", image.Path)
	}
	digest, err := fileDigest(image.Path)
	if err != nil {
		return err
	}
	if digest != image.SHA256 {
		return fmt.Errorf("module input %s SHA-256 mismatch", image.Path)
	}
	return nil
}

func fileDigest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
