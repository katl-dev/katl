package vmtest

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/installer"
)

var errPublishedFirstInstallRuntimeFixtureMissing = errors.New("published installed runtime fixture is missing")

type InstalledRuntimeWorldNode struct {
	Node    Node
	Fixture InstalledRuntimeFixture
	Config  InstalledRuntimeConfig
}

type PublishedFirstInstallRuntimeFixture struct {
	APIVersion           string   `json:"apiVersion"`
	Kind                 string   `json:"kind"`
	NodeName             string   `json:"nodeName"`
	SystemRole           string   `json:"systemRole"`
	FixtureManifest      string   `json:"fixtureManifest"`
	DiskFormat           string   `json:"diskFormat"`
	InputDigest          string   `json:"inputDigest,omitempty"`
	InstallerUKI         string   `json:"installerUKI,omitempty"`
	InstallerKernel      string   `json:"installerKernel,omitempty"`
	InstallerInitrd      string   `json:"installerInitrd,omitempty"`
	InstallerCommandLine []string `json:"installerCommandLine,omitempty"`
	RuntimeArtifact      string   `json:"runtimeArtifact,omitempty"`
	InstallManifest      string   `json:"installManifest,omitempty"`
	FirstInstallMode     string   `json:"firstInstallMode,omitempty"`
	UseInstalledESP      bool     `json:"useInstalledESP,omitempty"`
}

type publishedFixtureCandidate struct {
	Path    string
	ModTime time.Time
}

func (p PublishedFirstInstallRuntimeFixture) HasInstallerProvenance() bool {
	return strings.TrimSpace(p.InstallManifest) != "" &&
		(strings.TrimSpace(p.InstallerUKI) != "" || (strings.TrimSpace(p.InstallerKernel) != "" && strings.TrimSpace(p.InstallerInitrd) != ""))
}

func AddPublishedInstalledRuntimeNode(scenario *WorldScenario, repo string, spec NodeSpec) (InstalledRuntimeWorldNode, error) {
	return AddPublishedInstalledRuntimeNodeFromBuildRoots(scenario, []string{DefaultVMTestCacheDir(repo)}, spec)
}

func AddPublishedInstalledRuntimeNodeFromBuildRoots(scenario *WorldScenario, buildRoots []string, spec NodeSpec) (InstalledRuntimeWorldNode, error) {
	return addPublishedInstalledRuntimeNodeFromBuildRoots(scenario, buildRoots, spec, "")
}

func addPublishedInstalledRuntimeNodeFromBuildRoots(scenario *WorldScenario, buildRoots []string, spec NodeSpec, inputDigest string) (InstalledRuntimeWorldNode, error) {
	node, err := scenario.AddNode(spec)
	if err != nil {
		return InstalledRuntimeWorldNode{}, err
	}
	published, err := findPublishedFirstInstallRuntimeFixtureInBuildRoots(buildRoots, spec, inputDigest)
	if err != nil {
		return InstalledRuntimeWorldNode{Node: node}, err
	}
	factory := scenario.NodeFixtures(node)
	format := DiskFormat(published.DiskFormat)
	if format == "" {
		format = DiskQCOW2
	}
	fixture, err := factory.PublishInstalledRuntimeFromFirstInstall(published.FixtureManifest, format)
	if err != nil {
		return InstalledRuntimeWorldNode{Node: node}, err
	}
	return InstalledRuntimeWorldNode{
		Node:    node,
		Fixture: fixture,
		Config: InstalledRuntimeConfig{
			Disk:            fixture.Disk,
			DiskFormat:      fixture.DiskFormat,
			ESPArtifacts:    fixture.ESPArtifacts,
			FixtureManifest: fixture.ManifestPath,
			NodeMetadata:    fixture.NodeMetadata,
		},
	}, nil
}

func FindPublishedFirstInstallRuntimeFixture(repo string, spec NodeSpec) (PublishedFirstInstallRuntimeFixture, error) {
	return FindPublishedFirstInstallRuntimeFixtureInBuildRoots([]string{DefaultVMTestCacheDir(repo)}, spec)
}

func DefaultVMTestCacheDir(repo string) string {
	return filepath.Join(repo, "_build", "vmtest")
}

func WorldFixtureCacheDir(world World) string {
	if strings.TrimSpace(world.CacheDir) != "" {
		return world.CacheDir
	}
	return filepath.Join(world.RunDir, "_build")
}

func validateInstalledRuntimeArtifactSet(world World) error {
	if world.ArtifactSet == "runtime" {
		return errors.New("installed-runtime VM tests require --artifact-set=default or --artifact-set=install; --artifact-set=runtime is only for direct-runtime tests")
	}
	return nil
}

func FindPublishedFirstInstallRuntimeFixtureInBuildRoots(buildRoots []string, spec NodeSpec) (PublishedFirstInstallRuntimeFixture, error) {
	return findPublishedFirstInstallRuntimeFixtureInBuildRoots(buildRoots, spec, "")
}

func findPublishedFirstInstallRuntimeFixtureInBuildRoots(buildRoots []string, spec NodeSpec, inputDigest string) (PublishedFirstInstallRuntimeFixture, error) {
	for _, root := range buildRoots {
		if strings.TrimSpace(root) == "" {
			continue
		}
		var candidates []publishedFixtureCandidate
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return nil
				}
				return err
			}
			if entry.IsDir() || entry.Name() != "published-first-install-runtime-fixture.json" {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			candidates = append(candidates, publishedFixtureCandidate{Path: path, ModTime: info.ModTime()})
			return nil
		})
		if errors.Is(err, os.ErrNotExist) {
			err = nil
		}
		if err != nil {
			return PublishedFirstInstallRuntimeFixture{}, err
		}
		best, ok, err := selectPublishedFirstInstallRuntimeFixture(candidates, spec, inputDigest)
		if err != nil {
			return PublishedFirstInstallRuntimeFixture{}, err
		}
		if ok {
			return best, nil
		}
	}
	return PublishedFirstInstallRuntimeFixture{}, fmt.Errorf("%w: run the first-install fixture contract", errPublishedFirstInstallRuntimeFixtureMissing)
}

func selectPublishedFirstInstallRuntimeFixture(candidates []publishedFixtureCandidate, spec NodeSpec, inputDigest string) (PublishedFirstInstallRuntimeFixture, bool, error) {
	var best PublishedFirstInstallRuntimeFixture
	var bestTime time.Time
	for _, candidate := range candidates {
		published, err := ReadPublishedFirstInstallRuntimeFixture(candidate.Path)
		if err != nil {
			if errors.Is(err, errPublishedFirstInstallRuntimeFixtureMissing) {
				continue
			}
			return PublishedFirstInstallRuntimeFixture{}, false, err
		}
		if spec.Name != "" && published.NodeName != spec.Name {
			continue
		}
		if spec.Role != "" && NodeRole(published.SystemRole) != spec.Role {
			continue
		}
		if inputDigest != "" && published.InputDigest != inputDigest {
			continue
		}
		if best.FixtureManifest == "" || candidate.ModTime.After(bestTime) {
			best = published
			bestTime = candidate.ModTime
		}
	}
	if best.FixtureManifest == "" {
		return PublishedFirstInstallRuntimeFixture{}, false, nil
	}
	return best, true, nil
}

func WritePublishedFirstInstallRuntimeFixture(root, name, fixtureManifest string, format DiskFormat) (string, error) {
	return writePublishedFirstInstallRuntimeFixture(root, name, fixtureManifest, format, "")
}

func writePublishedFirstInstallRuntimeFixture(root, name, fixtureManifest string, format DiskFormat, inputDigest string) (string, error) {
	return writePublishedFirstInstallRuntimeFixtureForContract(root, name, fixtureManifest, format, inputDigest, FirstInstallRuntimeFixtureContract{})
}

func writePublishedFirstInstallRuntimeFixtureForContract(root, name, fixtureManifest string, format DiskFormat, inputDigest string, contract FirstInstallRuntimeFixtureContract) (string, error) {
	source, err := readInstalledRuntimeFixture(fixtureManifest)
	if err != nil {
		return "", err
	}
	if source == nil {
		return "", errors.New("first-install installed runtime fixture manifest is required")
	}
	nodeName := strings.TrimSpace(source.NodeName)
	systemRole := strings.TrimSpace(source.SystemRole)
	if nodeName == "" || systemRole == "" {
		return "", errors.New("first-install installed runtime fixture identity is incomplete")
	}
	if format == "" {
		format = DiskFormat(source.Disk.Format)
	}
	if format == "" {
		format = DiskQCOW2
	}
	id := clean(first(name, nodeName+"-"+systemRole))
	if id == "" {
		return "", errors.New("published installed runtime fixture name is required")
	}
	path := filepath.Join(root, "published-first-install-runtime", id, "published-first-install-runtime-fixture.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	published := PublishedFirstInstallRuntimeFixture{
		APIVersion:      WorldAPIVersion,
		Kind:            "PublishedFirstInstallRuntimeFixture",
		NodeName:        nodeName,
		SystemRole:      systemRole,
		FixtureManifest: relFrom(filepath.Dir(path), fixtureManifest),
		DiskFormat:      string(format),
		InputDigest:     inputDigest,
	}
	if strings.TrimSpace(contract.ManifestPath) != "" {
		provenance, err := cachePublishedFirstInstallProvenance(filepath.Dir(path), inputDigest, contract)
		if err != nil {
			return "", err
		}
		relOptional := func(value string) string {
			if strings.TrimSpace(value) == "" {
				return ""
			}
			return relFrom(filepath.Dir(path), value)
		}
		published.InstallerUKI = relOptional(provenance.InstallerUKI)
		published.InstallerKernel = relOptional(provenance.InstallerKernel)
		published.InstallerInitrd = relOptional(provenance.InstallerInitrd)
		published.InstallerCommandLine = slices.Clone(contract.InstallerBoot.CommandLine)
		published.RuntimeArtifact = relOptional(provenance.RuntimeArtifact)
		published.InstallManifest = relOptional(provenance.InstallManifest)
		published.FirstInstallMode = string(firstInstallModeForContract(contract))
		published.UseInstalledESP = contract.UseInstalledESP
	}
	if err := writeJSON(path, published); err != nil {
		return "", err
	}
	return path, nil
}

type publishedFirstInstallProvenance struct {
	InstallerUKI    string
	InstallerKernel string
	InstallerInitrd string
	RuntimeArtifact string
	InstallManifest string
}

func cachePublishedFirstInstallProvenance(root, inputDigest string, contract FirstInstallRuntimeFixtureContract) (publishedFirstInstallProvenance, error) {
	id := clean(inputDigest)
	if id == "" {
		id = "current"
	}
	dir := filepath.Join(root, "inputs", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return publishedFirstInstallProvenance{}, err
	}
	cacheFile := func(source, name string) (string, error) {
		if strings.TrimSpace(source) == "" {
			return "", nil
		}
		target := filepath.Join(dir, name)
		if err := copyRequiredFile(source, target, 0o644); err != nil {
			return "", err
		}
		return target, nil
	}
	var cached publishedFirstInstallProvenance
	var err error
	if cached.InstallerUKI, err = cacheFile(contract.InstallerBoot.InstallerUKI, "katl-installer.efi"); err != nil {
		return publishedFirstInstallProvenance{}, fmt.Errorf("cache installer UKI: %w", err)
	}
	if cached.InstallerKernel, err = cacheFile(contract.InstallerBoot.InstallerKernel, "katl-installer-kernel"); err != nil {
		return publishedFirstInstallProvenance{}, fmt.Errorf("cache installer kernel: %w", err)
	}
	if cached.InstallerInitrd, err = cacheFile(contract.InstallerBoot.InstallerInitrd, "katl-installer-initrd"); err != nil {
		return publishedFirstInstallProvenance{}, fmt.Errorf("cache installer initrd: %w", err)
	}
	if cached.RuntimeArtifact, err = cacheFile(contract.RuntimeArtifact, "katl-runtime-artifact"); err != nil {
		return publishedFirstInstallProvenance{}, fmt.Errorf("cache runtime artifact: %w", err)
	}
	if cached.InstallManifest, err = cacheFile(contract.ManifestPath, "install-manifest.json"); err != nil {
		return publishedFirstInstallProvenance{}, fmt.Errorf("cache install manifest: %w", err)
	}
	if _, err := cacheFile(filepath.Join(filepath.Dir(contract.ManifestPath), "single-image-proof.json"), "single-image-proof.json"); err != nil {
		return publishedFirstInstallProvenance{}, fmt.Errorf("cache install single-image proof: %w", err)
	}
	localRef, err := installManifestLocalImageRef(contract.ManifestPath)
	if err != nil {
		return publishedFirstInstallProvenance{}, err
	}
	if localRef != "" {
		if err := copyRequiredFile(
			filepath.Join(filepath.Dir(contract.ManifestPath), filepath.FromSlash(localRef)),
			filepath.Join(dir, filepath.FromSlash(localRef)),
			0o644,
		); err != nil {
			return publishedFirstInstallProvenance{}, fmt.Errorf("cache install manifest local image: %w", err)
		}
	}
	for _, name := range []string{installer.KubeadmConfigObjectsDir, installer.KubeadmConfigFilesDir} {
		if err := copyOptionalDir(filepath.Join(filepath.Dir(contract.ManifestPath), name), filepath.Join(dir, name)); err != nil {
			return publishedFirstInstallProvenance{}, fmt.Errorf("cache install manifest %s: %w", name, err)
		}
	}
	return cached, nil
}

func ReadPublishedFirstInstallRuntimeFixture(path string) (PublishedFirstInstallRuntimeFixture, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return PublishedFirstInstallRuntimeFixture{}, err
	}
	var published PublishedFirstInstallRuntimeFixture
	if err := json.Unmarshal(data, &published); err != nil {
		return PublishedFirstInstallRuntimeFixture{}, err
	}
	if published.APIVersion != WorldAPIVersion || published.Kind != "PublishedFirstInstallRuntimeFixture" {
		return PublishedFirstInstallRuntimeFixture{}, errors.New("published installed runtime fixture has unsupported apiVersion or kind")
	}
	if strings.TrimSpace(published.NodeName) == "" || strings.TrimSpace(published.SystemRole) == "" {
		return PublishedFirstInstallRuntimeFixture{}, errors.New("published installed runtime fixture identity is incomplete")
	}
	if strings.TrimSpace(published.FixtureManifest) == "" {
		return PublishedFirstInstallRuntimeFixture{}, errors.New("published installed runtime fixture manifest is required")
	}
	if !filepath.IsAbs(published.FixtureManifest) {
		published.FixtureManifest = filepath.Join(filepath.Dir(path), published.FixtureManifest)
	}
	resolve := func(value string) string {
		if strings.TrimSpace(value) == "" || filepath.IsAbs(value) {
			return value
		}
		return filepath.Join(filepath.Dir(path), value)
	}
	published.InstallerUKI = resolve(published.InstallerUKI)
	published.InstallerKernel = resolve(published.InstallerKernel)
	published.InstallerInitrd = resolve(published.InstallerInitrd)
	published.RuntimeArtifact = resolve(published.RuntimeArtifact)
	published.InstallManifest = resolve(published.InstallManifest)
	if published.DiskFormat == "" {
		published.DiskFormat = string(DiskQCOW2)
	}
	for label, referenced := range map[string]string{
		"fixture manifest": published.FixtureManifest,
		"installer UKI":    published.InstallerUKI,
		"installer kernel": published.InstallerKernel,
		"installer initrd": published.InstallerInitrd,
		"runtime artifact": published.RuntimeArtifact,
		"install manifest": published.InstallManifest,
	} {
		if strings.TrimSpace(referenced) == "" {
			continue
		}
		info, err := os.Stat(referenced)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return PublishedFirstInstallRuntimeFixture{}, fmt.Errorf("%w: %s %s", errPublishedFirstInstallRuntimeFixtureMissing, label, referenced)
			}
			return PublishedFirstInstallRuntimeFixture{}, err
		}
		if !info.Mode().IsRegular() {
			return PublishedFirstInstallRuntimeFixture{}, fmt.Errorf("published installed runtime fixture %s is not a regular file: %s", label, referenced)
		}
	}
	if published.InstallManifest != "" && published.RuntimeArtifact == "" {
		image, err := installManifestLocalImagePath(published.InstallManifest)
		if err != nil {
			return PublishedFirstInstallRuntimeFixture{}, err
		}
		info, err := os.Stat(image)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return PublishedFirstInstallRuntimeFixture{}, fmt.Errorf("%w: install image %s", errPublishedFirstInstallRuntimeFixtureMissing, image)
			}
			return PublishedFirstInstallRuntimeFixture{}, err
		}
		if !info.Mode().IsRegular() {
			return PublishedFirstInstallRuntimeFixture{}, fmt.Errorf("published installed runtime fixture install image is not a regular file: %s", image)
		}
	}
	if published.InstallManifest != "" {
		proof := filepath.Join(filepath.Dir(published.InstallManifest), "single-image-proof.json")
		info, err := os.Stat(proof)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return PublishedFirstInstallRuntimeFixture{}, fmt.Errorf("%w: single-image proof %s", errPublishedFirstInstallRuntimeFixtureMissing, proof)
			}
			return PublishedFirstInstallRuntimeFixture{}, err
		}
		if !info.Mode().IsRegular() {
			return PublishedFirstInstallRuntimeFixture{}, fmt.Errorf("published installed runtime fixture single-image proof is not a regular file: %s", proof)
		}
	}
	return published, nil
}
