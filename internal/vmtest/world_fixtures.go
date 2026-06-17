package vmtest

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zariel/katl/internal/installer"
	"github.com/zariel/katl/internal/installer/manifest"
)

const (
	FixtureMkosiArtifactIndex    = "mkosi-artifact-index"
	FixtureInstallerUKI          = "installer-uki"
	FixtureInstallerKernel       = "installer-kernel"
	FixtureInstallerInitrd       = "installer-initrd"
	FixtureRuntimeArtifact       = "runtime-artifact"
	FixtureKatlOSInstallImage    = "katlos-install-image"
	FixtureFirstInstallDisk      = "first-install-target-disk"
	FixtureInstalledRuntime      = "installed-runtime"
	FixtureInstalledRuntimeDisk  = "installed-runtime-disk"
	FixtureESPArtifacts          = "esp-artifacts"
	FixtureNodeMetadata          = "node-metadata"
	FixtureInstallManifest       = "install-manifest"
	FixturePublishedFirstInstall = "published-first-install-runtime"
)

type FixtureRecord struct {
	Kind         string            `json:"kind"`
	Name         string            `json:"name"`
	Node         string            `json:"node,omitempty"`
	Path         string            `json:"path,omitempty"`
	SHA256       string            `json:"sha256,omitempty"`
	TreeSHA256   string            `json:"treeSHA256,omitempty"`
	SizeBytes    int64             `json:"sizeBytes,omitempty"`
	DiskFormat   string            `json:"diskFormat,omitempty"`
	Disk         *FixtureRecord    `json:"disk,omitempty"`
	ESP          *FixtureRecord    `json:"espArtifacts,omitempty"`
	NodeMetadata *FixtureRecord    `json:"nodeMetadata,omitempty"`
	Properties   map[string]string `json:"properties,omitempty"`
	Provenance   FixtureProvenance `json:"provenance"`
}

type FixtureProvenance struct {
	Source           string `json:"source"`
	SourcePath       string `json:"sourcePath,omitempty"`
	SourceSHA256     string `json:"sourceSHA256,omitempty"`
	SourceTreeSHA256 string `json:"sourceTreeSHA256,omitempty"`
}

type NodeFixtureFactory struct {
	scenario *WorldScenario
	node     Node
}

type InstalledRuntimeFixtureInput struct {
	Disk         string
	DiskFormat   DiskFormat
	ESPArtifacts string
	NodeMetadata string
	NodeName     string
	SystemRole   NodeRole
}

type InstalledRuntimeFixture struct {
	ManifestPath string
	Disk         string
	DiskFormat   DiskFormat
	ESPArtifacts string
	NodeMetadata string
	Record       FixtureRecord
}

func (scenario *WorldScenario) NodeFixtures(node Node) NodeFixtureFactory {
	return NodeFixtureFactory{scenario: scenario, node: node}
}

func (factory NodeFixtureFactory) MkosiArtifactIndex(source string) (FixtureRecord, error) {
	return factory.stageFileFixture(FixtureMkosiArtifactIndex, "mkosi-artifacts.json", source)
}

func (factory NodeFixtureFactory) InstallerBoot(input InstallerBootConfig) (InstallerBootConfig, error) {
	output := input
	if strings.TrimSpace(input.InstallerKernel) != "" || strings.TrimSpace(input.InstallerInitrd) != "" {
		kernel, err := factory.stageFileFixture(FixtureInstallerKernel, filepath.Base(input.InstallerKernel), input.InstallerKernel)
		if err != nil {
			return InstallerBootConfig{}, err
		}
		initrd, err := factory.stageFileFixture(FixtureInstallerInitrd, filepath.Base(input.InstallerInitrd), input.InstallerInitrd)
		if err != nil {
			return InstallerBootConfig{}, err
		}
		output.InstallerKernel = kernel.Path
		output.InstallerInitrd = initrd.Path
		output.InstallerUKI = ""
		return output, nil
	}
	uki, err := factory.stageFileFixture(FixtureInstallerUKI, filepath.Base(input.InstallerUKI), input.InstallerUKI)
	if err != nil {
		return InstallerBootConfig{}, err
	}
	output.InstallerUKI = uki.Path
	return output, nil
}

func (factory NodeFixtureFactory) RuntimeArtifact(source string) (FixtureRecord, error) {
	name := filepath.Base(source)
	if strings.TrimSpace(name) == "" || name == "." {
		name = "runtime-root.squashfs"
	}
	return factory.stageFileFixture(FixtureRuntimeArtifact, name, source)
}

func (factory NodeFixtureFactory) KatlOSInstallImage(source string) (FixtureRecord, error) {
	return factory.stageFileFixture(FixtureKatlOSInstallImage, filepath.Base(source), source)
}

func (factory NodeFixtureFactory) ESPArtifacts(source string) (FixtureRecord, error) {
	dst := filepath.Join(factory.node.ArtifactDir, "esp")
	record, err := factory.stageTreeFixture(FixtureESPArtifacts, "esp", source, dst)
	if err != nil {
		return FixtureRecord{}, err
	}
	if err := CheckESP(record.Path); err != nil {
		return FixtureRecord{}, err
	}
	return record, nil
}

func (factory NodeFixtureFactory) NodeMetadata(source string) (FixtureRecord, error) {
	record, err := factory.stageFileFixture(FixtureNodeMetadata, "node.json", source)
	if err != nil {
		return FixtureRecord{}, err
	}
	if err := validateNodeMetadata(record.Path, factory.node); err != nil {
		return FixtureRecord{}, err
	}
	return record, nil
}

func (factory NodeFixtureFactory) InstallManifest(source string) (FixtureRecord, error) {
	record, err := factory.stageFileFixture(FixtureInstallManifest, "install-manifest.json", source)
	if err != nil {
		return FixtureRecord{}, err
	}
	file, err := os.Open(record.Path)
	if err != nil {
		return FixtureRecord{}, err
	}
	defer file.Close()
	if _, err := manifest.Decode(file); err != nil {
		return FixtureRecord{}, err
	}
	if err := factory.stageInstallManifestLocalRef(source, record.Path); err != nil {
		return FixtureRecord{}, err
	}
	if err := factory.stageInstallManifestKubeadmDirs(source, record.Path); err != nil {
		return FixtureRecord{}, err
	}
	if props, err := installManifestSidecarProperties(record.Path); err != nil {
		return FixtureRecord{}, err
	} else if len(props) > 0 {
		record.Properties = props
		if err := factory.replaceRecord(FixtureInstallManifest, record); err != nil {
			return FixtureRecord{}, err
		}
	}
	return record, nil
}

func installManifestSidecarProperties(stagedManifest string) (map[string]string, error) {
	properties := make(map[string]string)
	for _, input := range []struct {
		dir string
		key string
	}{
		{installer.KubeadmConfigObjectsDir, "kubeadmConfigObjectsTreeSHA256"},
		{installer.KubeadmConfigFilesDir, "kubeadmConfigFilesTreeSHA256"},
	} {
		path := filepath.Join(filepath.Dir(stagedManifest), input.dir)
		info, err := os.Stat(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		if !info.IsDir() {
			continue
		}
		sha, err := espTreeSHA256(path)
		if err != nil {
			return nil, err
		}
		properties[input.key] = sha
	}
	return properties, nil
}

func (factory NodeFixtureFactory) stageInstallManifestLocalRef(sourceManifest, stagedManifest string) error {
	data, err := os.ReadFile(stagedManifest)
	if err != nil {
		return err
	}
	var input struct {
		KatlosImage struct {
			LocalRef string `json:"localRef"`
		} `json:"katlosImage"`
	}
	if err := json.Unmarshal(data, &input); err != nil {
		return fmt.Errorf("decode install manifest localRef: %w", err)
	}
	localRef := strings.TrimSpace(input.KatlosImage.LocalRef)
	if localRef == "" {
		return nil
	}
	if filepath.IsAbs(localRef) || filepath.Clean(localRef) != localRef || localRef == "." || strings.HasPrefix(localRef, "../") || strings.Contains(localRef, "/../") {
		return fmt.Errorf("install manifest localRef %q must be a clean relative path", localRef)
	}
	src := filepath.Join(filepath.Dir(sourceManifest), filepath.FromSlash(localRef))
	dst := filepath.Join(filepath.Dir(stagedManifest), filepath.FromSlash(localRef))
	_, err = factory.stageFile(FixtureKatlOSInstallImage, localRef, src, dst)
	return err
}

func (factory NodeFixtureFactory) stageInstallManifestKubeadmDirs(sourceManifest, stagedManifest string) error {
	for _, name := range []string{installer.KubeadmConfigObjectsDir, installer.KubeadmConfigFilesDir} {
		src := filepath.Join(filepath.Dir(sourceManifest), name)
		info, err := os.Stat(src)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("install manifest %s is not a directory: %s", name, src)
		}
		sha, err := espTreeSHA256(src)
		if err != nil {
			return fmt.Errorf("hash install manifest %s: %w", name, err)
		}
		dst := filepath.Join(filepath.Dir(stagedManifest), name)
		if err := copyOrRejectStaleTree(src, dst, sha); err != nil {
			return fmt.Errorf("stage install manifest %s: %w", name, err)
		}
	}
	return nil
}

func (factory NodeFixtureFactory) FirstInstallTargetDisk(name string, format DiskFormat, size string) (DiskFixture, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "root"
	}
	if format == "" {
		format = DiskQCOW2
	}
	if strings.TrimSpace(size) == "" {
		return DiskFixture{}, errors.New("first-install target disk size is required")
	}
	fixture := TargetDisk(name, string(format), size)
	if err := factory.record(FixtureRecord{
		Kind:       FixtureFirstInstallDisk,
		Name:       name,
		Node:       factory.node.Name,
		DiskFormat: string(format),
		Properties: map[string]string{
			"size": size,
			"kind": string(fixture.Kind),
		},
		Provenance: FixtureProvenance{Source: "generated"},
	}); err != nil {
		return DiskFixture{}, err
	}
	return fixture, nil
}

func (factory NodeFixtureFactory) InstalledRuntime(input InstalledRuntimeFixtureInput) (InstalledRuntimeFixture, error) {
	if strings.TrimSpace(input.Disk) == "" {
		return InstalledRuntimeFixture{}, errors.New("installed runtime disk is required")
	}
	if strings.TrimSpace(input.ESPArtifacts) == "" {
		return InstalledRuntimeFixture{}, errors.New("installed runtime ESP artifacts are required")
	}
	format := diskFormat(input.DiskFormat)
	nodeName := first(input.NodeName, factory.node.Name)
	systemRole := input.SystemRole
	if systemRole == "" {
		systemRole = factory.node.Role
	}
	disk, err := factory.stageFile(FixtureInstalledRuntimeDisk, "installed-runtime."+string(format), input.Disk, filepath.Join(factory.node.DiskDir, "installed-runtime."+string(format)))
	if err != nil {
		return InstalledRuntimeFixture{}, err
	}
	esp, err := factory.stageTreeFixture(FixtureESPArtifacts, "esp", input.ESPArtifacts, filepath.Join(factory.node.ArtifactDir, "installed-runtime-esp"))
	if err != nil {
		return InstalledRuntimeFixture{}, err
	}
	if err := CheckESP(esp.Path); err != nil {
		return InstalledRuntimeFixture{}, err
	}
	var metadata *FixtureRecord
	metadataPath := ""
	if strings.TrimSpace(input.NodeMetadata) != "" {
		record, err := factory.stageFile(FixtureNodeMetadata, "node.json", input.NodeMetadata, filepath.Join(factory.node.ManifestDir, "node.json"))
		if err != nil {
			return InstalledRuntimeFixture{}, err
		}
		if err := validateNodeMetadata(record.Path, factory.node); err != nil {
			return InstalledRuntimeFixture{}, err
		}
		metadata = &record
		metadataPath = record.Path
	}

	manifestPath := filepath.Join(factory.node.ManifestDir, "installed-runtime-fixture.json")
	record := installedRuntimeFixtureRecord{
		APIVersion: "katl.dev/v1alpha1",
		Kind:       "InstalledRuntimeVMTestFixture",
		NodeName:   nodeName,
		SystemRole: string(systemRole),
		Disk: installedRuntimeFixtureDisk{
			Path:   relFrom(filepath.Dir(manifestPath), disk.Path),
			Format: string(format),
			SHA256: disk.SHA256,
		},
		ESPArtifacts: installedRuntimeFixtureESP{
			Path:       relFrom(filepath.Dir(manifestPath), esp.Path),
			TreeSHA256: esp.TreeSHA256,
		},
	}
	if metadata != nil {
		record.NodeMetadata = &installedRuntimeFixtureFile{
			Path:   relFrom(filepath.Dir(manifestPath), metadata.Path),
			SHA256: metadata.SHA256,
		}
	}
	if err := writeJSON(manifestPath, record); err != nil {
		return InstalledRuntimeFixture{}, err
	}
	if err := validateInstalledRuntimeFixture(manifestPath, record, InstalledRuntimeConfig{
		Disk:         disk.Path,
		DiskFormat:   format,
		ESPArtifacts: esp.Path,
	}, metadataPath); err != nil {
		return InstalledRuntimeFixture{}, err
	}
	fixtureRecord := FixtureRecord{
		Kind:         FixtureInstalledRuntime,
		Name:         "installed-runtime",
		Node:         factory.node.Name,
		Path:         manifestPath,
		DiskFormat:   string(format),
		Disk:         &disk,
		ESP:          &esp,
		NodeMetadata: metadata,
		Properties: map[string]string{
			"nodeName":   nodeName,
			"systemRole": string(systemRole),
		},
		Provenance: FixtureProvenance{Source: "installed-runtime-inputs"},
	}
	if err := factory.record(fixtureRecord); err != nil {
		return InstalledRuntimeFixture{}, err
	}
	return InstalledRuntimeFixture{
		ManifestPath: manifestPath,
		Disk:         disk.Path,
		DiskFormat:   format,
		ESPArtifacts: esp.Path,
		NodeMetadata: metadataPath,
		Record:       fixtureRecord,
	}, nil
}

func (factory NodeFixtureFactory) PublishInstalledRuntimeFromFirstInstall(sourceManifest string, format DiskFormat) (InstalledRuntimeFixture, error) {
	source, err := readInstalledRuntimeFixture(sourceManifest)
	if err != nil {
		return InstalledRuntimeFixture{}, err
	}
	if source == nil {
		return InstalledRuntimeFixture{}, errors.New("first-install installed runtime fixture manifest is required")
	}
	disk, err := fixtureRelativePath(sourceManifest, source.Disk.Path)
	if err != nil {
		return InstalledRuntimeFixture{}, err
	}
	esp, err := fixtureRelativePath(sourceManifest, source.ESPArtifacts.Path)
	if err != nil {
		return InstalledRuntimeFixture{}, err
	}
	metadata := ""
	if source.NodeMetadata != nil {
		metadata, err = fixtureRelativePath(sourceManifest, source.NodeMetadata.Path)
		if err != nil {
			return InstalledRuntimeFixture{}, err
		}
	}
	if format == "" {
		format = DiskFormat(source.Disk.Format)
	}
	if err := validateInstalledRuntimeFixture(sourceManifest, *source, InstalledRuntimeConfig{
		Disk:         disk,
		DiskFormat:   format,
		ESPArtifacts: esp,
	}, metadata); err != nil {
		return InstalledRuntimeFixture{}, err
	}
	fixture, err := factory.InstalledRuntime(InstalledRuntimeFixtureInput{
		Disk:         disk,
		DiskFormat:   format,
		ESPArtifacts: esp,
		NodeMetadata: metadata,
		NodeName:     first(source.NodeName, factory.node.Name),
		SystemRole:   NodeRole(first(source.SystemRole, string(factory.node.Role))),
	})
	if err != nil {
		return InstalledRuntimeFixture{}, err
	}
	fixture.Record.Kind = FixturePublishedFirstInstall
	fixture.Record.Provenance = FixtureProvenance{Source: "first-install", SourcePath: sourceManifest}
	if err := factory.replaceRecord(FixtureInstalledRuntime, fixture.Record); err != nil {
		return InstalledRuntimeFixture{}, err
	}
	return fixture, nil
}

func (factory NodeFixtureFactory) stageFileFixture(kind, name, source string) (FixtureRecord, error) {
	return factory.stageFile(kind, name, source, filepath.Join(factory.node.ManifestDir, name))
}

func (factory NodeFixtureFactory) stageFile(kind, name, source, dst string) (FixtureRecord, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return FixtureRecord{}, fmt.Errorf("%s source is required", kind)
	}
	src, err := cleanAbs(source)
	if err != nil {
		return FixtureRecord{}, err
	}
	info, err := os.Stat(src)
	if err != nil {
		return FixtureRecord{}, fmt.Errorf("stat %s source: %w", kind, err)
	}
	if !info.Mode().IsRegular() {
		return FixtureRecord{}, fmt.Errorf("%s source is not a regular file: %s", kind, src)
	}
	sha, err := fileSHA256(src)
	if err != nil {
		return FixtureRecord{}, fmt.Errorf("hash %s source: %w", kind, err)
	}
	dst, err = cleanAbs(dst)
	if err != nil {
		return FixtureRecord{}, err
	}
	if err := copyOrRejectStaleFile(src, dst, sha, info.Mode().Perm()); err != nil {
		return FixtureRecord{}, fmt.Errorf("stage %s: %w", kind, err)
	}
	record := FixtureRecord{
		Kind:      kind,
		Name:      name,
		Node:      factory.node.Name,
		Path:      dst,
		SHA256:    sha,
		SizeBytes: info.Size(),
		Provenance: FixtureProvenance{
			Source:       "file",
			SourcePath:   src,
			SourceSHA256: sha,
		},
	}
	if err := factory.record(record); err != nil {
		return FixtureRecord{}, err
	}
	return record, nil
}

func (factory NodeFixtureFactory) stageTreeFixture(kind, name, source, dst string) (FixtureRecord, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return FixtureRecord{}, fmt.Errorf("%s source is required", kind)
	}
	src, err := cleanAbs(source)
	if err != nil {
		return FixtureRecord{}, err
	}
	info, err := os.Stat(src)
	if err != nil {
		return FixtureRecord{}, fmt.Errorf("stat %s source: %w", kind, err)
	}
	if !info.IsDir() {
		return FixtureRecord{}, fmt.Errorf("%s source is not a directory: %s", kind, src)
	}
	sha, err := espTreeSHA256(src)
	if err != nil {
		return FixtureRecord{}, fmt.Errorf("hash %s source: %w", kind, err)
	}
	dst, err = cleanAbs(dst)
	if err != nil {
		return FixtureRecord{}, err
	}
	if err := copyOrRejectStaleTree(src, dst, sha); err != nil {
		return FixtureRecord{}, fmt.Errorf("stage %s: %w", kind, err)
	}
	record := FixtureRecord{
		Kind:       kind,
		Name:       name,
		Node:       factory.node.Name,
		Path:       dst,
		TreeSHA256: sha,
		Provenance: FixtureProvenance{
			Source:           "tree",
			SourcePath:       src,
			SourceTreeSHA256: sha,
		},
	}
	if err := factory.record(record); err != nil {
		return FixtureRecord{}, err
	}
	return record, nil
}

func (factory NodeFixtureFactory) record(record FixtureRecord) error {
	factory.scenario.Fixtures = append(factory.scenario.Fixtures, record)
	return factory.scenario.WriteManifest()
}

func (factory NodeFixtureFactory) replaceRecord(oldKind string, record FixtureRecord) error {
	replaced := false
	for i := range factory.scenario.Fixtures {
		if factory.scenario.Fixtures[i].Kind == oldKind && factory.scenario.Fixtures[i].Node == factory.node.Name {
			factory.scenario.Fixtures[i] = record
			replaced = true
			break
		}
	}
	if !replaced {
		factory.scenario.Fixtures = append(factory.scenario.Fixtures, record)
	}
	return factory.scenario.WriteManifest()
}

func copyOrRejectStaleFile(src, dst, sha string, mode os.FileMode) error {
	if existing, err := os.Stat(dst); err == nil {
		if !existing.Mode().IsRegular() {
			return fmt.Errorf("cached artifact is not a regular file: %s", dst)
		}
		got, err := fileSHA256(dst)
		if err != nil {
			return err
		}
		if got != sha {
			return fmt.Errorf("cached artifact %s digest %s does not match source %s", dst, got, sha)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if src == dst {
		return nil
	}
	if err := copyRequiredFile(src, dst, mode); err != nil {
		return err
	}
	got, err := fileSHA256(dst)
	if err != nil {
		return err
	}
	if got != sha {
		return fmt.Errorf("copied artifact %s digest %s does not match source %s", dst, got, sha)
	}
	return nil
}

func copyOrRejectStaleTree(src, dst, sha string) error {
	if existing, err := os.Stat(dst); err == nil {
		if !existing.IsDir() {
			return fmt.Errorf("cached artifact is not a directory: %s", dst)
		}
		got, err := espTreeSHA256(dst)
		if err != nil {
			return err
		}
		if got != sha {
			return fmt.Errorf("cached artifact %s tree digest %s does not match source %s", dst, got, sha)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if src == dst {
		return nil
	}
	if err := copyDir(src, dst); err != nil {
		return err
	}
	got, err := espTreeSHA256(dst)
	if err != nil {
		return err
	}
	if got != sha {
		return fmt.Errorf("copied artifact %s tree digest %s does not match source %s", dst, got, sha)
	}
	return nil
}

func validateNodeMetadata(path string, node Node) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var metadata struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Identity   struct {
			Hostname string `json:"hostname"`
		} `json:"identity"`
		SystemRole string `json:"systemRole"`
	}
	if err := json.Unmarshal(data, &metadata); err != nil {
		return fmt.Errorf("decode node metadata: %w", err)
	}
	if metadata.APIVersion != "katl.dev/v1alpha1" || metadata.Kind != "NodeMetadata" {
		return fmt.Errorf("node metadata apiVersion/kind is %s/%s, want katl.dev/v1alpha1/NodeMetadata", metadata.APIVersion, metadata.Kind)
	}
	if strings.TrimSpace(metadata.Identity.Hostname) != "" && metadata.Identity.Hostname != node.Name {
		return fmt.Errorf("node metadata hostname %q does not match node %q", metadata.Identity.Hostname, node.Name)
	}
	if strings.TrimSpace(metadata.SystemRole) == "" {
		return errors.New("node metadata systemRole is required")
	}
	if NodeRole(metadata.SystemRole) != node.Role {
		return fmt.Errorf("node metadata systemRole %q does not match node role %q", metadata.SystemRole, node.Role)
	}
	return nil
}

func relFrom(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(rel)
}
