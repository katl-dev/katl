package kubernetesbundle

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/installer/artifact"
	"github.com/katl-dev/katl/internal/installer/sysextcatalog"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
)

func TestFetchAndStage(t *testing.T) {
	fixture := writeFixture(t, "v1.36.0", "kubernetes sysext 1.36.0")
	server := fixtureServer(t, fixture.root)
	cacheDir := t.TempDir()

	staged, err := FetchAndStage(context.Background(), Request{
		Source:           server.URL,
		Ref:              fixture.ref(),
		CacheDir:         cacheDir,
		RuntimeInterface: "katl-runtime-1",
		Architecture:     "x86_64",
		Client:           server.Client(),
	})
	if err != nil {
		t.Fatalf("FetchAndStage() error = %v", err)
	}

	if staged.PayloadVersion != "v1.36.0" || staged.Architecture != "x86_64" {
		t.Fatalf("staged identity = %#v", staged)
	}
	if staged.BundleManifestDigest != fixture.staged.BundleManifestDigest {
		t.Fatalf("bundle digest = %q, want %q", staged.BundleManifestDigest, fixture.staged.BundleManifestDigest)
	}
	if staged.SysextPayloadDigest != fixture.indexEntry.SysextPayloadDigest {
		t.Fatalf("payload digest = %q, want %q", staged.SysextPayloadDigest, fixture.indexEntry.SysextPayloadDigest)
	}
	for _, path := range []string{staged.SysextPath, staged.MetadataPath, filepath.Join(staged.BundleDir, "bundle.json"), filepath.Join(staged.BundleDir, "package-provenance.json"), filepath.Join(staged.BundleDir, "catalog-entry.json"), filepath.Join(cacheDir, "index.json")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("stat staged path %s: %v", path, err)
		}
	}
	payload, err := os.ReadFile(staged.SysextPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != "kubernetes sysext 1.36.0" {
		t.Fatalf("payload = %q", payload)
	}

	ref := staged.ExtensionRef
	if ref.Name != sysextcatalog.KubernetesName || ref.ActivationPath != "/run/extensions/katl-kubernetes.raw" || ref.Path != staged.SysextPath {
		t.Fatalf("extension ref location = %#v", ref)
	}
	if ref.SHA256 != strings.TrimPrefix(staged.SysextPayloadDigest, "sha256:") || ref.PayloadVersion != "v1.36.0" || ref.ArtifactVersion == "" {
		t.Fatalf("extension ref identity = %#v", ref)
	}
	if len(ref.Compatibility.RuntimeInterfaces) != 1 || ref.Compatibility.RuntimeInterfaces[0] != "katl-runtime-1" {
		t.Fatalf("extension ref compatibility = %#v", ref.Compatibility)
	}

	var index Index
	readJSON(t, filepath.Join(cacheDir, "index.json"), &index)
	if len(index.Entries) != 1 || index.Entries[0].BundleManifestDigest != staged.BundleManifestDigest {
		t.Fatalf("local index = %#v", index)
	}
}

func TestFetchAndStageOCI(t *testing.T) {
	fixture := writeFixture(t, "v1.36.0", "kubernetes sysext 1.36.0")
	repository := writeOCIRepository(t, fixture, nil)
	parsedRef, err := parseRef(fixture.ref())
	if err != nil {
		t.Fatal(err)
	}
	request := Request{
		Source:           "https://ghcr.io/v2/katl-dev/kubernetes",
		Ref:              fixture.ref(),
		CacheDir:         t.TempDir(),
		RuntimeInterface: "katl-runtime-1",
		Architecture:     "x86_64",
	}

	tag := registryTagPrefix + strings.TrimPrefix(parsedRef.BundleDigest, "sha256:")
	staged, err := fetchAndStageOCI(context.Background(), request, parsedRef, repository, tag, parsedRef.BundleDigest, "", "")
	if err != nil {
		t.Fatalf("fetchAndStageOCI() error = %v", err)
	}
	if staged.BundleManifestDigest != fixture.staged.BundleManifestDigest || staged.PayloadVersion != "v1.36.0" {
		t.Fatalf("staged identity = %#v", staged)
	}
	payload, err := os.ReadFile(staged.SysextPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != "kubernetes sysext 1.36.0" {
		t.Fatalf("payload = %q", payload)
	}
}

func TestFetchAndStageOCIStreamsSysextPayload(t *testing.T) {
	fixture := writeFixture(t, "v1.36.0", strings.Repeat("k", 4<<20))
	repository := &trackingOCIRepository{
		ociRepository: writeOCIRepository(t, fixture, nil),
		payloadDigest: fixture.indexEntry.SysextPayloadDigest,
	}
	parsedRef, err := parseRef(fixture.ref())
	if err != nil {
		t.Fatal(err)
	}
	request := Request{
		Source:           "https://ghcr.io/v2/katl-dev/kubernetes",
		Ref:              fixture.ref(),
		CacheDir:         t.TempDir(),
		RuntimeInterface: "katl-runtime-1",
		Architecture:     "x86_64",
	}

	tag := registryTagPrefix + strings.TrimPrefix(parsedRef.BundleDigest, "sha256:")
	if _, err := fetchAndStageOCI(context.Background(), request, parsedRef, repository, tag, parsedRef.BundleDigest, "", ""); err != nil {
		t.Fatalf("fetchAndStageOCI() error = %v", err)
	}
	if repository.payloadBytes != 4<<20 {
		t.Fatalf("payload bytes read = %d, want %d", repository.payloadBytes, 4<<20)
	}
	if repository.maxPayloadRead > 64<<10 {
		t.Fatalf("maximum payload read buffer = %d, want streaming-sized reads", repository.maxPayloadRead)
	}
}

func TestFetchAndStageOCIImageReference(t *testing.T) {
	fixture := writeFixture(t, "v1.36.0", "kubernetes sysext 1.36.0")
	repository := writeOCIRepository(t, fixture, nil)
	manifest, err := repository.Resolve(context.Background(), "v1.36.0-katl.1")
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{
		"ghcr.io/katl-dev/kubernetes:v1.36.0-katl.1",
		"ghcr.io/katl-dev/kubernetes:v1.36.0-katl.1@" + manifest.Digest.String(),
	} {
		image, err := ParseImageReference(value)
		if err != nil {
			t.Fatal(err)
		}
		request := Request{
			Source:           image.Source,
			Ref:              image.Value,
			CacheDir:         t.TempDir(),
			RuntimeInterface: "katl-runtime-1",
			Architecture:     "x86_64",
		}
		staged, err := fetchAndStageOCI(context.Background(), request, ref{PayloadVersion: image.PayloadVersion}, repository, image.Tag, "", image.ManifestDigest, image.ArtifactVersion)
		if err != nil {
			t.Fatalf("fetchAndStageOCI(%q) error = %v", value, err)
		}
		if staged.BundleManifestDigest != fixture.staged.BundleManifestDigest || staged.ArtifactVersion != image.ArtifactVersion {
			t.Fatalf("staged identity = %#v", staged)
		}
	}
}

func TestFetchAndStagePublicOCI(t *testing.T) {
	if os.Getenv("KATL_LIVE_GHCR") != "1" {
		t.Skip("set KATL_LIVE_GHCR=1 to verify the published Kubernetes bundle")
	}
	for _, ref := range []string{
		"ghcr.io/katl-dev/kubernetes:v1.36.0-katl.1",
		"ghcr.io/katl-dev/kubernetes:v1.36.0-katl.1@sha256:1dc92e6d16bec47feea17647289e4a8912dd941fae7aae37d02e02308f9830e3",
	} {
		staged, err := FetchAndStage(context.Background(), Request{
			Source:           "https://ghcr.io/v2/katl-dev/kubernetes",
			Ref:              ref,
			CacheDir:         t.TempDir(),
			RuntimeInterface: "katl-runtime-1",
			Architecture:     "x86_64",
		})
		if err != nil {
			t.Fatalf("FetchAndStage(%q) error = %v", ref, err)
		}
		if staged.PayloadVersion != "v1.36.0" || staged.ArtifactVersion != "v1.36.0-katl.1" || staged.BundleManifestDigest != "sha256:a928cad17e0179f6811d1da262b016a429b422fd46a6014b7f21db3efeb4cba2" {
			t.Fatalf("staged identity = %#v", staged)
		}
	}
}

func TestFetchAndStageOCIRejectsMissingLayer(t *testing.T) {
	fixture := writeFixture(t, "v1.36.0", "kubernetes sysext 1.36.0")
	repository := writeOCIRepository(t, fixture, func(manifest *ocispec.Manifest) {
		manifest.Layers = manifest.Layers[:len(manifest.Layers)-1]
	})
	parsedRef, err := parseRef(fixture.ref())
	if err != nil {
		t.Fatal(err)
	}
	request := Request{
		Source:           "https://ghcr.io/v2/katl-dev/kubernetes",
		Ref:              fixture.ref(),
		CacheDir:         t.TempDir(),
		RuntimeInterface: "katl-runtime-1",
		Architecture:     "x86_64",
	}

	tag := registryTagPrefix + strings.TrimPrefix(parsedRef.BundleDigest, "sha256:")
	_, err = fetchAndStageOCI(context.Background(), request, parsedRef, repository, tag, parsedRef.BundleDigest, "", "")
	if !errors.Is(err, ErrInvalidBundle) || !strings.Contains(err.Error(), "OCI manifest has 3 layers, want 4") {
		t.Fatalf("fetchAndStageOCI() error = %v, want missing layer rejection", err)
	}
}

func TestParseImageReference(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	image, err := ParseImageReference("ghcr.io/katl-dev/kubernetes:v1.36.0-katl.1@" + digest)
	if err != nil {
		t.Fatal(err)
	}
	if image.Source != "https://ghcr.io/v2/katl-dev/kubernetes" || image.Tag != "v1.36.0-katl.1" || image.PayloadVersion != "v1.36.0" || image.ManifestDigest != digest {
		t.Fatalf("image reference = %#v", image)
	}
	if version, err := PayloadVersionFromRef("ghcr.io/katl-dev/kubernetes:v1.36.0-katl.1"); err != nil || version != "v1.36.0" {
		t.Fatalf("PayloadVersionFromRef() = %q, %v", version, err)
	}
}

func TestParseImageReferenceRejectsInvalid(t *testing.T) {
	for _, value := range []string{
		"ghcr.io/katl-dev/kubernetes",
		"ghcr.io/katl-dev/kubernetes:latest",
		"https://ghcr.io/katl-dev/kubernetes:v1.36.0-katl.1",
		"ghcr.io/katl-dev/kubernetes:v1.36.0-katl.1@sha256:bad",
	} {
		if _, err := ParseImageReference(value); !errors.Is(err, ErrInvalidBundle) {
			t.Fatalf("ParseImageReference(%q) error = %v", value, err)
		}
	}
}

func TestRegistryRepositorySource(t *testing.T) {
	for _, test := range []struct {
		source string
		want   bool
	}{
		{source: "https://ghcr.io/v2/katl-dev/kubernetes", want: true},
		{source: "https://packages.katl.dev/kubernetes", want: false},
		{source: "https://ghcr.io/v2/", want: false},
		{source: "https://user:secret@ghcr.io/v2/katl-dev/kubernetes", want: false},
	} {
		_, got, err := registryRepository(test.source, http.DefaultClient)
		if err != nil {
			t.Fatalf("registryRepository(%q) error = %v", test.source, err)
		}
		if got != test.want {
			t.Fatalf("registryRepository(%q) match = %t, want %t", test.source, got, test.want)
		}
	}
}

func TestFetchAndStageRejectsDescriptorDigestMismatch(t *testing.T) {
	fixture := writeFixture(t, "v1.36.0", "kubernetes sysext 1.36.0")
	corruptBlob(t, fixture.root, fixture.indexEntry.SysextPayloadDigest, []byte("changed payload"))
	server := fixtureServer(t, fixture.root)

	_, err := FetchAndStage(context.Background(), fixture.request(t, server))
	if !errors.Is(err, ErrInvalidBundle) || !strings.Contains(err.Error(), "descriptor systemd-sysext digest got") {
		t.Fatalf("FetchAndStage() error = %v, want descriptor digest mismatch", err)
	}
}

func TestFetchAndStageRejectsIncompatibleSysextMetadata(t *testing.T) {
	fixture := writeFixture(t, "v1.36.0", "kubernetes sysext 1.36.0")
	fixture = rewriteBundle(t, fixture, func(bundle *Bundle) {
		var meta artifact.LocalMeta
		readJSON(t, fixture.staged.MetadataPath, &meta)
		meta.RuntimeInterface = "katl-runtime-2"
		meta.CompatibleRuntime.Interface = "katl-runtime-2"
		metadata := marshalJSON(t, meta)
		digest := writeBlob(t, fixture.root, metadata)
		for i := range bundle.Metadata {
			if bundle.Metadata[i].Role == metadataRole {
				bundle.Metadata[i].Digest = digest
				bundle.Metadata[i].SizeBytes = int64(len(metadata))
			}
		}
	})
	server := fixtureServer(t, fixture.root)

	_, err := FetchAndStage(context.Background(), fixture.request(t, server))
	if !errors.Is(err, ErrInvalidBundle) || !strings.Contains(err.Error(), "sysext metadata does not support runtime interface") {
		t.Fatalf("FetchAndStage() error = %v, want incompatible metadata", err)
	}
}

func TestFetchAndStageRejectsMissingPayload(t *testing.T) {
	fixture := writeFixture(t, "v1.36.0", "kubernetes sysext 1.36.0")
	if err := os.Remove(blobPath(fixture.root, fixture.indexEntry.SysextPayloadDigest)); err != nil {
		t.Fatal(err)
	}
	server := fixtureServer(t, fixture.root)

	_, err := FetchAndStage(context.Background(), fixture.request(t, server))
	if err == nil || !strings.Contains(err.Error(), "fetch descriptor systemd-sysext") || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("FetchAndStage() error = %v, want missing payload fetch error", err)
	}
}

func TestFetchAndStageRejectsWrongMediaType(t *testing.T) {
	fixture := writeFixture(t, "v1.36.0", "kubernetes sysext 1.36.0")
	fixture = rewriteBundle(t, fixture, func(bundle *Bundle) {
		for i := range bundle.Payloads {
			if bundle.Payloads[i].Role == sysextRole {
				bundle.Payloads[i].MediaType = "application/octet-stream"
			}
		}
	})
	server := fixtureServer(t, fixture.root)

	_, err := FetchAndStage(context.Background(), fixture.request(t, server))
	if !errors.Is(err, ErrInvalidBundle) || !strings.Contains(err.Error(), "descriptor systemd-sysext media type") {
		t.Fatalf("FetchAndStage() error = %v, want media type rejection", err)
	}
}

func TestFetchAndStageRejectsUnsafePayloadFileName(t *testing.T) {
	fixture := writeFixture(t, "v1.36.0", "kubernetes sysext 1.36.0")
	fixture = rewriteBundle(t, fixture, func(bundle *Bundle) {
		for i := range bundle.Payloads {
			if bundle.Payloads[i].Role == sysextRole {
				bundle.Payloads[i].FileName = "../katl-kubernetes.raw"
			}
		}
	})
	server := fixtureServer(t, fixture.root)

	_, err := FetchAndStage(context.Background(), fixture.request(t, server))
	if !errors.Is(err, ErrInvalidBundle) || !strings.Contains(err.Error(), "not a safe file name") {
		t.Fatalf("FetchAndStage() error = %v, want unsafe file name rejection", err)
	}
}

func TestFetchAndStageRejectsStaleRef(t *testing.T) {
	fixture := writeFixture(t, "v1.36.0", "kubernetes sysext 1.36.0")
	server := fixtureServer(t, fixture.root)
	request := fixture.request(t, server)
	request.Ref = "v1.36.0@sha256:" + strings.Repeat("0", sha256.Size*2)

	_, err := FetchAndStage(context.Background(), request)
	if !errors.Is(err, ErrInvalidBundle) || !strings.Contains(err.Error(), "no index entry matches ref") {
		t.Fatalf("FetchAndStage() error = %v, want stale ref", err)
	}
}

func TestFetchAndStageRejectsRawSysextSource(t *testing.T) {
	_, err := FetchAndStage(context.Background(), Request{
		Source:           "https://artifacts.example.invalid/katl-kubernetes.SYSEXT.RAW?token=secret",
		Ref:              "v1.36.0@sha256:" + strings.Repeat("0", sha256.Size*2),
		CacheDir:         t.TempDir(),
		RuntimeInterface: "katl-runtime-1",
		Architecture:     "x86_64",
		Client:           http.DefaultClient,
	})
	if !errors.Is(err, ErrInvalidBundle) || !strings.Contains(err.Error(), "raw sysext URLs") {
		t.Fatalf("FetchAndStage() error = %v, want raw sysext rejection", err)
	}
}

func TestFetchAndStageRedactsSourceCredentials(t *testing.T) {
	fixture := writeFixture(t, "v1.36.0", "kubernetes sysext 1.36.0")
	server := fixtureServer(t, fixture.root)
	source, err := url.Parse(server.URL + "/missing")
	if err != nil {
		t.Fatal(err)
	}
	source.User = url.UserPassword("user", "secret")
	request := fixture.request(t, server)
	request.Source = source.String()

	_, err = FetchAndStage(context.Background(), request)
	if err == nil {
		t.Fatal("FetchAndStage() error = nil, want missing index")
	}
	if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "user:") {
		t.Fatalf("FetchAndStage() leaked source credentials: %v", err)
	}
}

type fixture struct {
	root       string
	staged     sysextcatalog.StagedArtifact
	indexEntry IndexEntry
}

type trackingOCIRepository struct {
	ociRepository
	payloadDigest  string
	payloadBytes   int
	maxPayloadRead int
}

func (r *trackingOCIRepository) Fetch(ctx context.Context, descriptor ocispec.Descriptor) (io.ReadCloser, error) {
	reader, err := r.ociRepository.Fetch(ctx, descriptor)
	if err != nil || descriptor.Digest.String() != r.payloadDigest {
		return reader, err
	}
	return &trackingReadCloser{ReadCloser: reader, repository: r}, nil
}

type trackingReadCloser struct {
	io.ReadCloser
	repository *trackingOCIRepository
}

func (r *trackingReadCloser) Read(p []byte) (int, error) {
	if len(p) > r.repository.maxPayloadRead {
		r.repository.maxPayloadRead = len(p)
	}
	n, err := r.ReadCloser.Read(p)
	r.repository.payloadBytes += n
	return n, err
}

func (f fixture) ref() string {
	return f.indexEntry.PayloadVersion + "@" + f.indexEntry.BundleManifestDigest
}

func (f fixture) request(t *testing.T, server *httptest.Server) Request {
	t.Helper()
	return Request{
		Source:           server.URL,
		Ref:              f.ref(),
		CacheDir:         t.TempDir(),
		RuntimeInterface: "katl-runtime-1",
		Architecture:     "x86_64",
		Client:           server.Client(),
	}
}

func writeFixture(t *testing.T, payloadVersion string, payload string) fixture {
	t.Helper()
	sourceDir := t.TempDir()
	outputDir := t.TempDir()
	_, metadataPath := writeSysextArtifact(t, sourceDir, payloadVersion, payload)
	staged, err := sysextcatalog.StageKubernetesSysext(sysextcatalog.StageRequest{
		MetadataPath: metadataPath,
		OutputDir:    outputDir,
	})
	if err != nil {
		t.Fatalf("StageKubernetesSysext() error = %v", err)
	}
	var index Index
	readJSON(t, filepath.Join(outputDir, "index.json"), &index)
	if len(index.Entries) != 1 {
		t.Fatalf("index entries = %d, want 1", len(index.Entries))
	}
	return fixture{root: outputDir, staged: staged, indexEntry: index.Entries[0]}
}

func writeOCIRepository(t *testing.T, fixture fixture, mutate func(*ocispec.Manifest)) *memory.Store {
	t.Helper()
	ctx := context.Background()
	repository := memory.New()
	bundleBytes, err := os.ReadFile(fixture.staged.BundlePath)
	if err != nil {
		t.Fatal(err)
	}
	config := content.NewDescriptorFromBytes(bundleMediaType, bundleBytes)
	if config.Digest.String() != fixture.staged.BundleManifestDigest {
		t.Fatalf("OCI config digest = %s, want %s", config.Digest, fixture.staged.BundleManifestDigest)
	}
	if err := repository.Push(ctx, config, bytes.NewReader(bundleBytes)); err != nil {
		t.Fatal(err)
	}

	var bundle Bundle
	readJSON(t, fixture.staged.BundlePath, &bundle)
	paths := map[string]string{
		sysextRole:     fixture.staged.ArtifactPath,
		metadataRole:   filepath.Join(filepath.Dir(fixture.staged.BundlePath), "metadata.json"),
		provenanceRole: fixture.staged.PackageProvenancePath,
		catalogRole:    fixture.staged.CatalogFragmentPath,
	}
	var layers []ocispec.Descriptor
	for _, descriptor := range append(append([]Descriptor(nil), bundle.Payloads...), bundle.Metadata...) {
		data, err := os.ReadFile(paths[descriptor.Role])
		if err != nil {
			t.Fatal(err)
		}
		layer := content.NewDescriptorFromBytes(descriptor.MediaType, data)
		if layer.Digest.String() != descriptor.Digest || layer.Size != descriptor.SizeBytes {
			t.Fatalf("OCI layer for %s = %#v, want digest %s size %d", descriptor.Role, layer, descriptor.Digest, descriptor.SizeBytes)
		}
		if err := repository.Push(ctx, layer, bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
		layers = append(layers, layer)
	}
	manifest := ocispec.Manifest{
		Versioned:    specs.Versioned{SchemaVersion: 2},
		MediaType:    ocispec.MediaTypeImageManifest,
		ArtifactType: bundleArtifactType,
		Config:       config,
		Layers:       layers,
	}
	if mutate != nil {
		mutate(&manifest)
	}
	manifestBytes := marshalJSON(t, manifest)
	manifestDescriptor := content.NewDescriptorFromBytes(ocispec.MediaTypeImageManifest, manifestBytes)
	if err := repository.Push(ctx, manifestDescriptor, bytes.NewReader(manifestBytes)); err != nil {
		t.Fatal(err)
	}
	tag := registryTagPrefix + strings.TrimPrefix(fixture.staged.BundleManifestDigest, "sha256:")
	if err := repository.Tag(ctx, manifestDescriptor, tag); err != nil {
		t.Fatal(err)
	}
	if err := repository.Tag(ctx, manifestDescriptor, bundle.ArtifactVersion); err != nil {
		t.Fatal(err)
	}
	return repository
}

func fixtureServer(t *testing.T, root string) *httptest.Server {
	t.Helper()
	server := httptest.NewTLSServer(http.FileServer(http.Dir(root)))
	t.Cleanup(server.Close)
	return server
}

func writeSysextArtifact(t *testing.T, dir string, payloadVersion string, payload string) (string, string) {
	t.Helper()
	rawPath := filepath.Join(dir, "katl-kubernetes-"+payloadVersion+".raw")
	if err := os.WriteFile(rawPath, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	digestBytes := sha256.Sum256([]byte(payload))
	digest := hex.EncodeToString(digestBytes[:])
	meta := artifact.LocalMeta{
		Name:           sysextcatalog.KubernetesName,
		Kind:           artifact.ArtifactSysext,
		Format:         "sysext",
		Path:           filepath.Base(rawPath),
		SizeBytes:      int64(len(payload)),
		SHA256:         digest,
		Version:        payloadVersion + "-katl.1",
		PayloadVersion: payloadVersion,
		Architecture:   "x86_64",
		SourceRepo: &artifact.SourceRepo{
			ID:      "kubernetes",
			BaseURL: "https://pkgs.k8s.io/core:/stable:/v1.36/rpm/",
			Minor:   "v1.36",
		},
		PackageVersions: map[string]string{
			"cri-tools": "0:" + strings.TrimPrefix(payloadVersion, "v") + "-150500.1.1",
			"ethtool":   "2:7.0-1.fc44",
			"kubeadm":   "0:" + strings.TrimPrefix(payloadVersion, "v") + "-150500.1.1",
			"kubectl":   "0:" + strings.TrimPrefix(payloadVersion, "v") + "-150500.1.1",
			"kubelet":   "0:" + strings.TrimPrefix(payloadVersion, "v") + "-150500.1.1",
			"socat":     "0:1.8.1.1-1.fc44",
		},
		RuntimeInterface: "katl-runtime-1",
		CompatibleRuntime: &artifact.Compat{
			Interface:    "katl-runtime-1",
			ArtifactPath: filepath.Join(dir, "katl-runtime-root.squashfs"),
		},
		Created: "2026-06-04T20:00:00Z",
	}
	metadataPath := rawPath + ".json"
	if err := os.WriteFile(metadataPath, marshalJSON(t, meta), 0o600); err != nil {
		t.Fatal(err)
	}
	return rawPath, metadataPath
}

func rewriteBundle(t *testing.T, f fixture, mutate func(*Bundle)) fixture {
	t.Helper()
	bundlePath := filepath.Join(f.root, filepath.FromSlash(f.indexEntry.BundleManifestPath))
	var bundle Bundle
	readJSON(t, bundlePath, &bundle)
	mutate(&bundle)
	bundleBytes := marshalJSON(t, bundle)
	bundleDigest := writeBlob(t, f.root, bundleBytes)
	if err := os.WriteFile(bundlePath, bundleBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	var index Index
	indexPath := filepath.Join(f.root, "index.json")
	readJSON(t, indexPath, &index)
	for i := range index.Entries {
		if index.Entries[i].PayloadVersion == f.indexEntry.PayloadVersion {
			index.Entries[i].BundleManifestDigest = bundleDigest
			f.indexEntry = index.Entries[i]
		}
	}
	if err := os.WriteFile(indexPath, marshalJSON(t, index), 0o644); err != nil {
		t.Fatal(err)
	}
	f.staged.BundleManifestDigest = bundleDigest
	return f
}

func corruptBlob(t *testing.T, root string, digest string, data []byte) {
	t.Helper()
	if err := os.WriteFile(blobPath(root, digest), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeBlob(t *testing.T, root string, data []byte) string {
	t.Helper()
	digest := "sha256:" + testDataSHA256(data)
	if err := os.WriteFile(blobPath(root, digest), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return digest
}

func blobPath(root string, digest string) string {
	return filepath.Join(root, "blobs", "sha256", strings.TrimPrefix(digest, "sha256:"))
}

func readJSON(t *testing.T, path string, target any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, target); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
}

func marshalJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(data, '\n')
}

func testDataSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
