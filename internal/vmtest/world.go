package vmtest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
)

const (
	WorldManifestEnv = "KATL_VMTEST_WORLD_MANIFEST"
	WorldAPIVersion  = "katl.dev/v1alpha1"
	WorldKind        = "VMTestWorld"
)

type World struct {
	APIVersion        string                 `json:"apiVersion"`
	Kind              string                 `json:"kind"`
	RunID             string                 `json:"runID"`
	RunDir            string                 `json:"runDir"`
	CacheDir          string                 `json:"cacheDir"`
	ArtifactDir       string                 `json:"artifactDir"`
	ScenarioDir       string                 `json:"scenarioDir"`
	RunIndex          string                 `json:"runIndex,omitempty"`
	GoTestLog         string                 `json:"goTestLog,omitempty"`
	ResourceManifest  string                 `json:"resourceManifest,omitempty"`
	ResourceDigest    string                 `json:"resourceManifestSHA256,omitempty"`
	PackageLock       string                 `json:"packageLock,omitempty"`
	PackageLockDigest string                 `json:"packageLockSHA256,omitempty"`
	Artifacts         []WorldArtifact        `json:"vmtestArtifacts,omitempty"`
	ArtifactInputs    *WorldArtifactInputs   `json:"vmtestArtifactInputs,omitempty"`
	AutoRebuild       bool                   `json:"autoRebuild,omitempty"`
	ArtifactSet       string                 `json:"artifactSet,omitempty"`
	DebugOnFailure    bool                   `json:"debugOnFailure,omitempty"`
	DebugShell        bool                   `json:"debugShell,omitempty"`
	Libvirt           WorldLibvirt           `json:"libvirt"`
	Network           WorldNetwork           `json:"network"`
	Capabilities      map[string]WorldStatus `json:"capabilities"`
}

type WorldArtifact struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Path      string `json:"path"`
	RepoPath  string `json:"repoPath,omitempty"`
	Digest    string `json:"sha256"`
	SizeBytes int64  `json:"sizeBytes"`
	Source    string `json:"source"`
	Action    string `json:"action"`
}

type WorldArtifactInputs struct {
	ResourceManifestGitRevision string                 `json:"resourceManifestGitRevision,omitempty"`
	Tools                       []WorldToolInput       `json:"tools,omitempty"`
	MkosiProfiles               []WorldMkosiProfile    `json:"mkosiProfiles,omitempty"`
	PackageSets                 []WorldPackageSetInput `json:"packageSets,omitempty"`
}

type WorldToolInput struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	Path    string `json:"path,omitempty"`
	SHA256  string `json:"sha256,omitempty"`
}

type WorldMkosiProfile struct {
	Name          string `json:"name"`
	Path          string `json:"path"`
	ConfigSHA256  string `json:"configSHA256,omitempty"`
	PackageSetRef string `json:"packageSetRef,omitempty"`
}

type WorldPackageSetInput struct {
	Name         string `json:"name"`
	Source       string `json:"source,omitempty"`
	Profile      string `json:"profile,omitempty"`
	LockSHA256   string `json:"lockSHA256,omitempty"`
	PackageCount int    `json:"packageCount"`
}

type WorldLibvirt struct {
	URI          string `json:"uri"`
	Network      string `json:"network"`
	StoragePool  string `json:"storagePool"`
	StoragePath  string `json:"storagePath"`
	DomainPrefix string `json:"domainPrefix"`
}

type WorldNetwork struct {
	Backend   NetworkBackend `json:"backend"`
	Name      string         `json:"name"`
	CIDR      string         `json:"cidr"`
	Gateway   string         `json:"gateway"`
	LeaseFile string         `json:"leaseFile"`
}

type NetworkBackend string

const (
	NetworkLibvirt NetworkBackend = "libvirt"
)

type WorldStatus string

const (
	WorldStatusPassed      WorldStatus = "passed"
	WorldStatusFailed      WorldStatus = "failed"
	WorldStatusSetupFailed WorldStatus = "setup-failed"
	WorldStatusHostSkipped WorldStatus = "host-skipped"
	WorldStatusDisabled    WorldStatus = "disabled"
)

func DecodeWorld(reader io.Reader) (World, error) {
	var world World
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&world); err != nil {
		return World{}, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return World{}, errors.New("world manifest must contain exactly one JSON document")
	}
	if err := ValidateWorld(world); err != nil {
		return World{}, err
	}
	return world, nil
}

func LoadWorld(path string) (World, error) {
	file, err := os.Open(path)
	if err != nil {
		return World{}, err
	}
	defer file.Close()
	world, err := DecodeWorld(file)
	if err != nil {
		return World{}, fmt.Errorf("%s: %w", path, err)
	}
	return world, nil
}

func LoadWorldFromEnv() (World, error) {
	path := os.Getenv(WorldManifestEnv)
	if path == "" {
		return World{}, fmt.Errorf("%s is not set", WorldManifestEnv)
	}
	return LoadWorld(path)
}

func RequireWorld(t interface {
	Helper()
	Fatalf(format string, args ...any)
}) World {
	t.Helper()
	world, err := LoadWorldFromEnv()
	if err != nil {
		t.Fatalf("VM test world setup failed: %v; run enabled VM tests with scripts/vmtest-run", err)
	}
	return world
}

func ValidateWorld(world World) error {
	if world.APIVersion != WorldAPIVersion {
		return fmt.Errorf("unsupported apiVersion %q", world.APIVersion)
	}
	if world.Kind != WorldKind {
		return fmt.Errorf("unsupported kind %q", world.Kind)
	}
	if strings.TrimSpace(world.RunID) == "" {
		return errors.New("runID is required")
	}
	for name, path := range map[string]string{
		"runDir":      world.RunDir,
		"cacheDir":    world.CacheDir,
		"artifactDir": world.ArtifactDir,
		"scenarioDir": world.ScenarioDir,
		"leaseFile":   world.Network.LeaseFile,
	} {
		if strings.TrimSpace(path) == "" {
			return fmt.Errorf("%s is required", name)
		}
		if !filepath.IsAbs(path) {
			return fmt.Errorf("%s must be an absolute path: %s", name, path)
		}
	}
	if strings.TrimSpace(world.RunIndex) != "" && !filepath.IsAbs(world.RunIndex) {
		return fmt.Errorf("runIndex must be an absolute path: %s", world.RunIndex)
	}
	if strings.TrimSpace(world.GoTestLog) != "" && !filepath.IsAbs(world.GoTestLog) {
		return fmt.Errorf("goTestLog must be an absolute path: %s", world.GoTestLog)
	}
	if strings.TrimSpace(world.ResourceManifest) != "" && !filepath.IsAbs(world.ResourceManifest) {
		return fmt.Errorf("resourceManifest must be an absolute path: %s", world.ResourceManifest)
	}
	if strings.TrimSpace(world.ResourceDigest) != "" && !validWorldSHA256(world.ResourceDigest) {
		return fmt.Errorf("resourceManifestSHA256 must be lowercase SHA-256")
	}
	if strings.TrimSpace(world.PackageLock) != "" && !filepath.IsAbs(world.PackageLock) {
		return fmt.Errorf("packageLock must be an absolute path: %s", world.PackageLock)
	}
	if strings.TrimSpace(world.PackageLockDigest) != "" && !validWorldSHA256(world.PackageLockDigest) {
		return fmt.Errorf("packageLockSHA256 must be lowercase SHA-256")
	}
	for i, artifact := range world.Artifacts {
		if err := validateWorldArtifact(i, artifact); err != nil {
			return err
		}
	}
	if world.ArtifactInputs != nil {
		for i, profile := range world.ArtifactInputs.MkosiProfiles {
			if strings.TrimSpace(profile.Name) == "" {
				return fmt.Errorf("vmtestArtifactInputs.mkosiProfiles[%d].name is required", i)
			}
			if strings.TrimSpace(profile.Path) == "" {
				return fmt.Errorf("vmtestArtifactInputs.mkosiProfiles[%d].path is required", i)
			}
			if strings.TrimSpace(profile.ConfigSHA256) != "" && !validWorldSHA256(profile.ConfigSHA256) {
				return fmt.Errorf("vmtestArtifactInputs.mkosiProfiles[%d].configSHA256 must be lowercase SHA-256", i)
			}
		}
		for i, set := range world.ArtifactInputs.PackageSets {
			if strings.TrimSpace(set.Name) == "" {
				return fmt.Errorf("vmtestArtifactInputs.packageSets[%d].name is required", i)
			}
			if strings.TrimSpace(set.LockSHA256) != "" && !validWorldSHA256(set.LockSHA256) {
				return fmt.Errorf("vmtestArtifactInputs.packageSets[%d].lockSHA256 must be lowercase SHA-256", i)
			}
		}
	}
	if err := validateWorldLibvirt(world.Libvirt); err != nil {
		return err
	}
	if err := validateWorldNetwork(world.Network); err != nil {
		return err
	}
	if len(world.Capabilities) == 0 {
		return errors.New("capabilities are required")
	}
	for name, status := range world.Capabilities {
		if strings.TrimSpace(name) == "" {
			return errors.New("capability name is required")
		}
		if !validWorldStatus(status) {
			return fmt.Errorf("unsupported capability status %q for %q", status, name)
		}
	}
	return nil
}

func validateWorldArtifact(index int, artifact WorldArtifact) error {
	prefix := fmt.Sprintf("vmtestArtifacts[%d]", index)
	if strings.TrimSpace(artifact.Name) == "" {
		return fmt.Errorf("%s.name is required", prefix)
	}
	if strings.TrimSpace(artifact.Kind) == "" {
		return fmt.Errorf("%s.kind is required", prefix)
	}
	if strings.TrimSpace(artifact.Path) == "" {
		return fmt.Errorf("%s.path is required", prefix)
	}
	if !filepath.IsAbs(artifact.Path) {
		return fmt.Errorf("%s.path must be an absolute path: %s", prefix, artifact.Path)
	}
	if !validWorldSHA256(artifact.Digest) {
		return fmt.Errorf("%s.sha256 must be lowercase SHA-256", prefix)
	}
	if artifact.SizeBytes <= 0 {
		return fmt.Errorf("%s.sizeBytes must be positive", prefix)
	}
	switch artifact.Action {
	case "built", "cache-resolved", "validated":
	default:
		return fmt.Errorf("%s.action must be built, cache-resolved, or validated", prefix)
	}
	if strings.TrimSpace(artifact.Source) == "" {
		return fmt.Errorf("%s.source is required", prefix)
	}
	return nil
}

func validWorldSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, ch := range value {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return false
		}
	}
	return true
}

func validateWorldLibvirt(libvirt WorldLibvirt) error {
	for name, value := range map[string]string{
		"libvirt.uri":          libvirt.URI,
		"libvirt.network":      libvirt.Network,
		"libvirt.storagePool":  libvirt.StoragePool,
		"libvirt.storagePath":  libvirt.StoragePath,
		"libvirt.domainPrefix": libvirt.DomainPrefix,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	if !filepath.IsAbs(libvirt.StoragePath) {
		return fmt.Errorf("libvirt.storagePath must be an absolute path: %s", libvirt.StoragePath)
	}
	return nil
}

func validateWorldNetwork(network WorldNetwork) error {
	switch network.Backend {
	case NetworkLibvirt:
		if strings.TrimSpace(network.Name) == "" {
			return errors.New("network.name is required for libvirt backend")
		}
	case "":
		return errors.New("network.backend is required")
	default:
		return fmt.Errorf("unsupported network.backend %q", network.Backend)
	}
	_, cidr, err := net.ParseCIDR(network.CIDR)
	if err != nil {
		return fmt.Errorf("network.cidr %q is invalid: %w", network.CIDR, err)
	}
	gateway := net.ParseIP(network.Gateway)
	if gateway == nil {
		return fmt.Errorf("network.gateway %q is invalid", network.Gateway)
	}
	if !cidr.Contains(gateway) {
		return fmt.Errorf("network.gateway %q is outside network.cidr %q", network.Gateway, network.CIDR)
	}
	return nil
}

func validWorldStatus(status WorldStatus) bool {
	switch status {
	case WorldStatusPassed, WorldStatusFailed, WorldStatusSetupFailed, WorldStatusHostSkipped, WorldStatusDisabled:
		return true
	default:
		return false
	}
}
