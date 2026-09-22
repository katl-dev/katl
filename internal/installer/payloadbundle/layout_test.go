package payloadbundle

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestLayoutClosure(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	packed, err := Export(ctx, directory, PackRequest{
		ArtifactType:    "application/vnd.katl.test.bundle",
		ConfigMediaType: "application/vnd.katl.test.config",
		Config:          []byte(`{"kind":"TestBundle"}`),
		Blobs:           []Blob{DescribeBytes([]byte("driver image"), "systemd-sysext", "application/vnd.katl.sysext.raw.v1", "driver.raw")},
		Annotations: map[string]string{
			ocispec.AnnotationCreated: "2026-09-22T00:00:00Z",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Observe the standardized layout directly, not solely through our reader.
	layoutBytes, err := os.ReadFile(filepath.Join(directory, "oci-layout"))
	if err != nil {
		t.Fatal(err)
	}
	var layout ocispec.ImageLayout
	if err := json.Unmarshal(layoutBytes, &layout); err != nil || layout.Version != "1.0.0" {
		t.Fatalf("OCI layout header = %s, %v", layoutBytes, err)
	}
	var index ocispec.Index
	data, err := os.ReadFile(filepath.Join(directory, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &index); err != nil || len(index.Manifests) != 1 {
		t.Fatalf("OCI index = %s, %v", data, err)
	}
	manifestBytes, err := os.ReadFile(filepath.Join(directory, "blobs/sha256", index.Manifests[0].Digest.Encoded()))
	if err != nil || fmt.Sprintf("sha256:%x", sha256.Sum256(manifestBytes)) != packed.ManifestDigest {
		t.Fatalf("OCI manifest bytes do not match exported identity: %v", err)
	}
	var manifest ocispec.Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil || len(manifest.Layers) != 1 {
		t.Fatalf("OCI manifest = %s, %v", manifestBytes, err)
	}
	image, err := os.ReadFile(filepath.Join(directory, "blobs/sha256", manifest.Layers[0].Digest.Encoded()))
	if err != nil || string(image) != "driver image" {
		t.Fatalf("stored driver bytes = %s, %v", image, err)
	}

	request := FetchRequest{
		LayoutDir:       directory,
		Reference:       "registry.invalid/drivers/test@" + packed.ManifestDigest,
		ArtifactType:    "application/vnd.katl.test.bundle",
		ConfigMediaType: "application/vnd.katl.test.config",
		Client: &http.Client{
			Transport: rejectNetwork{t: t},
		},
	}
	copied := t.TempDir()
	if err := CopyLayout(ctx, copied, request); err != nil {
		t.Fatal(err)
	}
	request.LayoutDir = copied
	fetched, err := Fetch(ctx, request)
	if err != nil || fetched.ManifestDigest != packed.ManifestDigest || string(fetched.Config) != `{"kind":"TestBundle"}` {
		t.Fatalf("offline identity/config = %s %s, %v", fetched.ManifestDigest, fetched.Config, err)
	}

	blobPath := filepath.Join(copied, "blobs/sha256", manifest.Layers[0].Digest.Encoded())
	if err := os.Chmod(blobPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blobPath, []byte("tampered data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Fetch(ctx, request); err == nil {
		t.Fatal("accepted corrupt local payload")
	}

	copyRequest := request
	copyRequest.LayoutDir = directory
	if err := CopyLayout(ctx, copied, copyRequest); err == nil {
		t.Fatal("an existing damaged destination satisfied the verified copy")
	}

	if err := os.Remove(filepath.Join(copied, "blobs/sha256", manifest.Layers[0].Digest.Encoded())); err != nil {
		t.Fatal(err)
	}
	if _, err := Fetch(ctx, request); err == nil {
		t.Fatal("accepted incomplete local artifact closure")
	}
	request.Reference = "registry.invalid/drivers/test:latest"
	if _, err := Fetch(ctx, request); err == nil || !strings.Contains(err.Error(), "digest pin") {
		t.Fatalf("local mutable selection = %v", err)
	}
}

type rejectNetwork struct{ t *testing.T }

func (transport rejectNetwork) RoundTrip(*http.Request) (*http.Response, error) {
	transport.t.Error("local artifact resolution attempted registry access")
	return nil, fmt.Errorf("registry unavailable")
}
