package kubernetesbundle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/installer/artifact"
	"github.com/katl-dev/katl/internal/installer/generation"
	"github.com/katl-dev/katl/internal/installer/sysextcatalog"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
)

const (
	APIVersion = "payload.katl.dev/v1alpha1"

	BundleKind   = "KubernetesPayloadBundle"
	IndexKind    = "KubernetesPayloadIndex"
	BundleName   = "katl-kubernetes"
	ArtifactKind = "katl.kubernetes-payload.v1"

	sysextRole          = "systemd-sysext"
	metadataRole        = "sysext-metadata"
	provenanceRole      = "package-provenance"
	catalogRole         = "catalog-fragment"
	sysextMediaType     = "application/vnd.katl.sysext.raw.v1"
	metadataMediaType   = "application/vnd.katl.kubernetes.sysext.metadata.v1+json"
	provenanceMediaType = "application/vnd.katl.package-provenance.v1+json"
	catalogMediaType    = "application/vnd.katl.kubernetes.catalog.entry.v1+json"
	bundleArtifactType  = "application/vnd.katl.kubernetes.payload.bundle.v1"
	bundleMediaType     = "application/vnd.katl.kubernetes.payload.bundle.v1+json"
	registryTagPrefix   = "sha256-"
)

type Request struct {
	Source           string
	Ref              string
	CacheDir         string
	RuntimeInterface string
	Architecture     string
	Client           *http.Client
	ActivationPath   string
}

type Staged struct {
	PayloadVersion       string
	ArtifactVersion      string
	Architecture         string
	BundleManifestDigest string
	SysextPayloadDigest  string
	BundleDir            string
	SysextDir            string
	SysextPath           string
	MetadataPath         string
	ExtensionRef         generation.ExtensionRef
}

type Index struct {
	APIVersion string       `json:"apiVersion"`
	Kind       string       `json:"kind"`
	Entries    []IndexEntry `json:"entries"`
}

type IndexEntry struct {
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

type Bundle struct {
	APIVersion                        string              `json:"apiVersion"`
	Kind                              string              `json:"kind"`
	Name                              string              `json:"name"`
	ArtifactKind                      string              `json:"artifactKind"`
	ArtifactVersion                   string              `json:"artifactVersion"`
	PayloadVersion                    string              `json:"payloadVersion"`
	KubernetesMinor                   string              `json:"kubernetesMinor"`
	Architecture                      string              `json:"architecture"`
	Payloads                          []Descriptor        `json:"payloads"`
	Metadata                          []Descriptor        `json:"metadata"`
	SourceRepository                  artifact.SourceRepo `json:"sourceRepository"`
	PackageVersions                   map[string]string   `json:"packageVersions"`
	PackageLockDigest                 string              `json:"packageLockDigest,omitempty"`
	BuildInputDigest                  string              `json:"buildInputDigest,omitempty"`
	SupportedRuntimeInterfaces        []string            `json:"supportedRuntimeInterfaces"`
	SupportedKubeadmConfigAPIFamilies []string            `json:"supportedKubeadmConfigAPIFamilies"`
	SupportedSourceKubernetesMinors   []string            `json:"supportedSourceKubernetesMinors"`
	SkewPolicy                        string              `json:"skewPolicy"`
	CreatedAt                         string              `json:"createdAt"`
	Signatures                        []Signature         `json:"signatures,omitempty"`
}

type Descriptor struct {
	Role      string `json:"role"`
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	SizeBytes int64  `json:"sizeBytes"`
	FileName  string `json:"fileName"`
}

type Signature struct {
	Type   string `json:"type"`
	Reason string `json:"reason"`
}

var ErrInvalidBundle = errors.New("invalid Kubernetes payload bundle")

var artifactVersionPattern = regexp.MustCompile(`^v([0-9]+\.[0-9]+\.[0-9]+)-katl\.[0-9]+$`)

type ImageReference struct {
	Value           string
	Repository      string
	Tag             string
	ManifestDigest  string
	PayloadVersion  string
	ArtifactVersion string
	Source          string
}

func ParseImageReference(value string) (ImageReference, error) {
	value = strings.TrimSpace(value)
	if value == "" || strings.Contains(value, "://") {
		return ImageReference{}, fmt.Errorf("%w: image reference must be REGISTRY/REPOSITORY:TAG with an optional @sha256 digest", ErrInvalidBundle)
	}
	nameAndTag, manifestDigest, hasDigest := strings.Cut(value, "@")
	if hasDigest {
		if strings.Contains(manifestDigest, "@") || validateDigest(manifestDigest) != nil {
			return ImageReference{}, fmt.Errorf("%w: image reference manifest digest is invalid", ErrInvalidBundle)
		}
	}
	lastSlash := strings.LastIndex(nameAndTag, "/")
	lastColon := strings.LastIndex(nameAndTag, ":")
	if lastSlash <= 0 || lastColon <= lastSlash+1 || lastColon == len(nameAndTag)-1 {
		return ImageReference{}, fmt.Errorf("%w: image reference must include a registry, repository, and tag", ErrInvalidBundle)
	}
	repository := nameAndTag[:lastColon]
	tag := nameAndTag[lastColon+1:]
	parts := strings.SplitN(repository, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.ContainsAny(repository, "?#") {
		return ImageReference{}, fmt.Errorf("%w: image reference repository is invalid", ErrInvalidBundle)
	}
	match := artifactVersionPattern.FindStringSubmatch(tag)
	if match == nil {
		return ImageReference{}, fmt.Errorf("%w: image tag %q must look like v1.36.0-katl.1", ErrInvalidBundle, tag)
	}
	if _, err := remote.NewRepository(repository); err != nil {
		return ImageReference{}, fmt.Errorf("%w: image repository is invalid: %v", ErrInvalidBundle, err)
	}
	return ImageReference{
		Value:           value,
		Repository:      repository,
		Tag:             tag,
		ManifestDigest:  manifestDigest,
		PayloadVersion:  "v" + match[1],
		ArtifactVersion: tag,
		Source:          "https://" + parts[0] + "/v2/" + parts[1],
	}, nil
}

func FetchAndStage(ctx context.Context, request Request) (Staged, error) {
	if err := validateRequest(request); err != nil {
		return Staged{}, err
	}
	client := request.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Minute}
	}
	if image, err := ParseImageReference(request.Ref); err == nil {
		if strings.TrimRight(strings.TrimSpace(request.Source), "/") != image.Source {
			return Staged{}, fmt.Errorf("%w: image reference repository does not match source", ErrInvalidBundle)
		}
		repository, ok, err := registryRepository(request.Source, client)
		if err != nil {
			return Staged{}, err
		}
		if !ok {
			return Staged{}, fmt.Errorf("%w: image reference requires an OCI registry source", ErrInvalidBundle)
		}
		identifier := image.Tag
		if image.ManifestDigest != "" {
			identifier = image.ManifestDigest
		}
		return fetchAndStageOCI(ctx, request, ref{PayloadVersion: image.PayloadVersion}, repository, identifier, "", image.ManifestDigest, image.ArtifactVersion)
	}
	source := strings.TrimRight(strings.TrimSpace(request.Source), "/")
	ref, err := parseRef(request.Ref)
	if err != nil {
		return Staged{}, err
	}
	if repository, ok, err := registryRepository(request.Source, client); err != nil {
		return Staged{}, err
	} else if ok {
		tag := registryTagPrefix + strings.TrimPrefix(ref.BundleDigest, "sha256:")
		return fetchAndStageOCI(ctx, request, ref, repository, tag, ref.BundleDigest, "", "")
	}

	indexURL := source + "/index.json"
	indexBytes, err := fetch(ctx, client, indexURL)
	if err != nil {
		return Staged{}, fmt.Errorf("fetch Kubernetes payload index %s: %w", inventory.Redact(indexURL), err)
	}
	var index Index
	if err := json.Unmarshal(indexBytes, &index); err != nil {
		return Staged{}, fmt.Errorf("%w: decode index: %v", ErrInvalidBundle, err)
	}
	entry, err := selectEntry(index, ref, request)
	if err != nil {
		return Staged{}, err
	}

	bundlePath, err := cleanRelativePath("bundle manifest", entry.BundleManifestPath)
	if err != nil {
		return Staged{}, err
	}
	bundleURL := source + "/" + bundlePath
	bundleBytes, err := fetch(ctx, client, bundleURL)
	if err != nil {
		return Staged{}, fmt.Errorf("fetch Kubernetes payload bundle %s: %w", inventory.Redact(bundleURL), err)
	}
	if digest := sha256Digest(bundleBytes); digest != ref.BundleDigest {
		return Staged{}, fmt.Errorf("%w: bundle manifest digest got %s want %s", ErrInvalidBundle, digest, ref.BundleDigest)
	}
	var bundle Bundle
	if err := json.Unmarshal(bundleBytes, &bundle); err != nil {
		return Staged{}, fmt.Errorf("%w: decode bundle manifest: %v", ErrInvalidBundle, err)
	}
	if err := validateBundle(bundle, entry, ref, request); err != nil {
		return Staged{}, err
	}

	payload, err := descriptor(bundle.Payloads, sysextRole)
	if err != nil {
		return Staged{}, err
	}
	if payload == nil {
		return Staged{}, fmt.Errorf("%w: missing systemd-sysext payload descriptor", ErrInvalidBundle)
	}
	if payload.Digest != entry.SysextPayloadDigest {
		return Staged{}, fmt.Errorf("%w: bundle sysext digest does not match index entry", ErrInvalidBundle)
	}
	metadata, err := descriptor(bundle.Metadata, metadataRole)
	if err != nil {
		return Staged{}, err
	}
	if metadata == nil {
		return Staged{}, fmt.Errorf("%w: missing sysext metadata descriptor", ErrInvalidBundle)
	}
	metadataBytes, err := fetchDescriptor(ctx, client, source, *metadata, metadataMediaType)
	if err != nil {
		return Staged{}, err
	}
	if err := validateSysextMetadata(metadataBytes, bundle, *payload, request); err != nil {
		return Staged{}, err
	}
	provenance, err := descriptor(bundle.Metadata, provenanceRole)
	if err != nil {
		return Staged{}, err
	}
	if provenance == nil {
		return Staged{}, fmt.Errorf("%w: missing package provenance descriptor", ErrInvalidBundle)
	}
	provenanceBytes, err := fetchDescriptor(ctx, client, source, *provenance, provenanceMediaType)
	if err != nil {
		return Staged{}, err
	}
	if err := validatePackageProvenance(provenanceBytes, bundle); err != nil {
		return Staged{}, err
	}
	catalog, err := descriptor(bundle.Metadata, catalogRole)
	if err != nil {
		return Staged{}, err
	}
	if catalog == nil {
		return Staged{}, fmt.Errorf("%w: missing catalog fragment descriptor", ErrInvalidBundle)
	}
	catalogBytes, err := fetchDescriptor(ctx, client, source, *catalog, catalogMediaType)
	if err != nil {
		return Staged{}, err
	}
	if err := validateCatalogFragment(catalogBytes, bundle, entry, *payload); err != nil {
		return Staged{}, err
	}

	return stage(request, bundle, bundleBytes, metadataBytes, provenanceBytes, catalogBytes, *payload, func(path string) error {
		return fetchDescriptorToFile(ctx, client, source, *payload, sysextMediaType, path)
	})
}

type ociRepository interface {
	Resolve(context.Context, string) (ocispec.Descriptor, error)
	Fetch(context.Context, ocispec.Descriptor) (io.ReadCloser, error)
}

func registryRepository(source string, client *http.Client) (ociRepository, bool, error) {
	parsed, err := url.Parse(strings.TrimSpace(source))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return nil, false, nil
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, false, nil
	}
	repositoryName := strings.Trim(strings.TrimPrefix(parsed.EscapedPath(), "/v2/"), "/")
	if !strings.HasPrefix(parsed.EscapedPath(), "/v2/") || repositoryName == "" || strings.Contains(repositoryName, "%") {
		return nil, false, nil
	}
	repository, err := remote.NewRepository(parsed.Host + "/" + repositoryName)
	if err != nil {
		return nil, false, fmt.Errorf("%w: invalid OCI registry source %s: %v", ErrInvalidBundle, inventory.Redact(source), err)
	}
	repository.Client = &auth.Client{
		Client: client,
		Cache:  auth.NewCache(),
	}
	return repository, true, nil
}

func fetchAndStageOCI(ctx context.Context, request Request, ref ref, repository ociRepository, identifier, expectedBundleDigest, expectedManifestDigest, expectedArtifactVersion string) (Staged, error) {
	manifestDescriptor, err := repository.Resolve(ctx, identifier)
	if err != nil {
		return Staged{}, fmt.Errorf("resolve Kubernetes payload OCI reference %s from %s: %w", identifier, inventory.Redact(request.Source), err)
	}
	if expectedManifestDigest != "" && manifestDescriptor.Digest.String() != expectedManifestDigest {
		return Staged{}, fmt.Errorf("%w: resolved OCI manifest digest does not match image reference", ErrInvalidBundle)
	}
	manifestBytes, err := content.FetchAll(ctx, repository, manifestDescriptor)
	if err != nil {
		return Staged{}, fmt.Errorf("fetch Kubernetes payload OCI manifest from %s: %w", inventory.Redact(request.Source), err)
	}
	var manifest ocispec.Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return Staged{}, fmt.Errorf("%w: decode Kubernetes payload OCI manifest: %v", ErrInvalidBundle, err)
	}
	if manifest.SchemaVersion != 2 || manifest.MediaType != ocispec.MediaTypeImageManifest || manifest.ArtifactType != bundleArtifactType {
		return Staged{}, fmt.Errorf("%w: invalid Kubernetes payload OCI manifest identity", ErrInvalidBundle)
	}
	if manifest.Config.MediaType != bundleMediaType || (expectedBundleDigest != "" && manifest.Config.Digest.String() != expectedBundleDigest) {
		return Staged{}, fmt.Errorf("%w: OCI config does not match pinned bundle manifest digest", ErrInvalidBundle)
	}
	ref.BundleDigest = manifest.Config.Digest.String()
	bundleBytes, err := content.FetchAll(ctx, repository, manifest.Config)
	if err != nil {
		return Staged{}, fmt.Errorf("fetch Kubernetes payload bundle config from %s: %w", inventory.Redact(request.Source), err)
	}
	var bundle Bundle
	if err := json.Unmarshal(bundleBytes, &bundle); err != nil {
		return Staged{}, fmt.Errorf("%w: decode bundle manifest: %v", ErrInvalidBundle, err)
	}
	if expectedArtifactVersion != "" && bundle.ArtifactVersion != expectedArtifactVersion {
		return Staged{}, fmt.Errorf("%w: bundle artifact version %q does not match image tag %q", ErrInvalidBundle, bundle.ArtifactVersion, expectedArtifactVersion)
	}
	entry := IndexEntry{
		PayloadVersion:             ref.PayloadVersion,
		ArtifactVersion:            bundle.ArtifactVersion,
		KubernetesMinor:            bundle.KubernetesMinor,
		Architecture:               bundle.Architecture,
		BundleManifestDigest:       ref.BundleDigest,
		SysextPayloadDigest:        ociPayloadDigest(bundle),
		SupportedRuntimeInterfaces: append([]string(nil), bundle.SupportedRuntimeInterfaces...),
	}
	if err := validateBundle(bundle, entry, ref, request); err != nil {
		return Staged{}, err
	}

	payload, err := descriptor(bundle.Payloads, sysextRole)
	if err != nil {
		return Staged{}, err
	}
	if payload == nil {
		return Staged{}, fmt.Errorf("%w: missing systemd-sysext payload descriptor", ErrInvalidBundle)
	}
	metadata, err := requiredDescriptor(bundle.Metadata, metadataRole)
	if err != nil {
		return Staged{}, err
	}
	provenance, err := requiredDescriptor(bundle.Metadata, provenanceRole)
	if err != nil {
		return Staged{}, err
	}
	catalog, err := requiredDescriptor(bundle.Metadata, catalogRole)
	if err != nil {
		return Staged{}, err
	}
	expected := []Descriptor{*payload, *metadata, *provenance, *catalog}
	if len(manifest.Layers) != len(expected) {
		return Staged{}, fmt.Errorf("%w: OCI manifest has %d layers, want %d bundle descriptors", ErrInvalidBundle, len(manifest.Layers), len(expected))
	}
	payloadLayer, err := matchingOCILayer(manifest.Layers, *payload)
	if err != nil {
		return Staged{}, err
	}
	fetched := make(map[string][]byte, len(expected)-1)
	for _, descriptor := range expected {
		if descriptor.Role == sysextRole {
			continue
		}
		layer, err := matchingOCILayer(manifest.Layers, descriptor)
		if err != nil {
			return Staged{}, err
		}
		data, err := fetchOCIContent(ctx, repository, layer)
		if err != nil {
			return Staged{}, fmt.Errorf("fetch OCI layer for descriptor %s from %s: %w", descriptor.Role, inventory.Redact(request.Source), err)
		}
		fetched[descriptor.Role] = data
	}
	if err := validateSysextMetadata(fetched[metadataRole], bundle, *payload, request); err != nil {
		return Staged{}, err
	}
	if err := validatePackageProvenance(fetched[provenanceRole], bundle); err != nil {
		return Staged{}, err
	}
	if err := validateCatalogFragment(fetched[catalogRole], bundle, entry, *payload); err != nil {
		return Staged{}, err
	}
	return stage(request, bundle, bundleBytes, fetched[metadataRole], fetched[provenanceRole], fetched[catalogRole], *payload, func(path string) error {
		return fetchOCIContentToFile(ctx, repository, payloadLayer, path)
	})
}

func fetchOCIContent(ctx context.Context, repository ociRepository, descriptor ocispec.Descriptor) ([]byte, error) {
	reader, err := repository.Fetch(ctx, descriptor)
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	verified := content.NewVerifyReader(reader, descriptor)
	data, err := io.ReadAll(verified)
	if err != nil {
		return nil, err
	}
	if err := verified.Verify(); err != nil {
		return nil, err
	}
	return data, nil
}

func fetchOCIContentToFile(ctx context.Context, repository ociRepository, descriptor ocispec.Descriptor, path string) error {
	reader, err := repository.Fetch(ctx, descriptor)
	if err != nil {
		return err
	}
	defer reader.Close()

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	verified := content.NewVerifyReader(reader, descriptor)
	written, copyErr := io.Copy(file, verified)
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if written != descriptor.Size {
		return fmt.Errorf("OCI payload size got %d want %d", written, descriptor.Size)
	}
	return verified.Verify()
}

func ociPayloadDigest(bundle Bundle) string {
	payload, err := descriptor(bundle.Payloads, sysextRole)
	if err != nil || payload == nil {
		return ""
	}
	return payload.Digest
}

func requiredDescriptor(list []Descriptor, role string) (*Descriptor, error) {
	descriptor, err := descriptor(list, role)
	if err != nil {
		return nil, err
	}
	if descriptor == nil {
		return nil, fmt.Errorf("%w: missing %s descriptor", ErrInvalidBundle, role)
	}
	return descriptor, nil
}

func matchingOCILayer(layers []ocispec.Descriptor, descriptor Descriptor) (ocispec.Descriptor, error) {
	var match *ocispec.Descriptor
	for i := range layers {
		layer := &layers[i]
		if layer.Digest.String() != descriptor.Digest {
			continue
		}
		if match != nil {
			return ocispec.Descriptor{}, fmt.Errorf("%w: duplicate OCI layer for descriptor %s", ErrInvalidBundle, descriptor.Role)
		}
		match = layer
	}
	if match == nil || match.MediaType != descriptor.MediaType || match.Size != descriptor.SizeBytes {
		return ocispec.Descriptor{}, fmt.Errorf("%w: OCI layer does not match descriptor %s", ErrInvalidBundle, descriptor.Role)
	}
	return *match, nil
}

func PayloadVersionFromRef(value string) (string, error) {
	if image, err := ParseImageReference(value); err == nil {
		return image.PayloadVersion, nil
	}
	ref, err := parseRef(value)
	if err != nil {
		return "", err
	}
	return ref.PayloadVersion, nil
}

type ref struct {
	PayloadVersion string
	BundleDigest   string
}

func validateRequest(request Request) error {
	if strings.TrimSpace(request.CacheDir) == "" {
		return fmt.Errorf("cache dir is required")
	}
	if strings.TrimSpace(request.RuntimeInterface) == "" {
		return fmt.Errorf("runtime interface is required")
	}
	if strings.TrimSpace(request.Architecture) == "" {
		return fmt.Errorf("architecture is required")
	}
	source := strings.TrimSpace(request.Source)
	parsed, err := url.Parse(source)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return fmt.Errorf("%w: source must be an absolute HTTPS URL", ErrInvalidBundle)
	}
	if strings.HasSuffix(strings.ToLower(parsed.Path), ".raw") || strings.HasSuffix(strings.ToLower(parsed.Path), ".sysext.raw") {
		return fmt.Errorf("%w: raw sysext URLs are not Kubernetes bundle sources", ErrInvalidBundle)
	}
	return nil
}

func parseRef(value string) (ref, error) {
	parts := strings.Split(strings.TrimSpace(value), "@")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" {
		return ref{}, fmt.Errorf("%w: ref must be vMAJOR.MINOR.PATCH@sha256:<digest>", ErrInvalidBundle)
	}
	if sysextcatalog.KubernetesMinor(parts[0]) == "" {
		return ref{}, fmt.Errorf("%w: ref payload version %q must be vMAJOR.MINOR.PATCH", ErrInvalidBundle, parts[0])
	}
	if err := validateDigest(parts[1]); err != nil {
		return ref{}, fmt.Errorf("%w: ref digest: %v", ErrInvalidBundle, err)
	}
	return ref{PayloadVersion: parts[0], BundleDigest: parts[1]}, nil
}

func selectEntry(index Index, ref ref, request Request) (IndexEntry, error) {
	if index.APIVersion != APIVersion || index.Kind != IndexKind {
		return IndexEntry{}, fmt.Errorf("%w: invalid index header", ErrInvalidBundle)
	}
	for _, entry := range index.Entries {
		if entry.PayloadVersion != ref.PayloadVersion || entry.BundleManifestDigest != ref.BundleDigest {
			continue
		}
		if entry.Deprecated {
			return IndexEntry{}, fmt.Errorf("%w: selected bundle is deprecated", ErrInvalidBundle)
		}
		if entry.Architecture != request.Architecture {
			return IndexEntry{}, fmt.Errorf("%w: architecture %q does not match runtime architecture %q", ErrInvalidBundle, entry.Architecture, request.Architecture)
		}
		if !contains(entry.SupportedRuntimeInterfaces, request.RuntimeInterface) {
			return IndexEntry{}, fmt.Errorf("%w: runtime interface %q is unsupported", ErrInvalidBundle, request.RuntimeInterface)
		}
		if _, err := cleanRelativePath("bundle manifest", entry.BundleManifestPath); err != nil {
			return IndexEntry{}, err
		}
		if _, err := cleanRelativePath("catalog entry", entry.CatalogEntryPath); err != nil {
			return IndexEntry{}, err
		}
		return entry, nil
	}
	return IndexEntry{}, fmt.Errorf("%w: no index entry matches ref %s@%s", ErrInvalidBundle, ref.PayloadVersion, ref.BundleDigest)
}

func validateBundle(bundle Bundle, entry IndexEntry, ref ref, request Request) error {
	if bundle.APIVersion != APIVersion || bundle.Kind != BundleKind {
		return fmt.Errorf("%w: invalid bundle header", ErrInvalidBundle)
	}
	if bundle.Name != BundleName || bundle.ArtifactKind != ArtifactKind {
		return fmt.Errorf("%w: unexpected bundle identity", ErrInvalidBundle)
	}
	if bundle.PayloadVersion != ref.PayloadVersion || bundle.PayloadVersion != entry.PayloadVersion {
		return fmt.Errorf("%w: bundle payload version does not match ref", ErrInvalidBundle)
	}
	if sysextcatalog.KubernetesMinor(bundle.PayloadVersion) != bundle.KubernetesMinor {
		return fmt.Errorf("%w: Kubernetes minor does not match payload version", ErrInvalidBundle)
	}
	if bundle.ArtifactVersion != entry.ArtifactVersion {
		return fmt.Errorf("%w: bundle artifact version does not match index entry", ErrInvalidBundle)
	}
	if bundle.KubernetesMinor != entry.KubernetesMinor {
		return fmt.Errorf("%w: bundle Kubernetes minor does not match index entry", ErrInvalidBundle)
	}
	if bundle.Architecture != request.Architecture || bundle.Architecture != entry.Architecture {
		return fmt.Errorf("%w: bundle architecture is incompatible", ErrInvalidBundle)
	}
	if !contains(bundle.SupportedRuntimeInterfaces, request.RuntimeInterface) {
		return fmt.Errorf("%w: bundle does not support runtime interface %q", ErrInvalidBundle, request.RuntimeInterface)
	}
	if !stringSlicesEqual(bundle.SupportedRuntimeInterfaces, entry.SupportedRuntimeInterfaces) {
		return fmt.Errorf("%w: bundle runtime interfaces do not match index entry", ErrInvalidBundle)
	}
	if bundle.PackageLockDigest == "" && bundle.BuildInputDigest == "" {
		return fmt.Errorf("%w: packageLockDigest or buildInputDigest is required", ErrInvalidBundle)
	}
	if len(bundle.Signatures) == 0 {
		return fmt.Errorf("%w: signature or unsigned-fixture marker is required", ErrInvalidBundle)
	}
	for _, signature := range bundle.Signatures {
		if signature.Type == "unsigned-fixture" {
			return nil
		}
	}
	return fmt.Errorf("%w: v0.1 accepts signed bundles only after signature policy lands; fixture requires unsigned-fixture marker", ErrInvalidBundle)
}

func validateSysextMetadata(data []byte, bundle Bundle, payload Descriptor, request Request) error {
	var meta artifact.LocalMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return fmt.Errorf("%w: decode sysext metadata: %v", ErrInvalidBundle, err)
	}
	if meta.Name != sysextcatalog.KubernetesName || meta.Kind != artifact.ArtifactSysext {
		return fmt.Errorf("%w: sysext metadata identity is not Kubernetes sysext", ErrInvalidBundle)
	}
	if meta.PayloadVersion != bundle.PayloadVersion {
		return fmt.Errorf("%w: sysext metadata payload version %q does not match bundle payload version %q", ErrInvalidBundle, meta.PayloadVersion, bundle.PayloadVersion)
	}
	if meta.Architecture != request.Architecture {
		return fmt.Errorf("%w: sysext metadata architecture %q does not match runtime architecture %q", ErrInvalidBundle, meta.Architecture, request.Architecture)
	}
	if "sha256:"+strings.ToLower(meta.SHA256) != payload.Digest {
		return fmt.Errorf("%w: sysext metadata payload digest does not match descriptor", ErrInvalidBundle)
	}
	if meta.SizeBytes != payload.SizeBytes {
		return fmt.Errorf("%w: sysext metadata payload size does not match descriptor", ErrInvalidBundle)
	}
	if meta.CompatibleRuntime == nil || meta.CompatibleRuntime.Interface != request.RuntimeInterface {
		return fmt.Errorf("%w: sysext metadata does not support runtime interface %q", ErrInvalidBundle, request.RuntimeInterface)
	}
	if meta.RuntimeInterface != "" && meta.RuntimeInterface != request.RuntimeInterface {
		return fmt.Errorf("%w: sysext metadata runtime interface %q does not match runtime interface %q", ErrInvalidBundle, meta.RuntimeInterface, request.RuntimeInterface)
	}
	return nil
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

func validatePackageProvenance(data []byte, bundle Bundle) error {
	var provenance packageProvenance
	if err := json.Unmarshal(data, &provenance); err != nil {
		return fmt.Errorf("%w: decode package provenance: %v", ErrInvalidBundle, err)
	}
	if provenance.APIVersion != APIVersion || provenance.Kind != "KubernetesPackageProvenance" {
		return fmt.Errorf("%w: invalid package provenance header", ErrInvalidBundle)
	}
	if provenance.PayloadVersion != bundle.PayloadVersion || provenance.ArtifactVersion != bundle.ArtifactVersion {
		return fmt.Errorf("%w: package provenance identity does not match bundle", ErrInvalidBundle)
	}
	if !stringMapEqual(provenance.PackageVersions, bundle.PackageVersions) {
		return fmt.Errorf("%w: package provenance versions do not match bundle", ErrInvalidBundle)
	}
	return nil
}

type catalogFragment struct {
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

func validateCatalogFragment(data []byte, bundle Bundle, entry IndexEntry, payload Descriptor) error {
	var fragment catalogFragment
	if err := json.Unmarshal(data, &fragment); err != nil {
		return fmt.Errorf("%w: decode catalog fragment: %v", ErrInvalidBundle, err)
	}
	if fragment.Name != sysextcatalog.KubernetesName || fragment.PayloadVersion != bundle.PayloadVersion || fragment.ArtifactVersion != bundle.ArtifactVersion {
		return fmt.Errorf("%w: catalog fragment identity does not match bundle", ErrInvalidBundle)
	}
	if fragment.KubernetesMinor != bundle.KubernetesMinor || fragment.Architecture != bundle.Architecture {
		return fmt.Errorf("%w: catalog fragment compatibility does not match bundle", ErrInvalidBundle)
	}
	if (entry.BundleManifestPath != "" && fragment.BundleManifestPath != entry.BundleManifestPath) || fragment.SysextPayloadDigest != payload.Digest || fragment.SysextPayloadSizeBytes != payload.SizeBytes {
		return fmt.Errorf("%w: catalog fragment payload location does not match index and bundle", ErrInvalidBundle)
	}
	if !stringSlicesEqual(fragment.SupportedRuntimeInterfaces, bundle.SupportedRuntimeInterfaces) || !stringMapEqual(fragment.PackageVersions, bundle.PackageVersions) {
		return fmt.Errorf("%w: catalog fragment metadata does not match bundle", ErrInvalidBundle)
	}
	return nil
}

func descriptor(list []Descriptor, role string) (*Descriptor, error) {
	var found *Descriptor
	for i := range list {
		if list[i].Role == role {
			if found != nil {
				return nil, fmt.Errorf("%w: duplicate %s descriptor", ErrInvalidBundle, role)
			}
			found = &list[i]
		}
	}
	return found, nil
}

func fetchDescriptor(ctx context.Context, client *http.Client, source string, descriptor Descriptor, mediaType string) ([]byte, error) {
	if descriptor.MediaType != mediaType {
		return nil, fmt.Errorf("%w: descriptor %s media type got %q want %q", ErrInvalidBundle, descriptor.Role, descriptor.MediaType, mediaType)
	}
	if err := validateDigest(descriptor.Digest); err != nil {
		return nil, fmt.Errorf("%w: descriptor %s digest: %v", ErrInvalidBundle, descriptor.Role, err)
	}
	if descriptor.SizeBytes <= 0 {
		return nil, fmt.Errorf("%w: descriptor %s size must be positive", ErrInvalidBundle, descriptor.Role)
	}
	url := source + "/blobs/sha256/" + strings.TrimPrefix(descriptor.Digest, "sha256:")
	data, err := fetch(ctx, client, url)
	if err != nil {
		return nil, fmt.Errorf("fetch descriptor %s %s: %w", descriptor.Role, inventory.Redact(url), err)
	}
	if got := sha256Digest(data); got != descriptor.Digest {
		return nil, fmt.Errorf("%w: descriptor %s digest got %s want %s", ErrInvalidBundle, descriptor.Role, got, descriptor.Digest)
	}
	if int64(len(data)) != descriptor.SizeBytes {
		return nil, fmt.Errorf("%w: descriptor %s size got %d want %d", ErrInvalidBundle, descriptor.Role, len(data), descriptor.SizeBytes)
	}
	return data, nil
}

func fetchDescriptorToFile(ctx context.Context, client *http.Client, source string, descriptor Descriptor, mediaType string, destination string) error {
	if descriptor.MediaType != mediaType {
		return fmt.Errorf("%w: descriptor %s media type got %q want %q", ErrInvalidBundle, descriptor.Role, descriptor.MediaType, mediaType)
	}
	if err := validateDigest(descriptor.Digest); err != nil {
		return fmt.Errorf("%w: descriptor %s digest: %v", ErrInvalidBundle, descriptor.Role, err)
	}
	if descriptor.SizeBytes <= 0 {
		return fmt.Errorf("%w: descriptor %s size must be positive", ErrInvalidBundle, descriptor.Role)
	}
	value := source + "/blobs/sha256/" + strings.TrimPrefix(descriptor.Digest, "sha256:")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, value, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch descriptor %s %s: %w", descriptor.Role, inventory.Redact(value), cleanFetchError(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch descriptor %s %s: HTTP %d", descriptor.Role, inventory.Redact(value), resp.StatusCode)
	}
	file, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	digest := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(file, digest), io.LimitReader(resp.Body, descriptor.SizeBytes+1))
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	got := "sha256:" + hex.EncodeToString(digest.Sum(nil))
	if got != descriptor.Digest {
		return fmt.Errorf("%w: descriptor %s digest got %s want %s", ErrInvalidBundle, descriptor.Role, got, descriptor.Digest)
	}
	if written != descriptor.SizeBytes {
		return fmt.Errorf("%w: descriptor %s size got %d want %d", ErrInvalidBundle, descriptor.Role, written, descriptor.SizeBytes)
	}
	return nil
}

func stage(request Request, bundle Bundle, bundleBytes []byte, metadataBytes []byte, provenanceBytes []byte, catalogBytes []byte, payload Descriptor, writePayload func(string) error) (Staged, error) {
	bundleDigest := sha256Digest(bundleBytes)
	payloadDigest := payload.Digest
	bundleDir := filepath.Join(request.CacheDir, "bundles", digestDir(bundleDigest))
	sysextDir := filepath.Join(request.CacheDir, "sysext", digestDir(payloadDigest))
	tmp := filepath.Join(request.CacheDir, ".tmp-"+strings.TrimPrefix(bundleDigest, "sha256:"))
	if err := os.RemoveAll(tmp); err != nil {
		return Staged{}, err
	}
	if err := os.MkdirAll(filepath.Join(tmp, "bundle"), 0o755); err != nil {
		return Staged{}, fmt.Errorf("create staging temp dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(tmp, "sysext"), 0o755); err != nil {
		return Staged{}, fmt.Errorf("create sysext temp dir: %w", err)
	}
	defer os.RemoveAll(tmp)

	if err := os.WriteFile(filepath.Join(tmp, "bundle", "bundle.json"), bundleBytes, 0o644); err != nil {
		return Staged{}, err
	}
	if err := os.WriteFile(filepath.Join(tmp, "bundle", "package-provenance.json"), provenanceBytes, 0o644); err != nil {
		return Staged{}, err
	}
	if err := os.WriteFile(filepath.Join(tmp, "bundle", "catalog-entry.json"), catalogBytes, 0o644); err != nil {
		return Staged{}, err
	}
	payloadName, err := cleanFileName(payload.FileName)
	if err != nil {
		return Staged{}, err
	}
	if writePayload == nil {
		return Staged{}, fmt.Errorf("stage sysext payload: writer is required")
	}
	if err := writePayload(filepath.Join(tmp, "sysext", payloadName)); err != nil {
		return Staged{}, err
	}
	if err := os.WriteFile(filepath.Join(tmp, "sysext", "metadata.json"), metadataBytes, 0o644); err != nil {
		return Staged{}, err
	}
	if err := os.MkdirAll(filepath.Dir(bundleDir), 0o755); err != nil {
		return Staged{}, err
	}
	if err := os.MkdirAll(filepath.Dir(sysextDir), 0o755); err != nil {
		return Staged{}, err
	}
	if err := replaceDir(filepath.Join(tmp, "bundle"), bundleDir); err != nil {
		return Staged{}, err
	}
	if err := replaceDir(filepath.Join(tmp, "sysext"), sysextDir); err != nil {
		return Staged{}, err
	}
	if err := writeLocalIndex(request.CacheDir, bundle, bundleDigest, payloadDigest); err != nil {
		return Staged{}, err
	}

	activation := strings.TrimSpace(request.ActivationPath)
	if activation == "" {
		activation = "/run/extensions/katl-kubernetes.raw"
	}
	sysextPath := filepath.Join(sysextDir, payloadName)
	return Staged{
		PayloadVersion:       bundle.PayloadVersion,
		ArtifactVersion:      bundle.ArtifactVersion,
		Architecture:         bundle.Architecture,
		BundleManifestDigest: bundleDigest,
		SysextPayloadDigest:  payloadDigest,
		BundleDir:            bundleDir,
		SysextDir:            sysextDir,
		SysextPath:           sysextPath,
		MetadataPath:         filepath.Join(sysextDir, "metadata.json"),
		ExtensionRef: generation.ExtensionRef{
			Name:            sysextcatalog.KubernetesName,
			Path:            sysextPath,
			ActivationPath:  activation,
			SHA256:          strings.TrimPrefix(payloadDigest, "sha256:"),
			ArtifactVersion: bundle.ArtifactVersion,
			PayloadVersion:  bundle.PayloadVersion,
			Architecture:    bundle.Architecture,
			Compatibility: generation.ExtensionCompatibility{
				RuntimeInterfaces: append([]string(nil), bundle.SupportedRuntimeInterfaces...),
			},
		},
	}, nil
}

func fetch(ctx context.Context, client *http.Client, value string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, value, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, cleanFetchError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<30))
}

func cleanRelativePath(name string, value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	cleaned := path.Clean(trimmed)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") || path.IsAbs(cleaned) || hasUnsafePathSegment(trimmed) {
		return "", fmt.Errorf("%w: %s path %q is not relative", ErrInvalidBundle, name, value)
	}
	return cleaned, nil
}

func cleanFileName(value string) (string, error) {
	base := path.Base(strings.TrimSpace(value))
	if base == "." || base == "/" || base != strings.TrimSpace(value) || strings.Contains(base, "..") {
		return "", fmt.Errorf("%w: descriptor fileName %q is not a safe file name", ErrInvalidBundle, value)
	}
	return base, nil
}

func validateDigest(value string) error {
	if !strings.HasPrefix(value, "sha256:") {
		return fmt.Errorf("must start with sha256:")
	}
	hexPart := strings.TrimPrefix(value, "sha256:")
	if len(hexPart) != sha256.Size*2 || hexPart != strings.ToLower(hexPart) {
		return fmt.Errorf("must be lowercase sha256:<hex>")
	}
	_, err := hex.DecodeString(hexPart)
	return err
}

func sha256Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func digestDir(digest string) string {
	return "sha256-" + strings.TrimPrefix(digest, "sha256:")
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func hasUnsafePathSegment(value string) bool {
	for _, part := range strings.Split(value, "/") {
		if part == "." || part == ".." {
			return true
		}
	}
	return false
}

func stringSlicesEqual(left []string, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func stringMapEqual(left map[string]string, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, leftValue := range left {
		if right[key] != leftValue {
			return false
		}
	}
	return true
}

func cleanFetchError(err error) error {
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		return err
	}
	return fmt.Errorf("%s: %w", urlErr.Op, urlErr.Err)
}

func replaceDir(src string, dst string) error {
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	return os.Rename(src, dst)
}

func writeLocalIndex(cacheDir string, bundle Bundle, bundleDigest string, payloadDigest string) error {
	index := Index{
		APIVersion: APIVersion,
		Kind:       IndexKind,
		Entries: []IndexEntry{{
			PayloadVersion:             bundle.PayloadVersion,
			ArtifactVersion:            bundle.ArtifactVersion,
			KubernetesMinor:            bundle.KubernetesMinor,
			Architecture:               bundle.Architecture,
			BundleManifestDigest:       bundleDigest,
			BundleManifestPath:         filepath.ToSlash(filepath.Join("bundles", digestDir(bundleDigest), "bundle.json")),
			SysextPayloadDigest:        payloadDigest,
			SupportedRuntimeInterfaces: append([]string(nil), bundle.SupportedRuntimeInterfaces...),
			CatalogEntryPath:           filepath.ToSlash(filepath.Join("bundles", digestDir(bundleDigest), "catalog-entry.json")),
		}},
	}
	data, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(cacheDir, "index.json"), append(data, '\n'), 0o644)
}
