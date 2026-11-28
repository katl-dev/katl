package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/installer/katlosimage"
	"github.com/katl-dev/katl/internal/resourcetest"
)

func stubPrepareMkosi(t *testing.T) {
	t.Helper()
	oldQuery, oldIndex := queryRPMPackages, readKatlOSIndex
	t.Cleanup(func() { queryRPMPackages, readKatlOSIndex = oldQuery, oldIndex })
	queryRPMPackages = func(root string) ([]resourcetest.Package, error) {
		t.Fatalf("runtime package inventory should avoid querying the pruned root: %s", root)
		return nil, nil
	}
	readKatlOSIndex = func(string) (katlosIndex, error) {
		return katlosTestIndex(strings.Repeat("d", 64)), nil
	}
}

func pinTestBuild(t *testing.T) (mkosiDir string) {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "mkosi.conf"), "[Distribution]\nRelease=99\n")
	t.Setenv("KATL_REPO_ROOT", dir)
	mkosiDir = filepath.Join(dir, "_build", "mkosi")
	writeFile(t, filepath.Join(mkosiDir, "katl-runtime-root.squashfs"), "runtime-v1")
	writeFile(t, filepath.Join(mkosiDir, "katl-runtime.efi"), "runtime-uki-v1")
	writeFile(t, filepath.Join(mkosiDir, "katl-kubernetes.raw"), "kubernetes-v1")
	writeFile(t, filepath.Join(mkosiDir, "katl-kubernetes-upgrade.raw"), "kubernetes-upgrade-target")
	writeFile(t, filepath.Join(mkosiDir, "katl-kubernetes-upgrade-source.raw"), "kubernetes-upgrade-source")
	writeFile(t, filepath.Join(mkosiDir, "katl-vmtest-drbd-tools.raw"), "drbd-tools")
	writeKubernetesMetadata(t, filepath.Join(mkosiDir, "katl-kubernetes.raw.json"), "0:1.36.0-150500.1.1")
	writeInstallerPackages(t, filepath.Join(mkosiDir, "katl-installer.packages.tsv"), "systemd-0:259.6-1.fc44.x86_64")
	writeInstallerPackages(t, filepath.Join(mkosiDir, "katl-runtime.packages.tsv"), "systemd-0:259.6-1.fc44.x86_64")
	writeFile(t, filepath.Join(mkosiDir, "katl-installer.iso"), "iso-v1")
	writeBuiltKatlOSImage(t, mkosiDir, "katlos-install-0.0.0-dev-x86_64.squashfs")
	// Outputs of earlier builds stay in the directory and are not inputs.
	writeFile(t, filepath.Join(mkosiDir, "katlos-install-old-x86_64.squashfs"), "stale")
	writeFile(t, filepath.Join(mkosiDir, "katl-kubernetes-journey.raw"), "stale")
	return mkosiDir
}

func prepareMkosiPinned(t *testing.T, mkosiDir, pinDir string, extraArgs ...string) (resourcetest.Manifest, error) {
	t.Helper()
	manifestPath := filepath.Join(filepath.Dir(pinDir), "resource-manifest.json")
	err := run(append([]string{
		"prepare-mkosi",
		"--manifest", manifestPath,
		"--mkosi-dir", mkosiDir,
		"--runtime-root", filepath.Join(mkosiDir, "katl-runtime-root"),
		"--pin-dir", pinDir,
		"--mode", "record",
		"--run-id", "run-1",
		"--git-revision", "test",
		"--fedora-repository", "fedora=https://example.invalid/fedora/44",
		"--mkosi-version", "26",
	}, extraArgs...), &bytes.Buffer{}, &bytes.Buffer{})
	if err != nil {
		return resourcetest.Manifest{}, err
	}
	manifest, readErr := readManifest(manifestPath)
	if readErr != nil {
		t.Fatalf("read manifest: %v", readErr)
	}
	return manifest, nil
}

func sha256Hex(data string) string {
	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:])
}

// writeUpgradeImage writes an upgrade image and its sidecar metadata to
// mkosiDir and returns the metadata.
func writeUpgradeImage(t *testing.T, mkosiDir, name, version, contents string) string {
	t.Helper()
	data, err := json.Marshal(katlosimage.ArtifactMetadata{
		APIVersion: katlosimage.APIVersion, Kind: katlosimage.ArtifactMetadataKind,
		ImageRole: katlosimage.RoleUpgrade, Format: katlosimage.FormatSquashFS,
		Version: version, BuildID: "0.0.0-dev", Architecture: "x86_64",
		RuntimeInterface: "katl-runtime-1", Path: name, SizeBytes: int64(len(contents)),
		SHA256: sha256Hex(contents), ChecksumPath: name + ".sha256",
		EmbeddedIndexPath: "katlos/image.json", CreatedAt: "2026-11-26T00:00:00Z",
	})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(mkosiDir, name), contents)
	writeFile(t, filepath.Join(mkosiDir, name+".json"), string(data))
	return string(data)
}

func TestPrepareMkosiPinsBuildsUpgradeImage(t *testing.T) {
	stubPrepareMkosi(t)
	mkosiDir := pinTestBuild(t)
	const stale = "katlos-upgrade-0.0.0-dev.1-x86_64.squashfs"
	const selected = "katlos-upgrade-0000.0.0-dev.1-x86_64.squashfs"
	staleMetadata := writeUpgradeImage(t, mkosiDir, stale, "0.0.0-dev.1", "leftover")
	selectedMetadata := writeUpgradeImage(t, mkosiDir, selected, "0000.0.0-dev.1", "this build")
	sharedReceipt := filepath.Join(mkosiDir, "katlos-upgrade.json")

	for _, tc := range []struct {
		name     string
		useBuild bool
	}{
		{name: "prebuilt inputs"},
		{name: "receipt from this run's build", useBuild: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pinDir := filepath.Join(t.TempDir(), "artifacts")
			writeFile(t, sharedReceipt, selectedMetadata)
			writeFile(t, filepath.Join(mkosiDir, selected), "this build")
			var args []string
			if tc.useBuild {
				receipt := filepath.Join(t.TempDir(), "receipt.json")
				writeFile(t, receipt, selectedMetadata)
				// Another build published a receipt before this run started pinning.
				writeFile(t, sharedReceipt, staleMetadata)
				args = []string{"--upgrade-metadata", receipt}
			}
			manifest, err := prepareMkosiPinned(t, mkosiDir, pinDir, args...)
			if err != nil {
				t.Fatal(err)
			}

			writeFile(t, sharedReceipt, staleMetadata)
			replacement := filepath.Join(mkosiDir, "replacement")
			writeFile(t, replacement, "next build")
			if err := os.Rename(replacement, filepath.Join(mkosiDir, selected)); err != nil {
				t.Fatal(err)
			}

			image := artifactNamed(t, manifest, "katlos-upgrade-image")
			if image.Path != filepath.Join(pinDir, selected) || image.Digest != sha256Hex("this build") {
				t.Fatalf("run bound upgrade image %+v, want %s from this build", image, selected)
			}
			if got, err := os.ReadFile(image.Path); err != nil || string(got) != "this build" {
				t.Fatalf("pinned upgrade image = %q, %v", got, err)
			}
			if got, err := os.ReadFile(image.Path + ".json"); err != nil || string(got) != selectedMetadata {
				t.Fatalf("pinned upgrade metadata = %q, %v", got, err)
			}
			if _, err := os.Stat(filepath.Join(pinDir, stale)); !os.IsNotExist(err) {
				t.Fatalf("stale upgrade image should be absent from the pin, stat error = %v", err)
			}
		})
	}
}

// The receipt binds the run to one build's image, so an image that no longer
// matches it must stop the run rather than be pinned in its place.
func TestPrepareMkosiRejectsUpgradeImageThatDiffersFromReceipt(t *testing.T) {
	const selected = "katlos-upgrade-0000.0.0-dev.1-x86_64.squashfs"
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, image string)
	}{
		{name: "replaced after the receipt", change: func(t *testing.T, image string) {
			replacement := image + ".tmp"
			writeFile(t, replacement, "another build's image")
			if err := os.Rename(replacement, image); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "absent", change: func(t *testing.T, image string) {
			if err := os.Remove(image); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubPrepareMkosi(t)
			mkosiDir := pinTestBuild(t)
			metadata := writeUpgradeImage(t, mkosiDir, selected, "0000.0.0-dev.1", "this build")
			receipt := filepath.Join(t.TempDir(), "receipt.json")
			writeFile(t, receipt, metadata)
			tc.change(t, filepath.Join(mkosiDir, selected))
			pinDir := filepath.Join(t.TempDir(), "artifacts")

			_, err := prepareMkosiPinned(t, mkosiDir, pinDir, "--upgrade-metadata", receipt)

			if err == nil || !strings.Contains(err.Error(), selected) {
				t.Fatalf("prepare-mkosi error = %v, want a failure naming %s", err, selected)
			}
			manifest, readErr := readManifest(filepath.Join(filepath.Dir(pinDir), "resource-manifest.json"))
			if readErr != nil && !errors.Is(readErr, fs.ErrNotExist) {
				t.Fatalf("read manifest: %v", readErr)
			}
			for _, artifact := range manifest.Artifacts {
				if artifact.Name == "katlos-upgrade-image" {
					t.Fatalf("manifest records upgrade image %+v after the receipt check failed", artifact)
				}
			}
		})
	}
}

// A build that replaces an output after the run pinned it must not change
// what the run recorded or what it boots.
func TestPrepareMkosiPinsOutputs(t *testing.T) {
	stubPrepareMkosi(t)
	mkosiDir := pinTestBuild(t)
	pinDir := filepath.Join(t.TempDir(), "artifacts")

	manifest, err := prepareMkosiPinned(t, mkosiDir, pinDir)
	if err != nil {
		t.Fatalf("prepare-mkosi error = %v", err)
	}

	// The builder publishes by renaming a new file over the old name.
	replacement := filepath.Join(mkosiDir, "katl-runtime-root.squashfs.tmp")
	writeFile(t, replacement, "runtime-v2")
	if err := os.Rename(replacement, filepath.Join(mkosiDir, "katl-runtime-root.squashfs")); err != nil {
		t.Fatal(err)
	}

	for _, artifact := range manifest.Artifacts {
		if filepath.Dir(artifact.Path) != pinDir {
			t.Errorf("artifact %s recorded at %s, want a path under the pin directory", artifact.Name, artifact.Path)
		}
	}
	// Every pinned build output is a recorded artifact, so the run record
	// names each file a scenario can boot together with its digest.
	recorded := map[string]bool{}
	for _, artifact := range manifest.Artifacts {
		recorded[filepath.Base(artifact.Path)] = true
	}
	pinned, err := os.ReadDir(pinDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range pinned {
		name := entry.Name()
		if strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".sha256") || strings.HasSuffix(name, ".tsv") {
			continue
		}
		if !recorded[name] {
			t.Errorf("pinned %s has no manifest artifact entry", name)
		}
	}
	iso := artifactNamed(t, manifest, "installer-iso")
	if iso.Path != filepath.Join(pinDir, "katl-installer.iso") || iso.Digest != sha256Hex("iso-v1") {
		t.Errorf("installer-iso = %s %s, want the pinned ISO and its digest", iso.Path, iso.Digest)
	}
	root := artifactNamed(t, manifest, "runtime-root")
	got, err := os.ReadFile(root.Path)
	if err != nil || string(got) != "runtime-v1" || root.Digest != sha256Hex("runtime-v1") {
		t.Fatalf("pinned runtime-root = %q, %v, digest %s; want the bytes resolved at start", got, err, root.Digest)
	}
	for _, name := range []string{"katl-installer.iso", "katl-kubernetes.raw.json", "katlos-install-0.0.0-dev-x86_64.squashfs"} {
		if _, err := os.Stat(filepath.Join(pinDir, name)); err != nil {
			t.Errorf("%s was not pinned: %v", name, err)
		}
	}
	for _, name := range []string{"katlos-install-old-x86_64.squashfs", "katl-kubernetes-journey.raw"} {
		if _, err := os.Stat(filepath.Join(pinDir, name)); err == nil {
			t.Errorf("%s from an earlier build was pinned", name)
		}
	}
}

func TestPrepareMkosiPinnedIndexNamesPinnedFiles(t *testing.T) {
	stubPrepareMkosi(t)
	mkosiDir := pinTestBuild(t)
	pinDir := filepath.Join(t.TempDir(), "artifacts")
	if _, err := prepareMkosiPinned(t, mkosiDir, pinDir); err != nil {
		t.Fatalf("prepare-mkosi error = %v", err)
	}

	data, err := os.ReadFile(filepath.Join(pinDir, "artifacts.json"))
	if err != nil {
		t.Fatal(err)
	}
	var index struct {
		Artifacts []struct{ Kind, Path string } `json:"artifacts"`
	}
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatal(err)
	}
	if len(index.Artifacts) != 2 {
		t.Fatalf("pinned index = %s", data)
	}
	for _, artifact := range index.Artifacts {
		if filepath.Dir(artifact.Path) != pinDir {
			t.Errorf("index entry %s names %s, want a file in the pin directory", artifact.Kind, artifact.Path)
		}
	}
}

func TestPrepareMkosiRejectsOutputsFromTwoBuilds(t *testing.T) {
	stubPrepareMkosi(t)
	mkosiDir := pinTestBuild(t)
	writeFile(t, filepath.Join(mkosiDir, "artifacts.json"), `{"artifacts":[{"kind":"runtime-root","path":"_build/mkosi/katl-runtime-root.squashfs","sha256":"`+sha256Hex("runtime-v0")+`"}]}`)

	_, err := prepareMkosiPinned(t, mkosiDir, filepath.Join(t.TempDir(), "artifacts"))

	if err == nil || !strings.Contains(err.Error(), "katl-runtime-root.squashfs") || !strings.Contains(err.Error(), "rerun") {
		t.Fatalf("prepare-mkosi error = %v, want the mismatched output named with a rerun hint", err)
	}
}

// Where a hard link is impossible the run takes its own bytes, which a later
// in-place write to the source must not reach.
func TestCloneOrCopyIsIndependentOfSource(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
	writeFile(t, src, "original")

	if err := cloneOrCopy(src, dst, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("changed!"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "original" {
		t.Fatalf("copy = %q, %v; want the original bytes", got, err)
	}
	if info, err := os.Stat(dst); err != nil || info.Mode().Perm()&0o222 != 0 {
		t.Fatalf("copy mode = %v, %v; want read-only", info.Mode(), err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Fatalf("directory has %d entries, want only src and dst", len(entries))
	}
}

func artifactNamed(t *testing.T, manifest resourcetest.Manifest, name string) resourcetest.Artifact {
	t.Helper()
	for _, artifact := range manifest.Artifacts {
		if artifact.Name == name {
			return artifact
		}
	}
	t.Fatalf("manifest has no artifact %q: %#v", name, manifest.Artifacts)
	return resourcetest.Artifact{}
}
