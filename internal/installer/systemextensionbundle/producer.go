package systemextensionbundle

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/installer/payloadbundle"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type Input struct {
	Path      string
	Role      string
	MediaType string
	FileName  string
}

type BuildRequest struct {
	Name                       string
	ArtifactVersion            string
	PayloadVersion             string
	Architecture               string
	SupportedRuntimeInterfaces []string
	CreatedAt                  time.Time
	Payloads                   []Input
	Metadata                   []Input
}

type Built struct {
	Bundle       Bundle
	Manifest     []byte
	ManifestHash string
	Blobs        []payloadbundle.Blob
}

type PublishRequest struct {
	Reference   string
	Build       BuildRequest
	Annotations map[string]string
}

func Pack(ctx context.Context, request BuildRequest, annotations map[string]string) (payloadbundle.Packed, Built, error) {
	built, err := Build(request)
	if err != nil {
		return payloadbundle.Packed{}, Built{}, err
	}
	packed, err := payloadbundle.Pack(ctx, payloadbundle.PackRequest{
		ArtifactType: ArtifactType, ConfigMediaType: ConfigMediaType,
		Config: built.Manifest, Blobs: built.Blobs,
		Annotations: bundleAnnotations(built, annotations),
	})
	if err != nil {
		return payloadbundle.Packed{}, Built{}, err
	}
	return packed, built, nil
}

func Build(request BuildRequest) (Built, error) {
	createdAt := request.CreatedAt.UTC()
	if createdAt.IsZero() {
		return Built{}, fmt.Errorf("createdAt is required")
	}
	runtimeInterfaces := normalizeStrings(request.SupportedRuntimeInterfaces)
	bundle := Bundle{
		APIVersion:                 APIVersion,
		Kind:                       Kind,
		Name:                       strings.TrimSpace(request.Name),
		ArtifactKind:               ArtifactKind,
		ArtifactVersion:            strings.TrimSpace(request.ArtifactVersion),
		PayloadVersion:             strings.TrimSpace(request.PayloadVersion),
		Architecture:               strings.TrimSpace(request.Architecture),
		SupportedRuntimeInterfaces: runtimeInterfaces,
		CreatedAt:                  createdAt.Format(time.RFC3339),
	}
	blobs := make([]payloadbundle.Blob, 0, len(request.Payloads)+len(request.Metadata))
	layers := make([]ocispec.Descriptor, 0, cap(blobs))
	for _, input := range request.Payloads {
		blob, err := describeInput(input, true)
		if err != nil {
			return Built{}, err
		}
		bundle.Payloads = append(bundle.Payloads, blob.Descriptor)
		blobs = append(blobs, blob)
		layers = append(layers, ociDescriptor(blob.Descriptor))
	}
	for _, input := range request.Metadata {
		blob, err := describeInput(input, false)
		if err != nil {
			return Built{}, err
		}
		bundle.Metadata = append(bundle.Metadata, blob.Descriptor)
		blobs = append(blobs, blob)
		layers = append(layers, ociDescriptor(blob.Descriptor))
	}
	if err := validateBundle(bundle, ocispec.Manifest{Layers: layers}, ResolveRequest{}); err != nil {
		return Built{}, err
	}
	manifestBytes, err := json.Marshal(bundle)
	if err != nil {
		return Built{}, fmt.Errorf("encode system extension bundle manifest: %w", err)
	}
	return Built{
		Bundle:       bundle,
		Manifest:     manifestBytes,
		ManifestHash: digestBytes(manifestBytes),
		Blobs:        blobs,
	}, nil
}

func Publish(ctx context.Context, request PublishRequest) (payloadbundle.Published, Built, error) {
	built, err := Build(request.Build)
	if err != nil {
		return payloadbundle.Published{}, Built{}, err
	}
	annotations := bundleAnnotations(built, request.Annotations)
	published, err := payloadbundle.Publish(ctx, payloadbundle.PublishRequest{
		Reference:            request.Reference,
		ArtifactType:         ArtifactType,
		ConfigMediaType:      ConfigMediaType,
		Config:               built.Manifest,
		Blobs:                built.Blobs,
		Annotations:          annotations,
		UseDockerCredentials: true,
	})
	if err != nil {
		return payloadbundle.Published{}, Built{}, err
	}
	return published, built, nil
}

func bundleAnnotations(built Built, supplied map[string]string) map[string]string {
	annotations := make(map[string]string, len(supplied)+4)
	for key, value := range supplied {
		annotations[key] = value
	}
	annotations[ocispec.AnnotationCreated] = built.Bundle.CreatedAt
	annotations[ocispec.AnnotationVersion] = built.Bundle.ArtifactVersion
	annotations["dev.katl.bundle.kind"] = "system-extension"
	annotations["dev.katl.system-extension.name"] = built.Bundle.Name
	return annotations
}

func describeInput(input Input, payload bool) (payloadbundle.Blob, error) {
	role := strings.TrimSpace(input.Role)
	mediaType := strings.TrimSpace(input.MediaType)
	if payload {
		switch role {
		case SysextRole:
			if mediaType == "" {
				mediaType = SysextMediaType
			}
		case ConfextRole:
			if mediaType == "" {
				mediaType = ConfextMediaType
			}
		default:
			return payloadbundle.Blob{}, fmt.Errorf("unsupported system extension payload role %q", role)
		}
	} else if role == "" || mediaType == "" {
		return payloadbundle.Blob{}, fmt.Errorf("metadata role and media type are required")
	}
	return payloadbundle.DescribeFile(input.Path, role, mediaType, input.FileName)
}

func ociDescriptor(descriptor Descriptor) ocispec.Descriptor {
	return ocispec.Descriptor{
		MediaType:   descriptor.MediaType,
		Digest:      mustDigest(descriptor.Digest),
		Size:        descriptor.SizeBytes,
		Annotations: descriptor.Annotations,
	}
}

func normalizeStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
