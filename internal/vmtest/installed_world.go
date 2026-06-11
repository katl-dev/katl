package vmtest

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type InstalledRuntimeWorldNode struct {
	Node    Node
	Fixture InstalledRuntimeFixture
	Config  InstalledRuntimeConfig
}

type PublishedFirstInstallRuntimeFixture struct {
	APIVersion      string `json:"apiVersion"`
	Kind            string `json:"kind"`
	NodeName        string `json:"nodeName"`
	SystemRole      string `json:"systemRole"`
	FixtureManifest string `json:"fixtureManifest"`
	DiskFormat      string `json:"diskFormat"`
}

type publishedFixtureCandidate struct {
	Path    string
	ModTime time.Time
}

func AddPublishedInstalledRuntimeNode(scenario *WorldScenario, repo string, spec NodeSpec) (InstalledRuntimeWorldNode, error) {
	return AddPublishedInstalledRuntimeNodeFromBuildRoots(scenario, []string{filepath.Join(repo, "_build")}, spec)
}

func AddPublishedInstalledRuntimeNodeFromBuildRoots(scenario *WorldScenario, buildRoots []string, spec NodeSpec) (InstalledRuntimeWorldNode, error) {
	node, err := scenario.AddNode(spec)
	if err != nil {
		return InstalledRuntimeWorldNode{}, err
	}
	published, err := FindPublishedFirstInstallRuntimeFixtureInBuildRoots(buildRoots, spec)
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
	return FindPublishedFirstInstallRuntimeFixtureInBuildRoots([]string{filepath.Join(repo, "_build")}, spec)
}

func FindPublishedFirstInstallRuntimeFixtureInBuildRoots(buildRoots []string, spec NodeSpec) (PublishedFirstInstallRuntimeFixture, error) {
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
		best, ok, err := selectPublishedFirstInstallRuntimeFixture(candidates, spec)
		if err != nil {
			return PublishedFirstInstallRuntimeFixture{}, err
		}
		if ok {
			return best, nil
		}
	}
	return PublishedFirstInstallRuntimeFixture{}, errors.New("published installed runtime fixture is missing: run the first-install fixture contract")
}

func selectPublishedFirstInstallRuntimeFixture(candidates []publishedFixtureCandidate, spec NodeSpec) (PublishedFirstInstallRuntimeFixture, bool, error) {
	var best PublishedFirstInstallRuntimeFixture
	var bestTime time.Time
	for _, candidate := range candidates {
		published, err := ReadPublishedFirstInstallRuntimeFixture(candidate.Path)
		if err != nil {
			return PublishedFirstInstallRuntimeFixture{}, false, err
		}
		if spec.Name != "" && published.NodeName != spec.Name {
			continue
		}
		if spec.Role != "" && NodeRole(published.SystemRole) != spec.Role {
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
	path := filepath.Join(root, "_build", "published-first-install-runtime", id, "published-first-install-runtime-fixture.json")
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
	}
	if err := writeJSON(path, published); err != nil {
		return "", err
	}
	return path, nil
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
	if published.DiskFormat == "" {
		published.DiskFormat = string(DiskQCOW2)
	}
	return published, nil
}
