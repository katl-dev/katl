package generation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/installer/kernelcmdline"
)

const (
	APIVersion           = "katl.dev/v1alpha1"
	GeneratedConfextName = "zzzz-katl-node"
	Kind                 = "GenerationRecord"
)

func IsGeneratedConfextName(name string) bool {
	name = strings.TrimSpace(name)
	return name == GeneratedConfextName || name == "katl-node"
}

type Record struct {
	APIVersion                  string             `json:"apiVersion"`
	Kind                        string             `json:"kind"`
	GenerationID                string             `json:"generationID"`
	RuntimeVersion              string             `json:"runtimeVersion"`
	PreviousGenerationID        string             `json:"previousGenerationID,omitempty"`
	Root                        RootSelection      `json:"root"`
	Boot                        BootSelection      `json:"boot"`
	Sysexts                     []ExtensionRef     `json:"sysexts"`
	BundledConfexts             []ExtensionRef     `json:"bundledConfexts,omitempty"`
	Confexts                    []GeneratedConfext `json:"confexts"`
	KernelCommandLine           []string           `json:"kernelCommandLine"`
	ConfiguredKernelCommandLine []string           `json:"configuredKernelCommandLine,omitempty"`
	ConfigApply                 *ConfigApplyRecord `json:"configApply,omitempty"`
	KubernetesUpgrade           *KubernetesUpgrade `json:"kubernetesUpgrade,omitempty"`
	CreatedAt                   time.Time          `json:"createdAt"`
	BootState                   string             `json:"bootState"`
	HealthState                 string             `json:"healthState"`
}

type KubernetesUpgrade struct {
	OperationID             string `json:"operationID"`
	TargetKubeadmAccessMode string `json:"targetKubeadmAccessMode"`
	KubeletActivationGate   string `json:"kubeletActivationGate"`
}

type RootSelection struct {
	Slot                  string `json:"slot"`
	PartitionUUID         string `json:"partitionUUID"`
	RuntimeVersion        string `json:"runtimeVersion"`
	RuntimeInterface      string `json:"runtimeInterface"`
	Architecture          string `json:"architecture"`
	RuntimeArtifactSHA256 string `json:"runtimeArtifactSHA256"`
}

type BootSelection struct {
	UKIPath         string `json:"ukiPath"`
	LoaderEntryPath string `json:"loaderEntryPath,omitempty"`
}

type ExtensionRef struct {
	Name            string                 `json:"name"`
	Path            string                 `json:"path"`
	ActivationPath  string                 `json:"activationPath"`
	SHA256          string                 `json:"sha256"`
	ArtifactVersion string                 `json:"artifactVersion"`
	PayloadVersion  string                 `json:"payloadVersion"`
	Architecture    string                 `json:"architecture"`
	Compatibility   ExtensionCompatibility `json:"compatibility"`
}

type ExtensionCompatibility struct {
	RuntimeInterfaces []string `json:"runtimeInterfaces"`
}

type GeneratedConfext struct {
	Name           string               `json:"name"`
	Path           string               `json:"path"`
	ActivationPath string               `json:"activationPath"`
	SHA256         string               `json:"sha256"`
	Compatibility  ConfextCompatibility `json:"compatibility"`
}

type ConfextCompatibility struct {
	ID           string `json:"id"`
	VersionID    string `json:"versionID"`
	ConfextLevel int    `json:"confextLevel"`
}

type FirstInstallRequest struct {
	GenerationID                string
	RuntimeVersion              string
	RuntimeInterface            string
	RuntimeArchitecture         string
	RootSlot                    string
	RootPartitionUUID           string
	RuntimeArtifactSHA256       string
	UKIPath                     string
	Sysexts                     []ExtensionRef
	BundledConfexts             []ExtensionRef
	GeneratedConfext            GeneratedConfext
	KernelCommandLine           []string
	ConfiguredKernelCommandLine []string
	CreatedAt                   time.Time
}

type RuntimeConfigRequest struct {
	GenerationID                   string
	Previous                       Record
	SourceDigest                   string
	Sysexts                        []ExtensionRef
	BundledConfexts                []ExtensionRef
	GeneratedConfext               GeneratedConfext
	ChangedDomains                 []string
	RequestedApplyMode             string
	AcceptedApplyMode              string
	Kubeadm                        KubeadmActionRequired
	CreatedAt                      time.Time
	KernelCommandLine              []string
	ConfiguredKernelCommandLine    []string
	ConfiguredKernelCommandLineSet bool
}

type ConfigApplyRecord struct {
	SourceDigest       string                `json:"sourceDigest"`
	ChangedDomains     []string              `json:"changedDomains"`
	RequestedApplyMode string                `json:"requestedApplyMode"`
	AcceptedApplyMode  string                `json:"acceptedApplyMode"`
	PreviousGeneration string                `json:"previousGenerationID"`
	Kubeadm            KubeadmActionRequired `json:"kubeadm"`
}

func NewFirstInstallRecord(request FirstInstallRequest) (Record, error) {
	if strings.TrimSpace(request.GenerationID) == "" {
		return Record{}, fmt.Errorf("generation id is required")
	}
	if strings.TrimSpace(request.RuntimeVersion) == "" {
		return Record{}, fmt.Errorf("runtime version is required")
	}
	if strings.TrimSpace(request.RuntimeInterface) == "" {
		return Record{}, fmt.Errorf("runtime interface is required")
	}
	if strings.TrimSpace(request.RuntimeArchitecture) == "" {
		return Record{}, fmt.Errorf("runtime architecture is required")
	}
	if strings.TrimSpace(request.RootSlot) == "" {
		return Record{}, fmt.Errorf("root slot is required")
	}
	if strings.TrimSpace(request.RootPartitionUUID) == "" {
		return Record{}, fmt.Errorf("root partition UUID is required")
	}
	if err := validateSHA256("runtime artifact", request.RuntimeArtifactSHA256); err != nil {
		return Record{}, err
	}
	if strings.TrimSpace(request.UKIPath) == "" {
		return Record{}, fmt.Errorf("UKI path is required")
	}
	sysexts, err := cleanExts(request.Sysexts)
	if err != nil {
		return Record{}, err
	}
	bundledConfexts, err := cleanExtsNamed("bundled confext", request.BundledConfexts)
	if err != nil {
		return Record{}, err
	}
	confext, err := normalizeGeneratedConfext(request.GeneratedConfext)
	if err != nil {
		return Record{}, err
	}

	createdAt := request.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}

	record := Record{
		APIVersion:     APIVersion,
		Kind:           Kind,
		GenerationID:   request.GenerationID,
		RuntimeVersion: request.RuntimeVersion,
		Root: RootSelection{
			Slot:                  request.RootSlot,
			PartitionUUID:         request.RootPartitionUUID,
			RuntimeVersion:        request.RuntimeVersion,
			RuntimeInterface:      request.RuntimeInterface,
			Architecture:          request.RuntimeArchitecture,
			RuntimeArtifactSHA256: strings.ToLower(request.RuntimeArtifactSHA256),
		},
		Boot:                        BootSelection{UKIPath: request.UKIPath},
		Sysexts:                     sysexts,
		BundledConfexts:             bundledConfexts,
		Confexts:                    []GeneratedConfext{confext},
		KernelCommandLine:           slices.Clone(request.KernelCommandLine),
		ConfiguredKernelCommandLine: slices.Clone(request.ConfiguredKernelCommandLine),
		CreatedAt:                   createdAt.UTC(),
		BootState:                   "pending",
		HealthState:                 "unknown",
	}
	if err := ValidateRecord(record); err != nil {
		return Record{}, err
	}
	return record, nil
}

func NewRuntimeConfigRecord(request RuntimeConfigRequest) (Record, error) {
	if err := ValidateRecord(request.Previous); err != nil {
		return Record{}, fmt.Errorf("previous generation metadata is invalid: %w", err)
	}
	if strings.TrimSpace(request.Previous.GenerationID) == "" {
		return Record{}, fmt.Errorf("previous generation id is required")
	}
	if strings.TrimSpace(request.GenerationID) == "" {
		return Record{}, fmt.Errorf("generation id is required")
	}
	generationID, err := cleanSegment("generation id", request.GenerationID)
	if err != nil {
		return Record{}, err
	}
	if generationID == request.Previous.GenerationID {
		return Record{}, fmt.Errorf("runtime configuration generation must differ from previous generation")
	}
	if err := validateSHA256("configuration source", request.SourceDigest); err != nil {
		return Record{}, err
	}
	if err := validateRequestedApplyMode("requested apply mode", request.RequestedApplyMode); err != nil {
		return Record{}, err
	}
	if strings.TrimSpace(request.AcceptedApplyMode) == "" {
		request.AcceptedApplyMode = request.RequestedApplyMode
	}
	if err := validateAcceptedApplyMode("accepted apply mode", request.AcceptedApplyMode); err != nil {
		return Record{}, err
	}
	domains, err := cleanChangedDomains(request.ChangedDomains)
	if err != nil {
		return Record{}, err
	}
	sysexts := request.Sysexts
	if len(sysexts) == 0 {
		sysexts = request.Previous.Sysexts
	}
	sysexts, err = cleanExts(sysexts)
	if err != nil {
		return Record{}, err
	}
	bundledConfexts := request.BundledConfexts
	if len(bundledConfexts) == 0 {
		bundledConfexts = request.Previous.BundledConfexts
	}
	bundledConfexts, err = cleanExtsNamed("bundled confext", bundledConfexts)
	if err != nil {
		return Record{}, err
	}
	confext, err := normalizeGeneratedConfext(request.GeneratedConfext)
	if err != nil {
		return Record{}, err
	}
	createdAt := request.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}

	record := Record{
		APIVersion:           APIVersion,
		Kind:                 Kind,
		GenerationID:         generationID,
		RuntimeVersion:       request.Previous.RuntimeVersion,
		PreviousGenerationID: strings.TrimSpace(request.Previous.GenerationID),
		Root:                 request.Previous.Root,
		Boot: BootSelection{
			UKIPath:         request.Previous.Boot.UKIPath,
			LoaderEntryPath: filepath.ToSlash(filepath.Join("loader/entries", "katl-"+generationID+".conf")),
		},
		Sysexts:                     sysexts,
		BundledConfexts:             bundledConfexts,
		Confexts:                    []GeneratedConfext{confext},
		KernelCommandLine:           runtimeKernelCommandLine(request),
		ConfiguredKernelCommandLine: runtimeConfiguredKernelCommandLine(request),
		ConfigApply: &ConfigApplyRecord{
			SourceDigest:       strings.ToLower(request.SourceDigest),
			ChangedDomains:     domains,
			RequestedApplyMode: strings.TrimSpace(request.RequestedApplyMode),
			AcceptedApplyMode:  strings.TrimSpace(request.AcceptedApplyMode),
			PreviousGeneration: strings.TrimSpace(request.Previous.GenerationID),
			Kubeadm:            redactKubeadmActionRequired(request.Kubeadm),
		},
		CreatedAt:   createdAt.UTC(),
		BootState:   "pending",
		HealthState: "unknown",
	}
	if err := ValidateRecord(record); err != nil {
		return Record{}, err
	}
	return record, nil
}

func runtimeKernelCommandLine(request RuntimeConfigRequest) []string {
	if request.KernelCommandLine != nil {
		return slices.Clone(request.KernelCommandLine)
	}
	return slices.Clone(request.Previous.KernelCommandLine)
}

func runtimeConfiguredKernelCommandLine(request RuntimeConfigRequest) []string {
	if request.ConfiguredKernelCommandLineSet {
		return slices.Clone(request.ConfiguredKernelCommandLine)
	}
	return slices.Clone(request.Previous.ConfiguredKernelCommandLine)
}

func MarshalRecord(record Record) ([]byte, error) {
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal generation record: %w", err)
	}
	return append(data, '\n'), nil
}

func WriteRecord(path string, record Record) error {
	data, err := MarshalRecord(record)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create generation metadata directory: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write generation metadata: %w", err)
	}
	return nil
}

func DigestDirectory(root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", fmt.Errorf("digest root is required")
	}
	var entries []string
	if err := filepath.WalkDir(root, func(path string, dirent fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if dirent.IsDir() {
			return nil
		}
		info, err := dirent.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() && info.Mode()&fs.ModeSymlink == 0 {
			return fmt.Errorf("digest input %s is neither a regular file nor a symbolic link", path)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		entries = append(entries, filepath.ToSlash(rel))
		return nil
	}); err != nil {
		return "", fmt.Errorf("walk digest root: %w", err)
	}
	sort.Strings(entries)

	hash := sha256.New()
	for _, rel := range entries {
		path := filepath.Join(root, filepath.FromSlash(rel))
		info, err := os.Lstat(path)
		if err != nil {
			return "", err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return "", err
			}
			fmt.Fprintf(hash, "path=%s symlink-target=%q\n", rel, target)
			continue
		}
		fmt.Fprintf(hash, "path=%s mode=%04o\n", rel, info.Mode().Perm())
		file, err := os.Open(path)
		if err != nil {
			return "", err
		}
		if _, err := io.Copy(hash, file); err != nil {
			_ = file.Close()
			return "", err
		}
		if err := file.Close(); err != nil {
			return "", err
		}
		fmt.Fprintln(hash)
	}

	return hex.EncodeToString(hash.Sum(nil)), nil
}

func normalizeGeneratedConfext(confext GeneratedConfext) (GeneratedConfext, error) {
	if strings.TrimSpace(confext.Name) == "" {
		confext.Name = GeneratedConfextName
	}
	if strings.TrimSpace(confext.Path) == "" {
		return GeneratedConfext{}, fmt.Errorf("generated confext path is required")
	}
	if strings.TrimSpace(confext.ActivationPath) == "" {
		confext.ActivationPath = "/run/confexts/" + confext.Name
	}
	if err := validateSHA256("generated confext", confext.SHA256); err != nil {
		return GeneratedConfext{}, err
	}
	if strings.TrimSpace(confext.Compatibility.ID) == "" || strings.TrimSpace(confext.Compatibility.VersionID) == "" || confext.Compatibility.ConfextLevel < 1 {
		return GeneratedConfext{}, fmt.Errorf("generated confext compatibility metadata is required")
	}
	confext.SHA256 = strings.ToLower(confext.SHA256)
	return confext, nil
}

func ValidatePair(root RootSelection, sysext ExtensionRef) error {
	if strings.TrimSpace(root.RuntimeInterface) == "" {
		return fmt.Errorf("runtime interface is required")
	}
	if strings.TrimSpace(root.Architecture) == "" {
		return fmt.Errorf("runtime architecture is required")
	}
	if strings.TrimSpace(sysext.Name) == "" {
		return fmt.Errorf("sysext name is required")
	}
	if sysext.Architecture != root.Architecture {
		return fmt.Errorf("sysext %q architecture %q is incompatible with runtime architecture %q", sysext.Name, sysext.Architecture, root.Architecture)
	}
	for _, candidate := range sysext.Compatibility.RuntimeInterfaces {
		if candidate == root.RuntimeInterface {
			return nil
		}
	}
	return fmt.Errorf("sysext %q does not support runtime interface %q", sysext.Name, root.RuntimeInterface)
}

func ValidateRecord(record Record) error {
	if err := kernelcmdline.ValidateEffective(record.ConfiguredKernelCommandLine, record.KernelCommandLine); err != nil {
		return fmt.Errorf("configured kernel command line: %w", err)
	}
	for _, sysext := range record.Sysexts {
		if err := ValidatePair(record.Root, sysext); err != nil {
			return err
		}
	}
	for _, confext := range record.BundledConfexts {
		if err := ValidatePair(record.Root, confext); err != nil {
			return fmt.Errorf("bundled confext: %w", err)
		}
	}
	if record.ConfigApply != nil {
		if err := validateConfigApplyRecord(*record.ConfigApply); err != nil {
			return err
		}
	}
	return nil
}

func validateConfigApplyRecord(config ConfigApplyRecord) error {
	if err := validateSHA256("configuration source", config.SourceDigest); err != nil {
		return err
	}
	if _, err := cleanChangedDomains(config.ChangedDomains); err != nil {
		return fmt.Errorf("config apply metadata: %w", err)
	}
	if err := validateRequestedApplyMode("requested apply mode", config.RequestedApplyMode); err != nil {
		return err
	}
	if err := validateAcceptedApplyMode("accepted apply mode", config.AcceptedApplyMode); err != nil {
		return err
	}
	if strings.TrimSpace(config.PreviousGeneration) == "" {
		return fmt.Errorf("config apply metadata previous generation id is required")
	}
	return nil
}

func cleanExts(refs []ExtensionRef) ([]ExtensionRef, error) {
	return cleanExtsNamed("sysext", refs)
}

func cleanExtsNamed(kind string, refs []ExtensionRef) ([]ExtensionRef, error) {
	cleaned := make([]ExtensionRef, 0, len(refs))
	seen := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		if strings.TrimSpace(ref.Name) == "" || strings.TrimSpace(ref.Path) == "" || strings.TrimSpace(ref.ActivationPath) == "" {
			return nil, fmt.Errorf("%s name, path, and activation path are required", kind)
		}
		if _, ok := seen[ref.Name]; ok {
			return nil, fmt.Errorf("duplicate %s %q", kind, ref.Name)
		}
		seen[ref.Name] = struct{}{}
		if err := validateSHA256(kind+" "+ref.Name, ref.SHA256); err != nil {
			return nil, err
		}
		if strings.TrimSpace(ref.ArtifactVersion) == "" || strings.TrimSpace(ref.PayloadVersion) == "" || strings.TrimSpace(ref.Architecture) == "" {
			return nil, fmt.Errorf("%s %q version and architecture metadata is required", kind, ref.Name)
		}
		if len(ref.Compatibility.RuntimeInterfaces) == 0 {
			return nil, fmt.Errorf("%s %q runtime compatibility metadata is required", kind, ref.Name)
		}
		ref.SHA256 = strings.ToLower(ref.SHA256)
		cleaned = append(cleaned, ref)
	}
	return cleaned, nil
}

func validateSHA256(name string, value string) error {
	if len(value) != sha256.Size*2 {
		return fmt.Errorf("%s SHA-256 must be %d lowercase hex characters", name, sha256.Size*2)
	}
	if value != strings.ToLower(value) {
		return fmt.Errorf("%s SHA-256 must be lowercase hex", name)
	}
	if _, err := hex.DecodeString(value); err != nil {
		return fmt.Errorf("%s SHA-256 is invalid: %w", name, err)
	}
	return nil
}
