package systemextensionbundle

import (
	"context"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/extensionrelease"
	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/kernelmodule"
)

func TestResolveReleaseIntent(t *testing.T) {
	target := extensionrelease.Target{
		Version:          "2026.9.2",
		Architecture:     "x86_64",
		Flavour:          "standard",
		RuntimeInterface: "katl-runtime-1",
		Kernel: kernelmodule.Target{
			Release:       "6.12",
			RuntimeSHA256: strings.Repeat("a", 64),
		},
	}
	pin := "sha256:" + strings.Repeat("b", 64)
	ref := "registry.example/drbd9@" + pin
	release := extensionrelease.Manifest{
		Target:     target,
		Extensions: map[string]string{"registry.example/drbd9": ref},
	}
	stale := target
	stale.Version = "2026.9.1"
	source := manifest.SystemExtension{
		Release:           "registry.example/drbd9",
		ReleaseTarget:     &stale,
		ResolvedBundle:    "registry.example/drbd9:old",
		OCIManifestDigest: "sha256:" + strings.Repeat("c", 64),
	}
	fetch := func(_ context.Context, request ResolveRequest) (Resolved, error) {
		if request.Reference != ref || request.RuntimeSHA256 != target.Kernel.RuntimeSHA256 {
			t.Fatalf("resolved stale selection: %#v", request)
		}
		return Resolved{
			Reference:            ref,
			OCIManifestDigest:    pin,
			BundleManifestDigest: "sha256:" + strings.Repeat("d", 64),
			Bundle: Bundle{
				Name:                       "drbd9",
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
			Payloads: []Payload{{Descriptor: Descriptor{
				FileName:  "drbd.raw",
				Role:      SysextRole,
				MediaType: SysextMediaType,
				Digest:    "sha256:" + strings.Repeat("f", 64),
				SizeBytes: 100,
			}}},
		}, nil
	}
	desired, _, err := ResolveSelection(context.Background(), target, &release, source, fetch)
	if err != nil {
		t.Fatal(err)
	}
	if desired.Release != "registry.example/drbd9" || desired.Bundle != "" || desired.Repository() != "registry.example/drbd9" || desired.ResolvedBundle != ref || *desired.ReleaseTarget != target {
		t.Fatalf("intent and resolved identity = %#v", desired)
	}

	source.Release = ""
	source.Bundle = "registry.example/drbd9@sha256:" + strings.Repeat("c", 64)
	if _, _, err := ResolveSelection(context.Background(), target, &release, source, func(context.Context, ResolveRequest) (Resolved, error) {
		return fetch(context.Background(), ResolveRequest{
			Reference:     ref,
			RuntimeSHA256: target.Kernel.RuntimeSHA256,
		})
	}); err == nil {
		t.Fatal("silently replaced explicit pin")
	}
}

func TestRemovedSelectionDoesNotFetch(t *testing.T) {
	for _, source := range []manifest.SystemExtension{
		{
			Release: "registry.example/driver",
			State:   manifest.SystemExtensionAbsent,
		},
		{
			Bundle: "registry.example/driver@sha256:" + strings.Repeat("a", 64),
			State:  manifest.SystemExtensionAbsent,
		},
	} {
		desired, _, err := ResolveSelection(context.Background(), extensionrelease.Target{}, nil, source, func(context.Context, ResolveRequest) (Resolved, error) {
			t.Fatal("removal fetched an artifact")
			return Resolved{}, nil
		})
		if err != nil || desired.State != manifest.SystemExtensionAbsent || desired.Repository() != "registry.example/driver" {
			t.Fatalf("removal without target metadata: %#v, %v", desired, err)
		}
	}
}
