package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/extensionrelease"
	"github.com/katl-dev/katl/internal/installer/katlosimage"
	"github.com/katl-dev/katl/internal/installer/systemextensionbundle"
	"github.com/katl-dev/katl/internal/kernelmodule"
)

var composeTarget = extensionrelease.Target{
	Version:          "2026.11.0",
	Architecture:     "x86_64",
	Flavour:          "standard",
	RuntimeInterface: "katl-runtime-1",
	Kernel: kernelmodule.Target{
		Release:       "6.18.1",
		RuntimeSHA256: strings.Repeat("a", 64),
	},
}

// exportComposeBundle exports a release extension bundle named name into
// layout and returns its immutable reference. A bundle with modules carries a
// kernel contract; its sysext payload is published as fileName.
func exportComposeBundle(t *testing.T, layout, name, fileName string, modules bool) string {
	t.Helper()
	image := filepath.Join(t.TempDir(), name+".raw")
	if err := os.WriteFile(image, []byte(name+" image"), 0o600); err != nil {
		t.Fatal(err)
	}
	request := systemextensionbundle.BuildRequest{
		Name:                       name,
		ArtifactVersion:            composeTarget.Version,
		PayloadVersion:             "1.2.3",
		Architecture:               composeTarget.Architecture,
		SupportedRuntimeInterfaces: []string{composeTarget.RuntimeInterface},
		CreatedAt:                  time.Unix(0, 0).UTC(),
		Payloads: []systemextensionbundle.Input{{
			Path:     image,
			Role:     systemextensionbundle.SysextRole,
			FileName: fileName,
		}},
	}
	if modules {
		request.Kernel = &kernelmodule.Contract{
			Target: composeTarget.Kernel,
			Modules: []kernelmodule.Module{{
				Name:   name,
				Path:   "usr/lib/modules/" + composeTarget.Kernel.Release + "/extra/" + name + ".ko",
				SHA256: strings.Repeat("b", 64),
			}},
		}
	}
	packed, _, err := systemextensionbundle.Export(context.Background(), layout, request, nil)
	if err != nil {
		t.Fatal(err)
	}
	return "registry.invalid/extensions/" + name + "@" + packed.ManifestDigest
}

func composeRecipes(names ...string) map[string]kernelRecipe {
	recipes := map[string]kernelRecipe{}
	for _, name := range names {
		recipes[name] = kernelRecipe{Version: "1.2.3", Repository: "registry.invalid/extensions/" + name}
	}
	return recipes
}

// composePayload returns an extracted image root advertising the named
// extensions, each with a kernel contract unless listed in withoutModules.
func composePayload(t *testing.T, names []string, withoutModules ...string) katlosimage.Payload {
	t.Helper()
	root := t.TempDir()
	release := &extensionrelease.Manifest{Target: composeTarget, Extensions: map[string]string{}}
	for _, name := range names {
		modules := true
		for _, without := range withoutModules {
			modules = modules && without != name
		}
		ref := exportComposeBundle(t, filepath.Join(root, katlosimage.ExtensionLayoutPath), name, name+".raw", modules)
		release.Extensions["registry.invalid/extensions/"+name] = ref
	}
	return katlosimage.Payload{
		Root: root,
		Index: katlosimage.Index{
			Architecture:     composeTarget.Architecture,
			RuntimeInterface: composeTarget.RuntimeInterface,
			ExtensionRelease: release,
		},
		Runtime: katlosimage.Component{
			Path:   "components/runtime/root.squashfs",
			SHA256: composeTarget.Kernel.RuntimeSHA256,
		},
	}
}

func TestComposeReleaseExtensions(t *testing.T) {
	for _, test := range []struct {
		name    string
		payload func(*testing.T) katlosimage.Payload
		recipes map[string]kernelRecipe
		refusal string
	}{
		{
			name: "composes the declared closure",
			payload: func(t *testing.T) katlosimage.Payload {
				return composePayload(t, []string{"drbd9", "nvidia"})
			},
			recipes: composeRecipes("drbd9", "nvidia"),
		},
		{
			name: "no extensions",
			payload: func(t *testing.T) katlosimage.Payload {
				payload := composePayload(t, nil)
				payload.Index.ExtensionRelease = nil
				return payload
			},
			recipes: composeRecipes("drbd9"),
			refusal: "advertises no release extensions",
		},
		{
			// An image that dropped an extension would otherwise qualify
			// without ever loading it.
			name: "closure lacks a declared extension",
			payload: func(t *testing.T) katlosimage.Payload {
				return composePayload(t, []string{"drbd9"})
			},
			recipes: composeRecipes("drbd9", "nvidia"),
			refusal: "the release declares",
		},
		{
			name: "closure adds an undeclared extension",
			payload: func(t *testing.T) katlosimage.Payload {
				return composePayload(t, []string{"drbd9", "nvidia"})
			},
			recipes: composeRecipes("drbd9"),
			refusal: "the release declares",
		},
		{
			name: "no kernel contracts",
			payload: func(t *testing.T) katlosimage.Payload {
				return composePayload(t, []string{"drbd9"}, "drbd9")
			},
			recipes: composeRecipes("drbd9"),
			refusal: "ship no kernel modules",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload := test.payload(t)
			output := t.TempDir()
			var operands []string
			err := composeReleaseExtensions(context.Background(), payload, test.recipes, output, func(args []string) error {
				operands = args
				return nil
			})

			if test.refusal != "" {
				if err == nil || !strings.Contains(err.Error(), test.refusal) {
					t.Fatalf("error = %v, want %q", err, test.refusal)
				}
				if operands != nil {
					t.Fatalf("composed a refused closure: %v", operands)
				}
				if _, err := os.Stat(filepath.Join(output, releaseExtensionModulesFile)); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("refused closure wrote a module inventory: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}

			want := []string{
				filepath.Join(payload.Root, "release-extensions.json"),
				filepath.Join(payload.Root, "components/runtime/root.squashfs"),
				filepath.Join(payload.Root, katlosimage.ExtensionLayoutPath),
				output,
			}
			if strings.Join(operands, "\n") != strings.Join(want, "\n") {
				t.Fatalf("composer operands = %q, want %q", operands, want)
			}
			release, err := readExtensionRelease(operands[0])
			if err != nil || len(release.Extensions) != 2 || release.Extensions["registry.invalid/extensions/nvidia"] != payload.Index.ExtensionRelease.Extensions["registry.invalid/extensions/nvidia"] {
				t.Fatalf("composed manifest = %#v, %v", release, err)
			}

			var contracts map[string]kernelmodule.Contract
			data, err := os.ReadFile(filepath.Join(output, releaseExtensionModulesFile))
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(data, &contracts); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"drbd9", "nvidia"} {
				contract := contracts["registry.invalid/extensions/"+name]
				if len(contract.Modules) != 1 || contract.Modules[0].Name != name || contract.Target != composeTarget.Kernel {
					t.Fatalf("module inventory for %s = %#v", name, contract)
				}
			}

			// The inventory is written once; a second composition into the
			// same output must not replace it.
			err = composeReleaseExtensions(context.Background(), payload, test.recipes, output, func([]string) error { return nil })
			if !errors.Is(err, fs.ErrExist) {
				t.Fatalf("second composition into %s = %v, want an existing-file refusal", output, err)
			}
		})
	}
}

func TestComposeReleaseExtensionsRefusesExistingOutput(t *testing.T) {
	dir := t.TempDir()
	image := filepath.Join(dir, "katlos-install.squashfs")
	if err := os.WriteFile(image, []byte("not extracted"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeTestJSON(t, image+".json", katlosimage.ArtifactMetadata{
		Flavour:           "standard",
		APIVersion:        katlosimage.APIVersion,
		Kind:              katlosimage.ArtifactMetadataKind,
		ImageRole:         katlosimage.RoleInstall,
		Format:            katlosimage.FormatSquashFS,
		Version:           composeTarget.Version,
		BuildID:           "test-build",
		Architecture:      composeTarget.Architecture,
		RuntimeInterface:  composeTarget.RuntimeInterface,
		Path:              filepath.Base(image),
		SizeBytes:         int64(len("not extracted")),
		SHA256:            strings.Repeat("c", 64),
		ChecksumPath:      filepath.Base(image) + ".sha256",
		EmbeddedIndexPath: "katlos/image.json",
		CreatedAt:         "2026-11-27T00:00:00Z",
	})
	output := filepath.Join(dir, "composed")
	if err := os.Mkdir(output, 0o755); err != nil {
		t.Fatal(err)
	}
	earlier := writeTestFile(t, output, "drbd9.raw", "earlier composition")

	err := runComposeReleaseExtensions([]string{image, output}, io.Discard, io.Discard, config{RepoRoot: testRepoRoot(t)})
	if !errors.Is(err, fs.ErrExist) {
		t.Fatalf("composition into an existing directory = %v, want an existing-directory refusal", err)
	}
	if data, err := os.ReadFile(earlier); err != nil || string(data) != "earlier composition" {
		t.Fatalf("existing output changed: %q, %v", data, err)
	}
}

func TestVerifyReleaseExtensionsOutputRefusesCollision(t *testing.T) {
	for _, test := range []struct {
		name     string
		existing bool
		shared   bool
	}{
		// Both bundles publish the same sysext file name, so composing them
		// into one output would let the second replace the first.
		{name: "two extensions publish one file name", shared: true},
		{name: "output already holds the image", existing: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			layout := filepath.Join(dir, "extension-bundles")
			release := extensionrelease.Manifest{Target: composeTarget, Extensions: map[string]string{}}
			names := []string{"drbd9"}
			if test.shared {
				names = append(names, "nvidia")
			}
			for _, name := range names {
				release.Extensions["registry.invalid/extensions/"+name] = exportComposeBundle(t, layout, name, "extension.raw", true)
			}
			manifest := filepath.Join(dir, "release-extensions.json")
			writeTestJSON(t, manifest, release)
			output := filepath.Join(dir, "output")
			if err := os.Mkdir(output, 0o755); err != nil {
				t.Fatal(err)
			}
			if test.existing {
				writeTestFile(t, output, "extension.raw", "earlier composition")
			}

			err := runVerifyReleaseExtensions([]string{manifest, filepath.Join(dir, "runtime.squashfs"), layout, output})
			if !errors.Is(err, fs.ErrExist) {
				t.Fatalf("composition = %v, want an existing-file refusal", err)
			}
			if test.existing {
				if data, err := os.ReadFile(filepath.Join(output, "extension.raw")); err != nil || string(data) != "earlier composition" {
					t.Fatalf("existing image replaced: %q, %v", data, err)
				}
			}
		})
	}
}
