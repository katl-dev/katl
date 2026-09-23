package main

import (
	"fmt"
	"path/filepath"
	"sort"

	"github.com/katl-dev/katl/internal/extensionrelease"
)

const releaseExtensionInventoryKind = "katl.release-extension-inventory.v1"

type releaseExtensionInventory struct {
	SchemaVersion    int                       `json:"schemaVersion"`
	ArtifactKind     string                    `json:"artifactKind"`
	Version          string                    `json:"version"`
	Architecture     string                    `json:"architecture"`
	Flavour          string                    `json:"flavour"`
	RuntimeInterface string                    `json:"runtimeInterface"`
	KernelRelease    string                    `json:"kernelRelease"`
	RuntimeSHA256    string                    `json:"runtimeSHA256"`
	Extensions       []releaseExtensionVersion `json:"extensions"`
}

type releaseExtensionVersion struct {
	Name           string `json:"name"`
	PayloadVersion string `json:"payloadVersion"`
	Repository     string `json:"repository"`
	Reference      string `json:"reference"`
}

func collectReleaseExtensionInventory(release extensionrelease.Manifest, layout string) (releaseExtensionInventory, error) {
	inventory := releaseExtensionInventory{
		SchemaVersion:    1,
		ArtifactKind:     releaseExtensionInventoryKind,
		Version:          release.Target.Version,
		Architecture:     release.Target.Architecture,
		Flavour:          release.Target.Flavour,
		RuntimeInterface: release.Target.RuntimeInterface,
		KernelRelease:    release.Target.Kernel.Release,
		RuntimeSHA256:    release.Target.Kernel.RuntimeSHA256,
	}
	if err := release.Validate(); err != nil {
		return inventory, err
	}
	if len(release.Extensions) == 0 {
		return inventory, fmt.Errorf("release extension inventory requires at least one extension")
	}
	for repository := range release.Extensions {
		resolved, err := resolveReleaseExtension(release, repository, layout)
		if err != nil {
			return inventory, fmt.Errorf("resolve release extension %s: %w", repository, err)
		}
		if resolved.Bundle.ArtifactVersion != release.Target.Version {
			return inventory, fmt.Errorf("release extension %s artifact version %q does not match release %q", repository, resolved.Bundle.ArtifactVersion, release.Target.Version)
		}
		inventory.Extensions = append(inventory.Extensions, releaseExtensionVersion{
			Name:           resolved.Bundle.Name,
			PayloadVersion: resolved.Bundle.PayloadVersion,
			Repository:     repository,
			Reference:      release.Extensions[repository],
		})
	}
	sort.Slice(inventory.Extensions, func(i, j int) bool {
		if inventory.Extensions[i].Name != inventory.Extensions[j].Name {
			return inventory.Extensions[i].Name < inventory.Extensions[j].Name
		}
		return inventory.Extensions[i].Repository < inventory.Extensions[j].Repository
	})
	for i := 1; i < len(inventory.Extensions); i++ {
		if inventory.Extensions[i-1].Name == inventory.Extensions[i].Name {
			return inventory, fmt.Errorf("release extension name %q is ambiguous", inventory.Extensions[i].Name)
		}
	}
	return inventory, nil
}

func writeReleaseExtensionInventory(path string, release extensionrelease.Manifest, layout, repoRoot string) error {
	inventory, err := collectReleaseExtensionInventory(release, layout)
	if err != nil {
		return err
	}
	if err := writeJSON(path, inventory, repoRoot); err != nil {
		return fmt.Errorf("write release extension inventory %s: %w", filepath.Base(path), err)
	}
	return nil
}
