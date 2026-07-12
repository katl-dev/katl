package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/installer/manifest"
)

func TestWriteAndPath(t *testing.T) {
	repo := testRepoRoot(t)
	workDir := testWorkDir(t, repo)

	installerUKI := writeTestFile(t, workDir, "installer.efi", "installer uki")
	installerKernel := writeTestFile(t, workDir, "vmlinuz", "kernel")
	installerInitrd := writeTestFile(t, workDir, "initrd", "initrd")
	installerISO := writeTestFile(t, workDir, "installer.iso", "installer iso")
	runtimeUKI := writeTestFile(t, workDir, "runtime.efi", "runtime uki")
	runtimeRoot := writeTestFile(t, workDir, "runtime-root.squashfs", "runtime root")
	katlosImage := writeTestFile(t, workDir, "katlos image.squashfs", "katlos image")
	for _, path := range []string{runtimeUKI, runtimeRoot, katlosImage} {
		writeTestChecksum(t, path)
		writeTestJSON(t, path+".json", map[string]any{"path": filepath.Base(path)})
	}

	indexPath := filepath.Join(workDir, "artifacts.json")
	var stdout bytes.Buffer
	err := run([]string{"write", indexPath}, &stdout, &bytes.Buffer{}, []string{
		"KATL_BUILD_COMMIT=test-build",
		"KATL_VERSION=0.1.\"quoted\"\\version",
		"KATL_ARCHITECTURE=x86_64",
		"KATL_INSTALLER_INTERFACE=katl-installer-test",
		"KATL_INSTALLER_UKI=" + installerUKI,
		"KATL_INSTALLER_KERNEL=" + installerKernel,
		"KATL_INSTALLER_INITRD=" + installerInitrd,
		"KATL_INSTALLER_ISO=" + installerISO,
		"KATL_RUNTIME_UKI=" + runtimeUKI,
		"KATL_RUNTIME_UKI_METADATA=" + runtimeUKI + ".json",
		"KATL_RUNTIME_UKI_CHECKSUM=" + runtimeUKI + ".sha256",
		"KATL_RUNTIME_ARTIFACT=" + runtimeRoot,
		"KATL_RUNTIME_METADATA=" + runtimeRoot + ".json",
		"KATL_RUNTIME_CHECKSUM=" + runtimeRoot + ".sha256",
		"KATL_KATLOS_IMAGE=" + katlosImage,
		"KATL_KATLOS_IMAGE_METADATA=" + katlosImage + ".json",
		"KATL_KATLOS_IMAGE_CHECKSUM=" + katlosImage + ".sha256",
	})
	if err != nil {
		t.Fatalf("write error = %v", err)
	}
	if got := stdout.String(); !strings.Contains(got, "artifact index: ") {
		t.Fatalf("stdout = %q", got)
	}

	var index artifactIndex
	readTestJSON(t, indexPath, &index)
	if index.SchemaVersion != 1 || index.Generation != "test-build" {
		t.Fatalf("index header = %#v", index)
	}
	if len(index.Artifacts) != 7 {
		t.Fatalf("artifact count = %d, want 7: %#v", len(index.Artifacts), index.Artifacts)
	}
	byKind := map[string]artifactEntry{}
	for _, artifact := range index.Artifacts {
		byKind[artifact.Kind] = artifact
		if artifact.Path == "" || artifact.SHA256 == "" || artifact.SizeBytes == 0 {
			t.Fatalf("artifact missing bytes identity: %#v", artifact)
		}
	}
	if byKind["katlos-install-image"].Path != relPath(repo, katlosImage) {
		t.Fatalf("katlos path = %q, want %q", byKind["katlos-install-image"].Path, relPath(repo, katlosImage))
	}
	if byKind["installer-iso"].Path != relPath(repo, installerISO) || byKind["installer-iso"].Format != "iso" {
		t.Fatalf("installer ISO entry = %#v", byKind["installer-iso"])
	}

	var metadata bootMetadata
	readTestJSON(t, installerUKI+".json", &metadata)
	if metadata.Kind != "InstallerBootArtifact" || metadata.ArtifactRole != "installer-uki" {
		t.Fatalf("installer metadata = %#v", metadata)
	}
	if metadata.Version != `0.1."quoted"\version` {
		t.Fatalf("installer metadata version = %q", metadata.Version)
	}
	if metadata.InstallerInterface != "katl-installer-test" {
		t.Fatalf("installer interface = %q", metadata.InstallerInterface)
	}
	readTestJSON(t, installerISO+".json", &metadata)
	if metadata.ArtifactRole != "installer-iso" || metadata.Format != "iso" || metadata.BuildID != "test-build" {
		t.Fatalf("installer ISO metadata = %#v", metadata)
	}

	stdout.Reset()
	err = run([]string{"path", "runtime-root", indexPath}, &stdout, &bytes.Buffer{}, []string{})
	if err != nil {
		t.Fatalf("path error = %v", err)
	}
	if stdout.String() != runtimeRoot {
		t.Fatalf("runtime-root path = %q, want %q", stdout.String(), runtimeRoot)
	}
}

func TestWriteIncludesExistingInstallerISO(t *testing.T) {
	repo := testRepoRoot(t)
	workDir := testWorkDir(t, repo)
	installerUKI := writeTestFile(t, workDir, "installer.efi", "installer uki")
	installerKernel := writeTestFile(t, workDir, "vmlinuz", "kernel")
	installerInitrd := writeTestFile(t, workDir, "initrd", "initrd")
	installerISO := writeTestFile(t, workDir, "installer.iso", "installer iso")
	runtimeUKI := writeTestFile(t, workDir, "runtime.efi", "runtime uki")
	runtimeRoot := writeTestFile(t, workDir, "runtime-root.squashfs", "runtime root")
	for _, path := range []string{runtimeUKI, runtimeRoot} {
		writeTestChecksum(t, path)
		writeTestJSON(t, path+".json", map[string]any{"path": filepath.Base(path)})
	}

	indexPath := filepath.Join(workDir, "artifacts.json")
	cfg := config{
		RepoRoot:             repo,
		InstallerUKI:         installerUKI,
		InstallerKernel:      installerKernel,
		InstallerInitrd:      installerInitrd,
		InstallerISO:         installerISO,
		InstallerISOExplicit: false,
		RuntimeUKI:           runtimeUKI,
		RuntimeUKIMetadata:   runtimeUKI + ".json",
		RuntimeUKIChecksum:   runtimeUKI + ".sha256",
		RuntimeRoot:          runtimeRoot,
		RuntimeMetadata:      runtimeRoot + ".json",
		RuntimeChecksum:      runtimeRoot + ".sha256",
		Generation:           "test-build",
		Version:              "2026.7.0-dev.1",
		Architecture:         "x86_64",
		InstallerInterface:   "katl-installer-test",
	}
	if err := writeIndex(indexPath, cfg); err != nil {
		t.Fatalf("writeIndex error = %v", err)
	}

	var index artifactIndex
	readTestJSON(t, indexPath, &index)
	for _, artifact := range index.Artifacts {
		if artifact.Kind == "installer-iso" {
			return
		}
	}
	t.Fatalf("existing installer ISO omitted from index: %#v", index.Artifacts)
}

func TestWriteRuntimeIndexReplacesRuntimeAndPreservesInstaller(t *testing.T) {
	repo := testRepoRoot(t)
	workDir := testWorkDir(t, repo)
	runtimeUKI := writeTestFile(t, workDir, "runtime.efi", "new runtime uki")
	runtimeRoot := writeTestFile(t, workDir, "runtime-root.squashfs", "new runtime root")
	for _, path := range []string{runtimeUKI, runtimeRoot} {
		writeTestChecksum(t, path)
		writeTestJSON(t, path+".json", map[string]any{"path": filepath.Base(path)})
	}
	indexPath := filepath.Join(workDir, "artifacts.json")
	writeTestJSON(t, indexPath, artifactIndex{
		SchemaVersion: 1,
		Generation:    "old-build",
		Artifacts: []artifactEntry{
			{Kind: "installer-uki", Path: "installer.efi", SHA256: strings.Repeat("a", 64)},
			{Kind: "runtime-root", Path: "old-runtime.squashfs", SHA256: strings.Repeat("b", 64)},
		},
	})
	cfg := config{
		RepoRoot:           repo,
		RuntimeUKI:         runtimeUKI,
		RuntimeUKIMetadata: runtimeUKI + ".json",
		RuntimeUKIChecksum: runtimeUKI + ".sha256",
		RuntimeRoot:        runtimeRoot,
		RuntimeMetadata:    runtimeRoot + ".json",
		RuntimeChecksum:    runtimeRoot + ".sha256",
		Generation:         "new-build",
	}
	if err := writeRuntimeIndex(indexPath, cfg); err != nil {
		t.Fatal(err)
	}
	var index artifactIndex
	readTestJSON(t, indexPath, &index)
	if index.Generation != "new-build" || len(index.Artifacts) != 3 {
		t.Fatalf("index = %#v", index)
	}
	byKind := make(map[string]artifactEntry, len(index.Artifacts))
	for _, entry := range index.Artifacts {
		byKind[entry.Kind] = entry
	}
	if byKind["installer-uki"].Path != "installer.efi" {
		t.Fatalf("installer entry = %#v", byKind["installer-uki"])
	}
	if byKind["runtime-root"].Path != relPath(repo, runtimeRoot) || byKind["runtime-root"].SHA256 != testFileSHA256(t, runtimeRoot) {
		t.Fatalf("runtime root entry = %#v", byKind["runtime-root"])
	}
}

func TestPathForKindRejectsDuplicate(t *testing.T) {
	repo := testRepoRoot(t)
	indexPath := filepath.Join(testWorkDir(t, repo), "artifacts.json")
	writeTestJSON(t, indexPath, artifactIndex{
		SchemaVersion: 1,
		Artifacts: []artifactEntry{
			{Kind: "runtime-root", Path: "_build/mkosi/root-a.squashfs"},
			{Kind: "runtime-root", Path: "_build/mkosi/root-b.squashfs"},
		},
	})

	_, err := pathForKind(indexPath, repo, "runtime-root")
	if err == nil || !strings.Contains(err.Error(), "appears more than once") {
		t.Fatalf("pathForKind duplicate error = %v", err)
	}
}

func TestKubernetesSysextFromLog(t *testing.T) {
	repo := testRepoRoot(t)
	workDir := testWorkDir(t, repo)
	runtimeRoot := writeTestFile(t, workDir, "katl-runtime-root.squashfs", "runtime root")
	runtimeSHA := testFileSHA256(t, runtimeRoot)
	writeTestJSON(t, runtimeRoot+".json", map[string]any{"sha256": runtimeSHA})
	sysext := writeTestFile(t, workDir, "katl-kubernetes.raw", "kubernetes sysext")
	logPath := filepath.Join(workDir, "mkosi.log")
	if err := os.WriteFile(logPath, []byte(strings.Join([]string{
		"kubeadm x86_64 1.36.0-1 kubernetes installed",
		"kubelet x86_64 1.36.0-1 kubernetes installed",
		"kubectl x86_64 1.36.0-1 kubernetes installed",
		"cri-tools x86_64 1.36.0-1 kubernetes installed",
		"ethtool x86_64 2:7.0-1.fc44 fedora installed",
		"socat x86_64 0:1.8.1.1-1.fc44 updates installed",
		"",
	}, "\n")), 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", logPath, err)
	}

	var stdout bytes.Buffer
	err := run([]string{
		"write-kubernetes-sysext-from-log",
		"--artifact", sysext,
		"--log", logPath,
		"--runtime-artifact", runtimeRoot,
		"--runtime-metadata", runtimeRoot + ".json",
		"--repo-id", "kubernetes",
		"--repo-base-url", "https://pkgs.k8s.io/core:/stable:/v1.36/rpm/",
		"--repo-minor", "v1.36",
		"--expected-payload-version", "v1.36.0",
		"--expected-kubeadm-version", "1.36.0-1",
	}, &stdout, &bytes.Buffer{}, []string{"KATL_BUILD_COMMIT=test-build", "KATL_ARCHITECTURE=x86_64"})
	if err != nil {
		t.Fatalf("write-kubernetes-sysext-from-log error = %v", err)
	}
	var metadata localMetadata
	readTestJSON(t, sysext+".json", &metadata)
	if metadata.PayloadVersion != "v1.36.0" || metadata.PackageVersions["kubeadm"] != "1.36.0-1" {
		t.Fatalf("metadata = %#v", metadata)
	}
	if metadata.PackageVersions["ethtool"] != "2:7.0-1.fc44" || metadata.PackageVersions["socat"] != "0:1.8.1.1-1.fc44" {
		t.Fatalf("helper packageVersions = %#v", metadata.PackageVersions)
	}
	if metadata.CompatibleRuntime == nil || metadata.CompatibleRuntime.ArtifactSHA256 != runtimeSHA {
		t.Fatalf("compatibleRuntime = %#v", metadata.CompatibleRuntime)
	}
}

func TestMetadataWriters(t *testing.T) {
	repo := testRepoRoot(t)
	workDir := testWorkDir(t, repo)
	runtimeRoot := writeTestFile(t, workDir, "katl-runtime-root.squashfs", "runtime root")
	runtimeUKI := writeTestFile(t, workDir, "katl-runtime.efi", "runtime uki")
	sysext := writeTestFile(t, workDir, "katl-kubernetes.raw", "kubernetes sysext")
	env := []string{
		"KATL_BUILD_COMMIT=test-build",
		"KATL_ARCHITECTURE=x86_64",
	}

	var stdout bytes.Buffer
	if err := run([]string{"write-runtime-root", "--artifact", runtimeRoot}, &stdout, &bytes.Buffer{}, env); err != nil {
		t.Fatalf("write-runtime-root error = %v", err)
	}
	runtimeSHA := strings.TrimSpace(stdout.String())
	var rootMetadata localMetadata
	readTestJSON(t, runtimeRoot+".json", &rootMetadata)
	if rootMetadata.Name != "runtime-root" || rootMetadata.SHA256 != runtimeSHA || rootMetadata.Compression != "zstd" {
		t.Fatalf("runtime root metadata = %#v, sha stdout %q", rootMetadata, runtimeSHA)
	}
	if rootMetadata.CompatibleBoot == nil || rootMetadata.CompatibleBoot.Kind != "uki" {
		t.Fatalf("runtime root boot compatibility = %#v", rootMetadata.CompatibleBoot)
	}

	stdout.Reset()
	if err := run([]string{
		"write-runtime-uki",
		"--artifact", runtimeUKI,
		"--runtime-artifact", runtimeRoot,
		"--runtime-sha256", runtimeSHA,
		"--kernel-version", "6.12.0",
	}, &stdout, &bytes.Buffer{}, env); err != nil {
		t.Fatalf("write-runtime-uki error = %v", err)
	}
	var ukiMetadata localMetadata
	readTestJSON(t, runtimeUKI+".json", &ukiMetadata)
	if ukiMetadata.Name != "runtime-uki" || ukiMetadata.CompatibleRuntime == nil {
		t.Fatalf("runtime UKI metadata = %#v", ukiMetadata)
	}
	if ukiMetadata.CompatibleRuntime.ArtifactPath != filepath.Base(runtimeRoot) || ukiMetadata.CompatibleRuntime.ArtifactSHA256 != runtimeSHA {
		t.Fatalf("runtime UKI compatibility = %#v", ukiMetadata.CompatibleRuntime)
	}
	if ukiMetadata.KernelVersion != "6.12.0" {
		t.Fatalf("kernelVersion = %q", ukiMetadata.KernelVersion)
	}

	stdout.Reset()
	if err := run([]string{
		"write-kubernetes-sysext",
		"--artifact", sysext,
		"--payload-version", "v1.36.0",
		"--kubeadm-version", "1.36.0-1",
		"--kubelet-version", "1.36.0-1",
		"--kubectl-version", "1.36.0-1",
		"--cri-tools-version", "1.36.0-1",
		"--ethtool-version", "2:7.0-1.fc44",
		"--socat-version", "1.8.1.1-1.fc44",
		"--runtime-artifact", runtimeRoot,
		"--runtime-metadata", runtimeRoot + ".json",
		"--repo-id", "kubernetes",
		"--repo-base-url", "https://pkgs.k8s.io/core:/stable:/v1.36/rpm/",
		"--repo-minor", "v1.36",
	}, &stdout, &bytes.Buffer{}, env); err != nil {
		t.Fatalf("write-kubernetes-sysext error = %v", err)
	}
	var sysextMetadata localMetadata
	readTestJSON(t, sysext+".json", &sysextMetadata)
	if sysextMetadata.Name != "kubernetes" || sysextMetadata.PayloadVersion != "v1.36.0" {
		t.Fatalf("Kubernetes sysext metadata = %#v", sysextMetadata)
	}
	if sysextMetadata.CompatibleRuntime == nil || sysextMetadata.CompatibleRuntime.ArtifactSHA256 != runtimeSHA {
		t.Fatalf("Kubernetes sysext compatibility = %#v", sysextMetadata.CompatibleRuntime)
	}
	if sysextMetadata.SourceRepo == nil || sysextMetadata.SourceRepo.Minor != "v1.36" {
		t.Fatalf("Kubernetes source repo = %#v", sysextMetadata.SourceRepo)
	}
	if sysextMetadata.PackageVersions["cri-tools"] != "1.36.0-1" ||
		sysextMetadata.PackageVersions["ethtool"] != "2:7.0-1.fc44" ||
		sysextMetadata.PackageVersions["socat"] != "1.8.1.1-1.fc44" {
		t.Fatalf("packageVersions = %#v", sysextMetadata.PackageVersions)
	}

	indexPath := filepath.Join(workDir, "katlos-root", "katlos", "image.json")
	stdout.Reset()
	if err := run([]string{
		"write-katlos-index",
		"--output", indexPath,
		"--image-role", "install",
		"--version", "0.1.0",
		"--build-id", "test-build",
		"--architecture", "x86_64",
		"--runtime-interface", "katl-runtime-1",
		"--runtime-root", runtimeRoot,
		"--runtime-root-metadata", runtimeRoot + ".json",
		"--runtime-uki", runtimeUKI,
		"--runtime-uki-metadata", runtimeUKI + ".json",
	}, &stdout, &bytes.Buffer{}, env); err != nil {
		t.Fatalf("write-katlos-index error = %v", err)
	}
	var index katlosIndex
	readTestJSON(t, indexPath, &index)
	if index.Kind != "KatlOSImage" || len(index.Components) != 2 {
		t.Fatalf("KatlOS index = %#v", index)
	}
	if index.Components[0].Compatibility.Boot == nil || index.Components[1].Compatibility.RuntimeRoot == nil {
		t.Fatalf("KatlOS component compatibility = %#v", index.Components)
	}
	assertFileContains(t, filepath.Join(workDir, "katlos-root", "components", "metadata", "runtime-root.sha256"), runtimeSHA+"  ../runtime/root.squashfs\n")

	upgradeIndexPath := filepath.Join(workDir, "katlos-upgrade-root", "katlos", "image.json")
	stdout.Reset()
	if err := run([]string{
		"write-katlos-index",
		"--output", upgradeIndexPath,
		"--image-role", "upgrade",
		"--version", "0.1.0",
		"--build-id", "test-build",
		"--architecture", "x86_64",
		"--runtime-interface", "katl-runtime-1",
		"--runtime-root", runtimeRoot,
		"--runtime-root-metadata", runtimeRoot + ".json",
		"--runtime-uki", runtimeUKI,
		"--runtime-uki-metadata", runtimeUKI + ".json",
	}, &stdout, &bytes.Buffer{}, env); err != nil {
		t.Fatalf("write-katlos-index upgrade error = %v", err)
	}
	var upgradeIndex katlosIndex
	readTestJSON(t, upgradeIndexPath, &upgradeIndex)
	if upgradeIndex.ImageRole != "upgrade" || len(upgradeIndex.Components) != 2 {
		t.Fatalf("KatlOS upgrade index = %#v", upgradeIndex)
	}

	image := writeTestFile(t, workDir, "katlos-install.squashfs", "katlos image")
	stdout.Reset()
	if err := run([]string{
		"write-katlos-artifact",
		"--artifact", image,
		"--image-role", "install",
		"--version", "0.1.0",
		"--build-id", "test-build",
		"--architecture", "x86_64",
		"--runtime-interface", "katl-runtime-1",
	}, &stdout, &bytes.Buffer{}, env); err != nil {
		t.Fatalf("write-katlos-artifact error = %v", err)
	}
	var artifact katlosArtifactMetadata
	readTestJSON(t, image+".json", &artifact)
	if artifact.Kind != "KatlOSImageArtifact" || artifact.SHA256 != strings.TrimSpace(stdout.String()) {
		t.Fatalf("KatlOS artifact metadata = %#v, stdout %q", artifact, stdout.String())
	}
	assertFileContains(t, image+".sha256", artifact.SHA256+"  "+filepath.Base(image)+"\n")

	if err := run([]string{
		"write-katlos-artifact",
		"--artifact", image,
		"--image-role", "node-local",
		"--version", "0.1.0",
		"--build-id", "test-build",
		"--architecture", "x86_64",
		"--runtime-interface", "katl-runtime-1",
	}, &stdout, &bytes.Buffer{}, env); err == nil {
		t.Fatalf("write-katlos-artifact accepted unsupported image role")
	}
}

func TestBindInstallManifestImage(t *testing.T) {
	repo := testRepoRoot(t)
	workDir := testWorkDir(t, repo)
	image := writeTestFile(t, workDir, "katlos-install.squashfs", "katlos image")
	imageSHA := testFileSHA256(t, image)
	imageInfo, err := os.Stat(image)
	if err != nil {
		t.Fatalf("Stat(%s) error = %v", image, err)
	}
	writeTestJSON(t, image+".json", katlosArtifactMetadata{
		APIVersion:       "katl.dev/v1alpha1",
		Kind:             "KatlOSImageArtifact",
		ImageRole:        "install",
		Format:           "squashfs",
		Version:          "0.1.0",
		Architecture:     "x86_64",
		RuntimeInterface: "katl-runtime-1",
		Path:             filepath.Base(image),
		SizeBytes:        imageInfo.Size(),
		SHA256:           imageSHA,
	})
	indexPath := filepath.Join(workDir, "artifacts.json")
	writeTestJSON(t, indexPath, artifactIndex{
		SchemaVersion: 1,
		Artifacts: []artifactEntry{{
			Kind:         "katlos-install-image",
			Path:         relPath(repo, image),
			Format:       "squashfs",
			SizeBytes:    imageInfo.Size(),
			SHA256:       imageSHA,
			MetadataPath: relPath(repo, image+".json"),
		}},
	})
	template := filepath.Join(workDir, "template.yaml")
	if err := os.WriteFile(template, []byte(`apiVersion: install.katl.dev/v1alpha1
kind: InstallManifest
node:
  identity:
    hostname: cp-1
    ssh:
      authorizedKeys:
      - ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDAxMjM0NTY3ODlhYmNkZWYwMTIzNDU2Nzg5YWJjZGVm katl@example
  systemRole: control-plane
install:
  wipeTarget: true
  targetDisk:
    serial: old-target
katlosImage:
  localRef: old.squashfs
  sha256: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  sizeBytes: 1
  version: old
  architecture: x86_64
  runtimeInterface: old-runtime
  role: install
`), 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", template, err)
	}
	output := filepath.Join(workDir, "out", "install.yaml")
	var stdout bytes.Buffer
	err = run([]string{
		"bind-install-manifest-image",
		"--artifact-index", indexPath,
		"--template", template,
		"--output", output,
		"--local-ref", "images/katlos.squashfs",
		"--target-disk-by-id", "/dev/disk/by-id/ata-target",
	}, &stdout, &bytes.Buffer{}, []string{})
	if err != nil {
		t.Fatalf("bind-install-manifest-image error = %v", err)
	}
	if !strings.Contains(stdout.String(), "install manifest: ") || !strings.Contains(stdout.String(), "katlos image ref: ") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	file, err := os.Open(output)
	if err != nil {
		t.Fatalf("Open(%s) error = %v", output, err)
	}
	defer file.Close()
	bound, err := manifest.Decode(file)
	if err != nil {
		t.Fatalf("Decode(%s) error = %v", output, err)
	}
	if bound.KatlosImage.LocalRef != "images/katlos.squashfs" || bound.KatlosImage.SHA256 != imageSHA || bound.KatlosImage.SizeBytes != uint64(imageInfo.Size()) {
		t.Fatalf("bound image = %#v", bound.KatlosImage)
	}
	if bound.KatlosImage.Version != "0.1.0" || bound.KatlosImage.RuntimeInterface != "katl-runtime-1" || bound.KatlosImage.Role != "install" {
		t.Fatalf("bound image metadata = %#v", bound.KatlosImage)
	}
	if bound.Install.TargetDisk.ByID != "/dev/disk/by-id/ata-target" || bound.Install.TargetDisk.Serial != "" {
		t.Fatalf("target disk = %#v", bound.Install.TargetDisk)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(filepath.Dir(output), "images", "katlos.squashfs"))
	if err != nil {
		t.Fatalf("EvalSymlinks() error = %v", err)
	}
	if resolved != image {
		t.Fatalf("localRef resolved to %s, want %s", resolved, image)
	}
}

func TestBindInstallManifestImageRejectsMismatchedIndex(t *testing.T) {
	repo := testRepoRoot(t)
	workDir := testWorkDir(t, repo)
	image := writeTestFile(t, workDir, "katlos-install.squashfs", "katlos image")
	imageSHA := testFileSHA256(t, image)
	writeTestJSON(t, image+".json", katlosArtifactMetadata{
		Kind:             "KatlOSImageArtifact",
		ImageRole:        "install",
		Version:          "0.1.0",
		Architecture:     "x86_64",
		RuntimeInterface: "katl-runtime-1",
		SizeBytes:        int64(len("katlos image")),
		SHA256:           imageSHA,
	})
	indexPath := filepath.Join(workDir, "artifacts.json")
	writeTestJSON(t, indexPath, artifactIndex{
		SchemaVersion: 1,
		Artifacts: []artifactEntry{{
			Kind:         "katlos-install-image",
			Path:         relPath(repo, image),
			SizeBytes:    int64(len("katlos image")),
			SHA256:       strings.Repeat("b", 64),
			MetadataPath: relPath(repo, image+".json"),
		}},
	})
	err := run([]string{
		"bind-install-manifest-image",
		"--artifact-index", indexPath,
		"--template", filepath.Join(workDir, "missing.yaml"),
		"--output", filepath.Join(workDir, "out.yaml"),
	}, &bytes.Buffer{}, &bytes.Buffer{}, []string{})
	if err == nil || !strings.Contains(err.Error(), "sha256 does not match artifact index") {
		t.Fatalf("bind-install-manifest-image error = %v, want index mismatch", err)
	}
}

func testRepoRoot(t *testing.T) string {
	t.Helper()
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("repoRoot error = %v", err)
	}
	return root
}

func testWorkDir(t *testing.T, repo string) string {
	t.Helper()
	buildDir := filepath.Join(repo, "build")
	if err := os.MkdirAll(buildDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", buildDir, err)
	}
	dir, err := os.MkdirTemp(buildDir, "katl-mkosi-artifacts-")
	if err != nil {
		t.Fatalf("MkdirTemp(%s) error = %v", buildDir, err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Fatalf("RemoveAll(%s) error = %v", dir, err)
		}
	})
	return dir
}

func writeTestFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
	return path
}

func writeTestChecksum(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, err)
	}
	sum := sha256.Sum256(data)
	content := hex.EncodeToString(sum[:]) + "  " + filepath.Base(path) + "\n"
	if err := os.WriteFile(path+".sha256", []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s.sha256) error = %v", path, err)
	}
}

func testFileSHA256(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func writeTestJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("Marshal(%s) error = %v", path, err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
}

func readTestJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, err)
	}
	if err := json.Unmarshal(data, value); err != nil {
		t.Fatalf("Unmarshal(%s) error = %v\n%s", path, err, data)
	}
}

func assertFileContains(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, err)
	}
	if string(data) != want {
		t.Fatalf("%s = %q, want %q", path, data, want)
	}
}
