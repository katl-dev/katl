package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"golang.org/x/sys/unix"

	"github.com/katl-dev/katl/internal/resourcetest"
)

// pinnedMkosiOutputs are the build outputs a VM run consumes. Each is pinned
// with its metadata and checksum sidecars when present. A build that does not
// produce an output leaves it absent from the pin.
var pinnedMkosiOutputs = []string{
	"katl-installer.efi",
	"katl-installer.vmlinuz",
	"katl-installer.initrd",
	"katl-installer.iso",
	"katl-runtime.efi",
	"katl-runtime-root.squashfs",
	"katl-kubernetes.raw",
}

var pinnedMkosiFiles = []string{
	"katl-installer.packages.tsv",
	"katl-runtime.packages.tsv",
}

// pinMkosiOutputs binds the outputs of one mkosi build to pinDir and returns
// pinDir. The builder publishes by renaming, so a hard link keeps the bytes
// bound here after a later build replaces the directory entry. Where a hard
// link is not possible, a clone or a copy gives the run its own bytes.
//
// The pinned artifact index names the pinned files by absolute path.
func pinMkosiOutputs(mkosiDir, pinDir string) error {
	if err := os.MkdirAll(pinDir, 0o755); err != nil {
		return fmt.Errorf("create pin directory: %w", err)
	}
	names := slices.Clone(pinnedMkosiFiles)
	for _, output := range pinnedMkosiOutputs {
		names = append(names, output, output+".json", output+".sha256")
	}
	katlos, err := findKatlOSImage(mkosiDir)
	if err != nil {
		return err
	}
	if katlos != "" {
		base := filepath.Base(katlos)
		names = append(names, base, base+".json", base+".sha256")
	}
	for _, name := range names {
		if err := pinFile(filepath.Join(mkosiDir, name), filepath.Join(pinDir, name)); err != nil {
			return err
		}
	}
	return pinArtifactIndex(filepath.Join(mkosiDir, "artifacts.json"), pinDir)
}

// pinFile binds src at dst. A missing src is not an error.
func pinFile(src, dst string) error {
	info, err := os.Stat(src)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat %s: %w", src, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("pin %s: not a regular file", src)
	}
	if err := os.Link(src, dst); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrExist) {
		// Cross-device links and the kernel's protected_hardlinks policy both
		// land here; either way the run takes its own bytes.
		return cloneOrCopy(src, dst, info.Mode().Perm())
	}
	return fmt.Errorf("pin %s: %s already exists", src, dst)
}

// cloneOrCopy writes dst through a temporary name so a failed copy never
// leaves a partial file at dst.
func cloneOrCopy(src, dst string, perm fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".pin-*")
	if err != nil {
		return fmt.Errorf("create pin file: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := unix.IoctlFileClone(int(tmp.Fd()), int(in.Fd())); err != nil {
		if _, err := io.Copy(tmp, in); err != nil {
			tmp.Close()
			return fmt.Errorf("copy %s: %w", src, err)
		}
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write %s: %w", dst, err)
	}
	// The copy is private to the run, so it can be made read-only; a hard link
	// shares its inode with the build output and keeps that file's mode.
	if err := os.Chmod(tmp.Name(), perm&^0o222); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

// pinArtifactIndex writes the index with every file path rewritten to the
// pinned file. Index paths are relative to the checkout, so copying the index
// unchanged would send consumers back to the live build directory.
func pinArtifactIndex(src, pinDir string) error {
	data, err := os.ReadFile(src)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read mkosi artifact index: %w", err)
	}
	var index map[string]any
	if err := json.Unmarshal(data, &index); err != nil {
		return fmt.Errorf("decode mkosi artifact index: %w", err)
	}
	artifacts, _ := index["artifacts"].([]any)
	for _, entry := range artifacts {
		artifact, _ := entry.(map[string]any)
		for _, field := range []string{"path", "metadataPath", "checksumPath"} {
			if path, ok := artifact[field].(string); ok && path != "" {
				artifact[field] = filepath.Join(pinDir, filepath.Base(path))
			}
		}
	}
	data, err = json.MarshalIndent(index, "", "  ")
	if err != nil {
		return fmt.Errorf("encode pinned artifact index: %w", err)
	}
	return os.WriteFile(filepath.Join(pinDir, "artifacts.json"), append(data, '\n'), 0o444)
}

// verifyPinnedIndex rejects a pin assembled across two builds. Files are bound
// one at a time, so a build that publishes between two of them would leave
// outputs the index does not describe.
func verifyPinnedIndex(pinDir string, artifacts []resourcetest.Artifact) error {
	data, err := os.ReadFile(filepath.Join(pinDir, "artifacts.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var index struct {
		Artifacts []struct {
			Path   string `json:"path"`
			SHA256 string `json:"sha256"`
		} `json:"artifacts"`
	}
	if err := json.Unmarshal(data, &index); err != nil {
		return fmt.Errorf("decode pinned artifact index: %w", err)
	}
	for _, indexed := range index.Artifacts {
		for _, artifact := range artifacts {
			if indexed.SHA256 != "" && filepath.Base(artifact.Path) == filepath.Base(indexed.Path) && artifact.Digest != indexed.SHA256 {
				return fmt.Errorf("pinned %s has sha256 %s but the artifact index records %s: a build changed it while the run was pinning; rerun", filepath.Base(artifact.Path), artifact.Digest, indexed.SHA256)
			}
		}
	}
	return nil
}
