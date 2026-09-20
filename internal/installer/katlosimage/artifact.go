package katlosimage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/katl-dev/katl/internal/flavour"
)

const ArtifactMetadataKind = "KatlOSImageArtifact"

// ArtifactMetadata describes a complete KatlOS image file produced by the
// supported image build pipeline. It is the workstation-side contract used
// before an image is offered to an installer or running node.
type ArtifactMetadata struct {
	Flavour           string `json:"flavour,omitempty"`
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

func ReadArtifactMetadata(path string, expectedRole string) (ArtifactMetadata, error) {
	file, err := os.Open(path)
	if err != nil {
		return ArtifactMetadata{}, fmt.Errorf("open KatlOS image metadata: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var metadata ArtifactMetadata
	if err := decoder.Decode(&metadata); err != nil {
		return ArtifactMetadata{}, fmt.Errorf("decode KatlOS image metadata: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return ArtifactMetadata{}, fmt.Errorf("decode KatlOS image metadata: multiple JSON values")
	}
	if err := metadata.Validate(expectedRole); err != nil {
		return ArtifactMetadata{}, err
	}
	return metadata, nil
}

func (m ArtifactMetadata) Validate(expectedRole string) error {
	if _, err := flavour.Normalize(m.Flavour); err != nil {
		return err
	}
	if m.APIVersion != APIVersion {
		return fmt.Errorf("KatlOS image metadata apiVersion must be %s", APIVersion)
	}
	if m.Kind != ArtifactMetadataKind {
		return fmt.Errorf("KatlOS image metadata kind must be %s", ArtifactMetadataKind)
	}
	if expectedRole != RoleInstall && expectedRole != RoleUpgrade {
		return fmt.Errorf("expected KatlOS image role %q is unsupported", expectedRole)
	}
	if m.ImageRole != expectedRole {
		return fmt.Errorf("KatlOS image metadata role must be %s", expectedRole)
	}
	if m.Format != FormatSquashFS {
		return fmt.Errorf("KatlOS image metadata format must be %s", FormatSquashFS)
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "version", value: m.Version},
		{name: "buildID", value: m.BuildID},
		{name: "architecture", value: m.Architecture},
		{name: "runtimeInterface", value: m.RuntimeInterface},
		{name: "path", value: m.Path},
		{name: "checksumPath", value: m.ChecksumPath},
		{name: "embeddedIndexPath", value: m.EmbeddedIndexPath},
		{name: "createdAt", value: m.CreatedAt},
	} {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("KatlOS image metadata %s is required", field.name)
		}
	}
	if err := validateRelativePath(m.Path); err != nil {
		return fmt.Errorf("KatlOS image metadata path: %w", err)
	}
	if filepath.Base(filepath.FromSlash(m.Path)) != filepath.FromSlash(m.Path) {
		return fmt.Errorf("KatlOS image metadata path %q must name a companion file", m.Path)
	}
	if err := validateRelativePath(m.ChecksumPath); err != nil {
		return fmt.Errorf("KatlOS image metadata checksumPath: %w", err)
	}
	if err := validateRelativePath(m.EmbeddedIndexPath); err != nil {
		return fmt.Errorf("KatlOS image metadata embeddedIndexPath: %w", err)
	}
	if m.SizeBytes <= 0 {
		return fmt.Errorf("KatlOS image metadata size must be positive")
	}
	if err := validateSHA256(m.SHA256); err != nil {
		return fmt.Errorf("KatlOS image metadata SHA-256 is invalid: %w", err)
	}
	return nil
}

func (m ArtifactMetadata) VerifyFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("stat KatlOS image: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("KatlOS image is not a regular file")
	}
	if filepath.Base(path) != filepath.FromSlash(m.Path) {
		return fmt.Errorf("KatlOS image filename %q does not match metadata %q", filepath.Base(path), m.Path)
	}
	if info.Size() != m.SizeBytes {
		return fmt.Errorf("KatlOS image size %d does not match metadata %d", info.Size(), m.SizeBytes)
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open KatlOS image: %w", err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return fmt.Errorf("hash KatlOS image: %w", err)
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != m.SHA256 {
		return fmt.Errorf("KatlOS image SHA-256 %s does not match metadata %s", got, m.SHA256)
	}
	return nil
}
