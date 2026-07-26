package payloadbundle

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestParseReferenceUsesCommonOCIShape(t *testing.T) {
	pin := "sha256:" + strings.Repeat("a", 64)
	ref, err := ParseReference("registry.example/katl-dev/payload:v1@" + pin)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Repository != "registry.example/katl-dev/payload" || ref.Tag != "v1" ||
		ref.ManifestDigest != pin || ref.Source != "https://registry.example/v2/katl-dev/payload" {
		t.Fatalf("reference = %#v", ref)
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
