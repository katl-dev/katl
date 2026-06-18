package sysextcatalog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zariel/katl/internal/installer/artifact"
)

func TestStageKubernetesSysext(t *testing.T) {
	sourceDir := t.TempDir()
	outputDir := t.TempDir()
	rawPath, metadataPath := writeSysextArtifact(t, sourceDir, "sysext payload")

	staged, err := StageKubernetesSysext(StageRequest{
		MetadataPath: metadataPath,
		OutputDir:    outputDir,
	})
	if err != nil {
		t.Fatalf("StageKubernetesSysext() error = %v", err)
	}

	wantName := "katl-kubernetes-v1.36.1-x86_64.sysext.raw"
	if filepath.Base(staged.ArtifactPath) != wantName {
		t.Fatalf("artifact name = %q, want %q", filepath.Base(staged.ArtifactPath), wantName)
	}
	for _, path := range []string{staged.ArtifactPath, staged.ChecksumPath, staged.MetadataPath, staged.CatalogPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("stat staged output %s: %v", path, err)
		}
	}
	for _, path := range []string{staged.BundlePath, staged.IndexPath, staged.BundleCatalogPath, staged.CatalogFragmentPath, staged.PackageProvenancePath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("stat staged bundle output %s: %v", path, err)
		}
	}

	checksum := readText(t, staged.ChecksumPath)
	if !strings.HasSuffix(checksum, "  "+wantName+"\n") {
		t.Fatalf("checksum = %q", checksum)
	}

	var stagedMeta artifact.LocalMeta
	readJSON(t, staged.MetadataPath, &stagedMeta)
	if stagedMeta.Path != wantName {
		t.Fatalf("staged metadata path = %q, want %q", stagedMeta.Path, wantName)
	}
	if stagedMeta.CompatibleRuntime == nil || stagedMeta.CompatibleRuntime.ArtifactPath != "katl-runtime-root.squashfs" {
		t.Fatalf("staged compatible runtime = %#v", stagedMeta.CompatibleRuntime)
	}

	catalog := readCatalog(t, staged.CatalogPath)
	if len(catalog.Entries) != 1 {
		t.Fatalf("catalog entry count = %d, want 1", len(catalog.Entries))
	}
	entry := catalog.Entries[0]
	if entry.LocalPath != wantName || entry.URL != "" {
		t.Fatalf("catalog entry location = local %q URL %q", entry.LocalPath, entry.URL)
	}
	if entry.PayloadVersion != "v1.36.1" || entry.KubernetesMinor != "v1.36" || entry.Architecture != "x86_64" {
		t.Fatalf("catalog entry = %#v", entry)
	}

	var bundle KubernetesPayloadBundle
	readJSON(t, staged.BundlePath, &bundle)
	if bundle.Kind != kubernetesPayloadBundleKind || bundle.PayloadVersion != "v1.36.1" || bundle.ArtifactKind != kubernetesPayloadBundleArtifactKind {
		t.Fatalf("bundle identity = %#v", bundle)
	}
	if bundle.BuildInputDigest == "" {
		t.Fatalf("bundle missing buildInputDigest: %#v", bundle)
	}
	if len(bundle.Payloads) != 1 || bundle.Payloads[0].Digest != "sha256:"+stagedMeta.SHA256 {
		t.Fatalf("bundle payload descriptors = %#v", bundle.Payloads)
	}
	if staged.BundleManifestDigest != "sha256:"+testFileSHA256(t, staged.BundlePath) {
		t.Fatalf("bundle digest = %q, file digest %s", staged.BundleManifestDigest, testFileSHA256(t, staged.BundlePath))
	}
	assertBlobEquals(t, outputDir, staged.BundleManifestDigest, staged.BundlePath)
	assertBlobEquals(t, outputDir, bundle.Payloads[0].Digest, staged.ArtifactPath)
	assertBundleMetadataDescriptors(t, outputDir, filepath.Dir(staged.BundlePath), bundle.Metadata)

	var index KubernetesPayloadIndex
	readJSON(t, staged.IndexPath, &index)
	if len(index.Entries) != 1 || index.Entries[0].BundleManifestDigest != staged.BundleManifestDigest || index.Entries[0].PayloadVersion != "v1.36.1" {
		t.Fatalf("index = %#v", index)
	}
	var bundleCatalog KubernetesPayloadCatalog
	readJSON(t, staged.BundleCatalogPath, &bundleCatalog)
	if len(bundleCatalog.Entries) != 1 || bundleCatalog.Entries[0].BundleManifestDigest != staged.BundleManifestDigest {
		t.Fatalf("bundle catalog = %#v", bundleCatalog)
	}
	checksums := readText(t, filepath.Join(outputDir, "checksums.txt"))
	for _, want := range []string{"index.json", "bundles/v1.36.1/x86_64/bundle.json"} {
		if !strings.Contains(checksums, want) {
			t.Fatalf("checksums.txt missing %s", want)
		}
	}

	for _, path := range []string{staged.MetadataPath, staged.CatalogPath, staged.ChecksumPath, staged.BundlePath, staged.IndexPath, staged.BundleCatalogPath, staged.CatalogFragmentPath, staged.PackageProvenancePath} {
		data := readText(t, path)
		if strings.Contains(data, sourceDir) || strings.Contains(data, outputDir) || strings.Contains(data, rawPath) {
			t.Fatalf("%s contains host path: %s", path, data)
		}
	}
}

func TestStageKubernetesSysextAccumulatesBundleFixturePair(t *testing.T) {
	sourceDir := t.TempDir()
	outputDir := t.TempDir()
	_, v1360MetadataPath := writeSysextArtifactVersion(t, sourceDir, "v1.36.0", "kubernetes sysext 1.36.0")
	_, v1361MetadataPath := writeSysextArtifactVersion(t, sourceDir, "v1.36.1", "kubernetes sysext 1.36.1")

	staged1360, err := StageKubernetesSysext(StageRequest{
		MetadataPath: v1360MetadataPath,
		OutputDir:    outputDir,
	})
	if err != nil {
		t.Fatalf("StageKubernetesSysext(v1.36.0) error = %v", err)
	}
	staged1361, err := StageKubernetesSysext(StageRequest{
		MetadataPath: v1361MetadataPath,
		OutputDir:    outputDir,
	})
	if err != nil {
		t.Fatalf("StageKubernetesSysext(v1.36.1) error = %v", err)
	}

	var index KubernetesPayloadIndex
	readJSON(t, filepath.Join(outputDir, "index.json"), &index)
	if len(index.Entries) != 2 {
		t.Fatalf("index entries = %d, want 2: %#v", len(index.Entries), index.Entries)
	}
	if index.Entries[0].PayloadVersion != "v1.36.0" || index.Entries[1].PayloadVersion != "v1.36.1" {
		t.Fatalf("index order = %#v", index.Entries)
	}
	if index.Entries[0].BundleManifestDigest != staged1360.BundleManifestDigest || index.Entries[1].BundleManifestDigest != staged1361.BundleManifestDigest {
		t.Fatalf("index bundle digests = %#v", index.Entries)
	}

	var catalog KubernetesPayloadCatalog
	readJSON(t, filepath.Join(outputDir, "catalog", "v1.36.json"), &catalog)
	if len(catalog.Entries) != 2 {
		t.Fatalf("catalog entries = %d, want 2: %#v", len(catalog.Entries), catalog.Entries)
	}
	if catalog.Entries[0].PayloadVersion != "v1.36.0" || catalog.Entries[1].PayloadVersion != "v1.36.1" {
		t.Fatalf("catalog order = %#v", catalog.Entries)
	}

	for _, staged := range []StagedArtifact{staged1360, staged1361} {
		assertBlobEquals(t, outputDir, staged.BundleManifestDigest, staged.BundlePath)
		if _, err := os.Stat(filepath.Join(outputDir, "bundles", staged.Entry.PayloadVersion, staged.Entry.Architecture, "metadata.json")); err != nil {
			t.Fatalf("metadata copy for %s: %v", staged.Entry.PayloadVersion, err)
		}
	}
}

func TestStageKubernetesSysextWithBaseURL(t *testing.T) {
	sourceDir := t.TempDir()
	outputDir := t.TempDir()
	_, metadataPath := writeSysextArtifact(t, sourceDir, "sysext payload")

	staged, err := StageKubernetesSysext(StageRequest{
		MetadataPath: metadataPath,
		OutputDir:    outputDir,
		BaseURL:      "https://artifacts.example.invalid/katl/kubernetes",
	})
	if err != nil {
		t.Fatalf("StageKubernetesSysext() error = %v", err)
	}

	entry := readCatalog(t, staged.CatalogPath).Entries[0]
	if entry.LocalPath != "" {
		t.Fatalf("catalog local path = %q, want empty", entry.LocalPath)
	}
	if entry.URL != "https://artifacts.example.invalid/katl/kubernetes/katl-kubernetes-v1.36.1-x86_64.sysext.raw" {
		t.Fatalf("catalog URL = %q", entry.URL)
	}
}

func TestStageKubernetesSysextRejectsDigestMismatch(t *testing.T) {
	sourceDir := t.TempDir()
	outputDir := t.TempDir()
	rawPath, metadataPath := writeSysextArtifact(t, sourceDir, "sysext payload")
	if err := os.WriteFile(rawPath, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := StageKubernetesSysext(StageRequest{
		MetadataPath: metadataPath,
		OutputDir:    outputDir,
	})
	if !errors.Is(err, ErrInvalidCatalog) {
		t.Fatalf("StageKubernetesSysext() error = %v, want ErrInvalidCatalog", err)
	}
}

func TestStageKubernetesSysextRejectsNonKubernetesSysext(t *testing.T) {
	sourceDir := t.TempDir()
	outputDir := t.TempDir()
	_, metadataPath := writeSysextArtifact(t, sourceDir, "sysext payload")

	var meta artifact.LocalMeta
	readJSON(t, metadataPath, &meta)
	meta.Name = "storage"
	metadata, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metadataPath, append(metadata, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = StageKubernetesSysext(StageRequest{
		MetadataPath: metadataPath,
		OutputDir:    outputDir,
	})
	if !errors.Is(err, ErrInvalidCatalog) {
		t.Fatalf("StageKubernetesSysext() error = %v, want ErrInvalidCatalog", err)
	}
}

func writeSysextArtifact(t *testing.T, dir string, payload string) (string, string) {
	t.Helper()
	return writeSysextArtifactVersion(t, dir, "v1.36.1", payload)
}

func writeSysextArtifactVersion(t *testing.T, dir string, payloadVersion string, payload string) (string, string) {
	t.Helper()

	rawPath := filepath.Join(dir, "katl-kubernetes-"+payloadVersion+".raw")
	if err := os.WriteFile(rawPath, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	digestBytes := sha256.Sum256([]byte(payload))
	digest := hex.EncodeToString(digestBytes[:])

	meta := artifact.LocalMeta{
		Name:           "kubernetes",
		Kind:           artifact.ArtifactSysext,
		Format:         "sysext",
		Path:           filepath.Base(rawPath),
		SizeBytes:      int64(len(payload)),
		SHA256:         digest,
		Version:        payloadVersion + "-build.1",
		PayloadVersion: payloadVersion,
		Architecture:   "x86_64",
		SourceRepo: &artifact.SourceRepo{
			ID:      "kubernetes",
			BaseURL: "https://pkgs.k8s.io/core:/stable:/v1.36/rpm/",
			Minor:   "v1.36",
		},
		PackageVersions: map[string]string{
			"cri-tools": "0:" + strings.TrimPrefix(payloadVersion, "v") + "-150500.1.1",
			"kubeadm":   "0:" + strings.TrimPrefix(payloadVersion, "v") + "-150500.1.1",
			"kubectl":   "0:" + strings.TrimPrefix(payloadVersion, "v") + "-150500.1.1",
			"kubelet":   "0:" + strings.TrimPrefix(payloadVersion, "v") + "-150500.1.1",
		},
		RuntimeInterface: "katl-runtime-1",
		CompatibleRuntime: &artifact.Compat{
			Interface:    "katl-runtime-1",
			ArtifactPath: filepath.Join(dir, "katl-runtime-root.squashfs"),
		},
		Created: "2026-06-04T20:00:00Z",
	}
	metadata, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	metadataPath := rawPath + ".json"
	if err := os.WriteFile(metadataPath, append(metadata, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	return rawPath, metadataPath
}

func assertBlobEquals(t *testing.T, outputDir string, digest string, path string) {
	t.Helper()
	if !strings.HasPrefix(digest, "sha256:") {
		t.Fatalf("digest %q missing sha256 prefix", digest)
	}
	blobPath := filepath.Join(outputDir, "blobs", "sha256", strings.TrimPrefix(digest, "sha256:"))
	blob, err := os.ReadFile(blobPath)
	if err != nil {
		t.Fatalf("read blob %s: %v", blobPath, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file %s: %v", path, err)
	}
	if string(blob) != string(data) {
		t.Fatalf("blob %s does not match %s", blobPath, path)
	}
}

func assertBundleMetadataDescriptors(t *testing.T, outputDir string, bundleDir string, descriptors []BundleDescriptor) {
	t.Helper()
	byName := map[string]BundleDescriptor{}
	for _, descriptor := range descriptors {
		byName[descriptor.FileName] = descriptor
	}
	for _, name := range []string{"metadata.json", "package-provenance.json", "catalog-entry.json"} {
		descriptor, ok := byName[name]
		if !ok {
			t.Fatalf("descriptor %s missing from %#v", name, descriptors)
		}
		path := filepath.Join(bundleDir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if descriptor.SizeBytes != int64(len(data)) {
			t.Fatalf("%s descriptor size = %d, want %d", name, descriptor.SizeBytes, len(data))
		}
		if descriptor.Digest != "sha256:"+testDataSHA256(data) {
			t.Fatalf("%s descriptor digest = %q, want sha256:%s", name, descriptor.Digest, testDataSHA256(data))
		}
		assertBlobEquals(t, outputDir, descriptor.Digest, path)
	}
}

func testFileSHA256(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return testDataSHA256(data)
}

func testDataSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func readText(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func readJSON(t *testing.T, path string, dest any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, dest); err != nil {
		t.Fatal(err)
	}
}

func readCatalog(t *testing.T, path string) Catalog {
	t.Helper()
	catalog, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}
