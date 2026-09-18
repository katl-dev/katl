package payloadbundle

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/distribution/reference"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestParseReference(t *testing.T) {
	pin := "sha256:" + strings.Repeat("a", 64)
	for _, test := range []struct{ value, name, tag, digest string }{
		{"registry.example/katl/payload:v1", "registry.example/katl/payload", "v1", ""},
		{"localhost:5000/katl/payload@" + pin, "localhost:5000/katl/payload", "", pin},
		{"registry.example/katl/payload:v1@" + pin, "registry.example/katl/payload", "v1", pin},
	} {
		t.Run(test.value, func(t *testing.T) {
			ref, err := ParseReference(test.value)
			if err != nil {
				t.Fatal(err)
			}
			if ref.Name() != test.name || ref.String() != test.value {
				t.Fatalf("reference = %s", ref)
			}
			tagged, hasTag := ref.(reference.Tagged)
			if hasTag != (test.tag != "") || hasTag && tagged.Tag() != test.tag {
				t.Fatalf("tagged reference = %v", tagged)
			}
			pinned, hasDigest := ref.(reference.Digested)
			if hasDigest != (test.digest != "") || hasDigest && pinned.Digest().String() != test.digest {
				t.Fatalf("pinned reference = %v", pinned)
			}
		})
	}
}

func TestParseReferenceRejectsInvalid(t *testing.T) {
	for _, value := range []string{
		"", "https://registry.example/katl/payload:v1", "registry.example/katl/payload",
		"registry.example/katl/payload:", "registry.example/katl/payload:bad tag",
		"registry.example/katl/payload:bad/tag", "registry.example/Upper/payload:v1",
		"registry.example/katl/payload@sha256:abc", "registry.example/katl/payload@sha256:" + strings.Repeat("z", 64),
		"registry.example/katl/payload@sha256:" + strings.Repeat("a", 64) + "@sha256:" + strings.Repeat("b", 64),
		"alpine:latest",
	} {
		t.Run(value, func(t *testing.T) {
			if _, err := ParseReference(value); err == nil {
				t.Fatal("accepted invalid or unqualified reference")
			}
		})
	}
}

func TestPackUsesTheSameVerifiedEnvelopeAsPublish(t *testing.T) {
	blob := DescribeBytes([]byte("payload"), "systemd-sysext", "application/vnd.katl.sysext.raw.v1", "routing.raw")
	packed, err := Pack(context.Background(), PackRequest{
		ArtifactType: "application/vnd.katl.test.bundle.v1", ConfigMediaType: "application/vnd.katl.test.bundle.v1+json",
		Config: []byte(`{"kind":"TestBundle"}`), Blobs: []Blob{blob},
		Annotations: map[string]string{"dev.katl.bundle.kind": "test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !validDigest(packed.ManifestDigest) {
		t.Fatalf("manifest digest = %q", packed.ManifestDigest)
	}
	var manifest ocispec.Manifest
	if err := json.Unmarshal(packed.Manifest, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.ArtifactType != "application/vnd.katl.test.bundle.v1" || len(manifest.Layers) != 1 ||
		manifest.Layers[0].Digest.String() != blob.Descriptor.Digest {
		t.Fatalf("manifest = %#v", manifest)
	}
}

func TestPackAcceptsPayloadLargerThanORASReadAllLimit(t *testing.T) {
	data := make([]byte, 32*1024*1024+1)
	data[len(data)-1] = 1
	blob := DescribeBytes(data, "systemd-sysext", "application/vnd.katl.sysext.raw.v1", "kubernetes.raw")

	packed, err := Pack(context.Background(), PackRequest{
		ArtifactType: "application/vnd.katl.test.bundle.v1", ConfigMediaType: "application/vnd.katl.test.bundle.v1+json",
		Config: []byte(`{"kind":"LargeTestBundle"}`), Blobs: []Blob{blob},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !validDigest(packed.ManifestDigest) {
		t.Fatalf("manifest digest = %q", packed.ManifestDigest)
	}
}

func TestManifestDigestTagDoesNotUseOCIReferrersFallbackNamespace(t *testing.T) {
	manifestDigest := "sha256:" + strings.Repeat("a", 64)
	tag, err := ManifestDigestTag(manifestDigest)
	if err != nil {
		t.Fatal(err)
	}
	if tag != "manifest-sha256-"+strings.Repeat("a", 64) {
		t.Fatalf("manifest digest tag = %q", tag)
	}
	if strings.HasPrefix(tag, "sha256-") {
		t.Fatalf("manifest digest tag %q collides with the OCI referrers fallback namespace", tag)
	}
}

func TestVerifyDescriptorsRequiresExactLayerSet(t *testing.T) {
	data := []byte("payload")
	blob := DescribeBytes(data, "systemd-sysext", "application/vnd.katl.sysext.raw.v1", "routing.raw")
	layer := ocispec.Descriptor{
		MediaType: blob.Descriptor.MediaType,
		Digest:    digest.FromBytes(data),
		Size:      int64(len(data)),
	}
	if err := VerifyDescriptors(ocispec.Manifest{Layers: []ocispec.Descriptor{layer}}, []Descriptor{blob.Descriptor}); err != nil {
		t.Fatalf("VerifyDescriptors() error = %v", err)
	}
	layer.Size++
	if err := VerifyDescriptors(ocispec.Manifest{Layers: []ocispec.Descriptor{layer}}, []Descriptor{blob.Descriptor}); err == nil {
		t.Fatal("VerifyDescriptors() accepted a mismatched OCI layer")
	}
}
