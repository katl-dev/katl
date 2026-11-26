package payloadbundle

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/registry/remote"
)

func TestPublishLayout(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		wantError string
		deadline  bool
		maxJitter bool
	}{
		{name: "transient 404", status: http.StatusNotFound},
		{name: "transient 503", status: http.StatusServiceUnavailable},
		{name: "persistent 404 with minimum jitter", status: http.StatusNotFound, wantError: "not found", deadline: true},
		{name: "persistent 404 with maximum jitter", status: http.StatusNotFound, wantError: "not found", deadline: true, maxJitter: true},
		{name: "wrong digest", status: http.StatusOK, wantError: "observed digest"},
		{name: "auth failure", status: http.StatusForbidden, wantError: "Forbidden"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				testPublishLayout(t, tc.status, tc.wantError, tc.deadline, tc.maxJitter)
			})
		})
	}
}

func testPublishLayout(t *testing.T, firstStatus int, wantError string, deadline, maxJitter bool) {
	t.Helper()
	ctx := t.Context()
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
	tag := "manifest-sha256-" + strings.TrimPrefix(packed.ManifestDigest, "sha256:")
	alias := "v2026.9.0-beta.15-standard-x86_64-driver-1.0.0"
	conflictingData := []byte("different manifest")
	conflict := digest.FromBytes(conflictingData).String()

	// Observe registry bytes independently of the bundle reader. Repacking a
	// semantically equivalent manifest must not change the qualified identity.
	var mu sync.Mutex
	objects := map[string][]byte{}
	tags := map[string]string{}
	writes := 0
	requests := 0
	manifestPuts := map[string]int{}
	var retentionReads []time.Time
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			uploadedTag := strings.TrimPrefix(path, "manifests/")
			tags[uploadedTag] = pin
			manifestPuts[uploadedTag]++
			writes++
			w.Header().Set("Docker-Content-Digest", pin)
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodGet || r.Method == http.MethodHead:
			pin := strings.TrimPrefix(strings.TrimPrefix(path, "manifests/"), "blobs/")
			if tagged, ok := tags[pin]; ok {
				pin = tagged
			}
			data, ok := objects[pin]
			if path == "manifests/"+tag && manifestPuts[tag] > 0 {
				retentionReads = append(retentionReads, time.Now())
				if deadline || len(retentionReads) == 1 {
					if firstStatus != http.StatusOK {
						w.WriteHeader(firstStatus)
						return
					}
					pin, data, ok = conflict, conflictingData, true
				}
			}
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
	})
	// Keep registry requests inside the synthetic clock so that context
	// deadlines and backoff timers advance together without wall-clock sleeps.
	client := &http.Client{Transport: registryTransport{handler}}
	repositoryName := "registry.example.test/extensions/driver"
	request := FetchRequest{
		LayoutDir:       directory,
		Reference:       repositoryName + "@" + packed.ManifestDigest,
		ArtifactType:    "application/vnd.katl.test.v1",
		ConfigMediaType: "application/vnd.katl.test.config.v1+json",
		Client:          client,
	}
	aliased := request
	aliased.Reference = repositoryName + ":" + alias + "@" + packed.ManifestDigest
	randomN := func(n int64) int64 {
		if maxJitter {
			return n - 1
		}
		return 0
	}

	start := time.Now()
	published, err := publishLayout(ctx, aliased, randomN)
	if wantError != "" {
		for _, detail := range []string{repositoryName + ":" + tag, packed.ManifestDigest, wantError} {
			if err == nil || !strings.Contains(err.Error(), detail) {
				t.Fatalf("publication error = %v, want %q", err, detail)
			}
		}
		if firstStatus == http.StatusOK && !strings.Contains(err.Error(), conflict) {
			t.Errorf("publication error = %v, want conflicting digest %s", err, conflict)
		}
		if manifestPuts[tag] != 1 || manifestPuts[alias] != 0 {
			t.Errorf("manifest PUTs = %v, want only one PUT for %s", manifestPuts, tag)
		}
		if deadline {
			if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != 30*time.Second {
				t.Fatalf("publication took %s and returned %v, want deadline after 30s", time.Since(start), err)
			}
			var delays []time.Duration
			for i := 1; i < len(retentionReads); i++ {
				delays = append(delays, retentionReads[i].Sub(retentionReads[i-1]))
			}
			want := []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, time.Second, 2 * time.Second,
				4 * time.Second, 4 * time.Second, 4 * time.Second, 4 * time.Second, 4 * time.Second, 4 * time.Second}
			if maxJitter {
				want = []time.Duration{500*time.Millisecond - 1, time.Second - 1, 2*time.Second - 1,
					4*time.Second - 1, 8*time.Second - 1, 8*time.Second - 1}
			}
			if !slices.Equal(delays, want) {
				t.Errorf("backoff delays = %v, want %v", delays, want)
			}
		} else if len(retentionReads) != 1 || time.Since(start) != 0 {
			t.Errorf("terminal read took %s with %d reads, want one immediate read", time.Since(start), len(retentionReads))
		}
		return
	}

	if err != nil || published.ManifestDigest != packed.ManifestDigest || published.Existing {
		t.Fatalf("initial publication: %+v, %v", published, err)
	}
	initialWrites := writes
	if manifestPuts[tag] != 1 || manifestPuts[alias] != 1 || len(retentionReads) != 2 {
		t.Errorf("manifest PUTs = %v, retention reads = %d, want one PUT per tag and two retention reads", manifestPuts, len(retentionReads))
	}
	published, err = PublishLayout(ctx, aliased)
	if err != nil || published.ManifestDigest != packed.ManifestDigest || !published.Existing {
		t.Fatalf("repeated publication: %+v, %v", published, err)
	}
	if !bytes.Equal(objects[packed.ManifestDigest], packed.Manifest) || !bytes.Equal(objects[blob.Descriptor.Digest], blob.Data) || writes != initialWrites {
		t.Errorf("registry changed qualified bytes or repeated writes: writes=%d", writes)
	}

	repository, err := remote.NewRepository(repositoryName)
	if err != nil {
		t.Fatal(err)
	}
	repository.Client = client
	for _, name := range []string{tag, alias} {
		resolved, err := repository.Resolve(ctx, name)
		if err != nil || resolved.Digest.String() != packed.ManifestDigest {
			t.Errorf("resolve %s = %s, %v (want %s)", name, resolved.Digest, err, packed.ManifestDigest)
		}
	}
	// The build-and-publish interface uses the same transport contract.
	published, err = Publish(ctx, PublishRequest{
		Reference:       repositoryName + ":release-v1",
		ArtifactType:    build.ArtifactType,
		ConfigMediaType: build.ConfigMediaType,
		Config:          build.Config,
		Blobs:           build.Blobs,
		Annotations:     build.Annotations,
		Client:          client,
	})
	if err != nil || published.ManifestDigest != packed.ManifestDigest {
		t.Fatalf("build-and-publish identity: %+v, %v", published, err)
	}
	initialWrites = writes

	// A conflicting retention tag must not be overwritten, even though its name
	// was derived from the desired digest rather than supplied by an operator.
	objects[conflict] = conflictingData
	tags[tag] = conflict
	if _, err := PublishLayout(ctx, request); err == nil || !strings.Contains(err.Error(), "refusing to replace") {
		t.Fatalf("conflicting tag error = %v", err)
	}
	if writes != initialWrites || tags[tag] != conflict {
		t.Error("publication changed a conflicting immutable tag")
	}
	tags[tag] = packed.ManifestDigest
	tags[alias] = conflict
	if _, err := PublishLayout(ctx, aliased); err == nil || !strings.Contains(err.Error(), "refusing to replace") {
		t.Fatalf("conflicting alias error = %v", err)
	}
	if writes != initialWrites || tags[alias] != conflict {
		t.Error("publication changed a conflicting readable tag")
	}
	tags[alias] = packed.ManifestDigest
	beforeCorruption := requests

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
	if requests != beforeCorruption {
		t.Fatal("contacted registry before verifying local qualification inputs")
	}
}

type registryTransport struct {
	handler http.Handler
}

func (transport registryTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Body != nil {
		defer request.Body.Close()
	}
	recorder := httptest.NewRecorder()
	transport.handler.ServeHTTP(recorder, request)
	response := recorder.Result()
	response.Request = request
	return response, nil
}
