package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/extensionrelease"
	"github.com/katl-dev/katl/internal/installer/systemextensionbundle"
	"github.com/katl-dev/katl/internal/kernelmodule"
)

func TestCollectReleaseExtensionInventory(t *testing.T) {
	layout := t.TempDir()
	target := extensionrelease.Target{
		Version:          "2026.9.0",
		Architecture:     "x86_64",
		Flavour:          "lts",
		RuntimeInterface: "katl-runtime-1",
		Kernel: kernelmodule.Target{
			Release:       "6.18.1",
			RuntimeSHA256: strings.Repeat("a", 64),
		},
	}
	release := extensionrelease.Manifest{Target: target, Extensions: map[string]string{}}
	for _, extension := range []struct {
		name, version string
	}{{"zeta", "2.0.0"}, {"alpha", "1.2.3"}} {
		image := filepath.Join(t.TempDir(), extension.name+".raw")
		if err := os.WriteFile(image, []byte(extension.name), 0600); err != nil {
			t.Fatal(err)
		}
		packed, _, err := systemextensionbundle.Export(context.Background(), layout, systemextensionbundle.BuildRequest{
			Name:                       extension.name,
			ArtifactVersion:            target.Version,
			PayloadVersion:             extension.version,
			Architecture:               target.Architecture,
			SupportedRuntimeInterfaces: []string{target.RuntimeInterface},
			CreatedAt:                  time.Unix(0, 0).UTC(),
			Kernel: &kernelmodule.Contract{
				Target: target.Kernel,
				Modules: []kernelmodule.Module{{
					Name:   extension.name,
					Path:   "usr/lib/modules/" + target.Kernel.Release + "/extra/" + extension.name + ".ko",
					SHA256: strings.Repeat("b", 64),
				}},
			},
			Payloads: []systemextensionbundle.Input{{
				Path: image,
				Role: systemextensionbundle.SysextRole,
			}},
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		repository := "registry.invalid/extensions/" + extension.name
		release.Extensions[repository] = repository + "@" + packed.ManifestDigest
	}

	inventory, err := collectReleaseExtensionInventory(release, layout)
	if err != nil {
		t.Fatal(err)
	}
	if inventory.Version != target.Version || inventory.Flavour != target.Flavour || len(inventory.Extensions) != 2 {
		t.Fatalf("inventory = %#v", inventory)
	}
	if inventory.Extensions[0].Name != "alpha" || inventory.Extensions[0].PayloadVersion != "1.2.3" || inventory.Extensions[1].Name != "zeta" || inventory.Extensions[1].PayloadVersion != "2.0.0" {
		t.Fatalf("extension versions = %#v", inventory.Extensions)
	}

	t.Run("publication matches the qualified inventory", func(t *testing.T) {
		dir := t.TempDir()
		input := filepath.Join(dir, "release-extensions.json")
		if err := writeJSON(input, release, dir); err != nil {
			t.Fatal(err)
		}
		qualified := releaseExtensionInventory{
			SchemaVersion: 1, ArtifactKind: "katl.release-extension-inventory.v1",
			Version: "2026.9.0", Architecture: "x86_64", Flavour: "lts", RuntimeInterface: "katl-runtime-1",
			KernelRelease: "6.18.1", RuntimeSHA256: strings.Repeat("a", 64),
			Extensions: []releaseExtensionVersion{
				{"alpha", "1.2.3", "registry.invalid/extensions/alpha", release.Extensions["registry.invalid/extensions/alpha"]},
				{"zeta", "2.0.0", "registry.invalid/extensions/zeta", release.Extensions["registry.invalid/extensions/zeta"]},
			},
		}
		args := []string{dir, "lts", input, layout}
		if err := runMatchReleaseExtensions(args); err == nil {
			t.Fatal("accepted extension inputs without a qualified inventory")
		}
		qualifiedPath := filepath.Join(dir, "katl-runtime-lts.extensions.json")
		if err := writeJSON(qualifiedPath, qualified, dir); err != nil {
			t.Fatal(err)
		}
		if err := runMatchReleaseExtensions(args); err != nil {
			t.Fatalf("matching inputs: %v", err)
		}
		qualified.Extensions[0].Reference = "registry.invalid/extensions/alpha@sha256:" + strings.Repeat("9", 64)
		if err := writeJSON(qualifiedPath, qualified, dir); err != nil {
			t.Fatal(err)
		}
		if err := runMatchReleaseExtensions(args); err == nil || !strings.Contains(err.Error(), "differ from") {
			t.Fatalf("a different qualified reference was accepted: %v", err)
		}
	})

	release.Target.Version = "2026.9.1"
	if _, err := collectReleaseExtensionInventory(release, layout); err == nil || !strings.Contains(err.Error(), "artifact version") {
		t.Fatalf("mismatched release error = %v", err)
	}
}
