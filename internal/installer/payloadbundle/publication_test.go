package payloadbundle

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestPublishLayout(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	blob := DescribeBytes([]byte("qualified driver bytes"), "systemd-sysext", "application/vnd.katl.sysext.raw.v1", "driver.raw")
	build := PackRequest{
		ArtifactType:    "application/vnd.katl.test.v1",
		ConfigMediaType: "application/vnd.katl.test.config.v1+json",
		Config:          []byte(`{"qualified":true}`),
		Blobs:           []Blob{blob},
		Annotations: map[string]string{
			"example.test/qualification": "preserve this annotation",
			ocispec.AnnotationCreated:    "2026-09-23T00:00:00Z",
		},
	}
	packed, err := Export(ctx, directory, build)
	if err != nil {
		t.Fatal(err)
	}

	// Observe registry bytes independently of the bundle reader. Repacking a
	// semantically equivalent manifest must not change the qualified identity.
	var mu sync.Mutex
	objects := map[string][]byte{}
	tags := map[string]string{}
	writes := 0
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		requests++
		path := strings.TrimPrefix(r.URL.Path, "/v2/extensions/driver/")
		switch {
		case r.Method == http.MethodPost && path == "blobs/uploads/":
			w.Header().Set("Location", "/v2/extensions/driver/blobs/uploads/test")
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodPut && path == "blobs/uploads/test":
			data, _ := io.ReadAll(r.Body)
			pin := r.URL.Query().Get("digest")
			if digest.FromBytes(data).String() != pin {
				http.Error(w, "incorrect upload digest", http.StatusBadRequest)
				return
			}
			objects[pin] = data
			writes++
			w.Header().Set("Location", "/v2/extensions/driver/blobs/"+pin)
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPut && strings.HasPrefix(path, "manifests/"):
			data, _ := io.ReadAll(r.Body)
			pin := digest.FromBytes(data).String()
			objects[pin] = data
			tags[strings.TrimPrefix(path, "manifests/")] = pin
			writes++
			w.Header().Set("Docker-Content-Digest", pin)
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodGet || r.Method == http.MethodHead:
			pin := strings.TrimPrefix(strings.TrimPrefix(path, "manifests/"), "blobs/")
			if tagged, ok := tags[pin]; ok {
				pin = tagged
			}
			data, ok := objects[pin]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			if strings.HasPrefix(path, "manifests/") {
				w.Header().Set("Content-Type", ocispec.MediaTypeImageManifest)
			}
			w.Header().Set("Docker-Content-Digest", pin)
			w.Header().Set("Content-Length", fmt.Sprint(len(data)))
			if r.Method == http.MethodGet {
				_, _ = w.Write(data)
			}
		default:
			t.Errorf("unexpected registry request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()
	request := FetchRequest{
		LayoutDir:       directory,
		Reference:       strings.TrimPrefix(server.URL, "https://") + "/extensions/driver@" + packed.ManifestDigest,
		ArtifactType:    "application/vnd.katl.test.v1",
		ConfigMediaType: "application/vnd.katl.test.config.v1+json",
		Client:          server.Client(),
	}
	initialWrites := 0
	for attempt := 0; attempt < 2; attempt++ {
		published, err := PublishLayout(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		if published.ManifestDigest != packed.ManifestDigest || published.Existing != (attempt == 1) {
			t.Fatalf("publication identity or idempotence: %+v", published)
		}
		mu.Lock()
		if attempt == 0 {
			initialWrites = writes
		}
		if !bytes.Equal(objects[packed.ManifestDigest], packed.Manifest) || !bytes.Equal(objects[blob.Descriptor.Digest], blob.Data) || writes != initialWrites {
			t.Errorf("registry changed qualified bytes or repeated writes: writes=%d", writes)
		}
		mu.Unlock()
	}
	// The build-and-publish interface uses the same transport contract.
	published, err := Publish(ctx, PublishRequest{
		Reference:       strings.TrimPrefix(server.URL, "https://") + "/extensions/driver:release-v1",
		ArtifactType:    build.ArtifactType,
		ConfigMediaType: build.ConfigMediaType,
		Config:          build.Config,
		Blobs:           build.Blobs,
		Annotations:     build.Annotations,
		Client:          server.Client(),
	})
	if err != nil || published.ManifestDigest != packed.ManifestDigest {
		t.Fatalf("build-and-publish identity: %+v, %v", published, err)
	}
	mu.Lock()
	initialWrites = writes
	mu.Unlock()

	// A conflicting retention tag must not be overwritten, even though its name
	// was derived from the desired digest rather than supplied by an operator.
	mu.Lock()
	tag := "manifest-sha256-" + strings.TrimPrefix(packed.ManifestDigest, "sha256:")
	conflict := digest.FromString("different manifest").String()
	objects[conflict] = []byte("different manifest")
	tags[tag] = conflict
	mu.Unlock()
	if _, err := PublishLayout(ctx, request); err == nil || !strings.Contains(err.Error(), "refusing to replace") {
		t.Fatalf("conflicting tag error = %v", err)
	}
	mu.Lock()
	if writes != initialWrites || tags[tag] != conflict {
		t.Error("publication changed a conflicting immutable tag")
	}
	tags[tag] = packed.ManifestDigest
	beforeCorruption := requests
	mu.Unlock()

	// Existing remote content cannot excuse corrupt local qualification inputs.
	path := filepath.Join(directory, "blobs/sha256", strings.TrimPrefix(blob.Descriptor.Digest, "sha256:"))
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := PublishLayout(ctx, request); err == nil {
		t.Fatal("published a corrupt local closure")
	}
	mu.Lock()
	defer mu.Unlock()
	if requests != beforeCorruption {
		t.Fatal("contacted registry before verifying local qualification inputs")
	}
}
