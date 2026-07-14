package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/resourcetest"
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

func TestRepositoryMkosiProfileDigestsAreLocked(t *testing.T) {
	lockPath := resolveExistingPath("mkosi.profiles/resource-package-lock.json")
	data, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("read repository package lock: %v", err)
	}
	lock, err := resourcetest.DecodePackageLock(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode repository package lock: %v", err)
	}
	for _, profile := range lock.MkosiProfiles {
		got, err := profileConfigDigest(profile.Path)
		if err != nil {
			t.Fatalf("hash mkosi profile %q: %v", profile.Name, err)
		}
		if got != profile.ConfigDigest {
			t.Errorf("mkosi profile %q config SHA-256 = %s, lock has %s; refresh the resource package lock with the profile change", profile.Name, got, profile.ConfigDigest)
		}
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
		if root != "_build/mkosi/katl-runtime-root" {
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
		"--root", "_build/mkosi/katl-runtime-root",
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
		t.Fatalf("runtime package inventory should avoid querying the pruned root: %s", root)
		return nil, nil
	}
	readKatlOSIndex = func(path string) (katlosIndex, error) {
		return katlosTestIndex(strings.Repeat("d", 64)), nil
	}

	dir := t.TempDir()
	mkosiDir := filepath.Join(dir, "_build", "mkosi")
	runtimeRoot := filepath.Join(mkosiDir, "katl-runtime-root")
	if err := os.MkdirAll(runtimeRoot, 0o755); err != nil {
		t.Fatalf("mkdir runtime root: %v", err)
	}
	writeFile(t, filepath.Join(mkosiDir, "katl-runtime-root.squashfs"), "runtime")
	writeFile(t, filepath.Join(mkosiDir, "katl-runtime.efi"), "runtime-uki")
	writeFile(t, filepath.Join(mkosiDir, "katl-kubernetes.raw"), "kubernetes")
	writeKubernetesMetadata(t, filepath.Join(mkosiDir, "katl-kubernetes.raw.json"), "0:1.36.0-150500.1.1")
	writeInstallerPackages(t, filepath.Join(mkosiDir, "katl-installer.packages.tsv"), "systemd-0:259.6-1.fc44.x86_64")
	writeInstallerPackages(t, filepath.Join(mkosiDir, "katl-runtime.packages.tsv"), "systemd-0:259.6-1.fc44.x86_64")
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
	if packageChecksum(katlosSet.Packages, "katlos-component-runtime-root") != "" {
		t.Fatalf("KatlOS package set = %#v", katlosSet)
	}
	if packageNEVRA(katlosSet.Packages, "katlos-component-runtime-root") != "runtime-root-component.x86_64" {
		t.Fatalf("KatlOS component NEVRA = %q", packageNEVRA(katlosSet.Packages, "katlos-component-runtime-root"))
	}
	if packageNEVRA(katlosSet.Packages, "katlos-component-kubernetes-sysext") != "" {
		t.Fatalf("KatlOS package set includes Kubernetes sysext component: %#v", katlosSet)
	}
	kubernetesSet := packageSet(manifest.PackageSets, "kubernetes-sysext")
	if kubernetesSet.Name != "kubernetes-sysext" || packageNEVRA(kubernetesSet.Packages, "kubeadm") != "kubeadm-0:1.36.0-150500.1.1.x86_64" {
		t.Fatalf("Kubernetes package set = %#v", kubernetesSet)
	}
	if packageNEVRA(kubernetesSet.Packages, "ethtool") != "ethtool-2:7.0-1.fc44.x86_64" ||
		packageNEVRA(kubernetesSet.Packages, "socat") != "socat-0:1.8.1.1-1.fc44.x86_64" {
		t.Fatalf("Kubernetes helper packages = %#v", kubernetesSet.Packages)
	}
	if len(kubernetesSet.Repositories) != 2 || kubernetesSet.Repositories[1].ID != "fedora" {
		t.Fatalf("Kubernetes repositories = %#v", kubernetesSet.Repositories)
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

func TestRunPrepareMkosiPartialRefreshPreservesUnselectedSets(t *testing.T) {
	oldQuery := queryRPMPackages
	t.Cleanup(func() { queryRPMPackages = oldQuery })
	queryRPMPackages = func(root string) ([]resourcetest.Package, error) {
		t.Fatalf("runtime package inventory should use the generated package list: %s", root)
		return nil, nil
	}

	dir := t.TempDir()
	mkosiDir := filepath.Join(dir, "_build", "mkosi")
	runtimeRoot := filepath.Join(mkosiDir, "katl-runtime-root")
	if err := os.MkdirAll(runtimeRoot, 0o755); err != nil {
		t.Fatalf("mkdir runtime root: %v", err)
	}
	writeFile(t, filepath.Join(mkosiDir, "katl-runtime-root.squashfs"), "runtime")
	writeKubernetesMetadata(t, filepath.Join(mkosiDir, "katl-kubernetes.raw.json"), "0:1.36.0-150500.1.1")
	installerPackages := filepath.Join(mkosiDir, "katl-installer.packages.tsv")
	writeInstallerPackages(t, installerPackages, "systemd-0:259.6-1.fc44.x86_64")
	writeInstallerPackages(t, filepath.Join(mkosiDir, "katl-runtime.packages.tsv"), "systemd-0:259.6-1.fc44.x86_64")

	manifestPath := filepath.Join(dir, "resource-manifest.json")
	lockPath := filepath.Join(dir, "resource-package-lock.json")
	baseArgs := []string{
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
	if err := run(baseArgs, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("initial refresh error = %v", err)
	}

	oldReadKatlOSIndex := readKatlOSIndex
	t.Cleanup(func() { readKatlOSIndex = oldReadKatlOSIndex })
	writeFile(t, filepath.Join(mkosiDir, "katlos-install-stale-x86_64.squashfs"), "stale")
	readKatlOSIndex = func(path string) (katlosIndex, error) {
		t.Fatalf("partial Fedora refresh inspected unrelated KatlOS image: %s", path)
		return katlosIndex{}, nil
	}
	writeInstallerPackages(t, installerPackages, "systemd-0:259.7-1.fc44.x86_64")
	partialArgs := append(append([]string(nil), baseArgs...), "--package-set", "installer-image")
	if err := run(partialArgs, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("partial refresh error = %v", err)
	}
	data, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("read partial lock: %v", err)
	}
	lock, err := resourcetest.DecodePackageLock(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode partial lock: %v", err)
	}
	if got := lockedPackageNEVRA(lock, "installer-image", "systemd"); got != "systemd-0:259.7-1.fc44.x86_64" {
		t.Fatalf("installer systemd = %q", got)
	}
	if got := lockedPackageNEVRA(lock, "runtime", "systemd"); got != "systemd-0:259.6-1.fc44.x86_64" {
		t.Fatalf("runtime systemd = %q, want unselected package preserved", got)
	}
	if got := lockedPackageNEVRA(lock, "kubernetes-sysext", "kubeadm"); got != "kubeadm-0:1.36.0-150500.1.1.x86_64" {
		t.Fatalf("Kubernetes kubeadm = %q, want unselected package set preserved", got)
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
	mkosiDir := filepath.Join(dir, "_build", "mkosi")
	runtimeRoot := filepath.Join(mkosiDir, "katl-runtime-root")
	if err := os.MkdirAll(runtimeRoot, 0o755); err != nil {
		t.Fatalf("mkdir runtime root: %v", err)
	}
	writeFile(t, filepath.Join(mkosiDir, "katl-runtime-root.squashfs"), "runtime")
	writeKubernetesMetadata(t, filepath.Join(mkosiDir, "katl-kubernetes.raw.json"), "0:1.36.0-150500.1.1")
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
	writeKubernetesMetadata(t, filepath.Join(mkosiDir, "katl-kubernetes.raw.json"), "0:1.36.1-150500.1.1")
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

func TestRunPrepareMkosiRefreshIgnoresEmptyInstallerPackageFile(t *testing.T) {
	oldQuery := queryRPMPackages
	t.Cleanup(func() { queryRPMPackages = oldQuery })
	queryRPMPackages = func(root string) ([]resourcetest.Package, error) {
		return []resourcetest.Package{{
			Name:  "systemd",
			NEVRA: "systemd-0:259.6-1.fc44.x86_64",
		}}, nil
	}

	dir := t.TempDir()
	mkosiDir := filepath.Join(dir, "_build", "mkosi")
	runtimeRoot := filepath.Join(mkosiDir, "katl-runtime-root")
	if err := os.MkdirAll(runtimeRoot, 0o755); err != nil {
		t.Fatalf("mkdir runtime root: %v", err)
	}
	writeFile(t, filepath.Join(mkosiDir, "katl-runtime-root.squashfs"), "runtime")
	writeKubernetesMetadata(t, filepath.Join(mkosiDir, "katl-kubernetes.raw.json"), "0:1.36.1-150500.1.1")
	writeFile(t, filepath.Join(mkosiDir, "katl-installer.packages.tsv"), "")

	manifestPath := filepath.Join(dir, "resource-manifest.json")
	lockPath := filepath.Join(dir, "resource-package-lock.json")
	err := run([]string{
		"prepare-mkosi",
		"--manifest", manifestPath,
		"--lock", lockPath,
		"--mkosi-dir", mkosiDir,
		"--runtime-root", runtimeRoot,
		"--mode", "refresh",
		"--run-id", "run-1",
		"--git-revision", "test",
		"--mkosi-version", "26",
	}, &bytes.Buffer{}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("prepare refresh error = %v", err)
	}
	manifest, err := readManifest(manifestPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if packageSet(manifest.PackageSets, "installer-image").Name != "" {
		t.Fatalf("package sets = %#v, want no installer-image set", manifest.PackageSets)
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
	mkosiDir := filepath.Join(dir, "_build", "mkosi")
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

func TestRunPrepareMkosiStrictRejectsMissingInstallerLock(t *testing.T) {
	oldQuery := queryRPMPackages
	t.Cleanup(func() { queryRPMPackages = oldQuery })
	queryRPMPackages = func(root string) ([]resourcetest.Package, error) {
		return []resourcetest.Package{{
			Name:  "systemd",
			NEVRA: "systemd-0:259.6-1.fc44.x86_64",
		}}, nil
	}

	dir := t.TempDir()
	mkosiDir := filepath.Join(dir, "_build", "mkosi")
	runtimeRoot := filepath.Join(mkosiDir, "katl-runtime-root")
	if err := os.MkdirAll(runtimeRoot, 0o755); err != nil {
		t.Fatalf("mkdir runtime root: %v", err)
	}
	writeFile(t, filepath.Join(mkosiDir, "katl-runtime-root.squashfs"), "runtime")
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
	writeInstallerPackages(t, filepath.Join(mkosiDir, "katl-installer.packages.tsv"), "systemd-0:259.6-1.fc44.x86_64")
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
	if err == nil || !strings.Contains(err.Error(), `package set "installer-image" is missing from package lock`) {
		t.Fatalf("prepare strict error = %v, want missing installer-image package set", err)
	}
}

func TestRunPrepareMkosiStrictIgnoresKatlOSComponentChecksumDrift(t *testing.T) {
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
	mkosiDir := filepath.Join(dir, "_build", "mkosi")
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
	if err := run([]string{
		"prepare-mkosi",
		"--manifest", manifestPath,
		"--lock", lockPath,
		"--mkosi-dir", mkosiDir,
		"--runtime-root", runtimeRoot,
		"--mode", "strict",
		"--run-id", "run-1",
		"--git-revision", "test",
		"--mkosi-version", "26",
	}, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("prepare strict error = %v, want component checksum drift ignored", err)
	}
}

func TestRunPrepareMkosiStrictRejectsKatlOSPackageVersionDrift(t *testing.T) {
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
	mkosiDir := filepath.Join(dir, "_build", "mkosi")
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
		index := katlosTestIndex(strings.Repeat("d", 64))
		index.Components[0].Architecture = "aarch64"
		return index, nil
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
	if err == nil || !strings.Contains(err.Error(), "NEVRA drift") {
		t.Fatalf("prepare strict error = %v, want NEVRA drift", err)
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
	mkosiDir := filepath.Join(dir, "_build", "mkosi")
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
			"ethtool":   "2:7.0-1.fc44",
			"socat":     "0:1.8.1.1-1.fc44",
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

func lockedPackageNEVRA(lock resourcetest.PackageLock, setName, packageName string) string {
	for _, set := range lock.PackageSets {
		if set.Name != setName {
			continue
		}
		for _, pkg := range set.Packages {
			if pkg.Name == packageName {
				return pkg.NEVRA
			}
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
		}},
	}
}
