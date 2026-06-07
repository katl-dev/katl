package katlosimage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zariel/katl/internal/installer/manifest"
)

func TestResolveDirectoryAcceptsInstallImage(t *testing.T) {
	root, _ := writeImagePayload(t, func(*Index) {})

	payload, err := ResolveDirectory(context.Background(), root, expectedImage())
	if err != nil {
		t.Fatalf("ResolveDirectory() error = %v", err)
	}

	if payload.Runtime.Role != ComponentRuntimeRoot || payload.Boot.Role != ComponentRuntimeUKI || payload.Kubernetes.Role != ComponentKubernetes {
		t.Fatalf("resolved components = %#v %#v %#v", payload.Runtime, payload.Boot, payload.Kubernetes)
	}

	createdAt := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	request, err := payload.FirstInstallRequest(FirstInstallRequest{
		GenerationID:      "2026.06.06-001",
		RootSlot:          "root-a",
		RootPartitionUUID: "11111111-2222-3333-4444-555555555555",
		UKIPath:           "/efi/EFI/Linux/katl-2026.06.06-001.efi",
		CreatedAt:         createdAt,
	})
	if err != nil {
		t.Fatalf("FirstInstallRequest() error = %v", err)
	}
	if request.RuntimeArtifactSHA256 != payload.Runtime.SHA256 || request.Sysexts[0].SHA256 != payload.Kubernetes.SHA256 {
		t.Fatalf("generation request digests = %#v, payload = %#v", request, payload)
	}
	if request.RuntimeInterface != "katl-runtime-1" || request.RuntimeArchitecture != "x86_64" {
		t.Fatalf("generation runtime fields = %#v", request)
	}
	if request.Sysexts[0].Path != "/var/lib/katl/generations/2026.06.06-001/sysext/katl-kubernetes.raw" {
		t.Fatalf("Kubernetes sysext path = %q", request.Sysexts[0].Path)
	}
	if request.Sysexts[0].ActivationPath != "/run/extensions/katl-kubernetes.raw" {
		t.Fatalf("Kubernetes sysext activation path = %q", request.Sysexts[0].ActivationPath)
	}
	if !request.CreatedAt.Equal(createdAt) {
		t.Fatalf("createdAt = %s, want %s", request.CreatedAt, createdAt)
	}
	if got := strings.Join(request.KernelCommandLine, " "); !strings.Contains(got, "katl.generation=2026.06.06-001") {
		t.Fatalf("kernel command line = %#v", request.KernelCommandLine)
	}
}

func TestResolveDirectoryRejectsInvalidInstallImage(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Index)
		raw  func(Index) []byte
		want string
	}{
		{
			name: "digest mismatch",
			edit: func(index *Index) {
				index.Components[0].SHA256 = strings.Repeat("b", sha256.Size*2)
			},
			want: "digest",
		},
		{
			name: "missing component",
			edit: func(index *Index) {
				index.Components = append(index.Components[:1], index.Components[2:]...)
			},
			want: "missing required component role",
		},
		{
			name: "architecture mismatch",
			edit: func(index *Index) {
				index.Architecture = "aarch64"
			},
			want: "architecture",
		},
		{
			name: "runtime compatibility mismatch",
			edit: func(index *Index) {
				index.Components[2].Compatibility.RuntimeRoot.ArtifactSHA256 = strings.Repeat("c", sha256.Size*2)
			},
			want: "Kubernetes sysext root digest",
		},
		{
			name: "node scoped field",
			raw: func(index Index) []byte {
				data, err := json.Marshal(map[string]any{
					"apiVersion":       index.APIVersion,
					"kind":             index.Kind,
					"imageRole":        index.ImageRole,
					"format":           index.Format,
					"version":          index.Version,
					"buildID":          index.BuildID,
					"architecture":     index.Architecture,
					"runtimeInterface": index.RuntimeInterface,
					"createdAt":        index.CreatedAt,
					"components":       index.Components,
					"node":             map[string]string{"hostname": "node-01"},
				})
				if err != nil {
					t.Fatalf("marshal raw index: %v", err)
				}
				return data
			},
			want: `unknown field "node"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root, index := writeImagePayload(t, tt.edit)
			if tt.raw != nil {
				if err := os.WriteFile(filepath.Join(root, "katlos", "image.json"), tt.raw(index), 0o600); err != nil {
					t.Fatalf("write raw index: %v", err)
				}
			}

			_, err := ResolveDirectory(context.Background(), root, expectedImage())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ResolveDirectory() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestLocalResolverAcceptsDirectoryRef(t *testing.T) {
	mediaRoot := t.TempDir()
	imageRoot := filepath.Join(mediaRoot, "payloads", "katlos-install.squashfs")
	index := writeImagePayloadAt(t, imageRoot, func(*Index) {})
	expected := expectedImage()

	payload, err := (LocalResolver{MediaRoot: mediaRoot}).ResolveKatlosImage(context.Background(), expected)
	if err != nil {
		t.Fatalf("ResolveKatlosImage() error = %v", err)
	}
	if payload.Root != imageRoot || payload.Index.BuildID != index.BuildID {
		t.Fatalf("payload = %#v, index = %#v", payload, index)
	}
}

func TestLocalResolverMountsFileRef(t *testing.T) {
	mediaRoot := t.TempDir()
	imagePath := filepath.Join(mediaRoot, "payloads", "katlos-install.squashfs")
	imageBytes := []byte("squashfs image bytes")
	if err := os.MkdirAll(filepath.Dir(imagePath), 0o755); err != nil {
		t.Fatalf("mkdir image dir: %v", err)
	}
	if err := os.WriteFile(imagePath, imageBytes, 0o600); err != nil {
		t.Fatalf("write image file: %v", err)
	}
	sum := sha256.Sum256(imageBytes)
	expected := expectedImage()
	expected.SHA256 = hex.EncodeToString(sum[:])
	expected.SizeBytes = uint64(len(imageBytes))
	mounter := &fixtureMountRunner{populate: func(root string) {
		writeImagePayloadAt(t, root, func(*Index) {})
	}}

	payload, err := (LocalResolver{
		MediaRoot: mediaRoot,
		WorkDir:   filepath.Join(t.TempDir(), "mounts"),
		Commands:  mounter,
	}).ResolveKatlosImage(context.Background(), expected)
	if err != nil {
		t.Fatalf("ResolveKatlosImage() error = %v", err)
	}

	wantCall := []string{"mount", "-o", "ro,loop", imagePath}
	if len(mounter.calls) != 1 || !reflect.DeepEqual(mounter.calls[0][:4], wantCall) {
		t.Fatalf("mount calls = %#v, want prefix %#v", mounter.calls, wantCall)
	}
	if payload.Root != mounter.calls[0][4] {
		t.Fatalf("payload root = %q, mountpoint = %q", payload.Root, mounter.calls[0][4])
	}
}

func TestLocalResolverRejectsBadTopLevelDigest(t *testing.T) {
	mediaRoot := t.TempDir()
	imagePath := filepath.Join(mediaRoot, "payloads", "katlos-install.squashfs")
	if err := os.MkdirAll(filepath.Dir(imagePath), 0o755); err != nil {
		t.Fatalf("mkdir image dir: %v", err)
	}
	if err := os.WriteFile(imagePath, []byte("bad image"), 0o600); err != nil {
		t.Fatalf("write image file: %v", err)
	}
	expected := expectedImage()
	expected.SizeBytes = uint64(len("bad image"))

	_, err := (LocalResolver{
		MediaRoot: mediaRoot,
		WorkDir:   t.TempDir(),
		Commands:  &fixtureMountRunner{},
	}).ResolveKatlosImage(context.Background(), expected)
	if err == nil || !strings.Contains(err.Error(), "does not match manifest") {
		t.Fatalf("ResolveKatlosImage() error = %v, want digest mismatch", err)
	}
}

func TestRemoteResolverDownloadsAndMountsURL(t *testing.T) {
	imageBytes := []byte("remote squashfs image")
	sum := sha256.Sum256(imageBytes)
	expected := expectedImage()
	expected.LocalRef = ""
	expected.URL = "https://artifacts.example.invalid/katlos-install.squashfs"
	expected.SHA256 = hex.EncodeToString(sum[:])
	expected.SizeBytes = uint64(len(imageBytes))
	mounter := &fixtureMountRunner{populate: func(root string) {
		writeImagePayloadAt(t, root, func(*Index) {})
	}}
	client := &fixtureHTTPClient{response: &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Body:       io.NopCloser(bytes.NewReader(imageBytes)),
	}}

	payload, err := (RemoteResolver{
		WorkDir:  filepath.Join(t.TempDir(), "work"),
		Commands: mounter,
		Client:   client,
	}).ResolveKatlosImage(context.Background(), expected)
	if err != nil {
		t.Fatalf("ResolveKatlosImage() error = %v", err)
	}

	if client.requestURL != expected.URL {
		t.Fatalf("request URL = %q, want %q", client.requestURL, expected.URL)
	}
	if len(mounter.calls) != 1 || mounter.calls[0][0] != "mount" {
		t.Fatalf("mount calls = %#v", mounter.calls)
	}
	if payload.Root != mounter.calls[0][4] {
		t.Fatalf("payload root = %q, mountpoint = %q", payload.Root, mounter.calls[0][4])
	}
}

func TestRemoteResolverRejectsBadTopLevelDigest(t *testing.T) {
	expected := expectedImage()
	expected.LocalRef = ""
	expected.URL = "https://artifacts.example.invalid/katlos-install.squashfs"
	expected.SizeBytes = uint64(len("remote image"))

	_, err := (RemoteResolver{
		WorkDir:  t.TempDir(),
		Commands: &fixtureMountRunner{},
		Client: &fixtureHTTPClient{response: &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Body:       io.NopCloser(strings.NewReader("remote image")),
		}},
	}).ResolveKatlosImage(context.Background(), expected)
	if err == nil || !strings.Contains(err.Error(), "does not match manifest") {
		t.Fatalf("ResolveKatlosImage() error = %v, want digest mismatch", err)
	}
}

func TestResolverSelectsLocalOrRemoteRef(t *testing.T) {
	mediaRoot := t.TempDir()
	imageRoot := filepath.Join(mediaRoot, "payloads", "katlos-install.squashfs")
	writeImagePayloadAt(t, imageRoot, func(*Index) {})
	if _, err := (Resolver{MediaRoot: mediaRoot}).ResolveKatlosImage(context.Background(), expectedImage()); err != nil {
		t.Fatalf("local ResolveKatlosImage() error = %v", err)
	}

	imageBytes := []byte("remote image")
	sum := sha256.Sum256(imageBytes)
	expected := expectedImage()
	expected.LocalRef = ""
	expected.URL = "https://artifacts.example.invalid/katlos-install.squashfs"
	expected.SHA256 = hex.EncodeToString(sum[:])
	expected.SizeBytes = uint64(len(imageBytes))
	mounter := &fixtureMountRunner{populate: func(root string) {
		writeImagePayloadAt(t, root, func(*Index) {})
	}}
	if _, err := (Resolver{
		WorkDir:  t.TempDir(),
		Commands: mounter,
		Client: &fixtureHTTPClient{response: &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Body:       io.NopCloser(bytes.NewReader(imageBytes)),
		}},
	}).ResolveKatlosImage(context.Background(), expected); err != nil {
		t.Fatalf("remote ResolveKatlosImage() error = %v", err)
	}
}

func writeImagePayload(t *testing.T, edit func(*Index)) (string, Index) {
	t.Helper()
	root := t.TempDir()
	index := writeImagePayloadAt(t, root, edit)
	return root, index
}

func writeImagePayloadAt(t *testing.T, root string, edit func(*Index)) Index {
	t.Helper()
	files := map[string][]byte{
		"components/runtime/root.squashfs": []byte("runtime root"),
		"components/boot/katl.efi":         []byte("runtime uki"),
		"components/sysext/kubernetes.raw": []byte("kubernetes sysext"),
	}
	digests := make(map[string]string, len(files))
	sizes := make(map[string]int64, len(files))
	for rel, data := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir component: %v", err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatalf("write component: %v", err)
		}
		sum := sha256.Sum256(data)
		digests[rel] = hex.EncodeToString(sum[:])
		sizes[rel] = int64(len(data))
	}

	index := Index{
		APIVersion:       APIVersion,
		Kind:             Kind,
		ImageRole:        RoleInstall,
		Format:           FormatSquashFS,
		Version:          "2026.06.06",
		BuildID:          "test-build",
		Architecture:     "x86_64",
		RuntimeInterface: "katl-runtime-1",
		CreatedAt:        "2026-06-06T12:00:00Z",
		Components: []Component{
			{
				Name:         "runtime-root",
				Role:         ComponentRuntimeRoot,
				Path:         "components/runtime/root.squashfs",
				Format:       "squashfs",
				SizeBytes:    sizes["components/runtime/root.squashfs"],
				SHA256:       digests["components/runtime/root.squashfs"],
				Version:      "2026.06.06",
				Architecture: "x86_64",
				Compatibility: Compatibility{
					RuntimeInterface: "katl-runtime-1",
				},
				InstallTarget: InstallTarget{
					Kind:         "root-slot",
					Filesystem:   "squashfs",
					MinSizeBytes: sizes["components/runtime/root.squashfs"],
				},
			},
			{
				Name:         "runtime-uki",
				Role:         ComponentRuntimeUKI,
				Path:         "components/boot/katl.efi",
				Format:       "uki",
				SizeBytes:    sizes["components/boot/katl.efi"],
				SHA256:       digests["components/boot/katl.efi"],
				Version:      "2026.06.06",
				Architecture: "x86_64",
				Compatibility: Compatibility{
					RuntimeInterface: "katl-runtime-1",
					RuntimeRoot: RuntimeRoot{
						Interface:      "katl-runtime-1",
						ArtifactPath:   "components/runtime/root.squashfs",
						ArtifactSHA256: digests["components/runtime/root.squashfs"],
					},
					KernelCommandLine: []string{"katl.generation=2026.06.06-001"},
				},
				InstallTarget: InstallTarget{
					Kind:     "esp-or-xbootldr",
					Filename: "katl.efi",
				},
			},
			{
				Name:           "kubernetes",
				Role:           ComponentKubernetes,
				Path:           "components/sysext/kubernetes.raw",
				Format:         "raw",
				SizeBytes:      sizes["components/sysext/kubernetes.raw"],
				SHA256:         digests["components/sysext/kubernetes.raw"],
				Version:        "v1.34.8",
				PayloadVersion: "v1.34.8",
				Architecture:   "x86_64",
				Compatibility: Compatibility{
					RuntimeInterface: "katl-runtime-1",
					RuntimeRoot: RuntimeRoot{
						Interface:      "katl-runtime-1",
						ArtifactPath:   "components/runtime/root.squashfs",
						ArtifactSHA256: digests["components/runtime/root.squashfs"],
					},
				},
				InstallTarget: InstallTarget{
					Kind: "systemd-sysext",
					Name: "kubernetes.raw",
				},
			},
		},
	}
	if edit != nil {
		edit(&index)
	}
	if err := os.MkdirAll(filepath.Join(root, "katlos"), 0o755); err != nil {
		t.Fatalf("mkdir katlos: %v", err)
	}
	data, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		t.Fatalf("marshal index: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "katlos", "image.json"), data, 0o600); err != nil {
		t.Fatalf("write index: %v", err)
	}
	return index
}

func expectedImage() manifest.KatlosImage {
	return manifest.KatlosImage{
		LocalRef:         "payloads/katlos-install.squashfs",
		SHA256:           strings.Repeat("a", sha256.Size*2),
		SizeBytes:        1024,
		Version:          "2026.06.06",
		Architecture:     "x86_64",
		RuntimeInterface: "katl-runtime-1",
		Role:             "install",
	}
}

type fixtureMountRunner struct {
	calls    [][]string
	populate func(root string)
}

func (r *fixtureMountRunner) Run(_ context.Context, name string, args ...string) error {
	call := append([]string{name}, args...)
	r.calls = append(r.calls, call)
	if name != "mount" || len(args) != 4 {
		return nil
	}
	if r.populate != nil {
		r.populate(args[3])
	}
	return nil
}

type fixtureHTTPClient struct {
	response   *http.Response
	err        error
	requestURL string
}

func (c *fixtureHTTPClient) Do(request *http.Request) (*http.Response, error) {
	c.requestURL = request.URL.String()
	return c.response, c.err
}
