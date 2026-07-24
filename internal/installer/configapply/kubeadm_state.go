package configapply

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/katl-dev/katl/internal/installer/generation"
	"github.com/katl-dev/katl/internal/installer/kubeadmconfig"
	"github.com/katl-dev/katl/internal/installer/persistedrecord"
)

const (
	generationKubeadmInputDir = "kubeadm-input"
	maxGenerationAncestry     = 256
)

func WriteGenerationKubeadmConfig(root, generationID, ref string, plan kubeadmconfig.Plan) error {
	dir, err := generation.GenerationDir(root, generationID)
	if err != nil {
		return err
	}
	ref, err = cleanKubeadmInputRef(ref)
	if err != nil {
		return err
	}
	files := append([]kubeadmconfig.File{plan.Config}, plan.Patches...)
	if _, err := kubeadmconfig.PlanFromRenderedFiles(ref, files); err != nil {
		return fmt.Errorf("validate desired kubeadm input: %w", err)
	}
	base := filepath.Join(dir, generationKubeadmInputDir, ref)
	for _, file := range files {
		rel, err := kubeadmInputRelativePath(ref, file.RenderPath)
		if err != nil {
			return err
		}
		path := filepath.Join(base, rel)
		mode := file.Mode.Perm()
		if mode == 0 {
			mode = 0o644
		}
		if err := persistedrecord.WriteFileAtomic(path, file.Content, mode); err != nil {
			return fmt.Errorf("write desired kubeadm input %s: %w", rel, err)
		}
	}
	return nil
}

func ReadEffectiveGenerationKubeadmConfig(root, generationID, ref string) (kubeadmconfig.Plan, string, error) {
	var err error
	ref, err = cleanKubeadmInputRef(ref)
	if err != nil {
		return kubeadmconfig.Plan{}, "", err
	}
	selected := strings.TrimSpace(generationID)
	visited := map[string]bool{}
	for depth := 0; selected != "" && depth < maxGenerationAncestry; depth++ {
		if visited[selected] {
			return kubeadmconfig.Plan{}, "", fmt.Errorf("generation kubeadm input ancestry contains a cycle at %q", selected)
		}
		visited[selected] = true
		plan, err := readGenerationKubeadmConfig(root, selected, ref)
		if err == nil {
			return plan, selected, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return kubeadmconfig.Plan{}, "", err
		}
		spec, _, readErr := generation.ReadGeneration(root, selected)
		if readErr != nil {
			return kubeadmconfig.Plan{}, "", fmt.Errorf("read generation %q while resolving its kubeadm input: %w", selected, readErr)
		}
		selected = strings.TrimSpace(spec.PreviousGenerationID)
	}
	if selected != "" {
		return kubeadmconfig.Plan{}, "", fmt.Errorf("generation kubeadm input ancestry exceeds %d generations", maxGenerationAncestry)
	}
	return kubeadmconfig.Plan{}, "", fmt.Errorf("generation %q has no desired kubeadm input ancestry: %w", generationID, os.ErrNotExist)
}

func readGenerationKubeadmConfig(root, generationID, ref string) (kubeadmconfig.Plan, error) {
	dir, err := generation.GenerationDir(root, generationID)
	if err != nil {
		return kubeadmconfig.Plan{}, err
	}
	base := filepath.Join(dir, generationKubeadmInputDir, ref)
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
		return kubeadmconfig.Plan{}, fmt.Errorf("read generation %s desired kubeadm input: %w", generationID, err)
	}
	plan, err := kubeadmconfig.PlanFromRenderedFiles(ref, files)
	if err != nil {
		return kubeadmconfig.Plan{}, fmt.Errorf("decode generation %s desired kubeadm input: %w", generationID, err)
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
