package vmtest

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const (
	FixtureNspawnUserspaceRoot  = "nspawn-userspace-root"
	FixtureNspawnUserspaceImage = "nspawn-userspace-image"
	FixtureBindWorkspace        = "bind-workspace"
)

type NspawnUserspaceFixture struct {
	Root         string
	Image        string
	ManifestPath string
	Record       FixtureRecord
}

type BindWorkspace struct {
	Name   string
	Source string
	Target string
	Record FixtureRecord
}

type nspawnUserspaceFixtureRecord struct {
	APIVersion string                       `json:"apiVersion"`
	Kind       string                       `json:"kind"`
	Root       string                       `json:"root,omitempty"`
	Image      string                       `json:"image,omitempty"`
	Source     nspawnUserspaceFixtureSource `json:"source"`
	Checks     []string                     `json:"checks"`
}

type nspawnUserspaceFixtureSource struct {
	Kind       string `json:"kind"`
	Path       string `json:"path"`
	SHA256     string `json:"sha256,omitempty"`
	TreeSHA256 string `json:"treeSHA256,omitempty"`
}

func (scenario *WorldScenario) NspawnUserspaceRoot(sourceRoot string) (NspawnUserspaceFixture, error) {
	sourceRoot = strings.TrimSpace(sourceRoot)
	if sourceRoot == "" {
		return NspawnUserspaceFixture{}, errors.New("nspawn userspace source root is required")
	}
	source, err := cleanAbs(sourceRoot)
	if err != nil {
		return NspawnUserspaceFixture{}, err
	}
	info, err := os.Stat(source)
	if err != nil {
		return NspawnUserspaceFixture{}, fmt.Errorf("stat nspawn userspace source root: %w", err)
	}
	if !info.IsDir() {
		return NspawnUserspaceFixture{}, fmt.Errorf("nspawn userspace source root is not a directory: %s", source)
	}
	sha, err := nspawnTreeSHA256(source)
	if err != nil {
		return NspawnUserspaceFixture{}, fmt.Errorf("hash nspawn userspace source root: %w", err)
	}
	root := filepath.Join(scenario.Dir, "nspawn", "root")
	if err := copyOrRejectStaleNspawnRoot(source, root, sha); err != nil {
		return NspawnUserspaceFixture{}, err
	}
	checks, err := validateNspawnUserspaceRoot(root)
	if err != nil {
		return NspawnUserspaceFixture{}, err
	}
	manifestPath := filepath.Join(scenario.Dir, "manifests", "nspawn-userspace-root.json")
	manifest := nspawnUserspaceFixtureRecord{
		APIVersion: WorldAPIVersion,
		Kind:       "NspawnUserspaceFixture",
		Root:       root,
		Source: nspawnUserspaceFixtureSource{
			Kind:       "directory",
			Path:       source,
			TreeSHA256: sha,
		},
		Checks: checks,
	}
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o755); err != nil {
		return NspawnUserspaceFixture{}, err
	}
	if err := writeJSON(manifestPath, manifest); err != nil {
		return NspawnUserspaceFixture{}, err
	}
	record := FixtureRecord{
		Kind:       FixtureNspawnUserspaceRoot,
		Name:       "nspawn-userspace-root",
		Path:       root,
		TreeSHA256: sha,
		Provenance: FixtureProvenance{
			Source:           "directory",
			SourcePath:       source,
			SourceTreeSHA256: sha,
		},
		Properties: map[string]string{
			"manifest": manifestPath,
		},
	}
	if err := scenario.recordFixture(record); err != nil {
		return NspawnUserspaceFixture{}, err
	}
	return NspawnUserspaceFixture{Root: root, ManifestPath: manifestPath, Record: record}, nil
}

func (scenario *WorldScenario) NspawnUserspaceImage(sourceImage string) (NspawnUserspaceFixture, error) {
	sourceImage = strings.TrimSpace(sourceImage)
	if sourceImage == "" {
		return NspawnUserspaceFixture{}, errors.New("nspawn userspace source image is required")
	}
	source, err := cleanAbs(sourceImage)
	if err != nil {
		return NspawnUserspaceFixture{}, err
	}
	info, err := os.Stat(source)
	if err != nil {
		return NspawnUserspaceFixture{}, fmt.Errorf("stat nspawn userspace source image: %w", err)
	}
	if !info.Mode().IsRegular() {
		return NspawnUserspaceFixture{}, fmt.Errorf("nspawn userspace source image is not a regular file: %s", source)
	}
	sha, err := fileSHA256(source)
	if err != nil {
		return NspawnUserspaceFixture{}, fmt.Errorf("hash nspawn userspace source image: %w", err)
	}
	image := filepath.Join(scenario.Dir, "nspawn", filepath.Base(source))
	if err := copyOrRejectStaleFile(source, image, sha, info.Mode().Perm()); err != nil {
		return NspawnUserspaceFixture{}, fmt.Errorf("stage nspawn userspace image: %w", err)
	}
	manifestPath := filepath.Join(scenario.Dir, "manifests", "nspawn-userspace-image.json")
	manifest := nspawnUserspaceFixtureRecord{
		APIVersion: WorldAPIVersion,
		Kind:       "NspawnUserspaceFixture",
		Image:      image,
		Source: nspawnUserspaceFixtureSource{
			Kind:   "image",
			Path:   source,
			SHA256: sha,
		},
		Checks: []string{"regular-file"},
	}
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o755); err != nil {
		return NspawnUserspaceFixture{}, err
	}
	if err := writeJSON(manifestPath, manifest); err != nil {
		return NspawnUserspaceFixture{}, err
	}
	record := FixtureRecord{
		Kind:      FixtureNspawnUserspaceImage,
		Name:      "nspawn-userspace-image",
		Path:      image,
		SHA256:    sha,
		SizeBytes: info.Size(),
		Provenance: FixtureProvenance{
			Source:       "image",
			SourcePath:   source,
			SourceSHA256: sha,
		},
		Properties: map[string]string{
			"manifest": manifestPath,
		},
	}
	if err := scenario.recordFixture(record); err != nil {
		return NspawnUserspaceFixture{}, err
	}
	return NspawnUserspaceFixture{Image: image, ManifestPath: manifestPath, Record: record}, nil
}

func (scenario *WorldScenario) BindWorkspace(name, target string) (BindWorkspace, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return BindWorkspace{}, errors.New("bind workspace name is required")
	}
	id := clean(name)
	if id == "" {
		return BindWorkspace{}, fmt.Errorf("bind workspace name %q has no usable artifact path", name)
	}
	target = strings.TrimSpace(target)
	if target != "" && !filepath.IsAbs(target) {
		return BindWorkspace{}, fmt.Errorf("bind workspace target %q must be absolute", target)
	}
	source := filepath.Join(scenario.Dir, "binds", id)
	if err := os.MkdirAll(source, 0o755); err != nil {
		return BindWorkspace{}, err
	}
	record := FixtureRecord{
		Kind: FixtureBindWorkspace,
		Name: name,
		Path: source,
		Provenance: FixtureProvenance{
			Source: "generated",
		},
	}
	if target != "" {
		record.Properties = map[string]string{"target": target}
	}
	if err := scenario.recordFixture(record); err != nil {
		return BindWorkspace{}, err
	}
	return BindWorkspace{Name: name, Source: source, Target: target, Record: record}, nil
}

func (scenario *WorldScenario) recordFixture(record FixtureRecord) error {
	scenario.Fixtures = append(scenario.Fixtures, record)
	return scenario.WriteManifest()
}

func copyOrRejectStaleNspawnRoot(src, dst, sha string) error {
	if existing, err := os.Stat(dst); err == nil {
		if !existing.IsDir() {
			return fmt.Errorf("cached nspawn userspace root is not a directory: %s", dst)
		}
		got, err := nspawnTreeSHA256(dst)
		if err != nil {
			return err
		}
		if got != sha {
			return fmt.Errorf("cached nspawn userspace root %s tree digest %s does not match source %s", dst, got, sha)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if src == dst {
		return nil
	}
	if err := copyNspawnRoot(src, dst); err != nil {
		return err
	}
	got, err := nspawnTreeSHA256(dst)
	if err != nil {
		return err
	}
	if got != sha {
		return fmt.Errorf("copied nspawn userspace root %s tree digest %s does not match source %s", dst, got, sha)
	}
	return nil
}

func copyNspawnRoot(src, dst string) error {
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	return filepath.WalkDir(src, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			return os.Symlink(link, target)
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("unsupported nspawn userspace entry: %s", filepath.ToSlash(rel))
		}
		return copyRequiredFile(path, target, info.Mode().Perm())
	})
}

func nspawnTreeSHA256(root string) (string, error) {
	hash := sha256.New()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		mode := fmt.Sprintf("%o", info.Mode().Perm())
		if entry.IsDir() {
			_, _ = fmt.Fprintf(hash, "dir %s %s\n", mode, rel)
			return nil
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(hash, "symlink %s %s %s\n", mode, link, rel)
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("unsupported nspawn userspace entry: %s", rel)
		}
		fileSHA, err := fileSHA256(path)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(hash, "file %s %s %s\n", mode, fileSHA, rel)
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func validateNspawnUserspaceRoot(root string) ([]string, error) {
	checks := []string{
		"sh",
		"cp",
		"grep",
		"mktemp",
		"systemd-analyze",
		"katl-generation-activate",
		"katl-runtime-status",
	}
	for _, check := range checks {
		var paths []string
		switch check {
		case "katl-generation-activate":
			paths = []string{"usr/lib/katl/runtime/katl-generation-activate"}
		case "katl-runtime-status":
			paths = []string{"usr/lib/katl/runtime/katl-runtime-status"}
		default:
			paths = []string{"usr/bin/" + check, "bin/" + check}
		}
		if !hasExecutable(root, paths...) {
			return nil, fmt.Errorf("nspawn userspace root missing executable %s", check)
		}
	}
	return checks, nil
}

func hasExecutable(root string, paths ...string) bool {
	for _, path := range paths {
		info, err := os.Stat(filepath.Join(root, path))
		if err == nil && info.Mode()&0o111 != 0 && !info.IsDir() {
			return true
		}
	}
	return false
}
