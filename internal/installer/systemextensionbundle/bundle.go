package systemextensionbundle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/installer/payloadbundle"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	APIVersion = "payload.katl.dev/v1alpha1"
	Kind       = "SystemExtensionBundle"

	ArtifactKind        = "katl.system-extension.v1"
	ArtifactType        = "application/vnd.katl.system-extension.bundle.v1"
	ConfigMediaType     = "application/vnd.katl.system-extension.bundle.v1+json"
	SysextMediaType     = "application/vnd.katl.sysext.raw.v1"
	ConfextMediaType    = "application/vnd.katl.confext.raw.v1"
	SysextRole          = "systemd-sysext"
	ConfextRole         = "systemd-confext"
	maxPayloadSize      = int64(1 << 30)
	maxBundleLayerCount = 64
)

var ErrInvalidBundle = errors.New("invalid system extension bundle")

type Bundle struct {
	APIVersion                 string       `json:"apiVersion"`
	Kind                       string       `json:"kind"`
	Name                       string       `json:"name"`
	ArtifactKind               string       `json:"artifactKind"`
	ArtifactVersion            string       `json:"artifactVersion"`
	PayloadVersion             string       `json:"payloadVersion"`
	Architecture               string       `json:"architecture"`
	Payloads                   []Descriptor `json:"payloads"`
	Metadata                   []Descriptor `json:"metadata,omitempty"`
	SupportedRuntimeInterfaces []string     `json:"supportedRuntimeInterfaces"`
	CreatedAt                  string       `json:"createdAt"`
	Signatures                 []Signature  `json:"signatures,omitempty"`
}

type Descriptor = payloadbundle.Descriptor

type Signature struct {
	Type   string `json:"type"`
	Reason string `json:"reason,omitempty"`
}

type ResolveRequest struct {
	Reference        string
	Architecture     string
	RuntimeInterface string
}

type Resolved struct {
	Reference            string
	OCIManifestDigest    string
	BundleManifestDigest string
	BundleManifest       []byte
	Bundle               Bundle
	Payloads             []Payload
	Metadata             []Payload
}

type Payload struct {
	Descriptor Descriptor
	Data       []byte
}

func Resolve(ctx context.Context, request ResolveRequest) (Resolved, error) {
	fetched, err := payloadbundle.Fetch(ctx, payloadbundle.FetchRequest{
		Reference:            request.Reference,
		ArtifactType:         ArtifactType,
		ConfigMediaType:      ConfigMediaType,
		UseDockerCredentials: true,
	})
	if err != nil {
		return Resolved{}, fmt.Errorf("resolve system extension bundle %q: %w", request.Reference, err)
	}
	var bundle Bundle
	if err := json.Unmarshal(fetched.Config, &bundle); err != nil {
		return Resolved{}, fmt.Errorf("%w: decode custom manifest: %v", ErrInvalidBundle, err)
	}
	if err := validateBundle(bundle, fetched.Manifest, request); err != nil {
		return Resolved{}, err
	}
	payloads, err := selectContent(bundle.Payloads, fetched)
	if err != nil {
		return Resolved{}, err
	}
	metadata, err := selectContent(bundle.Metadata, fetched)
	if err != nil {
		return Resolved{}, err
	}
	return Resolved{
		Reference:            request.Reference,
		OCIManifestDigest:    fetched.ManifestDigest,
		BundleManifestDigest: digestBytes(fetched.Config),
		BundleManifest:       append([]byte(nil), fetched.Config...),
		Bundle:               bundle,
		Payloads:             payloads,
		Metadata:             metadata,
	}, nil
}

func (resolved Resolved) Desired(source manifest.SystemExtension) manifest.SystemExtension {
	payloads := make([]manifest.SystemExtensionPayloadRef, 0, len(resolved.Payloads))
	for _, payload := range resolved.Payloads {
		payloads = append(payloads, manifest.SystemExtensionPayloadRef{
			Name:      payload.Descriptor.FileName,
			Role:      payload.Descriptor.Role,
			MediaType: payload.Descriptor.MediaType,
			Digest:    payload.Descriptor.Digest,
			SizeBytes: payload.Descriptor.SizeBytes,
		})
	}
	sort.Slice(payloads, func(i, j int) bool {
		if payloads[i].Role != payloads[j].Role {
			return payloads[i].Role < payloads[j].Role
		}
		return payloads[i].Name < payloads[j].Name
	})
	source.State = manifest.SystemExtensionPresent
	source.OCIManifestDigest = resolved.OCIManifestDigest
	source.BundleManifestDigest = resolved.BundleManifestDigest
	source.ArtifactVersion = resolved.Bundle.ArtifactVersion
	source.PayloadVersion = resolved.Bundle.PayloadVersion
	source.Architecture = resolved.Bundle.Architecture
	source.SupportedRuntimeInterfaces = append([]string(nil), resolved.Bundle.SupportedRuntimeInterfaces...)
	source.Payloads = payloads
	return source
}

func validateBundle(bundle Bundle, ociManifest ocispec.Manifest, request ResolveRequest) error {
	if bundle.APIVersion != APIVersion || bundle.Kind != Kind || bundle.ArtifactKind != ArtifactKind {
		return fmt.Errorf("%w: unexpected custom manifest identity", ErrInvalidBundle)
	}
	if strings.TrimSpace(bundle.Name) == "" || strings.TrimSpace(bundle.ArtifactVersion) == "" || strings.TrimSpace(bundle.PayloadVersion) == "" {
		return fmt.Errorf("%w: name, artifactVersion, and payloadVersion are required", ErrInvalidBundle)
	}
	if strings.TrimSpace(bundle.Architecture) == "" || len(bundle.SupportedRuntimeInterfaces) == 0 {
		return fmt.Errorf("%w: architecture and supportedRuntimeInterfaces are required", ErrInvalidBundle)
	}
	if createdAt, err := time.Parse(time.RFC3339, bundle.CreatedAt); err != nil || createdAt.Format(time.RFC3339) != bundle.CreatedAt {
		return fmt.Errorf("%w: createdAt must be canonical RFC3339", ErrInvalidBundle)
	}
	if request.Architecture != "" && bundle.Architecture != request.Architecture {
		return fmt.Errorf("%w: architecture %q does not match target %q", ErrInvalidBundle, bundle.Architecture, request.Architecture)
	}
	if request.RuntimeInterface != "" && !contains(bundle.SupportedRuntimeInterfaces, request.RuntimeInterface) {
		return fmt.Errorf("%w: runtime interface %q is unsupported", ErrInvalidBundle, request.RuntimeInterface)
	}
	all := append(append([]Descriptor(nil), bundle.Payloads...), bundle.Metadata...)
	if len(all) == 0 || len(all) > maxBundleLayerCount || len(ociManifest.Layers) != len(all) {
		return fmt.Errorf("%w: OCI manifest has %d layers, custom manifest declares %d", ErrInvalidBundle, len(ociManifest.Layers), len(all))
	}
	if err := payloadbundle.VerifyDescriptors(ociManifest, all); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidBundle, err)
	}
	seenNames := make(map[string]struct{}, len(bundle.Payloads))
	sysexts := 0
	for _, descriptor := range bundle.Payloads {
		if err := validateDescriptor(descriptor); err != nil {
			return err
		}
		switch descriptor.Role {
		case SysextRole:
			sysexts++
			if descriptor.MediaType != SysextMediaType {
				return fmt.Errorf("%w: systemd-sysext payload %q has media type %q", ErrInvalidBundle, descriptor.FileName, descriptor.MediaType)
			}
		case ConfextRole:
			if descriptor.MediaType != ConfextMediaType {
				return fmt.Errorf("%w: systemd-confext payload %q has media type %q", ErrInvalidBundle, descriptor.FileName, descriptor.MediaType)
			}
		default:
			return fmt.Errorf("%w: payload %q has unsupported role %q", ErrInvalidBundle, descriptor.FileName, descriptor.Role)
		}
		if _, ok := seenNames[descriptor.FileName]; ok {
			return fmt.Errorf("%w: duplicate payload fileName %q", ErrInvalidBundle, descriptor.FileName)
		}
		seenNames[descriptor.FileName] = struct{}{}
	}
	if sysexts == 0 {
		return fmt.Errorf("%w: at least one systemd-sysext payload is required", ErrInvalidBundle)
	}
	for _, descriptor := range bundle.Metadata {
		if err := validateDescriptor(descriptor); err != nil {
			return err
		}
		if descriptor.Role == SysextRole || descriptor.Role == ConfextRole {
			return fmt.Errorf("%w: metadata descriptor %q uses a payload role", ErrInvalidBundle, descriptor.FileName)
		}
	}
	return nil
}

func validateDescriptor(descriptor Descriptor) error {
	if strings.TrimSpace(descriptor.Role) == "" || strings.TrimSpace(descriptor.MediaType) == "" {
		return fmt.Errorf("%w: descriptor role and mediaType are required", ErrInvalidBundle)
	}
	if !validDigest(descriptor.Digest) || descriptor.SizeBytes <= 0 || descriptor.SizeBytes > maxPayloadSize {
		return fmt.Errorf("%w: descriptor %q has invalid digest or size", ErrInvalidBundle, descriptor.Role)
	}
	name := strings.TrimSpace(descriptor.FileName)
	if name == "" || path.Base(name) != name || name == "." || name == ".." || strings.Contains(name, `\`) {
		return fmt.Errorf("%w: descriptor fileName %q is not a safe basename", ErrInvalidBundle, descriptor.FileName)
	}
	return nil
}

func selectContent(descriptors []Descriptor, fetched payloadbundle.Fetched) ([]Payload, error) {
	out := make([]Payload, 0, len(descriptors))
	for _, descriptor := range descriptors {
		data, err := payloadbundle.Content(fetched, descriptor)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidBundle, err)
		}
		out = append(out, Payload{Descriptor: descriptor, Data: append([]byte(nil), data...)})
	}
	return out, nil
}

func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func validDigest(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) != 71 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil && value == strings.ToLower(value)
}

func mustDigest(value string) digest.Digest {
	parsed, err := digest.Parse(value)
	if err != nil {
		panic(err)
	}
	return parsed
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) == strings.TrimSpace(want) {
			return true
		}
	}
	return false
}
