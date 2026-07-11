package installmedia

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zariel/katl/internal/installer/manifest"
)

const (
	APIVersion = "katl.dev/v1alpha1"
	Kind       = "KatlOSImageArtifact"
)

type Metadata struct {
	APIVersion        string `json:"apiVersion"`
	Kind              string `json:"kind"`
	ImageRole         string `json:"imageRole"`
	Format            string `json:"format"`
	Version           string `json:"version"`
	BuildID           string `json:"buildID"`
	Architecture      string `json:"architecture"`
	RuntimeInterface  string `json:"runtimeInterface"`
	Path              string `json:"path"`
	SizeBytes         int64  `json:"sizeBytes"`
	SHA256            string `json:"sha256"`
	ChecksumPath      string `json:"checksumPath"`
	EmbeddedIndexPath string `json:"embeddedIndexPath"`
	CreatedAt         string `json:"createdAt"`
}

type Media struct {
	Root     string
	Metadata Metadata
	Image    manifest.KatlosImage
}

func Load(root string) (Media, bool, error) {
	root = filepath.Clean(root)
	path := filepath.Join(root, "media.json")
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return Media{}, false, nil
	}
	if err != nil {
		return Media{}, false, fmt.Errorf("open install media metadata: %w", err)
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var metadata Metadata
	if err := decoder.Decode(&metadata); err != nil {
		return Media{}, false, fmt.Errorf("decode install media metadata: %w", err)
	}
	if err := validateMetadata(metadata); err != nil {
		return Media{}, false, err
	}
	imagePath := filepath.Join(root, "images", metadata.Path)
	info, err := os.Stat(imagePath)
	if err != nil {
		return Media{}, false, fmt.Errorf("stat install media KatlOS image: %w", err)
	}
	if !info.Mode().IsRegular() {
		return Media{}, false, fmt.Errorf("install media KatlOS image is not a regular file")
	}
	if info.Size() != metadata.SizeBytes {
		return Media{}, false, fmt.Errorf("install media KatlOS image size %d does not match metadata %d", info.Size(), metadata.SizeBytes)
	}
	return Media{
		Root:     root,
		Metadata: metadata,
		Image: manifest.KatlosImage{
			LocalRef:         filepath.ToSlash(filepath.Join("images", metadata.Path)),
			SHA256:           metadata.SHA256,
			SizeBytes:        uint64(metadata.SizeBytes),
			Version:          metadata.Version,
			Architecture:     metadata.Architecture,
			RuntimeInterface: metadata.RuntimeInterface,
			Role:             metadata.ImageRole,
		},
	}, true, nil
}

func validateMetadata(metadata Metadata) error {
	if metadata.APIVersion != APIVersion {
		return fmt.Errorf("install media apiVersion must be %s", APIVersion)
	}
	if metadata.Kind != Kind {
		return fmt.Errorf("install media kind must be %s", Kind)
	}
	if metadata.ImageRole != "install" {
		return fmt.Errorf("install media imageRole must be install")
	}
	if metadata.Format != "squashfs" {
		return fmt.Errorf("install media format must be squashfs")
	}
	if strings.TrimSpace(metadata.Version) == "" {
		return fmt.Errorf("install media version is required")
	}
	if strings.TrimSpace(metadata.Architecture) == "" {
		return fmt.Errorf("install media architecture is required")
	}
	if strings.TrimSpace(metadata.RuntimeInterface) == "" {
		return fmt.Errorf("install media runtimeInterface is required")
	}
	if metadata.Path != filepath.Base(metadata.Path) || metadata.Path == "." || strings.TrimSpace(metadata.Path) == "" {
		return fmt.Errorf("install media path must be a file name")
	}
	if metadata.SizeBytes <= 0 {
		return fmt.Errorf("install media sizeBytes must be positive")
	}
	if len(metadata.SHA256) != sha256.Size*2 || metadata.SHA256 != strings.ToLower(metadata.SHA256) {
		return fmt.Errorf("install media sha256 must be %d lowercase hex characters", sha256.Size*2)
	}
	if _, err := hex.DecodeString(metadata.SHA256); err != nil {
		return fmt.Errorf("install media sha256 is invalid: %w", err)
	}
	return nil
}
