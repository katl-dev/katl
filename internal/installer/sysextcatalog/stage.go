package sysextcatalog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
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
	ArtifactPath          string
	ChecksumPath          string
	MetadataPath          string
	CatalogPath           string
	BundlePath            string
	BundleManifestDigest  string
	IndexPath             string
	BundleCatalogPath     string
	CatalogFragmentPath   string
	PackageProvenancePath string
	Entry                 Entry
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

	bundle, err := stageKubernetesPayloadBundle(request.OutputDir, meta, entry, stagedRaw, stagedMetadata)
	if err != nil {
		return StagedArtifact{}, err
	}

	return StagedArtifact{
		ArtifactPath:          stagedRaw,
		ChecksumPath:          stagedChecksum,
		MetadataPath:          stagedMetadata,
		CatalogPath:           stagedCatalog,
		BundlePath:            bundle.BundlePath,
		BundleManifestDigest:  bundle.BundleManifestDigest,
		IndexPath:             bundle.IndexPath,
		BundleCatalogPath:     bundle.CatalogPath,
		CatalogFragmentPath:   bundle.CatalogFragmentPath,
		PackageProvenancePath: bundle.PackageProvenancePath,
		Entry:                 entry,
	}, nil
}

const (
	payloadAPIVersion = "payload.katl.dev/v1alpha1"

	kubernetesPayloadBundleKind         = "KubernetesPayloadBundle"
	kubernetesPayloadIndexKind          = "KubernetesPayloadIndex"
	kubernetesPayloadCatalogKind        = "KubernetesPayloadCatalog"
	kubernetesPayloadBundleArtifactKind = "katl.kubernetes-payload.v1"

	kubernetesSysextRawMediaType      = "application/vnd.katl.sysext.raw.v1"
	kubernetesSysextMetadataMediaType = "application/vnd.katl.kubernetes.sysext.metadata.v1+json"
	packageProvenanceMediaType        = "application/vnd.katl.package-provenance.v1+json"
	kubernetesCatalogEntryMediaType   = "application/vnd.katl.kubernetes.catalog.entry.v1+json"
)

type stagedBundle struct {
	BundlePath            string
	BundleManifestDigest  string
	IndexPath             string
	CatalogPath           string
	CatalogFragmentPath   string
	PackageProvenancePath string
}

type KubernetesPayloadBundle struct {
	APIVersion                        string              `json:"apiVersion"`
	Kind                              string              `json:"kind"`
	Name                              string              `json:"name"`
	ArtifactKind                      string              `json:"artifactKind"`
	ArtifactVersion                   string              `json:"artifactVersion"`
	PayloadVersion                    string              `json:"payloadVersion"`
	KubernetesMinor                   string              `json:"kubernetesMinor"`
	Architecture                      string              `json:"architecture"`
	Payloads                          []BundleDescriptor  `json:"payloads"`
	Metadata                          []BundleDescriptor  `json:"metadata"`
	SourceRepository                  artifact.SourceRepo `json:"sourceRepository"`
	PackageVersions                   map[string]string   `json:"packageVersions"`
	PackageLockDigest                 string              `json:"packageLockDigest,omitempty"`
	BuildInputDigest                  string              `json:"buildInputDigest,omitempty"`
	SupportedRuntimeInterfaces        []string            `json:"supportedRuntimeInterfaces"`
	SupportedKubeadmConfigAPIFamilies []string            `json:"supportedKubeadmConfigAPIFamilies"`
	SupportedSourceKubernetesMinors   []string            `json:"supportedSourceKubernetesMinors"`
	SkewPolicy                        string              `json:"skewPolicy"`
	CreatedAt                         string              `json:"createdAt"`
	Signatures                        []BundleSignature   `json:"signatures,omitempty"`
}

type BundleDescriptor struct {
	Role        string            `json:"role"`
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	SizeBytes   int64             `json:"sizeBytes"`
	FileName    string            `json:"fileName"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type BundleSignature struct {
	Type   string `json:"type"`
	Reason string `json:"reason"`
}

type KubernetesPayloadIndex struct {
	APIVersion string                        `json:"apiVersion"`
	Kind       string                        `json:"kind"`
	Entries    []KubernetesPayloadIndexEntry `json:"entries"`
}

type KubernetesPayloadIndexEntry struct {
	PayloadVersion             string   `json:"payloadVersion"`
	ArtifactVersion            string   `json:"artifactVersion"`
	KubernetesMinor            string   `json:"kubernetesMinor"`
	Architecture               string   `json:"architecture"`
	BundleManifestDigest       string   `json:"bundleManifestDigest"`
	BundleManifestPath         string   `json:"bundleManifestPath"`
	SysextPayloadDigest        string   `json:"sysextPayloadDigest"`
	SupportedRuntimeInterfaces []string `json:"supportedRuntimeInterfaces"`
	CatalogEntryPath           string   `json:"catalogEntryPath"`
	Deprecated                 bool     `json:"deprecated"`
}

type KubernetesPayloadCatalog struct {
	APIVersion      string                          `json:"apiVersion"`
	Kind            string                          `json:"kind"`
	KubernetesMinor string                          `json:"kubernetesMinor"`
	Entries         []KubernetesPayloadCatalogEntry `json:"entries"`
}

type KubernetesPayloadCatalogEntry struct {
	Name                       string              `json:"name"`
	PayloadVersion             string              `json:"payloadVersion"`
	ArtifactVersion            string              `json:"artifactVersion"`
	KubernetesMinor            string              `json:"kubernetesMinor"`
	Architecture               string              `json:"architecture"`
	BundleManifestDigest       string              `json:"bundleManifestDigest,omitempty"`
	BundleManifestPath         string              `json:"bundleManifestPath"`
	SysextPayloadDigest        string              `json:"sysextPayloadDigest"`
	SysextPayloadSizeBytes     int64               `json:"sysextPayloadSizeBytes"`
	SupportedRuntimeInterfaces []string            `json:"supportedRuntimeInterfaces"`
	SourceRepository           artifact.SourceRepo `json:"sourceRepository"`
	PackageVersions            map[string]string   `json:"packageVersions"`
	Deprecated                 bool                `json:"deprecated"`
}

type packageProvenance struct {
	APIVersion       string              `json:"apiVersion"`
	Kind             string              `json:"kind"`
	PayloadVersion   string              `json:"payloadVersion"`
	ArtifactVersion  string              `json:"artifactVersion"`
	SourceRepository artifact.SourceRepo `json:"sourceRepository"`
	PackageVersions  map[string]string   `json:"packageVersions"`
	CreatedAt        string              `json:"createdAt"`
}

func stageKubernetesPayloadBundle(outputDir string, meta artifact.LocalMeta, entry Entry, stagedRaw string, stagedMetadata string) (stagedBundle, error) {
	bundleDir := filepath.Join(outputDir, "bundles", meta.PayloadVersion, meta.Architecture)
	blobDir := filepath.Join(outputDir, "blobs", "sha256")
	catalogDir := filepath.Join(outputDir, "catalog")
	for _, dir := range []string{bundleDir, blobDir, catalogDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return stagedBundle{}, fmt.Errorf("create Kubernetes payload bundle directory: %w", err)
		}
	}

	sysextDigest := "sha256:" + strings.ToLower(meta.SHA256)
	sysextBlob := filepath.Join(blobDir, strings.TrimPrefix(sysextDigest, "sha256:"))
	if digest, size, err := copyRegularFile(sysextBlob, stagedRaw, 0o644); err != nil {
		return stagedBundle{}, err
	} else if digest != strings.TrimPrefix(sysextDigest, "sha256:") || size != meta.SizeBytes {
		return stagedBundle{}, fmt.Errorf("%w: staged payload blob mismatch", ErrInvalidCatalog)
	}

	metadataBytes, err := os.ReadFile(stagedMetadata)
	if err != nil {
		return stagedBundle{}, fmt.Errorf("read staged metadata: %w", err)
	}
	metadataDigest, err := writeBlob(blobDir, metadataBytes)
	if err != nil {
		return stagedBundle{}, err
	}

	provenance := packageProvenance{
		APIVersion:       payloadAPIVersion,
		Kind:             "KubernetesPackageProvenance",
		PayloadVersion:   meta.PayloadVersion,
		ArtifactVersion:  meta.Version,
		SourceRepository: entry.SourceRepo,
		PackageVersions:  copyPackageVersions(meta.PackageVersions),
		CreatedAt:        meta.Created,
	}
	provenanceBytes, err := marshalCanonical(provenance)
	if err != nil {
		return stagedBundle{}, err
	}
	provenanceDigest, err := writeBlob(blobDir, provenanceBytes)
	if err != nil {
		return stagedBundle{}, err
	}
	buildInputDigest := provenanceDigest
	provenancePath := filepath.Join(bundleDir, "package-provenance.json")
	if err := os.WriteFile(provenancePath, provenanceBytes, 0o644); err != nil {
		return stagedBundle{}, fmt.Errorf("write package provenance: %w", err)
	}

	catalogEntryPath := filepath.ToSlash(filepath.Join("bundles", meta.PayloadVersion, meta.Architecture, "catalog-entry.json"))
	bundlePath := filepath.ToSlash(filepath.Join("bundles", meta.PayloadVersion, meta.Architecture, "bundle.json"))
	metadataPath := filepath.Join(bundleDir, "metadata.json")
	if err := os.WriteFile(metadataPath, metadataBytes, 0o644); err != nil {
		return stagedBundle{}, fmt.Errorf("write bundle metadata copy: %w", err)
	}

	catalogFragment := KubernetesPayloadCatalogEntry{
		Name:                       KubernetesName,
		PayloadVersion:             meta.PayloadVersion,
		ArtifactVersion:            meta.Version,
		KubernetesMinor:            entry.KubernetesMinor,
		Architecture:               meta.Architecture,
		BundleManifestPath:         bundlePath,
		SysextPayloadDigest:        sysextDigest,
		SysextPayloadSizeBytes:     meta.SizeBytes,
		SupportedRuntimeInterfaces: append([]string(nil), entry.RuntimeInterfaces...),
		SourceRepository:           entry.SourceRepo,
		PackageVersions:            copyPackageVersions(meta.PackageVersions),
	}

	catalogEntryBytes, err := marshalCanonical(catalogFragment)
	if err != nil {
		return stagedBundle{}, err
	}
	catalogEntryDigest, err := writeBlob(blobDir, catalogEntryBytes)
	if err != nil {
		return stagedBundle{}, err
	}

	bundle := KubernetesPayloadBundle{
		APIVersion:      payloadAPIVersion,
		Kind:            kubernetesPayloadBundleKind,
		Name:            "katl-kubernetes",
		ArtifactKind:    kubernetesPayloadBundleArtifactKind,
		ArtifactVersion: meta.Version,
		PayloadVersion:  meta.PayloadVersion,
		KubernetesMinor: entry.KubernetesMinor,
		Architecture:    meta.Architecture,
		Payloads: []BundleDescriptor{{
			Role:      "systemd-sysext",
			MediaType: kubernetesSysextRawMediaType,
			Digest:    sysextDigest,
			SizeBytes: meta.SizeBytes,
			FileName:  publishBaseName(meta),
		}},
		Metadata: []BundleDescriptor{
			{
				Role:      "sysext-metadata",
				MediaType: kubernetesSysextMetadataMediaType,
				Digest:    metadataDigest,
				SizeBytes: int64(len(metadataBytes)),
				FileName:  "metadata.json",
			},
			{
				Role:      "package-provenance",
				MediaType: packageProvenanceMediaType,
				Digest:    provenanceDigest,
				SizeBytes: int64(len(provenanceBytes)),
				FileName:  "package-provenance.json",
			},
			{
				Role:      "catalog-fragment",
				MediaType: kubernetesCatalogEntryMediaType,
				Digest:    catalogEntryDigest,
				SizeBytes: int64(len(catalogEntryBytes)),
				FileName:  "catalog-entry.json",
			},
		},
		SourceRepository:                  entry.SourceRepo,
		PackageVersions:                   copyPackageVersions(meta.PackageVersions),
		BuildInputDigest:                  buildInputDigest,
		SupportedRuntimeInterfaces:        append([]string(nil), entry.RuntimeInterfaces...),
		SupportedKubeadmConfigAPIFamilies: []string{"kubeadm.k8s.io/v1beta4"},
		SupportedSourceKubernetesMinors:   []string{entry.KubernetesMinor},
		SkewPolicy:                        "v0.1 exact patch bootstrap; kubeadm-aware upgrades only",
		CreatedAt:                         meta.Created,
		Signatures: []BundleSignature{{
			Type:   "unsigned-fixture",
			Reason: "local or CI fixture; signature policy is deferred",
		}},
	}
	bundleBytes, err := marshalCanonical(bundle)
	if err != nil {
		return stagedBundle{}, err
	}
	bundleDigest, err := writeBlob(blobDir, bundleBytes)
	if err != nil {
		return stagedBundle{}, err
	}
	catalogEntry := catalogFragment
	catalogEntry.BundleManifestDigest = bundleDigest

	if err := os.WriteFile(filepath.Join(bundleDir, "bundle.json"), bundleBytes, 0o644); err != nil {
		return stagedBundle{}, fmt.Errorf("write bundle manifest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "catalog-entry.json"), catalogEntryBytes, 0o644); err != nil {
		return stagedBundle{}, fmt.Errorf("write catalog entry: %w", err)
	}

	indexPath := filepath.Join(outputDir, "index.json")
	indexEntry := KubernetesPayloadIndexEntry{
		PayloadVersion:             meta.PayloadVersion,
		ArtifactVersion:            meta.Version,
		KubernetesMinor:            entry.KubernetesMinor,
		Architecture:               meta.Architecture,
		BundleManifestDigest:       bundleDigest,
		BundleManifestPath:         bundlePath,
		SysextPayloadDigest:        sysextDigest,
		SupportedRuntimeInterfaces: append([]string(nil), entry.RuntimeInterfaces...),
		CatalogEntryPath:           catalogEntryPath,
	}
	if err := upsertPayloadIndex(indexPath, indexEntry); err != nil {
		return stagedBundle{}, err
	}

	catalogPath := filepath.Join(catalogDir, entry.KubernetesMinor+".json")
	if err := upsertPayloadCatalog(catalogPath, entry.KubernetesMinor, catalogEntry); err != nil {
		return stagedBundle{}, err
	}
	if err := writeChecksums(outputDir); err != nil {
		return stagedBundle{}, err
	}

	return stagedBundle{
		BundlePath:            filepath.Join(outputDir, filepath.FromSlash(bundlePath)),
		BundleManifestDigest:  bundleDigest,
		IndexPath:             indexPath,
		CatalogPath:           catalogPath,
		CatalogFragmentPath:   filepath.Join(outputDir, filepath.FromSlash(catalogEntryPath)),
		PackageProvenancePath: provenancePath,
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

func publishBaseName(meta artifact.LocalMeta) string {
	name, err := publishName(meta)
	if err != nil {
		return "katl-kubernetes-" + meta.PayloadVersion + "-" + meta.Architecture + ".sysext.raw"
	}
	return name
}

func marshalCanonical(value any) ([]byte, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func writeBlob(blobDir string, data []byte) (string, error) {
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	if err := os.WriteFile(filepath.Join(blobDir, digest), data, 0o644); err != nil {
		return "", fmt.Errorf("write digest-addressed blob: %w", err)
	}
	return "sha256:" + digest, nil
}

func upsertPayloadIndex(path string, entry KubernetesPayloadIndexEntry) error {
	index := KubernetesPayloadIndex{
		APIVersion: payloadAPIVersion,
		Kind:       kubernetesPayloadIndexKind,
	}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &index); err != nil {
			return fmt.Errorf("decode Kubernetes payload index: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read Kubernetes payload index: %w", err)
	}
	if index.APIVersion != payloadAPIVersion || index.Kind != kubernetesPayloadIndexKind {
		return fmt.Errorf("%w: invalid Kubernetes payload index header", ErrInvalidCatalog)
	}

	key := payloadBundleKey(entry.PayloadVersion, entry.Architecture)
	replaced := false
	for i := range index.Entries {
		if payloadBundleKey(index.Entries[i].PayloadVersion, index.Entries[i].Architecture) == key {
			index.Entries[i] = entry
			replaced = true
			break
		}
	}
	if !replaced {
		index.Entries = append(index.Entries, entry)
	}
	sort.Slice(index.Entries, func(i, j int) bool {
		if compare := comparePayloadVersion(index.Entries[i].PayloadVersion, index.Entries[j].PayloadVersion); compare != 0 {
			return compare < 0
		}
		return index.Entries[i].Architecture < index.Entries[j].Architecture
	})

	data, err := marshalCanonical(index)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write Kubernetes payload index: %w", err)
	}
	return nil
}

func upsertPayloadCatalog(path string, minor string, entry KubernetesPayloadCatalogEntry) error {
	catalog := KubernetesPayloadCatalog{
		APIVersion:      payloadAPIVersion,
		Kind:            kubernetesPayloadCatalogKind,
		KubernetesMinor: minor,
	}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &catalog); err != nil {
			return fmt.Errorf("decode Kubernetes payload catalog: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read Kubernetes payload catalog: %w", err)
	}
	if catalog.APIVersion != payloadAPIVersion || catalog.Kind != kubernetesPayloadCatalogKind || catalog.KubernetesMinor != minor {
		return fmt.Errorf("%w: invalid Kubernetes payload catalog header", ErrInvalidCatalog)
	}

	key := payloadBundleKey(entry.PayloadVersion, entry.Architecture)
	replaced := false
	for i := range catalog.Entries {
		if payloadBundleKey(catalog.Entries[i].PayloadVersion, catalog.Entries[i].Architecture) == key {
			catalog.Entries[i] = entry
			replaced = true
			break
		}
	}
	if !replaced {
		catalog.Entries = append(catalog.Entries, entry)
	}
	sort.Slice(catalog.Entries, func(i, j int) bool {
		if compare := comparePayloadVersion(catalog.Entries[i].PayloadVersion, catalog.Entries[j].PayloadVersion); compare != 0 {
			return compare < 0
		}
		return catalog.Entries[i].Architecture < catalog.Entries[j].Architecture
	})

	data, err := marshalCanonical(catalog)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write Kubernetes payload catalog: %w", err)
	}
	return nil
}

func payloadBundleKey(version string, arch string) string {
	return version + "/" + arch
}

func writeChecksums(outputDir string) error {
	var lines []string
	if err := appendChecksumLine(outputDir, "index.json", &lines); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("checksum Kubernetes payload index: %w", err)
	}
	for _, root := range []string{
		filepath.Join(outputDir, "bundles"),
		filepath.Join(outputDir, "blobs", "sha256"),
		filepath.Join(outputDir, "catalog"),
	} {
		if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(outputDir, path)
			if err != nil {
				return err
			}
			if filepath.ToSlash(rel) == "checksums.txt" {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			lines = append(lines, fmt.Sprintf("%s  %s", hex.EncodeToString(sum[:]), filepath.ToSlash(rel)))
			return nil
		}); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("walk bundle fixture files: %w", err)
		}
	}
	sort.Strings(lines)
	content := strings.Join(lines, "\n")
	if content != "" {
		content += "\n"
	}
	if err := os.WriteFile(filepath.Join(outputDir, "checksums.txt"), []byte(content), 0o644); err != nil {
		return fmt.Errorf("write bundle checksums: %w", err)
	}
	return nil
}

func appendChecksumLine(outputDir string, rel string, lines *[]string) error {
	path := filepath.Join(outputDir, filepath.FromSlash(rel))
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	*lines = append(*lines, fmt.Sprintf("%s  %s", hex.EncodeToString(sum[:]), rel))
	return nil
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
