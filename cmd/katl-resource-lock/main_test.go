package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zariel/katl/internal/resourcetest"
)

func TestRunRefreshAndVerify(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "resource-manifest.json")
	lockPath := filepath.Join(dir, "mkosi.profiles", "resource-package-lock.json")
	manifest := commandManifest("")
	writeTestManifest(t, manifestPath, manifest)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := run([]string{"refresh", "--manifest", manifestPath, "--output", lockPath}, &stdout, &stderr); err != nil {
		t.Fatalf("refresh error = %v stderr=%s", err, stderr.String())
	}
	lockData, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("read lock: %v", err)
	}
	manifest.PackageSets[0].LockDigest = resourcetest.PackageLockDigest(lockData)
	writeTestManifest(t, manifestPath, manifest)

	stdout.Reset()
	stderr.Reset()
	if err := run([]string{"verify", "--manifest", manifestPath, "--lock", lockPath}, &stdout, &stderr); err != nil {
		t.Fatalf("verify error = %v stderr=%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "verified:") {
		t.Fatalf("stdout = %q, want verified output", stdout.String())
	}
}

func TestRunVerifyRejectsPackageDrift(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "resource-manifest.json")
	lockPath := filepath.Join(dir, "resource-package-lock.json")
	manifest := commandManifest("")
	writeTestManifest(t, manifestPath, manifest)
	if err := run([]string{"refresh", "--manifest", manifestPath, "--output", lockPath}, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("refresh error = %v", err)
	}
	lockData, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("read lock: %v", err)
	}
	manifest.PackageSets[0].LockDigest = resourcetest.PackageLockDigest(lockData)
	manifest.PackageSets[0].Packages[0].NEVRA = "systemd-0:259.7-1.fc44.x86_64"
	writeTestManifest(t, manifestPath, manifest)

	err = run([]string{"verify", "--manifest", manifestPath, "--lock", lockPath}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "NEVRA drift") {
		t.Fatalf("verify error = %v, want NEVRA drift", err)
	}
}

func TestRunRequiresManifest(t *testing.T) {
	err := run([]string{"verify"}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "--manifest is required") {
		t.Fatalf("run() error = %v, want manifest requirement", err)
	}
}

func TestRunAddArtifact(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "resource-manifest.json")
	outputPath := filepath.Join(dir, "updated-manifest.json")
	artifactPath := filepath.Join(dir, "katl-runtime-root.squashfs")
	writeTestManifest(t, manifestPath, commandArtifactManifest())
	if err := os.WriteFile(artifactPath, []byte("runtime-root"), 0o644); err != nil {
		t.Fatalf("write artifact: %v", err)
	}

	var stdout bytes.Buffer
	err := run([]string{
		"add-artifact",
		"--manifest", manifestPath,
		"--output", outputPath,
		"--name", "runtime-root",
		"--kind", "squashfs",
		"--path", artifactPath,
	}, &stdout, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("add-artifact error = %v", err)
	}
	updated, err := readManifest(outputPath)
	if err != nil {
		t.Fatalf("read updated manifest: %v", err)
	}
	if len(updated.Artifacts) != 1 || updated.Artifacts[0].Digest == "" || updated.Artifacts[0].SizeBytes != int64(len("runtime-root")) {
		t.Fatalf("updated artifacts = %#v", updated.Artifacts)
	}
	if !strings.Contains(stdout.String(), "artifact: runtime-root") {
		t.Fatalf("stdout = %q, want artifact output", stdout.String())
	}
}

func TestRunAddRPMPackageSet(t *testing.T) {
	oldQuery := queryRPMPackages
	t.Cleanup(func() { queryRPMPackages = oldQuery })
	queryRPMPackages = func(root string) ([]resourcetest.Package, error) {
		if root != "build/mkosi/katl-runtime-root" {
			t.Fatalf("root = %q", root)
		}
		return []resourcetest.Package{{
			Name:  "systemd",
			NEVRA: "systemd-0:259.6-1.fc44.x86_64",
		}}, nil
	}

	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "resource-manifest.json")
	lockManifestPath := filepath.Join(dir, "lock-source-manifest.json")
	outputPath := filepath.Join(dir, "updated-manifest.json")
	lockPath := filepath.Join(dir, "resource-package-lock.json")
	writeTestManifest(t, lockManifestPath, commandManifest(""))
	if err := run([]string{"refresh", "--manifest", lockManifestPath, "--output", lockPath}, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("refresh error = %v", err)
	}
	writeTestManifest(t, manifestPath, commandManifestSkeleton())

	var stdout bytes.Buffer
	err := run([]string{
		"add-rpm-package-set",
		"--manifest", manifestPath,
		"--output", outputPath,
		"--name", "runtime",
		"--source", "mkosi.profiles/runtime",
		"--root", "build/mkosi/katl-runtime-root",
		"--lock", lockPath,
		"--distribution", "fedora",
		"--release", "44",
		"--architecture", "x86_64",
		"--repository", "fedora=https://example.invalid/fedora/44",
		"--profile-name", "runtime",
		"--profile-path", "mkosi.profiles/runtime",
		"--profile-config-sha256", strings.Repeat("a", 64),
	}, &stdout, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("add-rpm-package-set error = %v", err)
	}
	updated, err := readManifest(outputPath)
	if err != nil {
		t.Fatalf("read updated manifest: %v", err)
	}
	if updated.PackageSets[0].LockDigest == "" || updated.PackageSets[0].Repositories[0].ID != "fedora" {
		t.Fatalf("updated manifest package set = %#v", updated.PackageSets[0])
	}
	if !strings.Contains(stdout.String(), "packages: 1") {
		t.Fatalf("stdout = %q, want package count", stdout.String())
	}
}

func TestRunPrepareMkosiRefreshAndStrict(t *testing.T) {
	oldQuery := queryRPMPackages
	oldReadKatlOSIndex := readKatlOSIndex
	t.Cleanup(func() {
		queryRPMPackages = oldQuery
		readKatlOSIndex = oldReadKatlOSIndex
	})
	queryRPMPackages = func(root string) ([]resourcetest.Package, error) {
		if !strings.HasSuffix(root, "katl-runtime-root") {
			t.Fatalf("root = %q", root)
		}
		return []resourcetest.Package{{
			Name:  "systemd",
			NEVRA: "systemd-0:259.6-1.fc44.x86_64",
		}}, nil
	}
	readKatlOSIndex = func(path string) (katlosIndex, error) {
		return katlosTestIndex(strings.Repeat("d", 64)), nil
	}

	dir := t.TempDir()
	mkosiDir := filepath.Join(dir, "build", "mkosi")
	runtimeRoot := filepath.Join(mkosiDir, "katl-runtime-root")
	if err := os.MkdirAll(runtimeRoot, 0o755); err != nil {
		t.Fatalf("mkdir runtime root: %v", err)
	}
	writeFile(t, filepath.Join(mkosiDir, "katl-runtime-root.squashfs"), "runtime")
	writeFile(t, filepath.Join(mkosiDir, "katl-runtime.efi"), "runtime-uki")
	writeFile(t, filepath.Join(mkosiDir, "katl-kubernetes.raw"), "kubernetes")
	writeKubernetesMetadata(t, filepath.Join(mkosiDir, "katl-kubernetes.raw.json"), "0:1.36.1-150500.1.1")
	writeInstallerPackages(t, filepath.Join(mkosiDir, "katl-installer.packages.tsv"), "systemd-0:259.6-1.fc44.x86_64")
	writeFile(t, filepath.Join(mkosiDir, "katlos-install-0.0.0-dev-x86_64.squashfs"), "katlos")

	manifestPath := filepath.Join(dir, "resource-manifest.json")
	lockPath := filepath.Join(dir, "resource-package-lock.json")
	var stdout bytes.Buffer
	err := run([]string{
		"prepare-mkosi",
		"--manifest", manifestPath,
		"--lock", lockPath,
		"--mkosi-dir", mkosiDir,
		"--runtime-root", runtimeRoot,
		"--mode", "refresh",
		"--run-id", "run-1",
		"--git-revision", "test",
		"--fedora-repository", "fedora=https://example.invalid/fedora/44",
		"--mkosi-version", "26",
	}, &stdout, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("prepare refresh error = %v", err)
	}
	manifest, err := readManifest(manifestPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if len(manifest.Artifacts) != 4 || len(manifest.PackageSets) != 4 {
		t.Fatalf("manifest artifacts=%d packageSets=%#v", len(manifest.Artifacts), manifest.PackageSets)
	}
	for _, set := range manifest.PackageSets {
		if set.LockDigest == "" {
			t.Fatalf("package set %q missing lock digest", set.Name)
		}
	}
	if len(manifest.Tools) != 1 || manifest.Tools[0].Name != "mkosi" || manifest.Tools[0].Version != "26" {
		t.Fatalf("manifest tools = %#v", manifest.Tools)
	}
	for _, profile := range manifest.MkosiProfiles {
		if profile.ConfigDigest == "" {
			t.Fatalf("manifest mkosiProfiles = %#v", manifest.MkosiProfiles)
		}
	}
	installerSet := packageSet(manifest.PackageSets, "installer-image")
	if packageNEVRA(installerSet.Packages, "systemd") != "systemd-0:259.6-1.fc44.x86_64" {
		t.Fatalf("installer package set = %#v", installerSet)
	}
	katlosSet := packageSet(manifest.PackageSets, "katlos-install-image")
	if packageChecksum(katlosSet.Packages, "katlos-component-runtime-root") != strings.Repeat("d", 64) {
		t.Fatalf("KatlOS package set = %#v", katlosSet)
	}
	kubernetesSet := packageSet(manifest.PackageSets, "kubernetes-sysext")
	if kubernetesSet.Name != "kubernetes-sysext" || packageNEVRA(kubernetesSet.Packages, "kubeadm") != "kubeadm-0:1.36.1-150500.1.1.x86_64" {
		t.Fatalf("Kubernetes package set = %#v", kubernetesSet)
	}

	stdout.Reset()
	err = run([]string{
		"prepare-mkosi",
		"--manifest", manifestPath,
		"--lock", lockPath,
		"--mkosi-dir", mkosiDir,
		"--runtime-root", runtimeRoot,
		"--mode", "strict",
		"--run-id", "run-1",
		"--git-revision", "test",
		"--fedora-repository", "fedora=https://example.invalid/fedora/44",
		"--mkosi-version", "26",
	}, &stdout, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("prepare strict error = %v", err)
	}
	if !strings.Contains(stdout.String(), "mode: strict") {
		t.Fatalf("stdout = %q, want strict mode", stdout.String())
	}
}

func TestRunPrepareMkosiStrictRejectsKubernetesDrift(t *testing.T) {
	oldQuery := queryRPMPackages
	t.Cleanup(func() { queryRPMPackages = oldQuery })
	queryRPMPackages = func(root string) ([]resourcetest.Package, error) {
		return []resourcetest.Package{{
			Name:  "systemd",
			NEVRA: "systemd-0:259.6-1.fc44.x86_64",
		}}, nil
	}

	dir := t.TempDir()
	mkosiDir := filepath.Join(dir, "build", "mkosi")
	runtimeRoot := filepath.Join(mkosiDir, "katl-runtime-root")
	if err := os.MkdirAll(runtimeRoot, 0o755); err != nil {
		t.Fatalf("mkdir runtime root: %v", err)
	}
	writeFile(t, filepath.Join(mkosiDir, "katl-runtime-root.squashfs"), "runtime")
	writeKubernetesMetadata(t, filepath.Join(mkosiDir, "katl-kubernetes.raw.json"), "0:1.36.1-150500.1.1")
	manifestPath := filepath.Join(dir, "resource-manifest.json")
	lockPath := filepath.Join(dir, "resource-package-lock.json")
	args := []string{
		"prepare-mkosi",
		"--manifest", manifestPath,
		"--lock", lockPath,
		"--mkosi-dir", mkosiDir,
		"--runtime-root", runtimeRoot,
		"--mode", "refresh",
		"--run-id", "run-1",
		"--git-revision", "test",
		"--mkosi-version", "26",
	}
	if err := run(args, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("prepare refresh error = %v", err)
	}
	writeKubernetesMetadata(t, filepath.Join(mkosiDir, "katl-kubernetes.raw.json"), "0:1.36.2-150500.1.1")
	err := run([]string{
		"prepare-mkosi",
		"--manifest", manifestPath,
		"--lock", lockPath,
		"--mkosi-dir", mkosiDir,
		"--runtime-root", runtimeRoot,
		"--mode", "strict",
		"--run-id", "run-1",
		"--git-revision", "test",
		"--mkosi-version", "26",
	}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "NEVRA drift") {
		t.Fatalf("prepare strict error = %v, want NEVRA drift", err)
	}
}

func TestRunPrepareMkosiStrictRejectsInstallerDrift(t *testing.T) {
	oldQuery := queryRPMPackages
	t.Cleanup(func() { queryRPMPackages = oldQuery })
	queryRPMPackages = func(root string) ([]resourcetest.Package, error) {
		return []resourcetest.Package{{
			Name:  "systemd",
			NEVRA: "systemd-0:259.6-1.fc44.x86_64",
		}}, nil
	}

	dir := t.TempDir()
	mkosiDir := filepath.Join(dir, "build", "mkosi")
	runtimeRoot := filepath.Join(mkosiDir, "katl-runtime-root")
	if err := os.MkdirAll(runtimeRoot, 0o755); err != nil {
		t.Fatalf("mkdir runtime root: %v", err)
	}
	writeFile(t, filepath.Join(mkosiDir, "katl-runtime-root.squashfs"), "runtime")
	packagePath := filepath.Join(mkosiDir, "katl-installer.packages.tsv")
	writeInstallerPackages(t, packagePath, "systemd-0:259.6-1.fc44.x86_64")
	manifestPath := filepath.Join(dir, "resource-manifest.json")
	lockPath := filepath.Join(dir, "resource-package-lock.json")
	args := []string{
		"prepare-mkosi",
		"--manifest", manifestPath,
		"--lock", lockPath,
		"--mkosi-dir", mkosiDir,
		"--runtime-root", runtimeRoot,
		"--mode", "refresh",
		"--run-id", "run-1",
		"--git-revision", "test",
		"--mkosi-version", "26",
	}
	if err := run(args, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("prepare refresh error = %v", err)
	}
	writeInstallerPackages(t, packagePath, "systemd-0:259.7-1.fc44.x86_64")
	err := run([]string{
		"prepare-mkosi",
		"--manifest", manifestPath,
		"--lock", lockPath,
		"--mkosi-dir", mkosiDir,
		"--runtime-root", runtimeRoot,
		"--mode", "strict",
		"--run-id", "run-1",
		"--git-revision", "test",
		"--mkosi-version", "26",
	}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "NEVRA drift") {
		t.Fatalf("prepare strict error = %v, want NEVRA drift", err)
	}
}

func TestRunPrepareMkosiStrictRejectsKatlOSDrift(t *testing.T) {
	oldQuery := queryRPMPackages
	oldReadKatlOSIndex := readKatlOSIndex
	t.Cleanup(func() {
		queryRPMPackages = oldQuery
		readKatlOSIndex = oldReadKatlOSIndex
	})
	queryRPMPackages = func(root string) ([]resourcetest.Package, error) {
		return []resourcetest.Package{{
			Name:  "systemd",
			NEVRA: "systemd-0:259.6-1.fc44.x86_64",
		}}, nil
	}

	dir := t.TempDir()
	mkosiDir := filepath.Join(dir, "build", "mkosi")
	runtimeRoot := filepath.Join(mkosiDir, "katl-runtime-root")
	if err := os.MkdirAll(runtimeRoot, 0o755); err != nil {
		t.Fatalf("mkdir runtime root: %v", err)
	}
	writeFile(t, filepath.Join(mkosiDir, "katl-runtime-root.squashfs"), "runtime")
	writeFile(t, filepath.Join(mkosiDir, "katlos-install-0.0.0-dev-x86_64.squashfs"), "katlos")
	readKatlOSIndex = func(path string) (katlosIndex, error) {
		return katlosTestIndex(strings.Repeat("d", 64)), nil
	}
	manifestPath := filepath.Join(dir, "resource-manifest.json")
	lockPath := filepath.Join(dir, "resource-package-lock.json")
	args := []string{
		"prepare-mkosi",
		"--manifest", manifestPath,
		"--lock", lockPath,
		"--mkosi-dir", mkosiDir,
		"--runtime-root", runtimeRoot,
		"--mode", "refresh",
		"--run-id", "run-1",
		"--git-revision", "test",
		"--mkosi-version", "26",
	}
	if err := run(args, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("prepare refresh error = %v", err)
	}
	readKatlOSIndex = func(path string) (katlosIndex, error) {
		return katlosTestIndex(strings.Repeat("e", 64)), nil
	}
	err := run([]string{
		"prepare-mkosi",
		"--manifest", manifestPath,
		"--lock", lockPath,
		"--mkosi-dir", mkosiDir,
		"--runtime-root", runtimeRoot,
		"--mode", "strict",
		"--run-id", "run-1",
		"--git-revision", "test",
		"--mkosi-version", "26",
	}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "checksum drift") {
		t.Fatalf("prepare strict error = %v, want checksum drift", err)
	}
}

func TestRunPrepareMkosiStrictRejectsDrift(t *testing.T) {
	oldQuery := queryRPMPackages
	t.Cleanup(func() { queryRPMPackages = oldQuery })
	packages := []resourcetest.Package{{
		Name:  "systemd",
		NEVRA: "systemd-0:259.6-1.fc44.x86_64",
	}}
	queryRPMPackages = func(root string) ([]resourcetest.Package, error) {
		return append([]resourcetest.Package(nil), packages...), nil
	}

	dir := t.TempDir()
	mkosiDir := filepath.Join(dir, "build", "mkosi")
	runtimeRoot := filepath.Join(mkosiDir, "katl-runtime-root")
	if err := os.MkdirAll(runtimeRoot, 0o755); err != nil {
		t.Fatalf("mkdir runtime root: %v", err)
	}
	writeFile(t, filepath.Join(mkosiDir, "katl-runtime-root.squashfs"), "runtime")
	manifestPath := filepath.Join(dir, "resource-manifest.json")
	lockPath := filepath.Join(dir, "resource-package-lock.json")
	refreshArgs := []string{
		"prepare-mkosi",
		"--manifest", manifestPath,
		"--lock", lockPath,
		"--mkosi-dir", mkosiDir,
		"--runtime-root", runtimeRoot,
		"--mode", "refresh",
		"--run-id", "run-1",
		"--git-revision", "test",
		"--mkosi-version", "26",
	}
	if err := run(refreshArgs, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("prepare refresh error = %v", err)
	}
	packages[0].NEVRA = "systemd-0:259.7-1.fc44.x86_64"
	err := run([]string{
		"prepare-mkosi",
		"--manifest", manifestPath,
		"--lock", lockPath,
		"--mkosi-dir", mkosiDir,
		"--runtime-root", runtimeRoot,
		"--mode", "strict",
		"--run-id", "run-1",
		"--git-revision", "test",
		"--mkosi-version", "26",
	}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "NEVRA drift") {
		t.Fatalf("prepare strict error = %v, want NEVRA drift", err)
	}
}

func commandManifest(lockDigest string) resourcetest.Manifest {
	manifest := commandManifestSkeleton()
	manifest.PackageSets = []resourcetest.PackageSet{{
		Name:         "runtime",
		Source:       "mkosi.profiles/runtime",
		Digest:       strings.Repeat("b", 64),
		LockDigest:   lockDigest,
		Distribution: "fedora",
		Release:      "44",
		Architecture: "x86_64",
		Repositories: []resourcetest.PackageRepository{{
			ID:      "fedora",
			BaseURL: "https://example.invalid/fedora/44",
		}},
		Packages: []resourcetest.Package{{
			Name:     "systemd",
			NEVRA:    "systemd-0:259.6-1.fc44.x86_64",
			Checksum: strings.Repeat("c", 64),
		}},
	},
	}
	return manifest
}

func commandManifestSkeleton() resourcetest.Manifest {
	return resourcetest.Manifest{
		APIVersion: resourcetest.APIVersion,
		Kind:       resourcetest.Kind,
		RunID:      "resource-run",
		Created:    time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC),
		Git:        resourcetest.GitState{Revision: "baf1ac7"},
		Tools: []resourcetest.Tool{{
			Name:    "mkosi",
			Version: "26",
		}},
		MkosiProfiles: []resourcetest.MkosiProfile{{
			Name:          "runtime",
			Path:          "mkosi.profiles/runtime",
			ConfigDigest:  strings.Repeat("a", 64),
			PackageSetRef: "runtime",
		}},
	}
}

func commandArtifactManifest() resourcetest.Manifest {
	return resourcetest.Manifest{
		APIVersion: resourcetest.APIVersion,
		Kind:       resourcetest.Kind,
		RunID:      "resource-run",
		Created:    time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC),
		Git:        resourcetest.GitState{Revision: "baf1ac7"},
	}
}

func writeTestManifest(t *testing.T, path string, manifest resourcetest.Manifest) {
	t.Helper()
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func writeKubernetesMetadata(t *testing.T, path, version string) {
	t.Helper()
	metadata := map[string]any{
		"architecture": "x86_64",
		"sourceRepo": map[string]string{
			"id":      "kubernetes",
			"baseURL": "https://pkgs.k8s.io/core:/stable:/v1.36/rpm/",
			"minor":   "v1.36",
		},
		"packageVersions": map[string]string{
			"kubeadm":   version,
			"kubelet":   version,
			"kubectl":   version,
			"cri-tools": "0:1.36.0-150500.1.1",
		},
	}
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		t.Fatalf("marshal Kubernetes metadata: %v", err)
	}
	writeFile(t, path, string(append(data, '\n')))
}

func writeInstallerPackages(t *testing.T, path, systemd string) {
	t.Helper()
	writeFile(t, path, "bash\tbash-0:5.3.9-3.fc44.x86_64\nsystemd\t"+systemd+"\n")
}

func packageSet(sets []resourcetest.PackageSet, name string) resourcetest.PackageSet {
	for _, set := range sets {
		if set.Name == name {
			return set
		}
	}
	return resourcetest.PackageSet{}
}

func packageChecksum(packages []resourcetest.Package, name string) string {
	for _, pkg := range packages {
		if pkg.Name == name {
			return pkg.Checksum
		}
	}
	return ""
}

func packageNEVRA(packages []resourcetest.Package, name string) string {
	for _, pkg := range packages {
		if pkg.Name == name {
			return pkg.NEVRA
		}
	}
	return ""
}

func katlosTestIndex(runtimeSHA string) katlosIndex {
	return katlosIndex{
		Version:      "0.0.0-dev",
		BuildID:      "test",
		Architecture: "x86_64",
		Components: []katlosComponent{{
			Name:         "runtime-root",
			Role:         "runtime-root",
			SHA256:       runtimeSHA,
			Version:      "test",
			Architecture: "x86_64",
		}, {
			Name:           "kubernetes",
			Role:           "kubernetes-sysext",
			SHA256:         strings.Repeat("f", 64),
			Version:        "test",
			PayloadVersion: "v1.36.1",
			Architecture:   "x86_64",
			SourceRepo: &kubernetesRepo{
				ID:      "kubernetes",
				BaseURL: "https://pkgs.k8s.io/core:/stable:/v1.36/rpm/",
				Minor:   "v1.36",
			},
			PackageVersions: map[string]string{
				"kubeadm": "0:1.36.1-150500.1.1",
			},
		}},
	}
}
