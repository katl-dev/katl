package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/extensionrelease"
	"github.com/katl-dev/katl/internal/installer/systemextensionbundle"
	"github.com/katl-dev/katl/internal/kernelmodule"
)

func TestPublicationValidatesCompleteRelease(t *testing.T) {
	directory := t.TempDir()
	image := filepath.Join(directory, "driver.raw")
	if err := os.WriteFile(image, []byte("driver image"), 0o600); err != nil {
		t.Fatal(err)
	}
	release := extensionrelease.Manifest{
		Target: extensionrelease.Target{
			Version:          "2026.9.23",
			Architecture:     "x86_64",
			Flavour:          "standard",
			RuntimeInterface: "katl-runtime-1",
			Kernel: kernelmodule.Target{
				Release:       "6.12.1",
				RuntimeSHA256: strings.Repeat("a", 64),
			},
		},
		Extensions: map[string]string{},
	}
	layout := filepath.Join(directory, "layout")
	for _, name := range []string{"a-valid", "z-wrong-kernel"} {
		target := release.Target.Kernel
		if name == "z-wrong-kernel" {
			target.Release = "6.12.2"
		}
		packed, _, err := systemextensionbundle.Export(context.Background(), layout, systemextensionbundle.BuildRequest{
			Name:                       name,
			ArtifactVersion:            release.Target.Version,
			PayloadVersion:             "1.0.0",
			Architecture:               "x86_64",
			SupportedRuntimeInterfaces: []string{"katl-runtime-1"},
			CreatedAt:                  time.Unix(0, 0).UTC(),
			Kernel: &kernelmodule.Contract{
				Target: target,
				Modules: []kernelmodule.Module{{
					Name:   "example",
					Path:   "usr/lib/modules/" + target.Release + "/extra/example.ko",
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
		repository := "registry.invalid/extensions/" + name
		release.Extensions[repository] = repository + "@" + packed.ManifestDigest
	}
	data, err := json.Marshal(release)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "release.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	// Publishing while validating would try the first registry reference before
	// discovering the incompatible second member of the advertised combination.
	err = runPublishReleaseExtensions([]string{path, layout}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "verify release extension registry.invalid/extensions/z-wrong-kernel") || !strings.Contains(err.Error(), "kernel release does not match target") {
		t.Fatalf("publication did not reject the complete combination before writes: %v", err)
	}
}
