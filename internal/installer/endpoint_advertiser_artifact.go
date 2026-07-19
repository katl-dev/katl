package installer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/katl-dev/katl/internal/installer/generation"
	"github.com/katl-dev/katl/internal/installer/katlosimage"
	"github.com/katl-dev/katl/internal/installer/persistedrecord"
)

const (
	EndpointAdvertiserArtifactPath     = "/var/lib/katl/artifacts/katlos-image/katl-endpoint-advertiser.raw"
	EndpointAdvertiserArtifactMetadata = "/var/lib/katl/artifacts/katlos-image/katl-endpoint-advertiser.json"
	endpointAdvertiserArtifactKind     = "EndpointAdvertiserArtifact"
)

type EndpointAdvertiserArtifact struct {
	APIVersion string                  `json:"apiVersion"`
	Kind       string                  `json:"kind"`
	SizeBytes  int64                   `json:"sizeBytes"`
	Extension  generation.ExtensionRef `json:"extension"`
}

func endpointAdvertiserArtifact(payload katlosimage.Payload) (EndpointAdvertiserArtifact, error) {
	ref, err := payload.EndpointAdvertiserExtensionRef(EndpointAdvertiserArtifactPath)
	if err != nil {
		return EndpointAdvertiserArtifact{}, err
	}
	return EndpointAdvertiserArtifact{
		APIVersion: generation.APIVersion,
		Kind:       endpointAdvertiserArtifactKind,
		SizeBytes:  payload.EndpointAdvertiser.SizeBytes,
		Extension:  ref,
	}, nil
}

func writeEndpointAdvertiserArtifact(root string, artifact EndpointAdvertiserArtifact) error {
	if err := validateEndpointAdvertiserArtifact(artifact); err != nil {
		return err
	}
	data, err := json.MarshalIndent(artifact, "", "  ")
	if err != nil {
		return fmt.Errorf("encode endpoint advertiser artifact metadata: %w", err)
	}
	path := rootedInstallerPath(root, EndpointAdvertiserArtifactMetadata)
	if err := persistedrecord.WriteFileAtomic(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write endpoint advertiser artifact metadata: %w", err)
	}
	return nil
}

func ReadEndpointAdvertiserArtifact(root string) (EndpointAdvertiserArtifact, error) {
	path := rootedInstallerPath(root, EndpointAdvertiserArtifactMetadata)
	data, err := os.ReadFile(path)
	if err != nil {
		return EndpointAdvertiserArtifact{}, fmt.Errorf("read endpoint advertiser artifact metadata: %w", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var artifact EndpointAdvertiserArtifact
	if err := decoder.Decode(&artifact); err != nil {
		return EndpointAdvertiserArtifact{}, fmt.Errorf("decode endpoint advertiser artifact metadata: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return EndpointAdvertiserArtifact{}, fmt.Errorf("decode endpoint advertiser artifact metadata: multiple JSON values")
	}
	if err := validateEndpointAdvertiserArtifact(artifact); err != nil {
		return EndpointAdvertiserArtifact{}, err
	}
	artifactPath := rootedInstallerPath(root, artifact.Extension.Path)
	info, err := os.Stat(artifactPath)
	if err != nil {
		return EndpointAdvertiserArtifact{}, fmt.Errorf("stat endpoint advertiser artifact: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() != artifact.SizeBytes {
		return EndpointAdvertiserArtifact{}, fmt.Errorf("endpoint advertiser artifact size or type does not match metadata")
	}
	file, err := os.Open(artifactPath)
	if err != nil {
		return EndpointAdvertiserArtifact{}, err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	if copyErr != nil {
		return EndpointAdvertiserArtifact{}, fmt.Errorf("hash endpoint advertiser artifact: %w", copyErr)
	}
	if closeErr != nil {
		return EndpointAdvertiserArtifact{}, closeErr
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != artifact.Extension.SHA256 {
		return EndpointAdvertiserArtifact{}, fmt.Errorf("endpoint advertiser artifact SHA-256 mismatch")
	}
	return artifact, nil
}

func validateEndpointAdvertiserArtifact(artifact EndpointAdvertiserArtifact) error {
	if artifact.APIVersion != generation.APIVersion || artifact.Kind != endpointAdvertiserArtifactKind {
		return fmt.Errorf("endpoint advertiser artifact metadata has unsupported identity")
	}
	ref := artifact.Extension
	if ref.Name != katlosimage.EndpointAdvertiserName || filepath.Clean(ref.Path) != EndpointAdvertiserArtifactPath || ref.ActivationPath != "/run/extensions/katl-endpoint-advertiser.raw" {
		return fmt.Errorf("endpoint advertiser artifact metadata has invalid extension selection")
	}
	if artifact.SizeBytes <= 0 || len(ref.SHA256) != sha256.Size*2 || strings.TrimSpace(ref.PayloadVersion) == "" || strings.TrimSpace(ref.Architecture) == "" || len(ref.Compatibility.RuntimeInterfaces) == 0 {
		return fmt.Errorf("endpoint advertiser artifact metadata is incomplete")
	}
	return nil
}

func rootedInstallerPath(root, absolute string) string {
	return filepath.Join(filepath.Clean(root), strings.TrimPrefix(absolute, "/"))
}
