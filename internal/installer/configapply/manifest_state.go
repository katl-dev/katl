package configapply

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/katl-dev/katl/internal/installer/generation"
	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/installer/persistedrecord"
)

const generationManifestName = "manifest.json"

func GenerationManifestPath(root, generationID string) (string, error) {
	metadataPath, err := generation.MetadataPath(root, generationID)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(metadataPath), generationManifestName), nil
}

func WriteGenerationManifest(root, generationID string, value manifest.Manifest) error {
	if err := manifest.Validate(value); err != nil {
		return fmt.Errorf("validate generation manifest: %w", err)
	}
	path, err := GenerationManifestPath(root, generationID)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode generation manifest: %w", err)
	}
	if err := persistedrecord.WriteFileAtomic(path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write generation manifest: %w", err)
	}
	return nil
}

func ReadGenerationManifest(root, generationID string) (manifest.Manifest, error) {
	path, err := GenerationManifestPath(root, generationID)
	if err != nil {
		return manifest.Manifest{}, err
	}
	file, err := os.Open(path)
	if err != nil {
		return manifest.Manifest{}, fmt.Errorf("open generation manifest: %w", err)
	}
	defer file.Close()
	value, err := manifest.Decode(file)
	if err != nil {
		return manifest.Manifest{}, fmt.Errorf("decode generation manifest: %w", err)
	}
	return value, nil
}

func ReadEffectiveGenerationManifest(root, generationID string) (manifest.Manifest, string, error) {
	selected := generationID
	visited := map[string]bool{}
	for selected != "" {
		if visited[selected] {
			return manifest.Manifest{}, "", fmt.Errorf("generation manifest ancestry contains a cycle at %q", selected)
		}
		visited[selected] = true
		value, err := ReadGenerationManifest(root, selected)
		if err == nil {
			return value, selected, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return manifest.Manifest{}, "", err
		}
		spec, _, specErr := generation.ReadGeneration(root, selected)
		if specErr != nil {
			return manifest.Manifest{}, "", fmt.Errorf("read generation %q while resolving its configuration: %w", selected, specErr)
		}
		previous := spec.PreviousGenerationID
		if previous == "" {
			return manifest.Manifest{}, "", fmt.Errorf("generation %q and its ancestry have no manifest: %w", generationID, err)
		}
		selected = previous
	}
	return manifest.Manifest{}, "", fmt.Errorf("generation %q has no manifest ancestry", generationID)
}
