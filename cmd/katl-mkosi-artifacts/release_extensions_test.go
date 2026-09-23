package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/extensionrelease"
	"github.com/katl-dev/katl/internal/installer/payloadbundle"
	"github.com/katl-dev/katl/internal/installer/systemextensionbundle"
	"github.com/katl-dev/katl/internal/kernelmodule"
)

func TestExtensionInventory(t *testing.T) {
	repo := t.TempDir()
	for _, name := range []string{"zeta", "alpha"} {
		recipe := kernelRecipe{
			Version:    "1.2.3",
			Repository: "registry.invalid/extensions/" + name,
			Sources: map[string]kernelSource{"source": {
				URL:    "https://example.invalid/source.tar.gz",
				SHA256: strings.Repeat("a", 64),
			}},
		}
		if err := writeJSON(filepath.Join(repo, "extensions", name, "recipe.json"), recipe, repo); err != nil {
			t.Fatal(err)
		}
		profile := filepath.Join(repo, "mkosi.profiles", "kernel-extension-"+name)
		if err := os.MkdirAll(profile, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(profile, "mkosi.conf"), []byte("[Output]\nFormat=sysext\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	var output bytes.Buffer
	if err := runExtensionInventory(nil, &output, config{RepoRoot: repo}); err != nil {
		t.Fatal(err)
	}
	if output.String() != "{\"extension\":[\"alpha\",\"zeta\"]}\n" {
		t.Fatalf("inventory = %s", output.String())
	}

	if err := os.Remove(filepath.Join(repo, "mkosi.profiles", "kernel-extension-zeta", "mkosi.conf")); err != nil {
		t.Fatal(err)
	}
	if err := runExtensionInventory(nil, &output, config{RepoRoot: repo}); err == nil {
		t.Fatal("inventory silently accepted a recipe without its build profile")
	}
}

func TestCollectReleaseExtensions(t *testing.T) {
	for _, scenario := range []string{"complete", "missing", "wrong target", "wrong version", "corrupt closure"} {
		t.Run(scenario, func(t *testing.T) {
			directory := t.TempDir()
			target := extensionrelease.Target{
				Version:          "2026.9.0-beta.16",
				Architecture:     "x86_64",
				Flavour:          "lts",
				RuntimeInterface: "katl-runtime-1",
				Kernel: kernelmodule.Target{
					Release:       "6.18.1",
					RuntimeSHA256: strings.Repeat("a", 64),
				},
			}
			recipes := map[string]kernelRecipe{}
			refs := map[string]string{}
			for _, name := range []string{"alpha", "zeta"} {
				repository := "registry.invalid/extensions/" + name
				recipes[name] = kernelRecipe{
					Version:    "1.2.3",
					Repository: repository,
				}
				if name == "zeta" && scenario == "missing" {
					continue
				}
				memberDir := filepath.Join(directory, name)
				if err := os.MkdirAll(memberDir, 0755); err != nil {
					t.Fatal(err)
				}
				image := filepath.Join(memberDir, name+".raw")
				if err := os.WriteFile(image, []byte(name+" image"), 0600); err != nil {
					t.Fatal(err)
				}
				memberTarget := target
				version := "1.2.3"
				if name == "zeta" && scenario == "wrong target" {
					memberTarget.Kernel.Release = "6.18.2"
				}
				if name == "zeta" && scenario == "wrong version" {
					version = "1.2.4"
				}
				layout := filepath.Join(memberDir, "extension-bundles")
				packed, _, err := systemextensionbundle.Export(context.Background(), layout, systemextensionbundle.BuildRequest{
					Name:                       name,
					ArtifactVersion:            target.Version,
					PayloadVersion:             version,
					Architecture:               target.Architecture,
					SupportedRuntimeInterfaces: []string{target.RuntimeInterface},
					CreatedAt:                  time.Unix(0, 0).UTC(),
					Kernel: &kernelmodule.Contract{
						Target: memberTarget.Kernel,
						Modules: []kernelmodule.Module{{
							Name:   name,
							Path:   "usr/lib/modules/" + memberTarget.Kernel.Release + "/extra/" + name + ".ko",
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
				ref := repository + "@" + packed.ManifestDigest
				refs[repository] = ref
				if err := writeJSON(filepath.Join(memberDir, "release.json"), extensionrelease.Manifest{
					Target:     memberTarget,
					Extensions: map[string]string{repository: ref},
				}, directory); err != nil {
					t.Fatal(err)
				}
				if name == "zeta" && scenario == "corrupt closure" {
					path := filepath.Join(layout, "blobs", "sha256", strings.TrimPrefix(packed.ManifestDigest, "sha256:"))
					if err := os.Chmod(path, 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte("corrupt"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}

			destination := filepath.Join(t.TempDir(), "closure")
			combined, err := collectReleaseExtensions(recipes, directory, destination)
			if scenario != "complete" {
				if err == nil {
					t.Fatal("accepted incomplete or mismatched release")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(combined.Extensions) != 2 || combined.Target != target {
				t.Fatalf("combined release = %#v", combined)
			}
			for repository, ref := range refs {
				if combined.Extensions[repository] != ref {
					t.Fatal("assembly changed immutable identity")
				}
				fetched, err := payloadbundle.Fetch(context.Background(), payloadbundle.FetchRequest{
					LayoutDir:       destination,
					Reference:       ref,
					ArtifactType:    systemextensionbundle.ArtifactType,
					ConfigMediaType: systemextensionbundle.ConfigMediaType,
				})
				if err != nil || len(fetched.Layers) == 0 {
					t.Fatalf("assembled closure is not independently readable: %v", err)
				}
			}
		})
	}
}
