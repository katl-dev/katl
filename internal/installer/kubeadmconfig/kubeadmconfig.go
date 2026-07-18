package kubeadmconfig

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/katl-dev/katl/internal/installer/confext"
	"gopkg.in/yaml.v3"
)

const (
	APIVersion = "config.katl.dev/v1alpha1"
	Kind       = "KubeadmConfig"

	KubeletVolumePluginDir = "/var/lib/kubelet/plugins/volume/exec"
)

type Object struct {
	APIVersion string   `yaml:"apiVersion" json:"apiVersion"`
	Kind       string   `yaml:"kind" json:"kind"`
	Metadata   Metadata `yaml:"metadata" json:"metadata"`
	Spec       Spec     `yaml:"spec" json:"spec"`
}

type Metadata struct {
	Name string `yaml:"name" json:"name"`
}

type Spec struct {
	ConfigFile        string `yaml:"configFile" json:"configFile"`
	PatchesDir        string `yaml:"patchesDir,omitempty" json:"patchesDir,omitempty"`
	KubernetesVersion string `yaml:"kubernetesVersion,omitempty" json:"kubernetesVersion,omitempty"`
}

type ResolveRequest struct {
	RepoRoot          string
	Object            Object
	KubernetesVersion string
}

type Plan struct {
	Name      string
	Config    File
	Patches   []File
	Documents []Document
}

type File struct {
	SourcePath string
	RenderPath string
	Content    []byte
	Mode       fs.FileMode
}

type Document struct {
	APIVersion        string
	Kind              string
	ControlPlane      bool
	KubernetesVersion string
}

func Decode(reader io.Reader) (Object, error) {
	decoder := yaml.NewDecoder(reader)
	decoder.KnownFields(true)
	var object Object
	if err := decoder.Decode(&object); err != nil {
		return Object{}, fmt.Errorf("decode KubeadmConfig: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return Object{}, fmt.Errorf("decode KubeadmConfig: multiple YAML documents")
	}
	if err := validateObjectEnvelope(object); err != nil {
		return Object{}, err
	}
	return object, nil
}

func Resolve(request ResolveRequest) (Plan, error) {
	if strings.TrimSpace(request.RepoRoot) == "" {
		return Plan{}, fmt.Errorf("repository root is required")
	}
	if !filepath.IsAbs(request.RepoRoot) {
		return Plan{}, fmt.Errorf("repository root must be absolute")
	}
	if err := validateObjectEnvelope(request.Object); err != nil {
		return Plan{}, err
	}
	if request.Object.Spec.KubernetesVersion != "" && request.KubernetesVersion != "" && request.Object.Spec.KubernetesVersion != request.KubernetesVersion {
		return Plan{}, fmt.Errorf("KubeadmConfig %q kubernetesVersion %q does not match selected Kubernetes version %q", request.Object.Metadata.Name, request.Object.Spec.KubernetesVersion, request.KubernetesVersion)
	}

	repoRoot := filepath.Clean(request.RepoRoot)
	configSource, err := repoFile(repoRoot, request.Object.Spec.ConfigFile)
	if err != nil {
		return Plan{}, fmt.Errorf("configFile: %w", err)
	}
	configData, err := os.ReadFile(configSource)
	if err != nil {
		return Plan{}, fmt.Errorf("read kubeadm config: %w", err)
	}
	renderBase := "/etc/katl/kubeadm/" + request.Object.Metadata.Name
	patchesRenderDir := renderBase + "/patches"
	documents, err := validateKubeadmYAML(configData, patchesRenderDir)
	if err != nil {
		return Plan{}, err
	}
	patches, err := resolvePatches(repoRoot, request.Object.Spec.PatchesDir, patchesRenderDir)
	if err != nil {
		return Plan{}, err
	}
	return Plan{
		Name: request.Object.Metadata.Name,
		Config: File{
			SourcePath: configSource,
			RenderPath: renderBase + "/config.yaml",
			Content:    configData,
			Mode:       0o644,
		},
		Patches:   patches,
		Documents: documents,
	}, nil
}

func PlanFromRenderedFiles(name string, files []File) (Plan, error) {
	if err := validateName("name", name); err != nil {
		return Plan{}, err
	}
	renderBase := "/etc/katl/kubeadm/" + name
	configPath := renderBase + "/config.yaml"
	patchesRenderDir := renderBase + "/patches"
	var config File
	var patches []File
	for _, file := range files {
		renderPath := filepath.ToSlash(filepath.Clean("/" + strings.TrimPrefix(strings.TrimSpace(file.RenderPath), "/")))
		if renderPath == configPath {
			config = file
			config.RenderPath = renderPath
			if config.Mode == 0 {
				config.Mode = 0o644
			}
			continue
		}
		if strings.HasPrefix(renderPath, patchesRenderDir+"/") {
			patch := file
			patch.RenderPath = renderPath
			if patch.Mode == 0 {
				patch.Mode = 0o644
			}
			if err := validatePatchYAML(patch.Content); err != nil {
				return Plan{}, fmt.Errorf("patch %q: %w", strings.TrimPrefix(renderPath, patchesRenderDir+"/"), err)
			}
			patches = append(patches, patch)
			continue
		}
		return Plan{}, fmt.Errorf("rendered kubeadm file path %q must be %s or under %s", renderPath, configPath, patchesRenderDir)
	}
	if config.RenderPath == "" {
		return Plan{}, fmt.Errorf("rendered kubeadm input %q missing config.yaml", name)
	}
	documents, err := validateKubeadmYAML(config.Content, patchesRenderDir)
	if err != nil {
		return Plan{}, err
	}
	sort.Slice(patches, func(i, j int) bool {
		return patches[i].RenderPath < patches[j].RenderPath
	})
	return Plan{
		Name:      name,
		Config:    config,
		Patches:   patches,
		Documents: documents,
	}, nil
}

func (p Plan) NativeEtcFiles() []confext.NativeEtcFile {
	files := make([]confext.NativeEtcFile, 0, 1+len(p.Patches))
	files = append(files, confext.NativeEtcFile{
		Path:    p.Config.RenderPath,
		Content: string(p.Config.Content),
		Mode:    p.Config.Mode,
		UID:     0,
		GID:     0,
	})
	for _, patch := range p.Patches {
		files = append(files, confext.NativeEtcFile{
			Path:    patch.RenderPath,
			Content: string(patch.Content),
			Mode:    patch.Mode,
			UID:     0,
			GID:     0,
		})
	}
	return files
}

func validateObjectEnvelope(object Object) error {
	if object.APIVersion != APIVersion {
		return fmt.Errorf("KubeadmConfig apiVersion must be %s", APIVersion)
	}
	if object.Kind != Kind {
		return fmt.Errorf("KubeadmConfig kind must be %s", Kind)
	}
	if err := validateName("metadata.name", object.Metadata.Name); err != nil {
		return err
	}
	if strings.TrimSpace(object.Spec.ConfigFile) == "" {
		return fmt.Errorf("KubeadmConfig %q spec.configFile is required", object.Metadata.Name)
	}
	return nil
}

func validateName(field, name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("%s is required", field)
	}
	if strings.TrimSpace(name) != name {
		return fmt.Errorf("%s %q must not contain leading or trailing whitespace", field, name)
	}
	if name != filepath.Base(name) || name == "." || name == ".." {
		return fmt.Errorf("%s %q must be a single path segment", field, name)
	}
	if len(name) > 63 {
		return fmt.Errorf("%s %q must be 63 characters or fewer", field, name)
	}
	if !isLowercaseLetterOrDigit(rune(name[0])) || !isLowercaseLetterOrDigit(rune(name[len(name)-1])) {
		return fmt.Errorf("%s %q must start and end with a lowercase letter or digit", field, name)
	}
	for _, r := range name {
		ok := isLowercaseLetterOrDigit(r) || r == '-'
		if !ok {
			return fmt.Errorf("%s %q must contain only lowercase letters, digits, and dashes", field, name)
		}
	}
	return nil
}

func isLowercaseLetterOrDigit(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= '0' && r <= '9'
}

func repoFile(repoRoot, rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("%q must be repository-relative", rel)
	}
	cleanRel, err := cleanRepoRel(rel)
	if err != nil {
		return "", err
	}
	path := filepath.Join(repoRoot, cleanRel)
	if !pathWithinRoot(repoRoot, path) || !pathWithinRootAfterSymlinks(repoRoot, path) {
		return "", fmt.Errorf("%q escapes repository root", rel)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%q must not be a symlink", rel)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%q must be a regular file", rel)
	}
	return path, nil
}

func repoDir(repoRoot, rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("%q must be repository-relative", rel)
	}
	cleanRel, err := cleanRepoRel(rel)
	if err != nil {
		return "", err
	}
	path := filepath.Join(repoRoot, cleanRel)
	if !pathWithinRoot(repoRoot, path) || !pathWithinRootAfterSymlinks(repoRoot, path) {
		return "", fmt.Errorf("%q escapes repository root", rel)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%q must not be a symlink", rel)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%q must be a directory", rel)
	}
	return path, nil
}

func cleanRepoRel(rel string) (string, error) {
	cleanRel := filepath.Clean(rel)
	if cleanRel == "." {
		return "", fmt.Errorf("%q is not a file path", rel)
	}
	for _, segment := range strings.Split(cleanRel, string(os.PathSeparator)) {
		if segment == ".." {
			return "", fmt.Errorf("%q contains path traversal", rel)
		}
	}
	return cleanRel, nil
}

func pathWithinRoot(root, path string) bool {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	return path == root || strings.HasPrefix(path, root+string(os.PathSeparator))
}

func pathWithinRootAfterSymlinks(root, path string) bool {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	return pathWithinRoot(resolvedRoot, resolvedPath)
}

func validateKubeadmYAML(data []byte, patchesRenderDir string) ([]Document, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var documents []Document
	for index := 0; ; index++ {
		var node yaml.Node
		err := decoder.Decode(&node)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parse kubeadm YAML document %d: %w", index+1, err)
		}
		if emptyDocument(&node) {
			continue
		}
		apiVersion := mappingScalar(&node, "apiVersion")
		kind := mappingScalar(&node, "kind")
		if !allowedDocument(apiVersion, kind) {
			return nil, fmt.Errorf("unsupported kubeadm YAML document %d: apiVersion=%q kind=%q", index+1, apiVersion, kind)
		}
		if err := validateYAMLPaths(&node, patchesRenderDir); err != nil {
			return nil, fmt.Errorf("kubeadm YAML document %d: %w", index+1, err)
		}
		documents = append(documents, Document{
			APIVersion:        apiVersion,
			Kind:              kind,
			ControlPlane:      kind == "JoinConfiguration" && mappingHasKey(&node, "controlPlane"),
			KubernetesVersion: mappingScalar(&node, "kubernetesVersion"),
		})
	}
	if len(documents) == 0 {
		return nil, fmt.Errorf("kubeadm config must contain at least one YAML document")
	}
	return documents, nil
}

func emptyDocument(node *yaml.Node) bool {
	if node.Kind == 0 {
		return true
	}
	if node.Kind == yaml.DocumentNode && len(node.Content) == 0 {
		return true
	}
	if node.Kind == yaml.DocumentNode && len(node.Content) == 1 && node.Content[0].Kind == yaml.ScalarNode && node.Content[0].Value == "" {
		return true
	}
	return false
}

func mappingScalar(node *yaml.Node, key string) string {
	root := node
	if root.Kind == yaml.DocumentNode && len(root.Content) == 1 {
		root = root.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return ""
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == key {
			return strings.TrimSpace(root.Content[i+1].Value)
		}
	}
	return ""
}

func mappingHasKey(node *yaml.Node, key string) bool {
	root := node
	if root.Kind == yaml.DocumentNode && len(root.Content) == 1 {
		root = root.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == key {
			return true
		}
	}
	return false
}

func allowedDocument(apiVersion, kind string) bool {
	switch apiVersion {
	case "kubeadm.k8s.io/v1beta4":
		switch kind {
		case "InitConfiguration", "JoinConfiguration", "ClusterConfiguration":
			return true
		}
	case "kubelet.config.k8s.io/v1beta1":
		return kind == "KubeletConfiguration"
	default:
		return strings.HasPrefix(apiVersion, "kubeproxy.config.k8s.io/") && kind == "KubeProxyConfiguration"
	}
	return false
}

func validateYAMLPaths(node *yaml.Node, patchesRenderDir string) error {
	return walkYAML(node, nil, func(path []string, value string) error {
		if value == "" {
			return nil
		}
		if len(path) >= 2 && path[len(path)-2] == "patches" && path[len(path)-1] == "directory" {
			if value != patchesRenderDir {
				return fmt.Errorf("patches.directory must be %s, got %s", patchesRenderDir, value)
			}
			return nil
		}
		if len(path) == 1 && path[0] == "certificatesDir" && value == "/etc/kubernetes/pki" {
			return nil
		}
		if len(path) == 1 && path[0] == "volumePluginDir" && value == KubeletVolumePluginDir {
			return nil
		}
		if strings.HasPrefix(value, "/") && deniedHostPath(value) {
			return fmt.Errorf("host path %s is denied", value)
		}
		return nil
	})
}

func walkYAML(node *yaml.Node, path []string, visit func(path []string, value string) error) error {
	switch node.Kind {
	case yaml.DocumentNode:
		for _, child := range node.Content {
			if err := walkYAML(child, path, visit); err != nil {
				return err
			}
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i].Value
			if err := walkYAML(node.Content[i+1], append(path, key), visit); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		for _, child := range node.Content {
			if err := walkYAML(child, path, visit); err != nil {
				return err
			}
		}
	case yaml.ScalarNode:
		return visit(path, strings.TrimSpace(node.Value))
	}
	return nil
}

func deniedHostPath(path string) bool {
	path = filepath.Clean(path)
	for _, denied := range deniedHostPathPrefixes() {
		if path == denied || strings.HasPrefix(path, denied+"/") {
			return true
		}
	}
	return false
}

func deniedHostPathPrefixes() []string {
	return []string{
		"/etc/kubernetes",
		"/etc/passwd",
		"/etc/shadow",
		"/etc/group",
		"/etc/gshadow",
		"/etc/sudoers",
		"/etc/sudoers.d",
		"/etc/pam.d",
		"/etc/security",
		"/etc/ssh",
		"/usr",
		"/boot",
		"/efi",
		"/run",
		"/tmp",
		"/var/lib/katl/generations",
		"/var/lib/katl/kubernetes",
		"/var/lib/containerd",
		"/var/lib/kubelet",
	}
}

func resolvePatches(repoRoot, patchesDir, renderDir string) ([]File, error) {
	if strings.TrimSpace(patchesDir) == "" {
		return nil, nil
	}
	root, err := repoDir(repoRoot, patchesDir)
	if err != nil {
		return nil, fmt.Errorf("patchesDir: %w", err)
	}
	var patches []File
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
			return fmt.Errorf("patch path %q escapes patchesDir", rel)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("patch %q must not be a symlink", rel)
		}
		if entry.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("patch %q must be a regular file", rel)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := validatePatchYAML(data); err != nil {
			return fmt.Errorf("patch %q: %w", rel, err)
		}
		rel = filepath.ToSlash(filepath.Clean(rel))
		patches = append(patches, File{
			SourcePath: path,
			RenderPath: renderDir + "/" + rel,
			Content:    data,
			Mode:       0o644,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(patches, func(i, j int) bool {
		return patches[i].RenderPath < patches[j].RenderPath
	})
	return patches, nil
}

func validatePatchYAML(data []byte) error {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	for index := 0; ; index++ {
		var node yaml.Node
		err := decoder.Decode(&node)
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("parse patch YAML document %d: %w", index+1, err)
		}
		if emptyDocument(&node) {
			continue
		}
		if err := walkYAML(&node, nil, func(_ []string, value string) error {
			if strings.HasPrefix(value, "/") && deniedHostPath(value) {
				return fmt.Errorf("host path %s is denied", value)
			}
			return nil
		}); err != nil {
			return fmt.Errorf("patch YAML document %d: %w", index+1, err)
		}
	}
	return nil
}
