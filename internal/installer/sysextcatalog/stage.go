package sysextcatalog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/zariel/katl/internal/installer/artifact"
)

type StageRequest struct {
	MetadataPath string
	ArtifactPath string
	OutputDir    string
	BaseURL      string
}

type StagedArtifact struct {
	ArtifactPath string
	ChecksumPath string
	MetadataPath string
	CatalogPath  string
	Entry        Entry
}

func StageKubernetesSysext(request StageRequest) (StagedArtifact, error) {
	if strings.TrimSpace(request.MetadataPath) == "" {
		return StagedArtifact{}, fmt.Errorf("metadata path is required")
	}
	if strings.TrimSpace(request.OutputDir) == "" {
		return StagedArtifact{}, fmt.Errorf("output directory is required")
	}

	meta, err := artifact.ReadLocal(request.MetadataPath)
	if err != nil {
		return StagedArtifact{}, fmt.Errorf("read Kubernetes sysext metadata: %w", err)
	}
	if meta.Kind != artifact.ArtifactSysext {
		return StagedArtifact{}, fmt.Errorf("%w: metadata kind %q is not %q", ErrInvalidCatalog, meta.Kind, artifact.ArtifactSysext)
	}
	if meta.Name != "kubernetes" {
		return StagedArtifact{}, fmt.Errorf("%w: metadata name %q is not \"kubernetes\"", ErrInvalidCatalog, meta.Name)
	}

	artifactPath := strings.TrimSpace(request.ArtifactPath)
	if artifactPath == "" {
		artifactPath = filepath.Join(filepath.Dir(request.MetadataPath), meta.Path)
	}

	name, err := publishName(meta)
	if err != nil {
		return StagedArtifact{}, err
	}
	if err := os.MkdirAll(request.OutputDir, 0o755); err != nil {
		return StagedArtifact{}, fmt.Errorf("create publish directory: %w", err)
	}

	stagedRaw := filepath.Join(request.OutputDir, name)
	stagedChecksum := stagedRaw + ".sha256"
	stagedMetadata := stagedRaw + ".json"
	stagedCatalog := filepath.Join(request.OutputDir, "kubernetes-sysext-catalog.json")

	digest, size, err := copyRegularFile(stagedRaw, artifactPath, 0o644)
	if err != nil {
		return StagedArtifact{}, err
	}
	if digest != strings.ToLower(meta.SHA256) {
		return StagedArtifact{}, fmt.Errorf("%w: staged artifact digest got %s want %s", ErrInvalidCatalog, digest, meta.SHA256)
	}
	if size != meta.SizeBytes {
		return StagedArtifact{}, fmt.Errorf("%w: staged artifact size got %d want %d", ErrInvalidCatalog, size, meta.SizeBytes)
	}
	if err := os.WriteFile(stagedChecksum, []byte(fmt.Sprintf("%s  %s\n", meta.SHA256, name)), 0o644); err != nil {
		return StagedArtifact{}, fmt.Errorf("write staged checksum: %w", err)
	}

	meta = publishMetadata(meta, name)
	metadata, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return StagedArtifact{}, fmt.Errorf("marshal staged metadata: %w", err)
	}
	if err := os.WriteFile(stagedMetadata, append(metadata, '\n'), 0o644); err != nil {
		return StagedArtifact{}, fmt.Errorf("write staged metadata: %w", err)
	}

	entry, err := EntryFromLocalMeta(meta)
	if err != nil {
		return StagedArtifact{}, err
	}
	entry.LocalPath = name
	if strings.TrimSpace(request.BaseURL) != "" {
		entry.URL = strings.TrimRight(request.BaseURL, "/") + "/" + name
		entry.LocalPath = ""
	}

	catalog := Catalog{
		APIVersion: APIVersion,
		Kind:       Kind,
		Entries:    []Entry{entry},
	}
	catalogData, err := Marshal(catalog)
	if err != nil {
		return StagedArtifact{}, err
	}
	if err := os.WriteFile(stagedCatalog, catalogData, 0o644); err != nil {
		return StagedArtifact{}, fmt.Errorf("write staged catalog: %w", err)
	}

	return StagedArtifact{
		ArtifactPath: stagedRaw,
		ChecksumPath: stagedChecksum,
		MetadataPath: stagedMetadata,
		CatalogPath:  stagedCatalog,
		Entry:        entry,
	}, nil
}

func publishName(meta artifact.LocalMeta) (string, error) {
	if KubernetesMinor(meta.PayloadVersion) == "" {
		return "", fmt.Errorf("%w: payload version %q must be vMAJOR.MINOR.PATCH", ErrInvalidCatalog, meta.PayloadVersion)
	}
	if strings.TrimSpace(meta.Architecture) == "" {
		return "", fmt.Errorf("%w: architecture is required", ErrInvalidCatalog)
	}
	return fmt.Sprintf("katl-kubernetes-%s-%s.sysext.raw", meta.PayloadVersion, meta.Architecture), nil
}

func publishMetadata(meta artifact.LocalMeta, name string) artifact.LocalMeta {
	meta.Path = name
	if meta.CompatibleRuntime != nil && meta.CompatibleRuntime.ArtifactPath != "" {
		meta.CompatibleRuntime.ArtifactPath = filepath.Base(meta.CompatibleRuntime.ArtifactPath)
	}
	return meta
}

func copyRegularFile(dst, src string, mode os.FileMode) (string, int64, error) {
	source, err := os.Open(src)
	if err != nil {
		return "", 0, fmt.Errorf("open source artifact: %w", err)
	}
	defer source.Close()

	info, err := source.Stat()
	if err != nil {
		return "", 0, fmt.Errorf("stat source artifact: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", 0, fmt.Errorf("source artifact is not a regular file: %s", src)
	}

	target, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return "", 0, fmt.Errorf("create staged artifact: %w", err)
	}
	hash := sha256.New()
	size, err := io.Copy(target, io.TeeReader(source, hash))
	if err != nil {
		_ = target.Close()
		return "", 0, fmt.Errorf("copy staged artifact: %w", err)
	}
	if err := target.Close(); err != nil {
		return "", 0, fmt.Errorf("close staged artifact: %w", err)
	}
	return hex.EncodeToString(hash.Sum(nil)), size, nil
}
