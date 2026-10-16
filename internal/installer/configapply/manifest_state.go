package configapply

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/manifest"
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
	file, err := generationManifestFile(value)
	if err != nil {
		return err
	}
	return writeConfigurationFiles(root, generationID, []configurationFile{file})
}

func generationManifestFile(value manifest.Manifest) (configurationFile, error) {
	if err := manifest.Validate(value); err != nil {
		return configurationFile{}, fmt.Errorf("validate generation manifest: %w", err)
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return configurationFile{}, fmt.Errorf("encode generation manifest: %w", err)
	}
	return configurationFile{path: generationManifestName, content: append(data, '\n'), mode: 0o644}, nil
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
	value, source, err := readGenerationInput(root, generationID, "manifest", func(id string) (manifest.Manifest, error) {
		return ReadGenerationManifest(root, id)
	})
	if !errors.Is(err, os.ErrNotExist) {
		return value, source, err
	}
	value, err = readInstalledManifest(root)
	return value, "", err
}
