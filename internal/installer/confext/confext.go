package confext

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

type NativeEtcFileType string

const (
	NativeEtcRegularFile NativeEtcFileType = "regular"
	NativeEtcSymlink     NativeEtcFileType = "symlink"
	NativeEtcCharDevice  NativeEtcFileType = "char-device"
	NativeEtcBlockDevice NativeEtcFileType = "block-device"
)

type NativeEtcFile struct {
	Path    string            `json:"path"`
	Content string            `json:"content"`
	Type    NativeEtcFileType `json:"type,omitempty"`
	Mode    fs.FileMode       `json:"mode,omitempty"`
	UID     int               `json:"uid,omitempty"`
	GID     int               `json:"gid,omitempty"`
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
	if err := mkdirAllNoSymlink(filepath.Clean(request.GenerationsRoot), confextDir, 0o755); err != nil {
		return GenerationTree{}, err
	}
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
		chown = os.Lchown
	}

	for _, plan := range plans {
		if err := writeConfextRegularFile(confextDir, plan.ConfextPath, []byte(contentByPath[plan.Path]), plan.Mode); err != nil {
			return GenerationTree{}, fmt.Errorf("write %s: %w", plan.Path, err)
		}
		if err := chown(plan.ConfextPath, plan.UID, plan.GID); err != nil {
			return GenerationTree{}, fmt.Errorf("chown %s: %w", plan.Path, err)
		}
	}

	extensionPath := filepath.Join(confextDir, "etc", "extension-release.d", "extension-release."+extension.Name)
	if err := writeConfextRegularFile(confextDir, extensionPath, []byte(renderExtensionRelease(extension)), 0o644); err != nil {
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
	if isHostPolicyPath(normalized) {
		return "", fmt.Errorf("native /etc file path %q cannot own Katl-managed host account, authentication, or sshd policy", path)
	}

	return normalized, nil
}

func isHostPolicyPath(path string) bool {
	switch path {
	case "/etc/passwd",
		"/etc/shadow",
		"/etc/group",
		"/etc/gshadow",
		"/etc/sudoers",
		"/etc/subuid",
		"/etc/subgid",
		"/etc/ssh/sshd_config":
		return true
	}
	for _, prefix := range []string{
		"/etc/sudoers.d",
		"/etc/pam.d",
		"/etc/security",
		"/etc/sysusers.d",
		"/etc/ssh/sshd_config.d",
	} {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
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

func mkdirAllNoSymlink(root, path string, mode fs.FileMode) error {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	if !pathWithinRoot(root, path) && root != path {
		return fmt.Errorf("%s would write outside confext root", path)
	}
	current := string(filepath.Separator)
	for _, segment := range strings.Split(strings.TrimPrefix(root, string(filepath.Separator)), string(filepath.Separator)) {
		if segment == "" {
			continue
		}
		current = filepath.Join(current, segment)
		if err := ensureDirectoryNoSymlink(current, mode); err != nil {
			return err
		}
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return err
	}
	if rel == "." {
		return nil
	}
	for _, segment := range strings.Split(rel, string(filepath.Separator)) {
		if segment == "" || segment == "." {
			continue
		}
		current = filepath.Join(current, segment)
		if err := ensureDirectoryNoSymlink(current, mode); err != nil {
			return err
		}
	}
	return nil
}

func ensureDirectoryNoSymlink(path string, mode fs.FileMode) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return os.Mkdir(path, mode)
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to follow symlink in generated confext path: %s", path)
	}
	if !info.IsDir() {
		return fmt.Errorf("generated confext path component is not a directory: %s", path)
	}
	return nil
}

func writeConfextRegularFile(root, path string, content []byte, mode fs.FileMode) error {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	if !pathWithinRoot(root, path) {
		return fmt.Errorf("%s would write outside confext root", path)
	}
	if err := mkdirAllNoSymlink(root, filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to follow symlink in generated confext path: %s", path)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("generated confext target is not a regular file: %s", path)
		}
		if linkCount(info) > 1 {
			return fmt.Errorf("refusing to overwrite hard-linked generated confext file: %s", path)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, mode)
	if err != nil {
		return err
	}
	defer file.Close()
	n, err := file.Write(content)
	if err != nil {
		return err
	}
	if n != len(content) {
		return fmt.Errorf("short write: wrote %d of %d bytes", n, len(content))
	}
	return file.Chmod(mode)
}

func linkCount(info fs.FileInfo) uint64 {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 1
	}
	return uint64(stat.Nlink)
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
		release.ID = "katlos"
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
