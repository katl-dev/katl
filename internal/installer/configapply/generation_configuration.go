package configapply

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/kubeadmconfig"
	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/persistedrecord"
)

const maxGenerationAncestry = 256

// GenerationConfiguration is the desired configuration owned by a generation.
// Persist it before publishing the generation so pruning its history cannot
// change the configuration used by a later operation.
type GenerationConfiguration struct {
	Manifest manifest.Manifest
	// Kubeadm is the node's kubeadm input, zero when the manifest names none.
	Kubeadm kubeadmconfig.NodeInput
}

type configurationFile struct {
	path    string
	content []byte
	mode    os.FileMode
}

// SizeBytes returns the serialized file bytes written when materializing this
// configuration. Callers reserve space before writing into preparation state.
func (value GenerationConfiguration) SizeBytes() (uint64, error) {
	files, err := value.files()
	if err != nil {
		return 0, err
	}
	var size uint64
	for _, file := range files {
		size += uint64(len(file.content))
	}
	return size, nil
}

func ReadGenerationConfiguration(root, id string) (GenerationConfiguration, error) {
	value, _, err := ReadEffectiveGenerationManifest(root, id)
	if err != nil {
		return GenerationConfiguration{}, err
	}
	return readConfiguration(value, func(ref string) (kubeadmconfig.NodeInput, error) {
		input, _, err := ReadEffectiveGenerationKubeadmConfig(root, id, ref, value.Node)
		return input, err
	})
}

// ReadLocalGenerationConfiguration requires the generation's own inputs, without
// resolving ancestry or legacy installation state. Missing inputs retain
// os.ErrNotExist; invalid inputs do not count as absent.
func ReadLocalGenerationConfiguration(root, id string) (GenerationConfiguration, error) {
	value, err := ReadGenerationManifest(root, id)
	if err != nil {
		return GenerationConfiguration{}, err
	}
	return readConfiguration(value, func(ref string) (kubeadmconfig.NodeInput, error) {
		files, err := readGenerationKubeadmFiles(root, id, ref)
		if err != nil {
			return kubeadmconfig.NodeInput{}, err
		}
		return kubeadmconfig.ReadNodeInput(ref, files, value.Node)
	})
}

func readConfiguration(value manifest.Manifest, read func(string) (kubeadmconfig.NodeInput, error)) (GenerationConfiguration, error) {
	result := GenerationConfiguration{Manifest: value}
	ref := strings.TrimSpace(value.Node.Kubernetes.Kubeadm.ConfigRef)
	if ref != "" {
		input, err := read(ref)
		if err != nil {
			return GenerationConfiguration{}, err
		}
		result.Kubeadm = input
	}
	return result, nil
}

func WriteGenerationConfiguration(root, id string, value GenerationConfiguration) error {
	files, err := value.files()
	if err != nil {
		return err
	}
	return writeConfigurationFiles(root, id, files)
}

func (value GenerationConfiguration) files() ([]configurationFile, error) {
	manifestFile, err := generationManifestFile(value.Manifest)
	if err != nil {
		return nil, err
	}
	var files []configurationFile
	ref := strings.TrimSpace(value.Manifest.Node.Kubernetes.Kubeadm.ConfigRef)
	if ref != "" {
		if value.Kubeadm.IsZero() {
			return nil, fmt.Errorf("selected kubeadm config %q is missing", ref)
		}
		if value.Kubeadm.Name() != ref {
			return nil, fmt.Errorf("selected kubeadm config %q holds input %q", ref, value.Kubeadm.Name())
		}
		files, err = generationKubeadmFiles(value.Kubeadm)
		if err != nil {
			return nil, err
		}
	}
	return append(files, manifestFile), nil
}

func writeConfigurationFiles(root, id string, files []configurationFile) error {
	dir, err := generation.GenerationDir(root, id)
	if err != nil {
		return err
	}
	for _, file := range files {
		if err := persistedrecord.WriteFileAtomic(filepath.Join(dir, file.path), file.content, file.mode); err != nil {
			return fmt.Errorf("write generation configuration %s: %w", file.path, err)
		}
	}
	return nil
}

// A missing record in a declared ancestry is corruption, not an absent input.
// Only exhausting an intact chain permits legacy installation fallbacks.
func readGenerationInput[T any](root, id, kind string, read func(string) (T, error)) (T, string, error) {
	var zero T
	if _, err := generation.GenerationDir(root, id); err != nil {
		return zero, "", err
	}
	selected := strings.TrimSpace(id)
	visited := map[string]bool{}
	for depth := 0; selected != "" && depth < maxGenerationAncestry; depth++ {
		if visited[selected] {
			return zero, "", fmt.Errorf("generation %s ancestry contains a cycle at %q", kind, selected)
		}
		visited[selected] = true
		value, err := read(selected)
		if err == nil {
			return value, selected, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return zero, "", err
		}
		spec, _, err := generation.ReadGeneration(root, selected)
		if err != nil {
			return zero, "", fmt.Errorf("read generation %q while resolving its %s: %v", selected, kind, err)
		}
		selected = strings.TrimSpace(spec.PreviousGenerationID)
	}
	if selected != "" {
		return zero, "", fmt.Errorf("generation %s ancestry exceeds %d generations", kind, maxGenerationAncestry)
	}
	return zero, "", fmt.Errorf("generation %q has no %s: %w", id, kind, os.ErrNotExist)
}

func readInstalledManifest(root string) (manifest.Manifest, error) {
	file, err := os.Open(filepath.Join(root, "var/lib/katl/install/manifest.json"))
	if err != nil {
		return manifest.Manifest{}, fmt.Errorf("open installed manifest: %w", err)
	}
	defer file.Close()
	value, err := manifest.Decode(file)
	if err != nil {
		return manifest.Manifest{}, fmt.Errorf("decode installed manifest: %w", err)
	}
	return value, nil
}
