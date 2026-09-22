package configapply

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/katl-dev/katl/internal/extensionrelease"
	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/installer/systemextensionbundle"
	"gopkg.in/yaml.v3"
)

type PreparedNodeConfiguration struct {
	Request  TrustedBundleRequest
	Document []byte
}

// PrepareNodeConfigurationChange resolves against the operation's target before
// any generation mutation. Document freezes verified inputs for execution;
// replay does not resolve mutable references a second time.
func PrepareNodeConfigurationChange(ctx context.Context, input string, base TrustedBundleRequest, fetch func(context.Context, systemextensionbundle.ResolveRequest) (systemextensionbundle.Resolved, error)) (PreparedNodeConfiguration, error) {
	document, err := decodeNodeConfigurationChange(strings.NewReader(input))
	if err != nil {
		return PreparedNodeConfiguration{}, err
	}
	freeze := document.Spec.SystemExtensionSelections != nil
	if selections := document.Spec.SystemExtensionSelections; selections != nil {
		if base.NodeName == "" {
			return PreparedNodeConfiguration{}, fmt.Errorf("node name is required to resolve system extension selections")
		}
		if document.Spec.ClusterDefaults.SystemExtensions != nil || len(document.Spec.SystemExtensionPayloads) != 0 {
			return PreparedNodeConfiguration{}, fmt.Errorf("system extension selections cannot be combined with resolved extensions or payloads")
		}
		for _, overlays := range []map[string]nodeConfigurationOverlay{document.Spec.SystemRoleOverrides, document.Spec.NodeOverrides} {
			for _, overlay := range overlays {
				if overlay.SystemExtensions != nil {
					return PreparedNodeConfiguration{}, fmt.Errorf("system extension selections cannot be combined with resolved extensions")
				}
			}
		}
		desired, materials, err := resolveExtensionSelections(ctx, base, *selections, fetch)
		if err != nil {
			return PreparedNodeConfiguration{}, err
		}
		if document.Spec.NodeOverrides == nil {
			document.Spec.NodeOverrides = make(map[string]nodeConfigurationOverlay)
		}
		overlay := document.Spec.NodeOverrides[base.NodeName]
		overlay.SystemExtensions = &desired
		document.Spec.NodeOverrides[base.NodeName] = overlay
		document.Spec.SystemExtensionPayloads = materials
		document.Spec.SystemExtensionSelections = nil
	}
	request, err := document.request(base)
	if err != nil {
		return PreparedNodeConfiguration{}, err
	}
	desired, err := DesiredManifest(request)
	if err != nil {
		return PreparedNodeConfiguration{}, err
	}
	materials, err := completeExtensionMaterials(base, desired.Node.SystemExtensions, request.SystemExtensionPayloads)
	if err != nil {
		return PreparedNodeConfiguration{}, err
	}
	if err := ValidateSystemExtensionMaterials(base.CurrentRecord.Root, base.CurrentRecord.ExtensionRelease, desired.Node.SystemExtensions, materials); err != nil {
		return PreparedNodeConfiguration{}, err
	}
	request.SystemExtensionPayloads = materials
	freeze = freeze || len(materials) != len(document.Spec.SystemExtensionPayloads)
	document.Spec.SystemExtensionPayloads = materials
	data := []byte(input)
	if freeze {
		data, err = yaml.Marshal(document)
		if err != nil {
			return PreparedNodeConfiguration{}, err
		}
	}
	return PreparedNodeConfiguration{
		Request:  request,
		Document: data,
	}, nil
}

func resolveExtensionSelections(ctx context.Context, base TrustedBundleRequest, selections []systemextensionbundle.Selection, fetch func(context.Context, systemextensionbundle.ResolveRequest) (systemextensionbundle.Resolved, error)) ([]manifest.SystemExtension, []SystemExtensionPayload, error) {
	desired := make([]manifest.SystemExtension, 0, len(selections))
	for _, selection := range selections {
		desired = append(desired, manifest.SystemExtension{
			Release:       selection.Release,
			Bundle:        selection.Bundle,
			State:         selection.State,
			Configuration: selection.Configuration,
			Units:         selection.Units,
		})
	}
	if err := manifest.ValidateSystemExtensions(desired, true); err != nil {
		return nil, nil, err
	}
	release := base.CurrentRecord.ExtensionRelease
	target := extensionrelease.Target{
		Architecture:     base.CurrentRecord.Root.Architecture,
		RuntimeInterface: base.CurrentRecord.Root.RuntimeInterface,
	}
	target.Kernel.RuntimeSHA256 = base.CurrentRecord.Root.RuntimeArtifactSHA256
	if release != nil {
		target = release.Target
	}
	var materials []SystemExtensionPayload
	for i, selection := range desired {
		reused, ok, err := systemextensionbundle.ReuseSelection(target, release, selection, base.CurrentManifest.Node.SystemExtensions)
		if err != nil {
			return nil, nil, err
		}
		if ok {
			desired[i] = reused
			continue
		}
		resolved, bundle, err := systemextensionbundle.ResolveSelection(ctx, target, release, selection, fetch)
		if err != nil {
			return nil, nil, err
		}
		desired[i] = resolved
		for _, content := range bundle.Payloads {
			materials = append(materials, SystemExtensionPayload{
				Ref: manifest.SystemExtensionPayloadRef{
					Name:      content.Descriptor.FileName,
					Role:      content.Descriptor.Role,
					MediaType: content.Descriptor.MediaType,
					Digest:    content.Descriptor.Digest,
					SizeBytes: content.Descriptor.SizeBytes,
				},
				Data: content.Data,
			})
		}
	}
	desired = slices.DeleteFunc(desired, func(extension manifest.SystemExtension) bool {
		return extension.State == manifest.SystemExtensionAbsent
	})
	return desired, materials, nil
}

func completeExtensionMaterials(base TrustedBundleRequest, desired []manifest.SystemExtension, supplied []SystemExtensionPayload) ([]SystemExtensionPayload, error) {
	materials := slices.Clone(supplied)
	for _, extension := range desired {
		for _, payload := range extension.Payloads {
			if slices.ContainsFunc(materials, func(material SystemExtensionPayload) bool { return material.Ref == payload }) {
				continue
			}
			old := slices.IndexFunc(base.CurrentManifest.Node.SystemExtensions, func(old manifest.SystemExtension) bool {
				return old.Repository() == extension.Repository() && old.OCIManifestDigest == extension.OCIManifestDigest && slices.Contains(old.Payloads, payload)
			})
			if old < 0 {
				return nil, fmt.Errorf("system extension %q payload %q is neither supplied nor retained", extension.Repository(), payload.Name)
			}
			refs := base.CurrentRecord.Sysexts
			if payload.Role == systemextensionbundle.ConfextRole {
				refs = base.CurrentRecord.BundledConfexts
			}
			index := slices.IndexFunc(refs, func(ref generation.ExtensionRef) bool {
				return ref.Name == extension.PayloadID(payload.Name) && "sha256:"+ref.SHA256 == payload.Digest
			})
			if index < 0 {
				return nil, fmt.Errorf("retained system extension %q payload %q is missing from the generation", extension.Repository(), payload.Name)
			}
			root, err := os.OpenRoot(base.Root)
			if err != nil {
				return nil, err
			}
			data, err := root.ReadFile(strings.TrimPrefix(refs[index].Path, "/"))
			_ = root.Close()
			if err != nil {
				return nil, fmt.Errorf("read retained extension %q: %w", extension.Repository(), err)
			}
			if int64(len(data)) != payload.SizeBytes || digestPayload(data) != payload.Digest {
				return nil, fmt.Errorf("retained extension %q payload %q failed digest or size verification", extension.Repository(), payload.Name)
			}
			materials = append(materials, SystemExtensionPayload{
				Ref:  payload,
				Data: data,
			})
		}
	}
	return materials, nil
}
