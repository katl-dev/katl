package main

import (
	"bytes"
	"context"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/extensionrelease"
	"github.com/katl-dev/katl/internal/installer/configbundle"
	"github.com/katl-dev/katl/internal/installer/handoff"
	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/installer/systemextensionbundle"
	"github.com/katl-dev/katl/internal/kernelmodule"
)

func TestInstallCarriesMediaExtension(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		name := "complete media"
		if corrupt {
			name = "corrupt media"
		}
		t.Run(name, func(t *testing.T) {
			target := extensionrelease.Target{
				Version:          "2026.9.2",
				Architecture:     "x86_64",
				Flavour:          "standard",
				RuntimeInterface: "katl-runtime-1",
				Kernel: kernelmodule.Target{
					Release:       "6.12.0",
					RuntimeSHA256: strings.Repeat("b", 64),
				},
			}
			directory := t.TempDir()
			image := filepath.Join(t.TempDir(), "driver.raw")
			if err := os.WriteFile(image, []byte("media-owned driver"), 0o644); err != nil {
				t.Fatal(err)
			}
			packed, built, err := systemextensionbundle.Export(context.Background(), directory, systemextensionbundle.BuildRequest{
				Name:                       "drbd9",
				ArtifactVersion:            target.Version,
				PayloadVersion:             "9.3.4",
				Architecture:               target.Architecture,
				SupportedRuntimeInterfaces: []string{target.RuntimeInterface},
				CreatedAt:                  time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC),
				Kernel: &kernelmodule.Contract{
					Target: target.Kernel,
					Modules: []kernelmodule.Module{{
						Name:   "drbd",
						Path:   "usr/lib/modules/6.12.0/extra/drbd.ko",
						SHA256: strings.Repeat("c", 64),
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
			ref := "registry.example/drbd9@" + packed.ManifestDigest
			server := handoff.NewHandoffServerWithDefaultImage(nil, manifest.KatlosImage{
				LocalRef:         "images/katlos.squashfs",
				SHA256:           strings.Repeat("a", 64),
				SizeBytes:        1,
				Version:          target.Version,
				Architecture:     target.Architecture,
				RuntimeInterface: target.RuntimeInterface,
				Role:             "install",
				ExtensionRelease: &extensionrelease.Manifest{
					Target:     target,
					Extensions: map[string]string{"registry.example/drbd9": ref},
				},
			})
			root, err := os.OpenRoot(directory)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			server.SetExtensionLayout(root.FS())
			httpServer := httptest.NewServer(server.Handler())
			defer httpServer.Close()
			if corrupt {
				path := filepath.Join(directory, "blobs/sha256", strings.TrimPrefix(built.Bundle.Payloads[0].Digest, "sha256:"))
				if err := os.Chmod(path, 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("damaged driver"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			sourcePath := writeClusterConfig(t)
			source, err := os.ReadFile(sourcePath)
			if err != nil {
				t.Fatal(err)
			}
			source = bytes.Replace(source, []byte("  defaults:\n"), []byte("  defaults:\n    systemExtensions:\n      - release: registry.example/drbd9\n"), 1)
			if err := os.WriteFile(sourcePath, source, 0o600); err != nil {
				t.Fatal(err)
			}

			err = run(context.Background(), []string{"install", "apply", "--config", sourcePath, "--endpoint", httpServer.URL, "--no-wait"}, io.Discard, io.Discard)
			if corrupt {
				if err == nil || server.Status().State != handoff.HandoffWaiting || len(server.Bundle().Data) != 0 {
					t.Fatalf("corrupt media started installation: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			selected, err := configbundle.ReadSelectedNode(bytes.NewReader(server.Bundle().Data), configbundle.ReadOptions{
				NodeName:           "cp-1",
				DefaultKatlosImage: server.Status().Image,
			})
			if err != nil {
				t.Fatal(err)
			}
			if !selected.KatlosImageFromMedia {
				t.Fatal("installer lost media ownership of the resolved image")
			}
			extensions := selected.InstallManifest.Node.SystemExtensions
			if len(extensions) != 1 || extensions[0].Release != "registry.example/drbd9" || extensions[0].ResolvedBundle != ref || *extensions[0].ReleaseTarget != target {
				t.Fatalf("handoff selection = %+v", extensions)
			}
			if len(selected.SystemExtensionPayloads) != 1 || string(selected.SystemExtensionPayloads[0].Data) != "media-owned driver" {
				t.Fatalf("handoff omitted exact driver bytes: %+v", selected.SystemExtensionPayloads)
			}
		})
	}
}
