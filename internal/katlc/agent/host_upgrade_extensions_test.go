package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/extensionrelease"
	"github.com/katl-dev/katl/internal/installer/katlosimage"
	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/installer/systemextensionbundle"
	"github.com/katl-dev/katl/internal/kernelmodule"
)

func TestHostExtensionSelection(t *testing.T) {
	data := []byte("target driver image")
	pin := "sha256:" + strings.Repeat("a", 64)
	target := extensionrelease.Target{
		Version:          "2026.9.2",
		Architecture:     "x86_64",
		Flavour:          "standard",
		RuntimeInterface: "katl-runtime-1",
		Kernel: kernelmodule.Target{
			Release:       "6.12",
			RuntimeSHA256: strings.Repeat("b", 64),
		},
	}
	payload := katlosimage.Payload{
		Index: katlosimage.Index{
			Version:          target.Version,
			Architecture:     target.Architecture,
			Flavour:          target.Flavour,
			RuntimeInterface: target.RuntimeInterface,
			ExtensionRelease: &extensionrelease.Manifest{
				Target:     target,
				Extensions: map[string]string{"registry.example/drbd9": "registry.example/drbd9@" + pin},
			},
		},
		Runtime: katlosimage.Component{SHA256: target.Kernel.RuntimeSHA256},
	}
	current := manifest.Manifest{}
	current.Node.SystemExtensions = []manifest.SystemExtension{
		{

			Release: "registry.example/drbd9",
			Payloads: []manifest.SystemExtensionPayloadRef{{
				Name: "old-drbd.raw",
				Role: "systemd-sysext",
			}},
		},
		{

			Bundle:            "registry.example/bird:v1",
			OCIManifestDigest: "sha256:" + strings.Repeat("c", 64),
		},
	}
	fetch := func(_ context.Context, request systemextensionbundle.ResolveRequest) (systemextensionbundle.Resolved, error) {
		if request.Reference != "registry.example/drbd9@"+pin {
			t.Fatalf("unexpected acquisition %s", request.Reference)
		}
		return systemextensionbundle.Resolved{
			OCIManifestDigest:    pin,
			BundleManifestDigest: "sha256:" + strings.Repeat("d", 64),
			Bundle: systemextensionbundle.Bundle{
				ArtifactVersion:            "2026.9.2",
				PayloadVersion:             "9.3.0",
				Architecture:               "x86_64",
				SupportedRuntimeInterfaces: []string{"katl-runtime-1"},
				Kernel: &kernelmodule.Contract{
					Target: target.Kernel,
					Modules: []kernelmodule.Module{{
						Name:   "drbd",
						Path:   "usr/lib/modules/6.12/extra/drbd.ko",
						SHA256: strings.Repeat("e", 64),
					}},
				},
			},
			Payloads: []systemextensionbundle.Payload{{
				Descriptor: systemextensionbundle.Descriptor{
					FileName:  "drbd.raw",
					Role:      "systemd-sysext",
					MediaType: systemextensionbundle.SysextMediaType,
					Digest:    "sha256:" + testSHA(data),
					SizeBytes: int64(len(data)),
				},
				Data: data,
			}},
		}, nil
	}
	plan, err := planHostExtensions(context.Background(), current, payload, "next", fetch)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.sysexts) != 1 || plan.sysexts[0].SHA256 != testSHA(data) || plan.sysexts[0].Compatibility.Kernel.Target != target.Kernel {
		t.Fatalf("candidate driver = %#v", plan.sysexts)
	}
	if len(plan.replace) != 1 || plan.replace[0] != "registry.example/drbd9#old-drbd.raw" {
		t.Fatalf("replaced = %v", plan.replace)
	}
	if plan.manifest.Node.SystemExtensions[0].Release != "registry.example/drbd9" || *plan.manifest.Node.SystemExtensions[0].ReleaseTarget != target {
		t.Fatal("lost selection intent or target identity")
	}
	if plan.manifest.Node.SystemExtensions[1].OCIManifestDigest != current.Node.SystemExtensions[1].OCIManifestDigest {
		t.Fatal("changed explicit userspace selection")
	}
	if current.Node.SystemExtensions[0].OCIManifestDigest != "" {
		t.Fatal("mutated active configuration")
	}

	// Production planning must work with only the future image's closure;
	// the original registry is deliberately unavailable.
	payload.Root = t.TempDir()
	imagePath := filepath.Join(t.TempDir(), "drbd.raw")
	if err := os.WriteFile(imagePath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	resolved, err := fetch(context.Background(), systemextensionbundle.ResolveRequest{Reference: "registry.example/drbd9@" + pin})
	if err != nil {
		t.Fatal(err)
	}
	packed, _, err := systemextensionbundle.Export(context.Background(), filepath.Join(payload.Root, katlosimage.ExtensionLayoutPath), systemextensionbundle.BuildRequest{
		Kernel:                     resolved.Bundle.Kernel,
		Name:                       "drbd9",
		ArtifactVersion:            target.Version,
		PayloadVersion:             "9.3.0",
		Architecture:               target.Architecture,
		SupportedRuntimeInterfaces: []string{target.RuntimeInterface},
		CreatedAt:                  time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC),
		Payloads: []systemextensionbundle.Input{{
			Path: imagePath,
			Role: systemextensionbundle.SysextRole,
		}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	payload.Index.ExtensionRelease.Extensions["registry.example/drbd9"] = "registry.example/drbd9@" + packed.ManifestDigest
	offline, err := planHostExtensions(context.Background(), current, payload, "offline", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(offline.materials) != 1 || string(offline.materials[0].Data) != "target driver image" || offline.desired[0].OCIManifestDigest != packed.ManifestDigest {
		t.Fatalf("offline upgrade selection = %+v", offline.desired)
	}

	payload.Index.ExtensionRelease.Extensions = nil
	if _, err := planHostExtensions(context.Background(), current, payload, "next", fetch); err == nil {
		t.Fatal("accepted a target without the selected extension")
	}
}
