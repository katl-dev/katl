package scenarios

import (
	"archive/tar"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/installer/artifact"
	"github.com/katl-dev/katl/internal/vmtest"
	"github.com/katl-dev/katl/internal/workstationtest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestKubernetesImageReferencesFromSysext(t *testing.T) {
	// The fixture needs mkfs.erofs and an sfdisk with --sector-size, such as
	// the development shell provides, and reading it needs fsck.erofs.
	for _, tool := range []string{"mkfs.erofs", "fsck.erofs", "sfdisk"} {
		workstationtest.Tool(t, tool)
	}
	const root = "4F68BCE3-E8CD-4DB1-96E7-FBCAF984B709"
	for _, tc := range []struct {
		name       string
		sectorSize int
		// layout is the sfdisk script after the label; the root partition's
		// payload goes at 2 MiB.
		layout  string
		wantErr string
	}{
		{name: "512-byte sectors", sectorSize: 512, layout: "start=2048, size=2048, type=linux\nstart=4096, size=4096, type=" + root + "\n"},
		{name: "4096-byte sectors", sectorSize: 4096, layout: "start=256, size=256, type=linux\nstart=512, size=512, type=" + root + "\n"},
		{name: "two root partitions", sectorSize: 512, layout: "start=4096, size=4096, type=" + root + "\nstart=8192, size=4096, type=" + root + "\n", wantErr: "expected one x86-64 root partition"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const version = "v1.37.0"
			const image = "registry.k8s.io/kube-apiserver:" + version
			repo := writeSysextFixture(t, version, image, tc.sectorSize, tc.layout)

			images, err := kubernetesImageReferences(t.Context(), repo, version)

			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("images = %q, error = %v, want an error containing %q", images, err, tc.wantErr)
				}
				return
			}
			if err != nil || !slices.Equal(images, []string{image}) {
				t.Fatalf("images = %q, want %q, error = %v", images, image, err)
			}
		})
	}
}

// writeSysextFixture builds a repository whose local Kubernetes sysext has
// the given GPT layout and holds, at 2 MiB, an EROFS root with a kubeadm that
// lists image.
func writeSysextFixture(t *testing.T, version, image string, sectorSize int, layout string) string {
	t.Helper()
	repo := t.TempDir()
	root := filepath.Join(repo, "erofs")
	if err := os.MkdirAll(filepath.Join(root, "usr/bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	kubeadm := "#!/bin/sh\n[ \"$*\" = 'config images list --kubernetes-version " + version + "' ] || exit 1\necho " + image + "\n"
	if err := os.WriteFile(filepath.Join(root, "usr/bin/kubeadm"), []byte(kubeadm), 0o755); err != nil {
		t.Fatal(err)
	}
	part := filepath.Join(repo, "root.erofs")
	if output, err := exec.Command("mkfs.erofs", part, root).CombinedOutput(); err != nil {
		t.Fatalf("build EROFS fixture: %v\n%s", err, output)
	}
	build := (vmtest.World{}).MkosiDir(repo)
	if err := os.MkdirAll(build, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(build, "katl-kubernetes.raw")
	const size = 8 << 20
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	partition := exec.Command("sfdisk", "--sector-size", strconv.Itoa(sectorSize), path)
	partition.Stdin = strings.NewReader("label: gpt\n" + layout)
	if output, err := partition.CombinedOutput(); err != nil {
		t.Fatalf("partition sysext fixture: %v\n%s", err, output)
	}
	payload, err := os.ReadFile(part)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.WriteAt(payload, 2<<20)
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("write root partition: %v, close: %v", err, closeErr)
	}
	digest, err := fileSHA256Hex(path)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := json.Marshal(artifact.LocalMeta{
		Name: "kubernetes", Kind: artifact.ArtifactSysext, Format: "sysext",
		Path: filepath.Base(path), SizeBytes: size, SHA256: digest, PayloadVersion: version,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".json", meta, 0o644); err != nil {
		t.Fatal(err)
	}
	return repo
}

func TestKubernetesFixtureArchiveRetainsImageReferences(t *testing.T) {
	skopeo := workstationtest.Tool(t, "skopeo")
	dir := t.TempDir()
	source := filepath.Join(dir, "source.tar")
	binary := filepath.Join(dir, "binary")
	if err := os.WriteFile(binary, []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	images := []string{"registry.k8s.io/kube-apiserver:v1.37.0", "registry.k8s.io/etcd:3.7.0-0"}
	if err := writeDockerArchive(source, images[0], binary, "/fixture"); err != nil {
		t.Fatal(err)
	}
	// Substitute a local source for the registry and let Skopeo produce the layout.
	wrapper := `#!/usr/bin/env bash
set -eu
args=("$@")
case "${args[-2]}" in
    docker://registry.k8s.io/kube-apiserver:v1.37.0|docker://registry.k8s.io/etcd:3.7.0-0) ;;
    *) exit 2 ;;
esac
args[-2]="docker-archive:$KATL_TEST_IMAGE_SOURCE"
exec "$KATL_TEST_SKOPEO" "${args[@]}"
`
	if err := os.WriteFile(filepath.Join(dir, "skopeo"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("KATL_TEST_IMAGE_SOURCE", source)
	t.Setenv("KATL_TEST_SKOPEO", skopeo)
	archivePath := filepath.Join(dir, "images.oci.tar")
	manifest := filepath.Join(dir, "manifest.json")
	if err := buildKubernetesImageFixture(t.Context(), katlRepoRoot(t), archivePath, manifest, "v1.37.0", images); err != nil {
		t.Fatal(err)
	}

	file, err := os.Open(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	archive := tar.NewReader(file)
	for {
		entry, err := archive.Next()
		if err == io.EOF {
			t.Fatal("OCI archive is missing index.json")
		}
		if err != nil {
			t.Fatal(err)
		}
		if entry.Name == "index.json" {
			break
		}
	}
	var index ocispec.Index
	if err := json.NewDecoder(archive).Decode(&index); err != nil {
		t.Fatal(err)
	}
	var refs []string
	for _, image := range index.Manifests {
		refs = append(refs, image.Annotations[ocispec.AnnotationRefName])
	}
	slices.Sort(refs)
	if !slices.Equal(refs, []string{"registry.k8s.io/etcd:3.7.0-0", "registry.k8s.io/kube-apiserver:v1.37.0"}) {
		t.Fatalf("OCI archive image references = %q", refs)
	}
}
