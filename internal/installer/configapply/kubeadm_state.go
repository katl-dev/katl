package configapply

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/kubeadmconfig"
	"github.com/katl-dev/katl/internal/installer/manifest"
)

const generationKubeadmInputDir = "kubeadm-input"

func WriteGenerationKubeadmConfig(root, generationID string, input kubeadmconfig.NodeInput) error {
	files, err := generationKubeadmFiles(input)
	if err != nil {
		return err
	}
	return writeConfigurationFiles(root, generationID, files)
}

func generationKubeadmFiles(input kubeadmconfig.NodeInput) ([]configurationFile, error) {
	ref, err := cleanKubeadmInputRef(input.Name())
	if err != nil {
		return nil, err
	}
	files, err := input.InputFiles()
	if err != nil {
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

// ReadEffectiveGenerationKubeadmConfig returns the kubeadm input that a
// generation, or the nearest ancestor holding one, owns for node. Generations
// stored the input in different forms, so it is normalised for node.
func ReadEffectiveGenerationKubeadmConfig(root, generationID, ref string, node manifest.NodeConfig) (kubeadmconfig.NodeInput, string, error) {
	ref, err := cleanKubeadmInputRef(ref)
	if err != nil {
		return kubeadmconfig.NodeInput{}, "", err
	}
	files, source, err := readGenerationInput(root, generationID, "kubeadm input", func(id string) ([]kubeadmconfig.File, error) {
		files, err := readGenerationKubeadmFiles(root, id, ref)
		if !errors.Is(err, os.ErrNotExist) {
			return files, err
		}
		// Input ownership follows the generation, regardless of its storage format.
		dir, err := generation.GenerationDir(root, id)
		if err != nil {
			return nil, err
		}
		return readConfextKubeadmInput(filepath.Join(dir, "confext"), ref)
	})
	if errors.Is(err, os.ErrNotExist) {
		source = ""
		files, err = readKubeadmInputFiles(filepath.Join(root, "var/lib/katl/cluster/kubeadm", ref), ref)
	}
	if err != nil {
		return kubeadmconfig.NodeInput{}, source, err
	}
	input, err := kubeadmconfig.ReadNodeInput(ref, files, node)
	if err != nil {
		return kubeadmconfig.NodeInput{}, source, fmt.Errorf("decode kubeadm input: %w", err)
	}
	return input, source, nil
}

func readConfextKubeadmInput(confext, ref string) ([]kubeadmconfig.File, error) {
	files, err := readKubeadmInputFiles(filepath.Join(confext, kubeadmconfig.InputDir(ref)), ref)
	if err != nil {
		return nil, err
	}
	runtime, err := os.ReadFile(filepath.Join(confext, "etc/katl/bootstrap-runtime.json"))
	if errors.Is(err, os.ErrNotExist) {
		return files, nil
	}
	if err != nil {
		return nil, err
	}
	var metadata struct {
		Kubeadm struct {
			Path           string `json:"path"`
			Digest         string `json:"digest"`
			InitConfigPath string `json:"initConfigPath"`
		} `json:"kubeadm"`
	}
	if err := json.Unmarshal(runtime, &metadata); err != nil {
		return nil, fmt.Errorf("decode bootstrap runtime metadata: %w", err)
	}
	path := kubeadmconfig.ConfigPath(ref)
	if metadata.Kubeadm.InitConfigPath != "" || (metadata.Kubeadm.Path != "" && metadata.Kubeadm.Path != path) {
		return files, nil
	}

	// Legacy init overwrote the input with the config that delegates its health
	// check to Katl. A matching digest proves that the file is the original
	// input, which can also request that phase be skipped.
	for _, file := range files {
		if file.RenderPath != path {
			continue
		}
		delegated, err := kubeadmconfig.HasDelegatedInitHealth(file.Content)
		if err != nil {
			return nil, fmt.Errorf("decode confext kubeadm input: %w", err)
		}
		if !delegated {
			return files, nil
		}
		if metadata.Kubeadm.Digest == "" {
			return nil, os.ErrNotExist
		}
		plan, err := kubeadmconfig.PlanFromInputFiles(ref, files)
		if err != nil {
			return nil, err
		}
		digest, err := plan.InputDigest()
		if err != nil {
			return nil, err
		}
		if digest != metadata.Kubeadm.Digest {
			return nil, os.ErrNotExist
		}
		break
	}
	return files, nil
}

func readGenerationKubeadmFiles(root, generationID, ref string) ([]kubeadmconfig.File, error) {
	dir, err := generation.GenerationDir(root, generationID)
	if err != nil {
		return nil, err
	}
	return readKubeadmInputFiles(filepath.Join(dir, generationKubeadmInputDir, ref), ref)
}

func readKubeadmInputFiles(base, ref string) ([]kubeadmconfig.File, error) {
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
			RenderPath: filepath.ToSlash(filepath.Join(kubeadmconfig.InputDir(ref), rel)),
			Content:    content,
			Mode:       info.Mode().Perm(),
		})
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read kubeadm input %s: %w", base, err)
	}
	return files, nil
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
	base := filepath.ToSlash(kubeadmconfig.InputDir(ref))
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
