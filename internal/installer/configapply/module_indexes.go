package configapply

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/katl-dev/katl/internal/extensionrelease"
	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/kernelmodule"
)

type ModuleIndexRequest struct {
	Root             string
	GenerationID     string
	Runtime          generation.RootSelection
	Release          *extensionrelease.Manifest
	RuntimeImagePath string
	Extensions       []manifest.SystemExtension
	Sysexts          []generation.ExtensionRef
	Sources          map[string]string
	Materials        []SystemExtensionPayload
	WorkDir          string
}

type PreparedModuleIndexes struct {
	Ref   generation.ExtensionRef
	Path  string
	image kernelmodule.PreparedIndexes
}

func (p PreparedModuleIndexes) Close() error {
	return p.image.Close()
}

func (p PreparedModuleIndexes) Materialize(root string) error {
	if p.Path == "" {
		return nil
	}
	data, err := os.ReadFile(p.Path)
	if err != nil {
		return err
	}
	if digestPayload(data) != "sha256:"+p.Ref.SHA256 {
		return fmt.Errorf("prepared module indexes SHA-256 mismatch")
	}
	target := filepath.Join(root, p.Ref.Path)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	return os.WriteFile(target, data, 0o644)
}

// PrepareModuleIndexes freezes a complete future selection before lifecycle
// mutation. Sources overrides paths that will exist only after staging; other
// inputs come from verified materials or retained generation assets.
func PrepareModuleIndexes(ctx context.Context, request ModuleIndexRequest) (PreparedModuleIndexes, error) {
	hasModules := false
	for _, ref := range request.Sysexts {
		hasModules = hasModules || ref.Compatibility.Kernel != nil
	}
	if !hasModules {
		return PreparedModuleIndexes{}, nil
	}
	if request.GenerationID == "" || filepath.Base(request.GenerationID) != request.GenerationID || request.GenerationID == "." || request.GenerationID == ".." {
		return PreparedModuleIndexes{}, fmt.Errorf("invalid module index generation ID")
	}
	if request.Release == nil {
		return PreparedModuleIndexes{}, fmt.Errorf("target runtime does not declare its exact kernel identity")
	}
	var contracts []kernelmodule.Contract
	for _, ref := range request.Sysexts {
		if ref.Compatibility.Kernel != nil {
			contracts = append(contracts, *ref.Compatibility.Kernel)
		}
	}
	selection, err := kernelmodule.IndexInputs(contracts)
	if err != nil {
		return PreparedModuleIndexes{}, err
	}
	if err := request.Release.Target.ValidateRuntime(request.Runtime.RuntimeVersion, request.Runtime.Architecture, request.Runtime.Flavour, request.Runtime.RuntimeInterface, request.Runtime.RuntimeArtifactSHA256); err != nil {
		return PreparedModuleIndexes{}, err
	}
	if request.WorkDir != "" {
		if err := os.MkdirAll(request.WorkDir, 0o700); err != nil {
			return PreparedModuleIndexes{}, err
		}
	}
	inputs, err := os.MkdirTemp(request.WorkDir, "module-inputs-")
	if err != nil {
		return PreparedModuleIndexes{}, err
	}
	defer os.RemoveAll(inputs)
	materials := map[string]string{}
	for _, material := range request.Materials {
		if digestPayload(material.Data) != material.Ref.Digest {
			return PreparedModuleIndexes{}, fmt.Errorf("module input digest mismatch")
		}
		digest := strings.TrimPrefix(material.Ref.Digest, "sha256:")
		path := filepath.Join(inputs, digest+".raw")
		if err := os.WriteFile(path, material.Data, 0o600); err != nil {
			return PreparedModuleIndexes{}, err
		}
		materials[digest] = path
	}
	owners := map[string]string{}
	for _, extension := range request.Extensions {
		for _, payload := range extension.Payloads {
			owners[extension.PayloadID(payload.Name)] = extension.Repository()
		}
	}
	var bundles []kernelmodule.BundleImages
	groups := map[string]int{}
	for _, ref := range request.Sysexts {
		if ref.Compatibility.ModuleIndexes != nil {
			continue
		}
		owner := owners[ref.Name]
		if owner == "" {
			owner = ref.Name
		}
		index, exists := groups[owner]
		if !exists {
			index = len(bundles)
			groups[owner] = index
			bundles = append(bundles, kernelmodule.BundleImages{
				Name:     owner,
				Contract: ref.Compatibility.Kernel,
			})
		}
		source := request.Sources[ref.Path]
		if source == "" {
			source = materials[ref.SHA256]
		}
		if source == "" {
			source = filepath.Join(request.Root, ref.Path)
		}
		bundles[index].Images = append(bundles[index].Images, kernelmodule.Image{
			Name:   filepath.Base(ref.ActivationPath),
			Path:   source,
			SHA256: ref.SHA256,
		})
	}
	runtimeImage := request.RuntimeImagePath
	if runtimeImage == "" {
		uuid := request.Runtime.PartitionUUID
		if uuid == "" || filepath.Base(uuid) != uuid || uuid == "." || uuid == ".." {
			return PreparedModuleIndexes{}, fmt.Errorf("recorded runtime root partition is required for module composition")
		}
		runtimeImage = filepath.Join(request.Root, "dev/disk/by-partuuid", uuid)
	}
	target := request.Release.Target.Kernel
	image, err := kernelmodule.PrepareImages(ctx, kernelmodule.ImageRequest{
		Target: target,
		Runtime: kernelmodule.Image{
			Path:   runtimeImage,
			SHA256: request.Runtime.RuntimeArtifactSHA256,
		},
		RuntimeInterface: request.Runtime.RuntimeInterface,
		RuntimeVersion:   request.Runtime.RuntimeVersion,
		Architecture:     request.Runtime.Architecture,
		Bundles:          bundles,
		WorkDir:          request.WorkDir,
	})
	if err != nil {
		return PreparedModuleIndexes{}, err
	}
	return PreparedModuleIndexes{
		Path:  image.Path,
		image: image,
		Ref: generation.ExtensionRef{
			Name:            kernelmodule.IndexExtensionName,
			Path:            filepath.Join(generation.GenerationRecordsDir, request.GenerationID, "sysext", kernelmodule.IndexExtensionName+".raw"),
			ActivationPath:  generation.DefaultExtensionsActivationDir + "/" + kernelmodule.IndexExtensionName + ".raw",
			SHA256:          image.SHA256,
			ArtifactVersion: request.Runtime.RuntimeVersion,
			PayloadVersion:  target.Release,
			Architecture:    request.Runtime.Architecture,
			Compatibility: generation.ExtensionCompatibility{
				ModuleIndexes:     &selection,
				RuntimeInterfaces: []string{request.Runtime.RuntimeInterface},
			},
		},
	}, nil
}
