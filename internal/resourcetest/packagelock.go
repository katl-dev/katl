package resourcetest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const PackageLockKind = "ResourcePackageLock"

type PackageLock struct {
	APIVersion    string                  `json:"apiVersion"`
	Kind          string                  `json:"kind"`
	Tools         []Tool                  `json:"tools,omitempty"`
	MkosiProfiles []PackageLockProfile    `json:"mkosiProfiles"`
	PackageSets   []PackageLockPackageSet `json:"packageSets"`
}

type PackageLockProfile struct {
	Name          string `json:"name"`
	Path          string `json:"path"`
	ConfigDigest  string `json:"configSHA256,omitempty"`
	PackageSetRef string `json:"packageSetRef"`
	MkosiVersion  string `json:"mkosiVersion,omitempty"`
}

type PackageLockPackageSet struct {
	Name         string              `json:"name"`
	Source       string              `json:"source,omitempty"`
	Distribution string              `json:"distribution,omitempty"`
	Release      string              `json:"release,omitempty"`
	Architecture string              `json:"architecture,omitempty"`
	Repositories []PackageRepository `json:"repositories,omitempty"`
	Packages     []Package           `json:"packages"`
}

type PackageRepository struct {
	ID      string `json:"id"`
	BaseURL string `json:"baseURL,omitempty"`
	GPGKey  string `json:"gpgKey,omitempty"`
}

type PackageLockVerification struct {
	Lock       PackageLock
	Manifest   Manifest
	LockDigest string
}

func DecodePackageLock(r io.Reader) (PackageLock, error) {
	var lock PackageLock
	decoder := json.NewDecoder(r)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&lock); err != nil {
		return PackageLock{}, err
	}
	if err := ValidatePackageLock(lock); err != nil {
		return PackageLock{}, err
	}
	return lock, nil
}

func PackageLockDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func VerifyPackageLock(verification PackageLockVerification) error {
	if err := ValidatePackageLock(verification.Lock); err != nil {
		return err
	}
	if err := ValidateManifest(verification.Manifest); err != nil {
		return err
	}
	if !validSHA256(verification.LockDigest) {
		return errors.New("lock digest must be lowercase SHA-256")
	}
	if err := compareTools(verification.Manifest.Tools, verification.Lock.Tools); err != nil {
		return err
	}

	manifestProfiles := map[string]MkosiProfile{}
	for _, profile := range verification.Manifest.MkosiProfiles {
		manifestProfiles[profile.Name] = profile
	}
	manifestSets := map[string]PackageSet{}
	for _, set := range verification.Manifest.PackageSets {
		manifestSets[set.Name] = set
	}
	lockSets := map[string]PackageLockPackageSet{}
	for _, set := range verification.Lock.PackageSets {
		lockSets[set.Name] = set
	}

	for _, lockedProfile := range verification.Lock.MkosiProfiles {
		manifestProfile, ok := manifestProfiles[lockedProfile.Name]
		if !ok {
			return fmt.Errorf("mkosi profile %q is missing from resource manifest", lockedProfile.Name)
		}
		if manifestProfile.Path != lockedProfile.Path {
			return fmt.Errorf("mkosi profile %q path drift: got %q, want %q", lockedProfile.Name, manifestProfile.Path, lockedProfile.Path)
		}
		if lockedProfile.ConfigDigest != "" && manifestProfile.ConfigDigest != lockedProfile.ConfigDigest {
			return fmt.Errorf("mkosi profile %q config digest drift", lockedProfile.Name)
		}
		if manifestProfile.PackageSetRef != lockedProfile.PackageSetRef {
			return fmt.Errorf("mkosi profile %q package set drift: got %q, want %q", lockedProfile.Name, manifestProfile.PackageSetRef, lockedProfile.PackageSetRef)
		}
		if lockedProfile.MkosiVersion != "" {
			mkosiVersion := toolVersion(verification.Manifest.Tools, "mkosi")
			if mkosiVersion != lockedProfile.MkosiVersion {
				return fmt.Errorf("mkosi profile %q mkosi version drift: got %q, want %q", lockedProfile.Name, mkosiVersion, lockedProfile.MkosiVersion)
			}
		}

		lockedSet, ok := lockSets[lockedProfile.PackageSetRef]
		if !ok {
			return fmt.Errorf("package set %q is missing from package lock", lockedProfile.PackageSetRef)
		}
		manifestSet, ok := manifestSets[lockedProfile.PackageSetRef]
		if !ok {
			return fmt.Errorf("package set %q is missing from resource manifest", lockedProfile.PackageSetRef)
		}
		if manifestSet.LockDigest != verification.LockDigest {
			return fmt.Errorf("package set %q lock digest drift: got %q, want %q", manifestSet.Name, manifestSet.LockDigest, verification.LockDigest)
		}
		if lockedSet.Source != "" && manifestSet.Source != lockedSet.Source {
			return fmt.Errorf("package set %q source drift: got %q, want %q", manifestSet.Name, manifestSet.Source, lockedSet.Source)
		}
		if err := compareRepositories(manifestSet.Name, manifestSet.Repositories, lockedSet.Repositories); err != nil {
			return err
		}
		if err := comparePackages(manifestSet.Name, manifestSet.Packages, lockedSet.Packages); err != nil {
			return err
		}
	}
	return nil
}

func PackageLockFromManifest(manifest Manifest) (PackageLock, error) {
	if err := ValidateManifest(manifest); err != nil {
		return PackageLock{}, err
	}
	if len(manifest.MkosiProfiles) == 0 {
		return PackageLock{}, errors.New("manifest mkosiProfiles is required")
	}
	if len(manifest.PackageSets) == 0 {
		return PackageLock{}, errors.New("manifest packageSets is required")
	}
	sets := map[string]PackageSet{}
	for _, set := range manifest.PackageSets {
		if len(set.Repositories) == 0 {
			return PackageLock{}, fmt.Errorf("package set %q repositories are required for lock refresh", set.Name)
		}
		if len(set.Packages) == 0 {
			return PackageLock{}, fmt.Errorf("package set %q packages are required for lock refresh", set.Name)
		}
		sets[set.Name] = set
	}
	lock := PackageLock{
		APIVersion: APIVersion,
		Kind:       PackageLockKind,
		Tools:      append([]Tool(nil), manifest.Tools...),
	}
	mkosiVersion := toolVersion(manifest.Tools, "mkosi")
	for _, profile := range manifest.MkosiProfiles {
		if _, ok := sets[profile.PackageSetRef]; !ok {
			return PackageLock{}, fmt.Errorf("mkosi profile %q package set %q is missing from manifest", profile.Name, profile.PackageSetRef)
		}
		lock.MkosiProfiles = append(lock.MkosiProfiles, PackageLockProfile{
			Name:          profile.Name,
			Path:          profile.Path,
			ConfigDigest:  profile.ConfigDigest,
			PackageSetRef: profile.PackageSetRef,
			MkosiVersion:  mkosiVersion,
		})
	}
	for _, set := range manifest.PackageSets {
		lock.PackageSets = append(lock.PackageSets, PackageLockPackageSet{
			Name:         set.Name,
			Source:       set.Source,
			Distribution: set.Distribution,
			Release:      set.Release,
			Architecture: set.Architecture,
			Repositories: append([]PackageRepository(nil), set.Repositories...),
			Packages:     append([]Package(nil), set.Packages...),
		})
	}
	if err := ValidatePackageLock(lock); err != nil {
		return PackageLock{}, err
	}
	return lock, nil
}

func ValidatePackageLock(lock PackageLock) error {
	if lock.APIVersion != APIVersion {
		return fmt.Errorf("apiVersion must be %q", APIVersion)
	}
	if lock.Kind != PackageLockKind {
		return fmt.Errorf("kind must be %q", PackageLockKind)
	}
	if len(lock.MkosiProfiles) == 0 {
		return errors.New("mkosiProfiles is required")
	}
	if len(lock.PackageSets) == 0 {
		return errors.New("packageSets is required")
	}
	sets := map[string]bool{}
	for i, set := range lock.PackageSets {
		if err := validateLockedPackageSet(set); err != nil {
			return fmt.Errorf("packageSets[%d]: %w", i, err)
		}
		if sets[set.Name] {
			return fmt.Errorf("packageSets[%d]: duplicate package set %q", i, set.Name)
		}
		sets[set.Name] = true
	}
	profiles := map[string]bool{}
	for i, profile := range lock.MkosiProfiles {
		if strings.TrimSpace(profile.Name) == "" || strings.TrimSpace(profile.Path) == "" {
			return fmt.Errorf("mkosiProfiles[%d]: name and path are required", i)
		}
		if profiles[profile.Name] {
			return fmt.Errorf("mkosiProfiles[%d]: duplicate profile %q", i, profile.Name)
		}
		profiles[profile.Name] = true
		if profile.ConfigDigest != "" && !validSHA256(profile.ConfigDigest) {
			return fmt.Errorf("mkosiProfiles[%d]: configSHA256 must be lowercase SHA-256", i)
		}
		if strings.TrimSpace(profile.PackageSetRef) == "" {
			return fmt.Errorf("mkosiProfiles[%d]: packageSetRef is required", i)
		}
		if !sets[profile.PackageSetRef] {
			return fmt.Errorf("mkosiProfiles[%d]: packageSetRef %q is not defined", i, profile.PackageSetRef)
		}
	}
	return nil
}

func compareTools(got, want []Tool) error {
	if len(want) == 0 {
		return nil
	}
	gotTools := map[string]Tool{}
	for _, tool := range got {
		gotTools[tool.Name] = tool
	}
	wantTools := map[string]Tool{}
	for _, tool := range want {
		wantTools[tool.Name] = tool
		actual, ok := gotTools[tool.Name]
		if !ok {
			return fmt.Errorf("tool %q is missing from resource manifest", tool.Name)
		}
		if tool.Version != "" && actual.Version != tool.Version {
			return fmt.Errorf("tool %q version drift: got %q, want %q", tool.Name, actual.Version, tool.Version)
		}
		if tool.Path != "" && actual.Path != tool.Path {
			return fmt.Errorf("tool %q path drift: got %q, want %q", tool.Name, actual.Path, tool.Path)
		}
		if tool.Digest != "" && actual.Digest != tool.Digest {
			return fmt.Errorf("tool %q digest drift", tool.Name)
		}
	}
	for _, tool := range got {
		if _, ok := wantTools[tool.Name]; !ok {
			return fmt.Errorf("resource manifest contains unlocked tool %q", tool.Name)
		}
	}
	return nil
}

func toolVersion(tools []Tool, name string) string {
	for _, tool := range tools {
		if tool.Name == name {
			return tool.Version
		}
	}
	return ""
}

func validateLockedPackageSet(set PackageLockPackageSet) error {
	if strings.TrimSpace(set.Name) == "" {
		return errors.New("name is required")
	}
	if len(set.Repositories) == 0 {
		return errors.New("repositories is required")
	}
	if err := validatePackageRepositories(set.Repositories); err != nil {
		return err
	}
	if len(set.Packages) == 0 {
		return errors.New("packages is required")
	}
	for i, pkg := range set.Packages {
		if strings.TrimSpace(pkg.Name) == "" || strings.TrimSpace(pkg.NEVRA) == "" {
			return fmt.Errorf("packages[%d]: name and nevra are required", i)
		}
		if pkg.Checksum != "" && !validSHA256(pkg.Checksum) {
			return fmt.Errorf("packages[%d]: sha256 must be lowercase SHA-256", i)
		}
	}
	return nil
}

func validatePackageRepositories(repositories []PackageRepository) error {
	seen := map[string]bool{}
	for i, repo := range repositories {
		if strings.TrimSpace(repo.ID) == "" {
			return fmt.Errorf("repositories[%d]: id is required", i)
		}
		if seen[repo.ID] {
			return fmt.Errorf("repositories[%d]: duplicate repository %q", i, repo.ID)
		}
		seen[repo.ID] = true
	}
	return nil
}

func compareRepositories(name string, got, want []PackageRepository) error {
	gotRepos := map[string]PackageRepository{}
	for _, repo := range got {
		gotRepos[repo.ID] = repo
	}
	for _, locked := range want {
		actual, ok := gotRepos[locked.ID]
		if !ok {
			return fmt.Errorf("package set %q missing repository %q", name, locked.ID)
		}
		if locked.BaseURL != "" && actual.BaseURL != locked.BaseURL {
			return fmt.Errorf("package set %q repository %q baseURL drift: got %q, want %q", name, locked.ID, actual.BaseURL, locked.BaseURL)
		}
		if locked.GPGKey != "" && actual.GPGKey != locked.GPGKey {
			return fmt.Errorf("package set %q repository %q gpgKey drift", name, locked.ID)
		}
		delete(gotRepos, locked.ID)
	}
	if len(gotRepos) > 0 {
		for id := range gotRepos {
			return fmt.Errorf("package set %q contains unlocked repository %q", name, id)
		}
	}
	return nil
}

func comparePackages(name string, got, want []Package) error {
	gotPackages := map[string]Package{}
	for _, pkg := range got {
		gotPackages[pkg.Name] = pkg
	}
	for _, locked := range want {
		actual, ok := gotPackages[locked.Name]
		if !ok {
			return fmt.Errorf("package set %q missing package %q", name, locked.Name)
		}
		if actual.NEVRA != locked.NEVRA {
			return fmt.Errorf("package set %q package %q NEVRA drift: got %q, want %q", name, locked.Name, actual.NEVRA, locked.NEVRA)
		}
		if locked.Checksum != "" && actual.Checksum != locked.Checksum {
			return fmt.Errorf("package set %q package %q checksum drift", name, locked.Name)
		}
		delete(gotPackages, locked.Name)
	}
	if len(gotPackages) > 0 {
		for name := range gotPackages {
			return fmt.Errorf("package set contains unlocked package %q", name)
		}
	}
	return nil
}
