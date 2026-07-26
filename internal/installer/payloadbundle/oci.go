// Package payloadbundle implements the OCI distribution mechanics shared by
// Kubernetes payload bundles and user-owned system extension bundles.
package payloadbundle

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
)

type Reference struct {
	Value          string
	Repository     string
	Registry       string
	Tag            string
	ManifestDigest string
	Source         string
}

func ParseReference(value string) (Reference, error) {
	value = strings.TrimSpace(value)
	if value == "" || strings.Contains(value, "://") {
		return Reference{}, fmt.Errorf("OCI reference must be REGISTRY/REPOSITORY:TAG with an optional @sha256 digest")
	}
	nameAndTag, manifestDigest, hasDigest := strings.Cut(value, "@")
	if hasDigest && (strings.Contains(manifestDigest, "@") || !validDigest(manifestDigest)) {
		return Reference{}, fmt.Errorf("OCI reference manifest digest is invalid")
	}
	lastSlash := strings.LastIndex(nameAndTag, "/")
	lastColon := strings.LastIndex(nameAndTag, ":")
	if lastSlash <= 0 || lastColon <= lastSlash+1 || lastColon == len(nameAndTag)-1 {
		return Reference{}, fmt.Errorf("OCI reference must include a registry, repository, and tag")
	}
	repository := nameAndTag[:lastColon]
	tag := nameAndTag[lastColon+1:]
	parts := strings.SplitN(repository, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.ContainsAny(repository, "?#") {
		return Reference{}, fmt.Errorf("OCI reference repository is invalid")
	}
	if _, err := remote.NewRepository(repository); err != nil {
		return Reference{}, fmt.Errorf("OCI reference repository is invalid: %w", err)
	}
	return Reference{
		Value:          value,
		Repository:     repository,
		Registry:       parts[0],
		Tag:            tag,
		ManifestDigest: manifestDigest,
		Source:         "https://" + parts[0] + "/v2/" + parts[1],
	}, nil
}

type FetchRequest struct {
	Reference            string
	ArtifactType         string
	ConfigMediaType      string
	Client               *http.Client
	UseDockerCredentials bool
}

type Target interface {
	Resolve(context.Context, string) (ocispec.Descriptor, error)
	Fetch(context.Context, ocispec.Descriptor) (io.ReadCloser, error)
}

type Fetched struct {
	Reference      Reference
	ManifestDigest string
	Manifest       ocispec.Manifest
	Config         []byte
	Layers         map[string][]byte
}

// Descriptor is the common Katl custom-manifest descriptor used by every
// payload bundle. Artifact-specific manifests add policy around roles, but do
// not redefine transport or integrity verification.
type Descriptor struct {
	Role        string            `json:"role"`
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	SizeBytes   int64             `json:"sizeBytes"`
	FileName    string            `json:"fileName"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type Blob struct {
	Descriptor Descriptor
	Data       []byte
}

type PublishRequest struct {
	Reference            string
	ArtifactType         string
	ConfigMediaType      string
	Config               []byte
	Blobs                []Blob
	Annotations          map[string]string
	Client               *http.Client
	UseDockerCredentials bool
}

type PackRequest struct {
	ArtifactType    string
	ConfigMediaType string
	Config          []byte
	Blobs           []Blob
	Annotations     map[string]string
}

type Packed struct {
	ManifestDigest string
	Manifest       []byte
}

type Published struct {
	Reference      Reference
	ManifestDigest string
	Existing       bool
}

// ManifestDigestTag returns a stable registry tag for an OCI manifest digest.
// The "sha256-<hex>" shape is reserved by the OCI referrers fallback protocol,
// so Katl uses a distinct namespace for direct immutable manifest references.
func ManifestDigestTag(manifestDigest string) (string, error) {
	if !validDigest(manifestDigest) {
		return "", fmt.Errorf("OCI manifest digest is invalid")
	}
	return "manifest-sha256-" + strings.TrimPrefix(manifestDigest, "sha256:"), nil
}

// Pack builds and verifies the common OCI envelope without contacting a
// registry. Producers use this in presubmit checks so the bytes validated in
// CI are packed by the same implementation that publishes them.
func Pack(ctx context.Context, request PackRequest) (Packed, error) {
	store, manifestDescriptor, cleanup, err := pack(ctx, request, "bundle")
	if err != nil {
		return Packed{}, err
	}
	defer cleanup()
	manifest, err := ReadContent(ctx, store, manifestDescriptor)
	if err != nil {
		return Packed{}, fmt.Errorf("read packed OCI manifest: %w", err)
	}
	return Packed{ManifestDigest: manifestDescriptor.Digest.String(), Manifest: manifest}, nil
}

// DescribeFile calculates the descriptor used by every Katl payload bundle.
// Artifact-specific producers choose the role and media type; this common
// layer owns byte identity and safe OCI title annotations.
func DescribeFile(filePath, role, mediaType, fileName string) (Blob, error) {
	if strings.TrimSpace(filePath) == "" {
		return Blob{}, fmt.Errorf("payload path is required")
	}
	info, err := os.Stat(filePath)
	if err != nil {
		return Blob{}, fmt.Errorf("stat payload %s: %w", filePath, err)
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 {
		return Blob{}, fmt.Errorf("payload %s must be a non-empty regular file", filePath)
	}
	data, err := os.ReadFile(filePath)
	if err != nil {
		return Blob{}, fmt.Errorf("read payload %s: %w", filePath, err)
	}
	if strings.TrimSpace(fileName) == "" {
		fileName = filepath.Base(filePath)
	}
	return DescribeBytes(data, role, mediaType, fileName), nil
}

func DescribeBytes(data []byte, role, mediaType, fileName string) Blob {
	return Blob{
		Descriptor: Descriptor{
			Role:      role,
			MediaType: mediaType,
			Digest:    digest.FromBytes(data).String(),
			SizeBytes: int64(len(data)),
			FileName:  fileName,
			Annotations: map[string]string{
				ocispec.AnnotationTitle: "payload/" + fileName,
				"dev.katl.bundle.role":  role,
			},
		},
		Data: slices.Clone(data),
	}
}

// Publish packs and immutably publishes the common OCI payload-bundle
// envelope. Kubernetes and generic system extensions supply different config
// schemas, while sharing byte identity, manifest layout, authentication and
// immutable-tag behavior here.
func Publish(ctx context.Context, request PublishRequest) (Published, error) {
	ref, err := ParseReference(request.Reference)
	if err != nil {
		return Published{}, err
	}
	if ref.ManifestDigest != "" {
		return Published{}, fmt.Errorf("publish reference must not include a manifest digest")
	}
	packRequest := PackRequest{
		ArtifactType: request.ArtifactType, ConfigMediaType: request.ConfigMediaType,
		Config: request.Config, Blobs: request.Blobs, Annotations: request.Annotations,
	}
	store, manifestDescriptor, cleanup, err := pack(ctx, packRequest, ref.Tag)
	if err != nil {
		return Published{}, err
	}
	defer cleanup()

	repository, err := remote.NewRepository(ref.Repository)
	if err != nil {
		return Published{}, fmt.Errorf("open OCI repository %s: %w", ref.Repository, err)
	}
	client := request.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Minute}
	}
	authClient := &auth.Client{Client: client, Cache: auth.NewCache()}
	if request.UseDockerCredentials {
		if credentialStore, storeErr := credentials.NewStoreFromDocker(credentials.StoreOptions{}); storeErr == nil {
			authClient.Credential = credentialStore.Get
		}
	}
	repository.Client = authClient

	existing, err := repository.Resolve(ctx, ref.Tag)
	switch {
	case err == nil:
		if existing.Digest != manifestDescriptor.Digest {
			return Published{}, fmt.Errorf("immutable OCI tag %s already resolves to %s, refusing to replace it with %s", ref.Value, existing.Digest, manifestDescriptor.Digest)
		}
		return Published{Reference: ref, ManifestDigest: manifestDescriptor.Digest.String(), Existing: true}, nil
	case !errors.Is(err, errdef.ErrNotFound):
		return Published{}, fmt.Errorf("resolve existing OCI tag %s: %w", ref.Value, err)
	}

	published, err := oras.Copy(ctx, store, ref.Tag, repository, ref.Tag, oras.DefaultCopyOptions)
	if err != nil {
		return Published{}, fmt.Errorf("publish OCI payload bundle %s: %w", ref.Value, err)
	}
	if published.Digest != manifestDescriptor.Digest {
		return Published{}, fmt.Errorf("published OCI manifest digest %s does not match local digest %s", published.Digest, manifestDescriptor.Digest)
	}
	resolved, err := repository.Resolve(ctx, ref.Tag)
	if err != nil {
		return Published{}, fmt.Errorf("verify published OCI tag %s: %w", ref.Value, err)
	}
	if resolved.Digest != manifestDescriptor.Digest {
		return Published{}, fmt.Errorf("published OCI tag %s resolves to %s, want %s", ref.Value, resolved.Digest, manifestDescriptor.Digest)
	}
	return Published{Reference: ref, ManifestDigest: manifestDescriptor.Digest.String()}, nil
}

func pack(ctx context.Context, request PackRequest, tag string) (store oras.Target, manifestDescriptor ocispec.Descriptor, cleanup func(), err error) {
	if strings.TrimSpace(request.ArtifactType) == "" || strings.TrimSpace(request.ConfigMediaType) == "" {
		return nil, ocispec.Descriptor{}, nil, fmt.Errorf("OCI artifact and config media types are required")
	}
	if len(request.Config) == 0 || len(request.Blobs) == 0 {
		return nil, ocispec.Descriptor{}, nil, fmt.Errorf("OCI config and at least one payload blob are required")
	}

	storeDir, err := os.MkdirTemp("", "katl-payload-bundle-*")
	if err != nil {
		return nil, ocispec.Descriptor{}, nil, fmt.Errorf("create temporary OCI payload store: %w", err)
	}
	cleanup = func() {
		_ = os.RemoveAll(storeDir)
	}
	defer func() {
		if err != nil {
			cleanup()
			cleanup = nil
		}
	}()
	store, err = oci.New(storeDir)
	if err != nil {
		return nil, ocispec.Descriptor{}, cleanup, fmt.Errorf("open temporary OCI payload store: %w", err)
	}
	configDescriptor, err := oras.PushBytes(ctx, store, request.ConfigMediaType, request.Config)
	if err != nil {
		return nil, ocispec.Descriptor{}, cleanup, fmt.Errorf("stage OCI config: %w", err)
	}
	layers := make([]ocispec.Descriptor, 0, len(request.Blobs))
	for _, blob := range request.Blobs {
		if digest.FromBytes(blob.Data).String() != blob.Descriptor.Digest || int64(len(blob.Data)) != blob.Descriptor.SizeBytes {
			return nil, ocispec.Descriptor{}, cleanup, fmt.Errorf("payload %q bytes do not match its descriptor", blob.Descriptor.FileName)
		}
		layer, err := oras.PushBytes(ctx, store, blob.Descriptor.MediaType, blob.Data)
		if err != nil {
			return nil, ocispec.Descriptor{}, cleanup, fmt.Errorf("stage OCI payload %q: %w", blob.Descriptor.FileName, err)
		}
		layer.Annotations = cloneAnnotations(blob.Descriptor.Annotations)
		layers = append(layers, layer)
	}
	manifestDescriptor, err = oras.PackManifest(ctx, store, oras.PackManifestVersion1_1, request.ArtifactType, oras.PackManifestOptions{
		ConfigDescriptor:    &configDescriptor,
		Layers:              layers,
		ManifestAnnotations: cloneAnnotations(request.Annotations),
	})
	if err != nil {
		return nil, ocispec.Descriptor{}, cleanup, fmt.Errorf("pack OCI payload bundle: %w", err)
	}
	if err := store.Tag(ctx, manifestDescriptor, tag); err != nil {
		return nil, ocispec.Descriptor{}, cleanup, fmt.Errorf("tag staged OCI payload bundle: %w", err)
	}
	ref := Reference{Value: tag, Tag: tag}
	local, err := FetchTarget(ctx, store, tag, ref, request.ArtifactType, request.ConfigMediaType)
	if err != nil {
		return nil, ocispec.Descriptor{}, cleanup, fmt.Errorf("verify staged OCI payload bundle: %w", err)
	}
	descriptors := make([]Descriptor, 0, len(request.Blobs))
	for _, blob := range request.Blobs {
		descriptors = append(descriptors, blob.Descriptor)
	}
	if err := VerifyDescriptors(local.Manifest, descriptors); err != nil {
		return nil, ocispec.Descriptor{}, cleanup, fmt.Errorf("verify staged OCI payload descriptors: %w", err)
	}
	return store, manifestDescriptor, cleanup, nil
}

func Fetch(ctx context.Context, request FetchRequest) (Fetched, error) {
	ref, err := ParseReference(request.Reference)
	if err != nil {
		return Fetched{}, err
	}
	if strings.TrimSpace(request.ArtifactType) == "" || strings.TrimSpace(request.ConfigMediaType) == "" {
		return Fetched{}, fmt.Errorf("OCI artifact and config media types are required")
	}
	repository, err := remote.NewRepository(ref.Repository)
	if err != nil {
		return Fetched{}, fmt.Errorf("open OCI repository %s: %w", ref.Repository, err)
	}
	client := request.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Minute}
	}
	authClient := &auth.Client{Client: client, Cache: auth.NewCache()}
	if request.UseDockerCredentials {
		if store, storeErr := credentials.NewStoreFromDocker(credentials.StoreOptions{}); storeErr == nil {
			authClient.Credential = store.Get
		}
	}
	repository.Client = authClient
	identifier := ref.Tag
	if ref.ManifestDigest != "" {
		identifier = ref.ManifestDigest
	}
	return FetchTarget(ctx, repository, identifier, ref, request.ArtifactType, request.ConfigMediaType)
}

func cloneAnnotations(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

func FetchTarget(ctx context.Context, target Target, identifier string, ref Reference, artifactType, configMediaType string) (Fetched, error) {
	fetched, err := FetchTargetManifest(ctx, target, identifier, ref, artifactType, configMediaType)
	if err != nil {
		return Fetched{}, err
	}
	for _, layer := range fetched.Manifest.Layers {
		data, err := ReadContent(ctx, target, layer)
		if err != nil {
			return Fetched{}, fmt.Errorf("fetch OCI layer %s: %w", layer.Digest, err)
		}
		fetched.Layers[layer.Digest.String()] = data
	}
	return fetched, nil
}

// FetchTargetManifest resolves and verifies the OCI manifest and custom config
// without buffering payload layers. Consumers with large artifacts, including
// Kubernetes, can then stream exact descriptors with CopyContent.
func FetchTargetManifest(ctx context.Context, target Target, identifier string, ref Reference, artifactType, configMediaType string) (Fetched, error) {
	manifestDescriptor, err := target.Resolve(ctx, identifier)
	if err != nil {
		return Fetched{}, fmt.Errorf("resolve OCI reference %s: %w", ref.Value, err)
	}
	if ref.ManifestDigest != "" && manifestDescriptor.Digest.String() != ref.ManifestDigest {
		return Fetched{}, fmt.Errorf("resolved OCI manifest digest %s does not match reference %s", manifestDescriptor.Digest, ref.ManifestDigest)
	}
	manifestBytes, err := content.FetchAll(ctx, target, manifestDescriptor)
	if err != nil {
		return Fetched{}, fmt.Errorf("fetch OCI manifest %s: %w", manifestDescriptor.Digest, err)
	}
	var manifest ocispec.Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return Fetched{}, fmt.Errorf("decode OCI manifest: %w", err)
	}
	if manifest.SchemaVersion != 2 || manifest.MediaType != ocispec.MediaTypeImageManifest || manifest.ArtifactType != artifactType {
		return Fetched{}, fmt.Errorf("OCI manifest identity does not match artifact type %s", artifactType)
	}
	if manifest.Config.MediaType != configMediaType {
		return Fetched{}, fmt.Errorf("OCI config media type %q does not match %q", manifest.Config.MediaType, configMediaType)
	}
	configBytes, err := ReadContent(ctx, target, manifest.Config)
	if err != nil {
		return Fetched{}, fmt.Errorf("fetch OCI config %s: %w", manifest.Config.Digest, err)
	}
	return Fetched{
		Reference:      ref,
		ManifestDigest: manifestDescriptor.Digest.String(),
		Manifest:       manifest,
		Config:         configBytes,
		Layers:         make(map[string][]byte, len(manifest.Layers)),
	}, nil
}

// VerifyDescriptors proves that the OCI layer set and the custom-manifest
// descriptor set describe the exact same content. Role semantics remain with
// the Kubernetes or system-extension policy layer.
func VerifyDescriptors(manifest ocispec.Manifest, descriptors []Descriptor) error {
	if len(manifest.Layers) != len(descriptors) {
		return fmt.Errorf("OCI manifest has %d layers, custom manifest declares %d", len(manifest.Layers), len(descriptors))
	}
	seenDigests := make(map[string]struct{}, len(descriptors))
	for _, descriptor := range descriptors {
		if strings.TrimSpace(descriptor.Role) == "" || strings.TrimSpace(descriptor.MediaType) == "" || strings.TrimSpace(descriptor.FileName) == "" {
			return fmt.Errorf("custom-manifest descriptor role, mediaType, and fileName are required")
		}
		if !validDigest(descriptor.Digest) || descriptor.SizeBytes <= 0 {
			return fmt.Errorf("custom-manifest descriptor %q has invalid digest or size", descriptor.Role)
		}
		if _, ok := seenDigests[descriptor.Digest]; ok {
			return fmt.Errorf("custom-manifest descriptor digest %q is duplicated", descriptor.Digest)
		}
		seenDigests[descriptor.Digest] = struct{}{}
		matches := 0
		for _, layer := range manifest.Layers {
			if layer.Digest.String() != descriptor.Digest {
				continue
			}
			matches++
			if layer.MediaType != descriptor.MediaType || layer.Size != descriptor.SizeBytes {
				return fmt.Errorf("OCI layer does not match custom-manifest descriptor %q", descriptor.Role)
			}
		}
		if matches != 1 {
			return fmt.Errorf("OCI manifest has %d layers for custom-manifest descriptor %q", matches, descriptor.Role)
		}
	}
	return nil
}

func Content(fetched Fetched, descriptor Descriptor) ([]byte, error) {
	data, ok := fetched.Layers[descriptor.Digest]
	if !ok {
		return nil, fmt.Errorf("OCI layer %s for %s is missing", descriptor.Digest, descriptor.Role)
	}
	if int64(len(data)) != descriptor.SizeBytes {
		return nil, fmt.Errorf("OCI layer for %s has size %d, want %d", descriptor.Role, len(data), descriptor.SizeBytes)
	}
	got := digest.FromBytes(data).String()
	if got != descriptor.Digest {
		return nil, fmt.Errorf("OCI layer for %s has digest %s, want %s", descriptor.Role, got, descriptor.Digest)
	}
	return append([]byte(nil), data...), nil
}

type fetcher interface {
	Fetch(context.Context, ocispec.Descriptor) (io.ReadCloser, error)
}

func ReadContent(ctx context.Context, source fetcher, descriptor ocispec.Descriptor) ([]byte, error) {
	var out strings.Builder
	out.Grow(int(descriptor.Size))
	if _, err := CopyContent(ctx, source, descriptor, &out); err != nil {
		return nil, err
	}
	return []byte(out.String()), nil
}

func CopyContent(ctx context.Context, source fetcher, descriptor ocispec.Descriptor, destination io.Writer) (int64, error) {
	reader, err := source.Fetch(ctx, descriptor)
	if err != nil {
		return 0, err
	}
	defer reader.Close()
	verified := content.NewVerifyReader(reader, descriptor)
	written, err := io.Copy(destination, verified)
	if err != nil {
		return written, err
	}
	if err := verified.Verify(); err != nil {
		return written, err
	}
	if written != descriptor.Size {
		return written, fmt.Errorf("OCI content size got %d want %d", written, descriptor.Size)
	}
	return written, nil
}

func validDigest(value string) bool {
	parsed, err := digest.Parse(value)
	if err != nil || parsed.Algorithm() != digest.SHA256 {
		return false
	}
	encoded := parsed.Encoded()
	if len(encoded) != 64 || encoded != strings.ToLower(encoded) {
		return false
	}
	_, err = hex.DecodeString(encoded)
	return err == nil
}
