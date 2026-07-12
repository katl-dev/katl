package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/resourcetest"
)

const defaultLockPath = "mkosi.profiles/resource-package-lock.json"

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "katl-resource-lock: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("command is required: add-artifact, add-rpm-package-set, prepare-mkosi, refresh, or verify")
	}
	switch args[0] {
	case "add-artifact":
		return runAddArtifact(args[1:], stdout, stderr)
	case "add-rpm-package-set":
		return runAddRPMPackageSet(args[1:], stdout, stderr)
	case "prepare-mkosi":
		return runPrepareMkosi(args[1:], stdout, stderr)
	case "refresh":
		return runRefresh(args[1:], stdout, stderr)
	case "verify":
		return runVerify(args[1:], stdout, stderr)
	default:
		return fmt.Errorf("unsupported command %q", args[0])
	}
}

func runAddArtifact(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("katl-resource-lock add-artifact", flag.ContinueOnError)
	flags.SetOutput(stderr)
	manifestPath := flags.String("manifest", "", "resource-test manifest to update")
	outputPath := flags.String("output", "", "updated manifest output path, default is --manifest")
	name := flags.String("name", "", "artifact name")
	kind := flags.String("kind", "", "artifact kind")
	path := flags.String("path", "", "artifact path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if strings.TrimSpace(*manifestPath) == "" {
		return fmt.Errorf("--manifest is required")
	}
	if strings.TrimSpace(*name) == "" {
		return fmt.Errorf("--name is required")
	}
	if strings.TrimSpace(*kind) == "" {
		return fmt.Errorf("--kind is required")
	}
	if strings.TrimSpace(*path) == "" {
		return fmt.Errorf("--path is required")
	}
	if *outputPath == "" {
		*outputPath = *manifestPath
	}

	manifest, err := readManifestForUpdate(*manifestPath)
	if err != nil {
		return err
	}
	info, err := os.Stat(*path)
	if err != nil {
		return fmt.Errorf("stat artifact %s: %w", *path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("artifact path is not a regular file: %s", *path)
	}
	digest, err := fileSHA256(*path)
	if err != nil {
		return err
	}
	manifest.Artifacts = upsertArtifact(manifest.Artifacts, resourcetest.Artifact{
		Name:      *name,
		Kind:      *kind,
		Path:      *path,
		Digest:    digest,
		SizeBytes: info.Size(),
		Created:   info.ModTime().UTC(),
	})
	if err := resourcetest.ValidateManifest(manifest); err != nil {
		return err
	}
	if err := writeManifest(*outputPath, manifest); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "manifest: %s\n", *outputPath)
	fmt.Fprintf(stdout, "artifact: %s\n", *name)
	fmt.Fprintf(stdout, "sha256: %s\n", digest)
	fmt.Fprintf(stdout, "sizeBytes: %d\n", info.Size())
	return nil
}

func runAddRPMPackageSet(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("katl-resource-lock add-rpm-package-set", flag.ContinueOnError)
	flags.SetOutput(stderr)
	manifestPath := flags.String("manifest", "", "resource-test manifest to update")
	outputPath := flags.String("output", "", "updated manifest output path, default is --manifest")
	name := flags.String("name", "", "package set name")
	source := flags.String("source", "", "package set source, usually the mkosi profile path")
	root := flags.String("root", "", "mkosi root directory containing an RPM database")
	lockPath := flags.String("lock", "", "optional package lock whose digest should be recorded")
	distribution := flags.String("distribution", "", "package distribution name")
	release := flags.String("release", "", "package distribution release")
	architecture := flags.String("architecture", "", "package architecture")
	profileName := flags.String("profile-name", "", "optional mkosi profile name to add or update")
	profilePath := flags.String("profile-path", "", "optional mkosi profile path to add or update")
	profileConfigDigest := flags.String("profile-config-sha256", "", "optional mkosi profile config SHA-256")
	var repositories repositoryFlags
	flags.Var(&repositories, "repository", "package repository in id=baseURL form; may be repeated")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if strings.TrimSpace(*manifestPath) == "" {
		return fmt.Errorf("--manifest is required")
	}
	if strings.TrimSpace(*name) == "" {
		return fmt.Errorf("--name is required")
	}
	if strings.TrimSpace(*root) == "" {
		return fmt.Errorf("--root is required")
	}
	if *outputPath == "" {
		*outputPath = *manifestPath
	}

	manifest, err := readManifestForUpdate(*manifestPath)
	if err != nil {
		return err
	}
	packages, err := queryRPMPackages(*root)
	if err != nil {
		return err
	}
	lockDigest := ""
	if *lockPath != "" {
		lockData, err := os.ReadFile(*lockPath)
		if err != nil {
			return fmt.Errorf("read package lock %s: %w", *lockPath, err)
		}
		lockDigest = resourcetest.PackageLockDigest(lockData)
	}
	manifest.PackageSets = upsertPackageSet(manifest.PackageSets, resourcetest.PackageSet{
		Name:         *name,
		Source:       *source,
		LockDigest:   lockDigest,
		Distribution: *distribution,
		Release:      *release,
		Architecture: *architecture,
		Repositories: repositories.Repositories(),
		Packages:     packages,
	})
	if *profileName != "" || *profilePath != "" {
		if *profileName == "" || *profilePath == "" {
			return fmt.Errorf("--profile-name and --profile-path must be set together")
		}
		manifest.MkosiProfiles = upsertMkosiProfile(manifest.MkosiProfiles, resourcetest.MkosiProfile{
			Name:          *profileName,
			Path:          *profilePath,
			ConfigDigest:  *profileConfigDigest,
			PackageSetRef: *name,
		})
	}
	if err := resourcetest.ValidateManifest(manifest); err != nil {
		return err
	}
	if err := writeManifest(*outputPath, manifest); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "manifest: %s\n", *outputPath)
	fmt.Fprintf(stdout, "packageSet: %s\n", *name)
	fmt.Fprintf(stdout, "packages: %d\n", len(packages))
	if lockDigest != "" {
		fmt.Fprintf(stdout, "lockSHA256: %s\n", lockDigest)
	}
	return nil
}

func runPrepareMkosi(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("katl-resource-lock prepare-mkosi", flag.ContinueOnError)
	flags.SetOutput(stderr)
	manifestPath := flags.String("manifest", "", "resource-test manifest output path")
	lockPath := flags.String("lock", defaultLockPath, "package lock path")
	mkosiDir := flags.String("mkosi-dir", "_build/mkosi", "mkosi output directory")
	runtimeRoot := flags.String("runtime-root", "", "runtime root directory containing an RPM database")
	mode := flags.String("mode", "strict", "lock mode: strict or refresh")
	runID := flags.String("run-id", "", "resource-test run id")
	gitRevision := flags.String("git-revision", "", "git revision to record")
	fedoraRepo := flags.String("fedora-repository", "fedora=", "Fedora repository in id=baseURL form")
	mkosiVersionFlag := flags.String("mkosi-version", "auto", "mkosi version to record; auto detects mkosi and empty disables recording")
	var packageSets []string
	flags.Func("package-set", "package set required by strict verification; repeat to select a subset", func(value string) error {
		value = strings.TrimSpace(value)
		if value == "" {
			return errors.New("package set must not be empty")
		}
		packageSets = append(packageSets, value)
		return nil
	})
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if strings.TrimSpace(*manifestPath) == "" {
		return fmt.Errorf("--manifest is required")
	}
	if *runtimeRoot == "" {
		*runtimeRoot = filepath.Join(*mkosiDir, "katl-runtime-root")
	}
	if *runID == "" {
		*runID = "resource-" + time.Now().UTC().Format("20060102T150405Z")
	}
	if *gitRevision == "" {
		*gitRevision = "unknown"
	}

	manifest := resourcetest.NewManifest(*runID, resourcetest.GitState{Revision: *gitRevision})
	mkosiVersion, err := resolveMkosiVersion(*mkosiVersionFlag)
	if err != nil {
		return err
	}
	if mkosiVersion != "" {
		manifest.Tools = append(manifest.Tools, resourcetest.Tool{Name: "mkosi", Version: mkosiVersion})
	}
	if err := addMkosiArtifacts(&manifest, *mkosiDir); err != nil {
		return err
	}
	repo, err := parseRepository(*fedoraRepo)
	if err != nil {
		return err
	}
	if err := addInstallerPackageSet(&manifest, filepath.Join(*mkosiDir, "katl-installer.packages.tsv"), repo, ""); err != nil {
		return err
	}
	if err := addRuntimePackageSet(&manifest, *runtimeRoot, repo, ""); err != nil {
		return err
	}
	if err := addKubernetesPackageSet(&manifest, filepath.Join(*mkosiDir, "katl-kubernetes.raw.json"), repo, ""); err != nil {
		return err
	}
	katlosImage, err := findKatlOSImage(*mkosiDir)
	if err != nil {
		return err
	}
	if katlosImage != "" {
		if err := addKatlOSPackageSet(&manifest, katlosImage, ""); err != nil {
			return err
		}
	}
	lockDigest := ""
	switch *mode {
	case "refresh":
		if len(packageSets) != 0 {
			return errors.New("--package-set requires --mode strict")
		}
		lock, err := resourcetest.PackageLockFromManifest(manifest)
		if err != nil {
			return err
		}
		data, err := json.MarshalIndent(lock, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal package lock: %w", err)
		}
		data = append(data, '\n')
		if err := os.MkdirAll(filepath.Dir(*lockPath), 0o755); err != nil {
			return fmt.Errorf("create package-lock directory: %w", err)
		}
		if err := os.WriteFile(*lockPath, data, 0o644); err != nil {
			return fmt.Errorf("write package lock %s: %w", *lockPath, err)
		}
		lockDigest = resourcetest.PackageLockDigest(data)
		manifest.PackageSets = setLockDigest(manifest.PackageSets, lockDigest)
	case "strict":
		lockData, err := os.ReadFile(*lockPath)
		if err != nil {
			return fmt.Errorf("read package lock %s: %w", *lockPath, err)
		}
		lockDigest = resourcetest.PackageLockDigest(lockData)
		manifest.PackageSets = setLockDigest(manifest.PackageSets, lockDigest)
		lock, err := resourcetest.DecodePackageLock(strings.NewReader(string(lockData)))
		if err != nil {
			return fmt.Errorf("decode package lock %s: %w", *lockPath, err)
		}
		if err := resourcetest.VerifyPackageLock(resourcetest.PackageLockVerification{
			Lock: lock, Manifest: manifest, LockDigest: lockDigest, RequiredPackageSets: packageSets,
		}); err != nil {
			return err
		}
	default:
		return fmt.Errorf("--mode must be strict or refresh, got %q", *mode)
	}
	if err := writeManifest(*manifestPath, manifest); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "manifest: %s\n", *manifestPath)
	fmt.Fprintf(stdout, "mode: %s\n", *mode)
	fmt.Fprintf(stdout, "artifacts: %d\n", len(manifest.Artifacts))
	fmt.Fprintf(stdout, "packageSets: %d\n", len(manifest.PackageSets))
	if lockDigest != "" {
		fmt.Fprintf(stdout, "lockSHA256: %s\n", lockDigest)
	}
	return nil
}

func runRefresh(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("katl-resource-lock refresh", flag.ContinueOnError)
	flags.SetOutput(stderr)
	manifestPath := flags.String("manifest", "", "resource-test manifest to convert into a package lock")
	outputPath := flags.String("output", defaultLockPath, "package-lock output path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if strings.TrimSpace(*manifestPath) == "" {
		return fmt.Errorf("--manifest is required")
	}
	if strings.TrimSpace(*outputPath) == "" {
		return fmt.Errorf("--output is required")
	}

	manifest, err := readManifest(*manifestPath)
	if err != nil {
		return err
	}
	lock, err := resourcetest.PackageLockFromManifest(manifest)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(lock, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal package lock: %w", err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(*outputPath), 0o755); err != nil {
		return fmt.Errorf("create package-lock directory: %w", err)
	}
	if err := os.WriteFile(*outputPath, data, 0o644); err != nil {
		return fmt.Errorf("write package lock %s: %w", *outputPath, err)
	}
	fmt.Fprintf(stdout, "lock: %s\n", *outputPath)
	fmt.Fprintf(stdout, "sha256: %s\n", resourcetest.PackageLockDigest(data))
	return nil
}

func runVerify(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("katl-resource-lock verify", flag.ContinueOnError)
	flags.SetOutput(stderr)
	manifestPath := flags.String("manifest", "", "resource-test manifest to verify")
	lockPath := flags.String("lock", defaultLockPath, "package lock path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if strings.TrimSpace(*manifestPath) == "" {
		return fmt.Errorf("--manifest is required")
	}
	if strings.TrimSpace(*lockPath) == "" {
		return fmt.Errorf("--lock is required")
	}

	manifest, err := readManifest(*manifestPath)
	if err != nil {
		return err
	}
	lockData, err := os.ReadFile(*lockPath)
	if err != nil {
		return fmt.Errorf("read package lock %s: %w", *lockPath, err)
	}
	lock, err := resourcetest.DecodePackageLock(strings.NewReader(string(lockData)))
	if err != nil {
		return fmt.Errorf("decode package lock %s: %w", *lockPath, err)
	}
	digest := resourcetest.PackageLockDigest(lockData)
	if err := resourcetest.VerifyPackageLock(resourcetest.PackageLockVerification{
		Lock:       lock,
		Manifest:   manifest,
		LockDigest: digest,
	}); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "verified: %s\n", *lockPath)
	fmt.Fprintf(stdout, "sha256: %s\n", digest)
	return nil
}

func addMkosiArtifacts(manifest *resourcetest.Manifest, mkosiDir string) error {
	candidates := []struct {
		Name string
		Kind string
		Path string
	}{
		{Name: "installer-uki", Kind: "uki", Path: filepath.Join(mkosiDir, "katl-installer.efi")},
		{Name: "installer-kernel", Kind: "linux-kernel", Path: filepath.Join(mkosiDir, "katl-installer.vmlinuz")},
		{Name: "installer-initrd", Kind: "initrd", Path: filepath.Join(mkosiDir, "katl-installer.initrd")},
		{Name: "runtime-uki", Kind: "uki", Path: filepath.Join(mkosiDir, "katl-runtime.efi")},
		{Name: "runtime-root", Kind: "squashfs", Path: filepath.Join(mkosiDir, "katl-runtime-root.squashfs")},
		{Name: "kubernetes-sysext", Kind: "sysext", Path: filepath.Join(mkosiDir, "katl-kubernetes.raw")},
	}
	if katlos, err := findKatlOSImage(mkosiDir); err == nil && katlos != "" {
		candidates = append(candidates, struct {
			Name string
			Kind string
			Path string
		}{Name: "katlos-install-image", Kind: "squashfs", Path: katlos})
	} else if err != nil {
		return err
	}
	for _, candidate := range candidates {
		artifact, ok, err := artifactFromPath(candidate.Name, candidate.Kind, candidate.Path)
		if err != nil {
			return err
		}
		if ok {
			manifest.Artifacts = upsertArtifact(manifest.Artifacts, artifact)
		}
	}
	return nil
}

func addRuntimePackageSet(manifest *resourcetest.Manifest, runtimeRoot string, repo resourcetest.PackageRepository, lockDigest string) error {
	packagePath := filepath.Join(filepath.Dir(runtimeRoot), "katl-runtime.packages.tsv")
	packages, ok, err := readRPMPackageFile(packagePath)
	if err != nil {
		return err
	}
	if !ok {
		packages, err = queryRPMPackages(runtimeRoot)
		if err != nil {
			return err
		}
	}
	profileDigest, err := profileConfigDigest("mkosi.profiles/runtime")
	if err != nil {
		return err
	}
	manifest.PackageSets = upsertPackageSet(manifest.PackageSets, resourcetest.PackageSet{
		Name:         "runtime",
		Source:       "mkosi.profiles/runtime",
		LockDigest:   lockDigest,
		Distribution: "fedora",
		Release:      "44",
		Architecture: "x86_64",
		Repositories: []resourcetest.PackageRepository{repo},
		Packages:     packages,
	})
	manifest.MkosiProfiles = upsertMkosiProfile(manifest.MkosiProfiles, resourcetest.MkosiProfile{
		Name:          "runtime",
		Path:          "mkosi.profiles/runtime",
		ConfigDigest:  profileDigest,
		PackageSetRef: "runtime",
	})
	return nil
}

func addInstallerPackageSet(manifest *resourcetest.Manifest, packagePath string, repo resourcetest.PackageRepository, lockDigest string) error {
	packages, ok, err := readRPMPackageFile(packagePath)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	profileDigest, err := profileConfigDigest("mkosi.profiles/installer-image")
	if err != nil {
		return err
	}
	manifest.PackageSets = upsertPackageSet(manifest.PackageSets, resourcetest.PackageSet{
		Name:         "installer-image",
		Source:       "mkosi.profiles/installer-image",
		LockDigest:   lockDigest,
		Distribution: "fedora",
		Release:      "44",
		Architecture: "x86_64",
		Repositories: []resourcetest.PackageRepository{repo},
		Packages:     packages,
	})
	manifest.MkosiProfiles = upsertMkosiProfile(manifest.MkosiProfiles, resourcetest.MkosiProfile{
		Name:          "installer-image",
		Path:          "mkosi.profiles/installer-image",
		ConfigDigest:  profileDigest,
		PackageSetRef: "installer-image",
	})
	return nil
}

type kubernetesSysextMetadata struct {
	Architecture    string            `json:"architecture"`
	SourceRepo      kubernetesRepo    `json:"sourceRepo"`
	PackageVersions map[string]string `json:"packageVersions"`
}

type kubernetesRepo struct {
	ID      string `json:"id"`
	BaseURL string `json:"baseURL"`
	Minor   string `json:"minor"`
}

type katlosIndex struct {
	Version      string            `json:"version"`
	BuildID      string            `json:"buildID"`
	Architecture string            `json:"architecture"`
	Components   []katlosComponent `json:"components"`
}

type katlosComponent struct {
	Name            string            `json:"name"`
	Role            string            `json:"role"`
	SHA256          string            `json:"sha256"`
	Version         string            `json:"version"`
	PayloadVersion  string            `json:"payloadVersion"`
	Architecture    string            `json:"architecture"`
	SourceRepo      *kubernetesRepo   `json:"sourceRepo"`
	PackageVersions map[string]string `json:"packageVersions"`
}

func addKatlOSPackageSet(manifest *resourcetest.Manifest, imagePath string, lockDigest string) error {
	index, err := readKatlOSIndex(imagePath)
	if err != nil {
		return err
	}
	packages, err := katlosPackages(index)
	if err != nil {
		return fmt.Errorf("read KatlOS package identities from %s: %w", imagePath, err)
	}
	repositories := []resourcetest.PackageRepository{{ID: "katlos-components"}}
	if repo := katlosKubernetesRepo(index); repo != nil {
		repositories = append(repositories, resourcetest.PackageRepository{ID: repo.ID, BaseURL: repo.BaseURL})
	}
	manifest.PackageSets = upsertPackageSet(manifest.PackageSets, resourcetest.PackageSet{
		Name:         "katlos-install-image",
		Source:       "scripts/build-katlos-install-image",
		LockDigest:   lockDigest,
		Distribution: "katl",
		Release:      firstNonEmpty(index.Version, index.BuildID),
		Architecture: index.Architecture,
		Repositories: repositories,
		Packages:     packages,
	})
	return nil
}

func katlosPackages(index katlosIndex) ([]resourcetest.Package, error) {
	if len(index.Components) == 0 {
		return nil, fmt.Errorf("components is required")
	}
	components := append([]katlosComponent(nil), index.Components...)
	slices.SortFunc(components, func(a, b katlosComponent) int {
		return strings.Compare(a.Role+"\x00"+a.Name, b.Role+"\x00"+b.Name)
	})
	var packages []resourcetest.Package
	for _, component := range components {
		role := firstNonEmpty(component.Role, component.Name)
		if strings.TrimSpace(role) == "" || strings.TrimSpace(component.SHA256) == "" {
			return nil, fmt.Errorf("component role/name and sha256 are required")
		}
		architecture := firstNonEmpty(component.Architecture, index.Architecture)
		nevra := role + "-component"
		if architecture != "" {
			nevra += "." + architecture
		}
		packages = append(packages, resourcetest.Package{
			Name:  "katlos-component-" + role,
			NEVRA: nevra,
		})
		if len(component.PackageVersions) > 0 {
			names := make([]string, 0, len(component.PackageVersions))
			for name := range component.PackageVersions {
				names = append(names, name)
			}
			slices.Sort(names)
			for _, name := range names {
				version := strings.TrimSpace(component.PackageVersions[name])
				if strings.TrimSpace(name) == "" || version == "" {
					return nil, fmt.Errorf("component %q contains an empty package name or version", role)
				}
				pkgNEVRA := name + "-" + version
				if architecture != "" {
					pkgNEVRA += "." + architecture
				}
				packages = append(packages, resourcetest.Package{
					Name:  role + "-" + name,
					NEVRA: pkgNEVRA,
				})
			}
		}
	}
	return packages, nil
}

func katlosKubernetesRepo(index katlosIndex) *kubernetesRepo {
	for _, component := range index.Components {
		if component.SourceRepo != nil {
			return component.SourceRepo
		}
	}
	return nil
}

var readKatlOSIndex = readKatlOSIndexFromSquashFS

func readKatlOSIndexFromSquashFS(imagePath string) (katlosIndex, error) {
	output, err := exec.Command("unsquashfs", "-cat", imagePath, "katlos/image.json").Output()
	if err != nil {
		return katlosIndex{}, fmt.Errorf("read KatlOS embedded index from %s: %w", imagePath, err)
	}
	var index katlosIndex
	if err := json.Unmarshal(output, &index); err != nil {
		return katlosIndex{}, fmt.Errorf("decode KatlOS embedded index from %s: %w", imagePath, err)
	}
	return index, nil
}

func addKubernetesPackageSet(manifest *resourcetest.Manifest, metadataPath string, fedoraRepo resourcetest.PackageRepository, lockDigest string) error {
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read Kubernetes sysext metadata %s: %w", metadataPath, err)
	}
	var metadata kubernetesSysextMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return fmt.Errorf("decode Kubernetes sysext metadata %s: %w", metadataPath, err)
	}
	packages, err := kubernetesPackages(metadata)
	if err != nil {
		return fmt.Errorf("read Kubernetes sysext packages from %s: %w", metadataPath, err)
	}
	profileDigest, err := profileConfigDigest("mkosi.profiles/kubernetes-sysext")
	if err != nil {
		return err
	}
	repositories := []resourcetest.PackageRepository{{
		ID:      metadata.SourceRepo.ID,
		BaseURL: metadata.SourceRepo.BaseURL,
	}}
	if kubernetesMetadataHasFedoraPackages(metadata) && strings.TrimSpace(fedoraRepo.ID) != "" {
		repositories = append(repositories, fedoraRepo)
	}
	manifest.PackageSets = upsertPackageSet(manifest.PackageSets, resourcetest.PackageSet{
		Name:         "kubernetes-sysext",
		Source:       "mkosi.profiles/kubernetes-sysext",
		LockDigest:   lockDigest,
		Distribution: "kubernetes",
		Release:      metadata.SourceRepo.Minor,
		Architecture: metadata.Architecture,
		Repositories: repositories,
		Packages:     packages,
	})
	manifest.MkosiProfiles = upsertMkosiProfile(manifest.MkosiProfiles, resourcetest.MkosiProfile{
		Name:          "kubernetes-sysext",
		Path:          "mkosi.profiles/kubernetes-sysext",
		ConfigDigest:  profileDigest,
		PackageSetRef: "kubernetes-sysext",
	})
	return nil
}

func kubernetesMetadataHasFedoraPackages(metadata kubernetesSysextMetadata) bool {
	for name := range metadata.PackageVersions {
		switch name {
		case "kubeadm", "kubelet", "kubectl", "cri-tools":
			continue
		default:
			return true
		}
	}
	return false
}

func kubernetesPackages(metadata kubernetesSysextMetadata) ([]resourcetest.Package, error) {
	if len(metadata.PackageVersions) == 0 {
		return nil, fmt.Errorf("packageVersions is required")
	}
	names := make([]string, 0, len(metadata.PackageVersions))
	for name := range metadata.PackageVersions {
		names = append(names, name)
	}
	slices.Sort(names)
	packages := make([]resourcetest.Package, 0, len(names))
	for _, name := range names {
		version := strings.TrimSpace(metadata.PackageVersions[name])
		if strings.TrimSpace(name) == "" || version == "" {
			return nil, fmt.Errorf("packageVersions contains an empty package name or version")
		}
		nevra := name + "-" + version
		if metadata.Architecture != "" {
			nevra += "." + metadata.Architecture
		}
		packages = append(packages, resourcetest.Package{Name: name, NEVRA: nevra})
	}
	return packages, nil
}

func resolveMkosiVersion(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if value != "auto" {
		return value, nil
	}
	path, err := exec.LookPath("mkosi")
	if err != nil {
		return "", nil
	}
	output, err := exec.Command(path, "--version").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("detect mkosi version: %w: %s", err, strings.TrimSpace(string(output)))
	}
	fields := strings.Fields(strings.TrimSpace(string(output)))
	if len(fields) >= 2 && fields[0] == "mkosi" {
		return fields[1], nil
	}
	return strings.TrimSpace(string(output)), nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func profileConfigDigest(profilePath string) (string, error) {
	resolvedPath := resolveExistingPath(profilePath)
	hash := sha256.New()
	if err := filepath.WalkDir(resolvedPath, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(resolvedPath, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			digest, err := fileSHA256(path)
			if err != nil {
				return err
			}
			fmt.Fprintf(hash, "file\x00%s\x00%s\n", rel, digest)
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			fmt.Fprintf(hash, "symlink\x00%s\x00%s\n", rel, target)
			return nil
		}
		if !entry.IsDir() {
			fmt.Fprintf(hash, "other\x00%s\x00%#o\n", rel, info.Mode())
		}
		return nil
	}); err != nil {
		return "", fmt.Errorf("hash mkosi profile %s: %w", profilePath, err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func resolveExistingPath(path string) string {
	if _, err := os.Stat(path); err == nil {
		return path
	}
	cwd, err := os.Getwd()
	if err != nil {
		return path
	}
	for {
		candidate := filepath.Join(cwd, path)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(cwd)
		if parent == cwd {
			return path
		}
		cwd = parent
	}
}

func artifactFromPath(name, kind, path string) (resourcetest.Artifact, bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return resourcetest.Artifact{}, false, nil
		}
		return resourcetest.Artifact{}, false, fmt.Errorf("stat artifact %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return resourcetest.Artifact{}, false, fmt.Errorf("artifact path is not a regular file: %s", path)
	}
	digest, err := fileSHA256(path)
	if err != nil {
		return resourcetest.Artifact{}, false, err
	}
	return resourcetest.Artifact{
		Name:      name,
		Kind:      kind,
		Path:      path,
		Digest:    digest,
		SizeBytes: info.Size(),
		Created:   info.ModTime().UTC(),
	}, true, nil
}

func findKatlOSImage(mkosiDir string) (string, error) {
	matches, err := filepath.Glob(filepath.Join(mkosiDir, "katlos-install-*.squashfs"))
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "", nil
	}
	slices.Sort(matches)
	return matches[len(matches)-1], nil
}

func setLockDigest(sets []resourcetest.PackageSet, digest string) []resourcetest.PackageSet {
	for i := range sets {
		sets[i].LockDigest = digest
	}
	return sets
}

func parseRepository(value string) (resourcetest.PackageRepository, error) {
	var flags repositoryFlags
	if err := flags.Set(value); err != nil {
		return resourcetest.PackageRepository{}, err
	}
	return flags[0], nil
}

type repositoryFlags []resourcetest.PackageRepository

func (f *repositoryFlags) String() string {
	var values []string
	for _, repo := range *f {
		values = append(values, repo.ID+"="+repo.BaseURL)
	}
	return strings.Join(values, ",")
}

func (f *repositoryFlags) Set(value string) error {
	id, baseURL, ok := strings.Cut(value, "=")
	if !ok || strings.TrimSpace(id) == "" {
		return fmt.Errorf("repository must use id=baseURL form")
	}
	*f = append(*f, resourcetest.PackageRepository{ID: strings.TrimSpace(id), BaseURL: strings.TrimSpace(baseURL)})
	return nil
}

func (f repositoryFlags) Repositories() []resourcetest.PackageRepository {
	return append([]resourcetest.PackageRepository(nil), f...)
}

func upsertPackageSet(sets []resourcetest.PackageSet, set resourcetest.PackageSet) []resourcetest.PackageSet {
	for i := range sets {
		if sets[i].Name == set.Name {
			sets[i] = set
			return sets
		}
	}
	return append(sets, set)
}

func upsertMkosiProfile(profiles []resourcetest.MkosiProfile, profile resourcetest.MkosiProfile) []resourcetest.MkosiProfile {
	for i := range profiles {
		if profiles[i].Name == profile.Name {
			profiles[i] = profile
			return profiles
		}
	}
	return append(profiles, profile)
}

func upsertArtifact(artifacts []resourcetest.Artifact, artifact resourcetest.Artifact) []resourcetest.Artifact {
	for i := range artifacts {
		if artifacts[i].Name == artifact.Name {
			artifacts[i] = artifact
			return artifacts
		}
	}
	return append(artifacts, artifact)
}

func writeManifest(path string, manifest resourcetest.Manifest) error {
	if err := resourcetest.ValidateManifest(manifest); err != nil {
		return err
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal resource manifest: %w", err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create manifest directory: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write resource manifest %s: %w", path, err)
	}
	return nil
}

var queryRPMPackages = queryRootRPMPackages

func readRPMPackageFile(path string) ([]resourcetest.Package, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read rpm package set %s: %w", path, err)
	}
	defer file.Close()
	packages, err := resourcetest.ParseRPMPackages(file)
	if err != nil {
		return nil, false, fmt.Errorf("parse rpm package set %s: %w", path, err)
	}
	if len(packages) == 0 {
		return nil, false, nil
	}
	return packages, true, nil
}

func queryRootRPMPackages(root string) ([]resourcetest.Package, error) {
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve rpm root %s: %w", root, err)
	}
	output, err := exec.Command("rpm", "--root", absoluteRoot, "--dbpath", "/usr/lib/sysimage/rpm", "-qa", "--queryformat", "%{NAME}\t%{EPOCHNUM}:%{VERSION}-%{RELEASE}.%{ARCH}\n").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("query rpm packages under %s: %w: %s", root, err, strings.TrimSpace(string(output)))
	}
	packages, err := resourcetest.ParseRPMPackages(bytes.NewReader(output))
	if err != nil {
		return nil, fmt.Errorf("parse rpm packages under %s: %w", root, err)
	}
	return packages, nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("read artifact %s: %w", path, err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", fmt.Errorf("hash artifact %s: %w", path, err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func readManifest(path string) (resourcetest.Manifest, error) {
	file, err := os.Open(path)
	if err != nil {
		return resourcetest.Manifest{}, fmt.Errorf("read resource manifest %s: %w", path, err)
	}
	defer file.Close()
	manifest, err := resourcetest.DecodeManifest(file)
	if err != nil {
		return resourcetest.Manifest{}, fmt.Errorf("decode resource manifest %s: %w", path, err)
	}
	return manifest, nil
}

func readManifestForUpdate(path string) (resourcetest.Manifest, error) {
	file, err := os.Open(path)
	if err != nil {
		return resourcetest.Manifest{}, fmt.Errorf("read resource manifest %s: %w", path, err)
	}
	defer file.Close()
	var manifest resourcetest.Manifest
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return resourcetest.Manifest{}, fmt.Errorf("decode resource manifest %s: %w", path, err)
	}
	return manifest, nil
}
