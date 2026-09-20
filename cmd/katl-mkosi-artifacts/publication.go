package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/katl-dev/katl/internal/flavour"
)

// Publication names distinguish kernels; internal paths stay local to each
// isolated build so embedded media paths do not depend on download names.
func publishFlavour(dir, value string) error {
	value, err := flavour.Normalize(value)
	if err != nil {
		return err
	}
	if value == flavour.Standard {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		published := publicationName(name, value)
		if published == name {
			continue
		}
		source, target := filepath.Join(dir, name), filepath.Join(dir, published)
		if _, err := os.Lstat(target); !os.IsNotExist(err) {
			return fmt.Errorf("publication target already exists: %s", target)
		}
		var data []byte
		if strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".sha256") {
			data, err = os.ReadFile(source)
			if err != nil {
				return err
			}
		}
		switch {
		case strings.HasSuffix(name, ".json") && name != "katl-release-build-inputs.json":
			var metadata map[string]any
			if err := json.Unmarshal(data, &metadata); err != nil {
				return err
			}
			actual, _ := metadata["flavour"].(string)
			if actual != value {
				return fmt.Errorf("%s has flavour %q, want %q", name, actual, value)
			}
			for _, key := range []string{"path", "checksumPath"} {
				if path, ok := metadata[key].(string); ok {
					metadata[key] = publicationName(filepath.Base(path), value)
				}
			}
			data, err = json.MarshalIndent(metadata, "", "  ")
			if err != nil {
				return err
			}
			data = append(data, '\n')
		case strings.HasSuffix(name, ".sha256"):
			data = []byte(strings.ReplaceAll(string(data), strings.TrimSuffix(name, ".sha256"), strings.TrimSuffix(published, ".sha256")))
		default:
			// Move large images without allocating their contents.
			if err := os.Rename(source, target); err != nil {
				return err
			}
			continue
		}
		if err := os.WriteFile(target, data, 0o644); err != nil {
			return err
		}
		if err := os.Remove(source); err != nil {
			return err
		}
	}
	return nil
}

func publicationName(name, value string) string {
	for _, prefix := range []string{"katlos-", "katl-installer.", "katl-runtime.", "katl-release-build-inputs."} {
		if strings.HasPrefix(name, prefix) {
			base := strings.TrimSuffix(strings.TrimSuffix(prefix, "-"), ".")
			return base + flavour.Suffix(value) + name[len(base):]
		}
	}
	return name
}

func verifyInstallerPair(installerPath, imagePath string) error {
	read := func(path string) (katlosArtifactMetadata, error) {
		data, err := os.ReadFile(path)
		if err != nil {
			return katlosArtifactMetadata{}, err
		}
		var metadata katlosArtifactMetadata
		if err := json.Unmarshal(data, &metadata); err != nil {
			return metadata, err
		}
		metadata.Flavour, err = flavour.Normalize(metadata.Flavour)
		return metadata, err
	}
	installer, err := read(installerPath)
	if err != nil {
		return fmt.Errorf("installer metadata: %w", err)
	}
	image, err := read(imagePath)
	if err != nil {
		return fmt.Errorf("install image metadata: %w", err)
	}
	if installer.Version == "" || installer.Architecture == "" || installer.Version != image.Version || installer.Architecture != image.Architecture || installer.Flavour != image.Flavour {
		return fmt.Errorf("installer and install image must have the same version, architecture and flavour; installer=%s/%s/%s image=%s/%s/%s", installer.Version, installer.Architecture, installer.Flavour, image.Version, image.Architecture, image.Flavour)
	}
	return nil
}
