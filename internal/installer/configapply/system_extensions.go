package configapply

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/katl-dev/katl/internal/extensionrelease"
	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/installer/payloadbundle"
)

func ValidateSystemExtensionMaterials(root generation.RootSelection, release *extensionrelease.Manifest, desired []manifest.SystemExtension, materials []SystemExtensionPayload) error {
	if err := manifest.ValidateSystemExtensions(desired, false); err != nil {
		return err
	}
	available := make(map[string]SystemExtensionPayload, len(materials))
	for _, material := range materials {
		if previous, ok := available[material.Ref.Digest]; ok {
			if previous.Ref != material.Ref || string(previous.Data) != string(material.Data) {
				return fmt.Errorf("system extension payload %s is duplicated with different content", material.Ref.Digest)
			}
			continue
		}
		if int64(len(material.Data)) != material.Ref.SizeBytes || digestPayload(material.Data) != material.Ref.Digest {
			return fmt.Errorf("system extension payload %s failed digest or size verification", material.Ref.Digest)
		}
		available[material.Ref.Digest] = material
	}
	wanted := make(map[string]struct{})
	activationNames := make(map[string]string)
	for _, extension := range desired {
		if extension.Release != "" {
			if release == nil || extension.ReleaseTarget == nil {
				return fmt.Errorf("system extension %q requires the target runtime's release manifest", extension.Repository())
			}
			ref, err := release.Resolve(*extension.ReleaseTarget, extension.Release)
			if err != nil {
				return err
			}
			parsed, err := payloadbundle.ParseReference(ref)
			if err != nil {
				return err
			}
			if extension.ResolvedBundle != parsed.Name()+"@"+payloadbundle.ManifestDigest(parsed) {
				return fmt.Errorf("system extension %q resolved artifact is not the target release's selection", extension.Repository())
			}
		}
		if extension.ReleaseTarget != nil {
			if err := extension.ReleaseTarget.ValidateRuntime(root.RuntimeVersion, root.Architecture, root.Flavour, root.RuntimeInterface, root.RuntimeArtifactSHA256); err != nil {
				return fmt.Errorf("system extension %q: %w", extension.Repository(), err)
			}
		}
		if extension.Kernel != nil {
			if err := extension.Kernel.ValidateRuntime(root.RuntimeArtifactSHA256); err != nil {
				return fmt.Errorf("system extension %q: %w", extension.Repository(), err)
			}
		}
		if extension.Architecture != root.Architecture {
			return fmt.Errorf("system extension %q architecture %q is incompatible with runtime architecture %q", extension.Repository(), extension.Architecture, root.Architecture)
		}
		if !containsString(extension.SupportedRuntimeInterfaces, root.RuntimeInterface) {
			return fmt.Errorf("system extension %q does not support runtime interface %q", extension.Repository(), root.RuntimeInterface)
		}
		for _, payload := range extension.Payloads {
			if _, ok := available[payload.Digest]; !ok {
				return fmt.Errorf("system extension %q payload %s is missing from the trusted config bundle", extension.Repository(), payload.Digest)
			}
			wanted[payload.Digest] = struct{}{}
			key := payload.Role + "\x00" + payload.Name
			if owner, ok := activationNames[key]; ok {
				return fmt.Errorf("system extension payload activation name %q is owned by both %q and %q", payload.Name, owner, extension.Repository())
			}
			activationNames[key] = extension.Repository()
			if payload.Role == "systemd-confext" {
				imageName := strings.TrimSuffix(payload.Name, ".raw")
				if imageName >= generation.GeneratedConfextName {
					return fmt.Errorf("system extension %q confext image name %q must sort before Katl's generated configuration layer %q", extension.Repository(), imageName, generation.GeneratedConfextName)
				}
			}
		}
	}
	for digest := range available {
		if _, ok := wanted[digest]; !ok {
			return fmt.Errorf("trusted config bundle carries unselected system extension payload %s", digest)
		}
	}
	return nil
}

func MaterializeSystemExtensions(root, generationID string, runtime generation.RootSelection, release *extensionrelease.Manifest, desired []manifest.SystemExtension, materials []SystemExtensionPayload) ([]generation.ExtensionRef, []generation.ExtensionRef, error) {
	sysexts, confexts, err := PlanSystemExtensions(generationID, runtime, release, desired, materials)
	if err != nil {
		return nil, nil, err
	}
	available := make(map[string][]byte, len(materials))
	for _, material := range materials {
		available[strings.TrimPrefix(material.Ref.Digest, "sha256:")] = material.Data
	}
	for _, refs := range [][]generation.ExtensionRef{sysexts, confexts} {
		for _, ref := range refs {
			target := filepath.Join(filepath.Clean(root), strings.TrimPrefix(ref.Path, "/"))
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return nil, nil, fmt.Errorf("create system extension payload directory: %w", err)
			}
			if err := os.WriteFile(target, available[ref.SHA256], 0o644); err != nil {
				return nil, nil, fmt.Errorf("materialize system extension %q: %w", ref.Name, err)
			}
		}
	}
	return sysexts, confexts, nil
}

// PlanSystemExtensions verifies the complete selection before a caller mutates
// inactive runtime slots or generation assets.
func PlanSystemExtensions(generationID string, runtime generation.RootSelection, release *extensionrelease.Manifest, desired []manifest.SystemExtension, materials []SystemExtensionPayload) ([]generation.ExtensionRef, []generation.ExtensionRef, error) {
	if generationID == "" || filepath.Base(generationID) != generationID || generationID == "." || generationID == ".." {
		return nil, nil, fmt.Errorf("invalid system extension generation ID")
	}
	if err := ValidateSystemExtensionMaterials(runtime, release, desired, materials); err != nil {
		return nil, nil, err
	}
	var sysexts []generation.ExtensionRef
	var confexts []generation.ExtensionRef
	for _, extension := range desired {
		for _, payload := range extension.Payloads {
			dir := "sysext"
			activationRoot := generation.DefaultExtensionsActivationDir
			switch payload.Role {
			case "systemd-sysext":
			case "systemd-confext":
				dir = "bundled-confext"
				activationRoot = generation.DefaultConfextsActivationDir
			default:
				return nil, nil, fmt.Errorf("system extension %q payload %q has unsupported role %q", extension.Repository(), payload.Name, payload.Role)
			}
			runtimePath := filepath.ToSlash(filepath.Join(generation.GenerationRecordsDir, generationID, dir, payload.Name))
			ref := generation.ExtensionRef{
				Name:            extension.PayloadID(payload.Name),
				Path:            runtimePath,
				ActivationPath:  activationRoot + "/" + payload.Name,
				SHA256:          strings.TrimPrefix(payload.Digest, "sha256:"),
				ArtifactVersion: extension.ArtifactVersion,
				PayloadVersion:  extension.PayloadVersion,
				Architecture:    extension.Architecture,
				Compatibility: generation.ExtensionCompatibility{
					Kernel:            extension.Kernel,
					RuntimeInterfaces: append([]string(nil), extension.SupportedRuntimeInterfaces...),
				},
			}
			if payload.Role == "systemd-sysext" {
				sysexts = append(sysexts, ref)
			} else {
				confexts = append(confexts, ref)
			}
		}
	}
	sort.Slice(sysexts, func(i, j int) bool { return sysexts[i].ActivationPath < sysexts[j].ActivationPath })
	sort.Slice(confexts, func(i, j int) bool { return confexts[i].ActivationPath < confexts[j].ActivationPath })
	return sysexts, confexts, nil
}

func digestPayload(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) == strings.TrimSpace(want) {
			return true
		}
	}
	return false
}
