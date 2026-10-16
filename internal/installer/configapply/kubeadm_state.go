package configapply

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/kubeadmconfig"
)

const generationKubeadmInputDir = "kubeadm-input"

func WriteGenerationKubeadmConfig(root, generationID, ref string, plan kubeadmconfig.Plan) error {
	files, err := generationKubeadmFiles(ref, plan)
	if err != nil {
		return err
	}
	return writeConfigurationFiles(root, generationID, files)
}

func generationKubeadmFiles(ref string, plan kubeadmconfig.Plan) ([]configurationFile, error) {
	ref, err := cleanKubeadmInputRef(ref)
	if err != nil {
		return nil, err
	}
	files := append([]kubeadmconfig.File{plan.Config}, plan.Patches...)
	if _, err := kubeadmconfig.PlanFromRenderedFiles(ref, files); err != nil {
		return nil, fmt.Errorf("validate desired kubeadm input: %w", err)
	}
	var result []configurationFile
	for _, file := range files {
		rel, err := kubeadmInputRelativePath(ref, file.RenderPath)
		if err != nil {
			return nil, err
		}
		mode := file.Mode.Perm()
		if mode == 0 {
			mode = 0o644
		}
		result = append(result, configurationFile{path: filepath.Join(generationKubeadmInputDir, ref, rel), content: file.Content, mode: mode})
	}
	return result, nil
}

func ReadEffectiveGenerationKubeadmConfig(root, generationID, ref string) (kubeadmconfig.Plan, string, error) {
	ref, err := cleanKubeadmInputRef(ref)
	if err != nil {
		return kubeadmconfig.Plan{}, "", err
	}
	plan, source, err := readGenerationInput(root, generationID, "kubeadm input", func(id string) (kubeadmconfig.Plan, error) {
		return readGenerationKubeadmConfig(root, id, ref)
	})
	if !errors.Is(err, os.ErrNotExist) {
		return plan, source, err
	}
	// Older generations kept only rendered kubeadm inputs in their confext.
	plan, source, err = readGenerationInput(root, generationID, "kubeadm input", func(id string) (kubeadmconfig.Plan, error) {
		dir, err := generation.GenerationDir(root, id)
		if err != nil {
			return kubeadmconfig.Plan{}, err
		}
		return readKubeadmConfigDir(filepath.Join(dir, "confext/etc/katl/kubeadm", ref), ref)
	})
	if !errors.Is(err, os.ErrNotExist) {
		return plan, source, err
	}
	plan, err = readKubeadmConfigDir(filepath.Join(root, "var/lib/katl/cluster/kubeadm", ref), ref)
	return plan, "", err
}

func readGenerationKubeadmConfig(root, generationID, ref string) (kubeadmconfig.Plan, error) {
	dir, err := generation.GenerationDir(root, generationID)
	if err != nil {
		return kubeadmconfig.Plan{}, err
	}
	return readKubeadmConfigDir(filepath.Join(dir, generationKubeadmInputDir, ref), ref)
}

func readKubeadmConfigDir(base, ref string) (kubeadmconfig.Plan, error) {
	var files []kubeadmconfig.File
	if err := filepath.WalkDir(base, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("desired kubeadm input %s is not a regular file", path)
		}
		rel, err := filepath.Rel(base, path)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files = append(files, kubeadmconfig.File{
			RenderPath: filepath.ToSlash(filepath.Join("/etc/katl/kubeadm", ref, rel)),
			Content:    content,
			Mode:       info.Mode().Perm(),
		})
		return nil
	}); err != nil {
		return kubeadmconfig.Plan{}, fmt.Errorf("read kubeadm input %s: %w", base, err)
	}
	plan, err := kubeadmconfig.PlanFromRenderedFiles(ref, files)
	if err != nil {
		return kubeadmconfig.Plan{}, fmt.Errorf("decode kubeadm input %s: %w", base, err)
	}
	return plan, nil
}

func cleanKubeadmInputRef(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", fmt.Errorf("desired kubeadm input ref is required")
	}
	if ref != filepath.Base(ref) || ref == "." || ref == ".." {
		return "", fmt.Errorf("desired kubeadm input ref %q must be a single path segment", ref)
	}
	return ref, nil
}

func kubeadmInputRelativePath(ref, renderPath string) (string, error) {
	base := filepath.ToSlash(filepath.Join("/etc/katl/kubeadm", ref))
	renderPath = filepath.ToSlash(filepath.Clean("/" + strings.TrimPrefix(strings.TrimSpace(renderPath), "/")))
	if !strings.HasPrefix(renderPath, base+"/") {
		return "", fmt.Errorf("desired kubeadm input path %q must be under %s", renderPath, base)
	}
	rel := strings.TrimPrefix(renderPath, base+"/")
	if rel == "" || rel == "." || strings.HasPrefix(rel, "../") || strings.Contains(rel, "/../") {
		return "", fmt.Errorf("desired kubeadm input path %q escapes its generation directory", renderPath)
	}
	return filepath.FromSlash(rel), nil
}
