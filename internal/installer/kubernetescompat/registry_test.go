package kubernetescompat

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/installer/payloadbundle"
	"github.com/katl-dev/katl/internal/installer/sysextcatalog"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/memory"
)

func TestPublishedSelection(t *testing.T) {
	for _, test := range []struct {
		name, version, runtime string
		missing                bool
		wantError              string
	}{
		{name: "new release", version: "v1.37.0", runtime: "katl-runtime-1"},
		{name: "wrong version", version: "v1.36.4", runtime: "katl-runtime-1", wantError: "does not match requested"},
		{name: "wrong runtime", version: "v1.37.0", runtime: "katl-runtime-2", wantError: "does not support"},
		{name: "missing payload", version: "v1.37.0", runtime: "katl-runtime-1", missing: true, wantError: "missing Kubernetes payload"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			store := memory.New()
			bundle := sysextcatalog.KubernetesPayloadBundle{
				APIVersion: "payload.katl.dev/v1alpha1", Kind: "KubernetesPayloadBundle", Name: "katl-kubernetes", ArtifactKind: "katl.kubernetes-payload.v1",
				PayloadVersion: test.version, ArtifactVersion: test.version + "-katl.1", Architecture: "x86_64", SupportedRuntimeInterfaces: []string{test.runtime},
			}
			var layers []ocispec.Descriptor
			for _, role := range []string{"systemd-sysext", "sysext-metadata", "package-provenance", "catalog-fragment"} {
				if test.missing && role == "systemd-sysext" {
					continue
				}
				// Payload bytes are deliberately absent: selection must only fetch metadata.
				d := digest.FromString(role)
				layers = append(layers, ocispec.Descriptor{MediaType: "application/octet-stream", Digest: d, Size: int64(len(role))})
				bundle.Payloads = append(bundle.Payloads, payloadbundle.Descriptor{Role: role, MediaType: "application/octet-stream", Digest: d.String(), SizeBytes: int64(len(role)), FileName: role})
			}
			data, err := json.Marshal(bundle)
			if err != nil {
				t.Fatal(err)
			}
			config, err := oras.PushBytes(ctx, store, sysextcatalog.KubernetesBundleConfigType, data)
			if err != nil {
				t.Fatal(err)
			}
			manifest, err := oras.PackManifest(ctx, store, oras.PackManifestVersion1_1, sysextcatalog.KubernetesBundleArtifactType, oras.PackManifestOptions{ConfigDescriptor: &config, Layers: layers})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Tag(ctx, manifest, "compatible-v1.37.0-x86_64-katl-runtime-1"); err != nil {
				t.Fatal(err)
			}
			if err := store.Tag(ctx, manifest, manifest.Digest.String()); err != nil {
				t.Fatal(err)
			}

			entry, err := ResolveTarget(ctx, store, Repository, "compatible-v1.37.0-x86_64-katl-runtime-1", Request{KubernetesVersion: "v1.37.0"})
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("error = %v, want %q", err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := Repository + ":v1.37.0-katl.1@" + manifest.Digest.String()
			if entry.Bundle != want {
				t.Fatalf("bundle = %s, want %s", entry.Bundle, want)
			}
			pinned, err := ResolveTarget(ctx, store, Repository, manifest.Digest.String(), Request{KubernetesVersion: "v1.37.0"})
			if err != nil || pinned.Bundle != want {
				t.Fatalf("digest selection = %v, %v", pinned, err)
			}
		})
	}
}

func TestOfflineSelection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	entry, err := ResolveAvailable(ctx, Request{KubernetesVersion: "v1.36.1"})
	if err != nil || !strings.Contains(entry.Bundle, "@sha256:") {
		t.Fatalf("offline selection = %v, %v", entry, err)
	}
}
