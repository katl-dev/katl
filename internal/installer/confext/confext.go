package confext

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type NativeEtcFileType string

const (
	NativeEtcRegularFile NativeEtcFileType = "regular"
	NativeEtcSymlink     NativeEtcFileType = "symlink"
	NativeEtcCharDevice  NativeEtcFileType = "char-device"
	NativeEtcBlockDevice NativeEtcFileType = "block-device"
)

type NativeEtcFile struct {
	Path    string
	Content string
	Type    NativeEtcFileType
	Mode    fs.FileMode
	UID     int
	GID     int
}

type NativeEtcFilePlan struct {
	Path        string
	ConfextPath string
	Mode        fs.FileMode
	UID         int
	GID         int
}

type GenerationTreeRequest struct {
	GenerationsRoot string
	GenerationID    string
	Files           []NativeEtcFile
	Extension       ExtensionRelease
	Chown           func(path string, uid int, gid int) error
}

type ExtensionRelease struct {
	Name         string
	ID           string
	VersionID    string
	ConfextLevel int
}

type GenerationTree struct {
	GenerationDir        string
	ConfextDir           string
	ExtensionReleasePath string
	Files                []NativeEtcFilePlan
}

func ValidateNativeEtcBundle(confextRoot string, files []NativeEtcFile) ([]NativeEtcFilePlan, error) {
	cleanRoot := ""
	if strings.TrimSpace(confextRoot) != "" {
		if !filepath.IsAbs(confextRoot) {
			return nil, fmt.Errorf("confext root must be absolute")
		}
		cleanRoot = filepath.Clean(confextRoot)
	}

	seen := make(map[string]struct{}, len(files))
	plans := make([]NativeEtcFilePlan, 0, len(files))
	for _, file := range files {
		normalizedPath, err := normalizeNativeEtcPath(file.Path)
		if err != nil {
			return nil, err
		}
		if _, ok := seen[normalizedPath]; ok {
			return nil, fmt.Errorf("duplicate /etc file path %q", normalizedPath)
		}
		seen[normalizedPath] = struct{}{}

		fileType := file.Type
		if fileType == "" {
			fileType = NativeEtcRegularFile
		}
		if fileType != NativeEtcRegularFile {
			return nil, fmt.Errorf("%s entries are not allowed in generated confext input: %q", fileType, file.Path)
		}

		mode := file.Mode
		if mode == 0 {
			mode = 0o644
		}
		if err := validateNativeEtcMode(file.Path, mode); err != nil {
			return nil, err
		}
		if file.UID != 0 || file.GID != 0 {
			return nil, fmt.Errorf("native /etc file %q must be owned by root:root", file.Path)
		}

		confextPath := ""
		if cleanRoot != "" {
			confextPath = filepath.Join(cleanRoot, strings.TrimPrefix(normalizedPath, "/"))
			if !pathWithinRoot(cleanRoot, confextPath) {
				return nil, fmt.Errorf("native /etc file %q would write outside confext root", file.Path)
			}
		}

		plans = append(plans, NativeEtcFilePlan{
			Path:        normalizedPath,
			ConfextPath: confextPath,
			Mode:        mode.Perm(),
			UID:         file.UID,
			GID:         file.GID,
		})
	}

	sort.Slice(plans, func(i, j int) bool {
		return plans[i].Path < plans[j].Path
	})

	return plans, nil
}

func NativeEtcFilesFromManifest(files map[string]string) []NativeEtcFile {
	nativeFiles := make([]NativeEtcFile, 0, len(files))
	for path, content := range files {
		nativeFiles = append(nativeFiles, NativeEtcFile{
			Path:    path,
			Content: content,
			Mode:    0o644,
			UID:     0,
			GID:     0,
		})
	}
	return nativeFiles
}

func RenderGenerationTree(request GenerationTreeRequest) (GenerationTree, error) {
	if strings.TrimSpace(request.GenerationsRoot) == "" {
		return GenerationTree{}, fmt.Errorf("generations root is required")
	}
	if !filepath.IsAbs(request.GenerationsRoot) {
		return GenerationTree{}, fmt.Errorf("generations root must be absolute")
	}
	generationID, err := normalizeGenerationID(request.GenerationID)
	if err != nil {
		return GenerationTree{}, err
	}

	extension, err := normalizeExtensionRelease(request.Extension)
	if err != nil {
		return GenerationTree{}, err
	}

	generationDir := filepath.Join(filepath.Clean(request.GenerationsRoot), generationID)
	confextDir := filepath.Join(generationDir, "confext")
	plans, err := ValidateNativeEtcBundle(confextDir, request.Files)
	if err != nil {
		return GenerationTree{}, err
	}

	contentByPath := make(map[string]string, len(request.Files))
	for _, file := range request.Files {
		normalizedPath, err := normalizeNativeEtcPath(file.Path)
		if err != nil {
			return GenerationTree{}, err
		}
		contentByPath[normalizedPath] = file.Content
	}

	chown := request.Chown
	if chown == nil {
		chown = os.Chown
	}

	for _, plan := range plans {
		if err := os.MkdirAll(filepath.Dir(plan.ConfextPath), 0o755); err != nil {
			return GenerationTree{}, fmt.Errorf("create parent for %s: %w", plan.Path, err)
		}
		if err := os.WriteFile(plan.ConfextPath, []byte(contentByPath[plan.Path]), plan.Mode); err != nil {
			return GenerationTree{}, fmt.Errorf("write %s: %w", plan.Path, err)
		}
		if err := os.Chmod(plan.ConfextPath, plan.Mode); err != nil {
			return GenerationTree{}, fmt.Errorf("chmod %s: %w", plan.Path, err)
		}
		if err := chown(plan.ConfextPath, plan.UID, plan.GID); err != nil {
			return GenerationTree{}, fmt.Errorf("chown %s: %w", plan.Path, err)
		}
	}

	extensionPath := filepath.Join(confextDir, "etc", "extension-release.d", "extension-release."+extension.Name)
	if err := os.MkdirAll(filepath.Dir(extensionPath), 0o755); err != nil {
		return GenerationTree{}, fmt.Errorf("create extension-release directory: %w", err)
	}
	if err := os.WriteFile(extensionPath, []byte(renderExtensionRelease(extension)), 0o644); err != nil {
		return GenerationTree{}, fmt.Errorf("write extension-release metadata: %w", err)
	}
	if err := chown(extensionPath, 0, 0); err != nil {
		return GenerationTree{}, fmt.Errorf("chown extension-release metadata: %w", err)
	}

	return GenerationTree{
		GenerationDir:        generationDir,
		ConfextDir:           confextDir,
		ExtensionReleasePath: extensionPath,
		Files:                plans,
	}, nil
}

func normalizeNativeEtcPath(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("native /etc file path is required")
	}
	if strings.ContainsRune(path, 0) {
		return "", fmt.Errorf("native /etc file path %q contains a NUL byte", path)
	}
	if !strings.HasPrefix(path, "/") {
		return "", fmt.Errorf("native /etc file path %q must be absolute", path)
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == ".." {
			return "", fmt.Errorf("native /etc file path %q contains path traversal", path)
		}
	}

	normalized := filepath.Clean(path)
	if normalized == "/etc" || !strings.HasPrefix(normalized, "/etc/") {
		return "", fmt.Errorf("native /etc file path %q must be under /etc", path)
	}
	if normalized == "/etc/kubernetes" || strings.HasPrefix(normalized, "/etc/kubernetes/") {
		return "", fmt.Errorf("native /etc file path %q cannot own kubeadm-managed /etc/kubernetes state", path)
	}
	if normalized == "/etc/extension-release.d" || strings.HasPrefix(normalized, "/etc/extension-release.d/") {
		return "", fmt.Errorf("native /etc file path %q cannot own generated confext extension-release metadata", path)
	}

	return normalized, nil
}

func validateNativeEtcMode(path string, mode fs.FileMode) error {
	if mode.Type() != 0 {
		return fmt.Errorf("native /etc file %q must be a regular file", path)
	}
	if mode&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky) != 0 {
		return fmt.Errorf("native /etc file %q cannot set special permission bits", path)
	}

	perm := mode.Perm()
	if perm&0o022 != 0 {
		return fmt.Errorf("native /etc file %q cannot be group- or world-writable", path)
	}
	if perm&0o111 != 0 {
		return fmt.Errorf("native /etc file %q cannot be executable", path)
	}
	if perm > 0o644 {
		return fmt.Errorf("native /etc file %q mode %04o is too permissive", path, perm)
	}
	return nil
}

func pathWithinRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, "../") && !filepath.IsAbs(rel)
}

func normalizeGenerationID(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("generation id is required")
	}
	if strings.ContainsAny(value, `/\`) || value == "." || value == ".." || filepath.Clean(value) != value {
		return "", fmt.Errorf("generation id %q must be a single path segment", value)
	}
	return value, nil
}

func normalizeExtensionRelease(release ExtensionRelease) (ExtensionRelease, error) {
	if release.Name == "" {
		release.Name = "katl-node"
	}
	if release.ID == "" {
		release.ID = "katl"
	}
	if release.VersionID == "" {
		release.VersionID = "0.1.0"
	}
	if release.ConfextLevel == 0 {
		release.ConfextLevel = 1
	}

	for field, value := range map[string]string{
		"name":       release.Name,
		"id":         release.ID,
		"version_id": release.VersionID,
	} {
		if strings.TrimSpace(value) == "" || strings.ContainsAny(value, "\n\r/") {
			return ExtensionRelease{}, fmt.Errorf("extension-release %s %q is invalid", field, value)
		}
	}
	if release.ConfextLevel < 1 {
		return ExtensionRelease{}, fmt.Errorf("extension-release confext level must be positive")
	}

	return release, nil
}

func renderExtensionRelease(release ExtensionRelease) string {
	return fmt.Sprintf("ID=%s\nVERSION_ID=%s\nCONFEXT_LEVEL=%d\n", release.ID, release.VersionID, release.ConfextLevel)
}
