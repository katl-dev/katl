package installer

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/zariel/katl/internal/installer/confext"
	"github.com/zariel/katl/internal/installer/configdomain"
	"github.com/zariel/katl/internal/installer/generation"
	"github.com/zariel/katl/internal/installer/kubeadmconfig"
	"github.com/zariel/katl/internal/installer/manifest"
)

const (
	generatedConfextName = "katl-node"
	generatedConfextID   = "fedora"
)

type InstallRecordRequest struct {
	TargetRoot        string
	Manifest          manifest.Manifest
	KubeadmConfigs    map[string]kubeadmconfig.Plan
	KubernetesVersion string
	Record            generation.Record
	Chown             func(path string, uid int, gid int) error
}

type InstallRecordResult struct {
	Tree         confext.GenerationTree
	Record       generation.Record
	MetadataPath string
}

func MaterializeInstallRecord(request InstallRecordRequest) (InstallRecordResult, error) {
	if strings.TrimSpace(request.TargetRoot) == "" {
		return InstallRecordResult{}, fmt.Errorf("target root is required")
	}
	generationID, err := cleanInstallGenerationID(request.Record.GenerationID)
	if err != nil {
		return InstallRecordResult{}, err
	}

	files, err := configdomain.NativeEtcFiles(configdomain.RenderRequest{
		Manifest:           request.Manifest,
		KubeadmConfigs:     request.KubeadmConfigs,
		KubernetesVersion:  firstNonEmpty(request.KubernetesVersion, selectedKubernetesPayloadVersion(request.Record)),
		DeferKubeadmInputs: true,
	})
	if err != nil {
		return InstallRecordResult{}, err
	}

	release, err := confextRelease(request.Record)
	if err != nil {
		return InstallRecordResult{}, err
	}
	generationsRoot := filepath.Join(filepath.Clean(request.TargetRoot), "var/lib/katl/generations")
	tree, err := confext.RenderGenerationTree(confext.GenerationTreeRequest{
		GenerationsRoot: generationsRoot,
		GenerationID:    generationID,
		Files:           files,
		Extension:       release,
		Chown:           request.Chown,
	})
	if err != nil {
		return InstallRecordResult{}, err
	}
	digest, err := generation.DigestDirectory(tree.ConfextDir)
	if err != nil {
		return InstallRecordResult{}, err
	}

	record := request.Record
	record.GenerationID = generationID
	record.Confexts = []generation.GeneratedConfext{{
		Name:           release.Name,
		Path:           generatedConfextRuntimePath(generationID),
		ActivationPath: "/run/confexts/" + release.Name,
		SHA256:         digest,
		Compatibility: generation.ConfextCompatibility{
			ID:           release.ID,
			VersionID:    release.VersionID,
			ConfextLevel: release.ConfextLevel,
		},
	}}
	if err := generation.ValidateRecord(record); err != nil {
		return InstallRecordResult{}, err
	}
	spec := generation.SpecFromRecord(record)
	status, err := generation.NewGenerationStatus(spec, generation.CommitStateCommitted, generation.BootStatePending, generation.HealthStateUnknown, record.CreatedAt)
	if err != nil {
		return InstallRecordResult{}, err
	}
	if err := generation.WriteGeneration(request.TargetRoot, spec, status); err != nil {
		return InstallRecordResult{}, err
	}
	metadataPath := filepath.Join(generationsRoot, generationID, "metadata.json")
	if err := generation.WriteRecord(metadataPath, record); err != nil {
		return InstallRecordResult{}, err
	}

	return InstallRecordResult{
		Tree:         tree,
		Record:       record,
		MetadataPath: metadataPath,
	}, nil
}

func confextRelease(record generation.Record) (confext.ExtensionRelease, error) {
	release := confext.ExtensionRelease{
		Name:         generatedConfextName,
		ID:           generatedConfextID,
		VersionID:    record.RuntimeVersion,
		ConfextLevel: 1,
	}
	if strings.TrimSpace(release.VersionID) == "" {
		return confext.ExtensionRelease{}, fmt.Errorf("runtime version is required for generated confext metadata")
	}
	return release, nil
}

func cleanInstallGenerationID(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("generation id is required")
	}
	if strings.TrimSpace(value) != value {
		return "", fmt.Errorf("generation id %q must not contain leading or trailing whitespace", value)
	}
	if strings.ContainsAny(value, `/\`) || value == "." || value == ".." || filepath.Clean(value) != value {
		return "", fmt.Errorf("generation id %q must be a single path segment", value)
	}
	return value, nil
}

func generatedConfextRuntimePath(generationID string) string {
	return filepath.Join("/var/lib/katl/generations", generationID, "confext")
}

func selectedKubernetesPayloadVersion(record generation.Record) string {
	for _, sysext := range record.Sysexts {
		if sysext.Name == "kubernetes" {
			return sysext.PayloadVersion
		}
	}
	return ""
}

func selectedKubernetesActivationPath(record generation.Record) string {
	for _, sysext := range record.Sysexts {
		if sysext.Name == "kubernetes" {
			return sysext.ActivationPath
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
