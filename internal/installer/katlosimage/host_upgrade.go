package katlosimage

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/installer/generation"
	"github.com/katl-dev/katl/internal/installer/kernelcmdline"
)

type HostUpgradeRequest struct {
	GenerationID         string
	PreviousSpec         generation.GenerationSpec
	PreviousStatus       generation.GenerationStatus
	RootSlot             string
	RootPartitionUUID    string
	UKIPath              string
	LoaderEntryPath      string
	OperationID          string
	BootCountedTrialPath string
	Bootstrapped         bool
	CreatedAt            time.Time
}

type HostUpgradePlan struct {
	Spec            generation.GenerationSpec
	Status          generation.GenerationStatus
	BootSelection   generation.BootSelectionRecord
	PreservedAssets []PreservedAsset
	BundledAssets   []BundledAsset
}

type PreservedAsset struct {
	Kind       string
	Name       string
	SourcePath string
	TargetPath string
	Directory  bool
	SHA256     string
}

type BundledAsset struct {
	Kind       string
	Name       string
	SourcePath string
	TargetPath string
	SHA256     string
}

func (p Payload) HostUpgradePlan(request HostUpgradeRequest) (HostUpgradePlan, error) {
	if p.Index.ImageRole != RoleUpgrade {
		return HostUpgradePlan{}, fmt.Errorf("KatlOS image role must be %s", RoleUpgrade)
	}
	if err := ValidateHostUpgradeSource(request.PreviousSpec, request.PreviousStatus, request.Bootstrapped); err != nil {
		return HostUpgradePlan{}, err
	}
	generationID := strings.TrimSpace(request.GenerationID)
	if generationID == "" {
		return HostUpgradePlan{}, fmt.Errorf("generation id is required")
	}
	if generationID == request.PreviousSpec.GenerationID {
		return HostUpgradePlan{}, fmt.Errorf("host upgrade generation must differ from previous generation")
	}
	rootSlot := strings.TrimSpace(request.RootSlot)
	if rootSlot == "" {
		return HostUpgradePlan{}, fmt.Errorf("root slot is required")
	}
	if rootSlot == request.PreviousSpec.Root.Slot {
		return HostUpgradePlan{}, fmt.Errorf("host upgrade must target the inactive root slot")
	}
	if strings.TrimSpace(request.RootPartitionUUID) == "" {
		return HostUpgradePlan{}, fmt.Errorf("root partition UUID is required")
	}
	if strings.TrimSpace(request.UKIPath) == "" {
		return HostUpgradePlan{}, fmt.Errorf("UKI path is required")
	}
	if strings.TrimSpace(request.LoaderEntryPath) == "" {
		return HostUpgradePlan{}, fmt.Errorf("loader entry path is required")
	}
	if strings.TrimSpace(request.OperationID) == "" {
		return HostUpgradePlan{}, fmt.Errorf("operation id is required")
	}
	if p.Index.Architecture != request.PreviousSpec.Root.Architecture {
		return HostUpgradePlan{}, fmt.Errorf("KatlOS image architecture %q does not match current runtime architecture %q", p.Index.Architecture, request.PreviousSpec.Root.Architecture)
	}
	if p.Index.RuntimeInterface != request.PreviousSpec.Root.RuntimeInterface {
		return HostUpgradePlan{}, fmt.Errorf("KatlOS image runtime interface %q does not match current runtime interface %q", p.Index.RuntimeInterface, request.PreviousSpec.Root.RuntimeInterface)
	}
	if err := kernelcmdline.ValidateRequiredCompatibility(request.PreviousSpec.ConfiguredKernelCommandLine, p.Boot.Compatibility.KernelCommandLine); err != nil {
		return HostUpgradePlan{}, fmt.Errorf("host upgrade kernel command line: %w", err)
	}

	createdAt := request.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	root := generation.RootSelection{
		Slot:                  rootSlot,
		PartitionUUID:         strings.TrimSpace(request.RootPartitionUUID),
		RuntimeVersion:        p.Index.Version,
		RuntimeInterface:      p.Index.RuntimeInterface,
		Architecture:          p.Index.Architecture,
		RuntimeArtifactSHA256: p.Runtime.SHA256,
	}
	sysexts, sysextAssets, bundledAssets, err := upgradeSysexts(p, request.PreviousSpec, generationID, root, request.Bootstrapped)
	if err != nil {
		return HostUpgradePlan{}, err
	}
	confexts, confextAssets, err := rehomeConfexts(request.PreviousSpec, generationID)
	if err != nil {
		return HostUpgradePlan{}, err
	}
	bundledConfexts, bundledConfextAssets, err := rehomeBundledConfexts(request.PreviousSpec, generationID, root)
	if err != nil {
		return HostUpgradePlan{}, err
	}
	spec := generation.GenerationSpec{
		APIVersion:           generation.APIVersion,
		Kind:                 generation.SpecKind,
		GenerationID:         generationID,
		RuntimeVersion:       root.RuntimeVersion,
		PreviousGenerationID: strings.TrimSpace(request.PreviousSpec.GenerationID),
		Root:                 root,
		Boot: generation.BootSelection{
			UKIPath:         strings.TrimSpace(request.UKIPath),
			LoaderEntryPath: strings.TrimSpace(request.LoaderEntryPath),
		},
		Sysexts:                     sysexts,
		BundledConfexts:             bundledConfexts,
		Confexts:                    confexts,
		KernelCommandLine:           mergeKernelCommandLine(request.PreviousSpec.KernelCommandLine, p.Boot.Compatibility.KernelCommandLine),
		ConfiguredKernelCommandLine: slices.Clone(request.PreviousSpec.ConfiguredKernelCommandLine),
		CreatedAt:                   createdAt.UTC(),
	}
	status, err := generation.NewGenerationStatus(spec, generation.CommitStateCandidate, generation.BootStatePending, generation.HealthStateUnknown, createdAt)
	if err != nil {
		return HostUpgradePlan{}, err
	}
	selection := generation.BootSelectionRecord{
		APIVersion:                    generation.APIVersion,
		Kind:                          generation.BootSelectionKind,
		DefaultGenerationID:           request.PreviousSpec.GenerationID,
		TargetBootGenerationID:        generationID,
		TrialGenerationID:             generationID,
		PreviousKnownGoodGenerationID: request.PreviousSpec.GenerationID,
		DefaultBootEntry:              strings.TrimSpace(request.PreviousSpec.Boot.LoaderEntryPath),
		TargetBootEntry:               strings.TrimSpace(request.LoaderEntryPath),
		TrialBootEntry:                strings.TrimSpace(request.LoaderEntryPath),
		PreviousKnownGoodBootEntry:    strings.TrimSpace(request.PreviousSpec.Boot.LoaderEntryPath),
		BootCountedTrialPath:          strings.TrimSpace(request.BootCountedTrialPath),
		PendingTransactionID:          strings.TrimSpace(request.OperationID),
		PendingHealthValidation:       true,
		PersistentDefaultPromotion:    generation.DefaultPromotionPending,
		UpdatedAt:                     createdAt.UTC(),
	}
	if err := generation.ValidateBootSelection(selection); err != nil {
		return HostUpgradePlan{}, err
	}
	return HostUpgradePlan{
		Spec:            spec,
		Status:          status,
		BootSelection:   selection,
		PreservedAssets: append(append(sysextAssets, bundledConfextAssets...), confextAssets...),
		BundledAssets:   bundledAssets,
	}, nil
}

func rehomeBundledConfexts(previous generation.GenerationSpec, generationID string, root generation.RootSelection) ([]generation.ExtensionRef, []PreservedAsset, error) {
	refs := make([]generation.ExtensionRef, 0, len(previous.BundledConfexts))
	assets := make([]PreservedAsset, 0, len(previous.BundledConfexts))
	for _, ref := range previous.BundledConfexts {
		if err := generation.ValidatePair(root, ref); err != nil {
			return nil, nil, fmt.Errorf("preserved bundled confext %q is incompatible with upgraded runtime: %w", ref.Name, err)
		}
		nextPath, err := rehomeGenerationPath(previous.GenerationID, generationID, ref.Path, "bundled-confext")
		if err != nil {
			return nil, nil, fmt.Errorf("preserve bundled confext %q: %w", ref.Name, err)
		}
		assets = append(assets, PreservedAsset{Kind: "bundled-confext", Name: ref.Name, SourcePath: ref.Path, TargetPath: nextPath, SHA256: ref.SHA256})
		ref.Path = nextPath
		refs = append(refs, ref)
	}
	return refs, assets, nil
}

func ValidateHostUpgradeSource(previousSpec generation.GenerationSpec, previousStatus generation.GenerationStatus, bootstrapped bool) error {
	if err := generation.ValidateGenerationSpec(previousSpec); err != nil {
		return fmt.Errorf("previous generation spec is invalid: %w", err)
	}
	if err := generation.ValidateGenerationStatus(previousSpec, previousStatus); err != nil {
		return fmt.Errorf("previous generation status is invalid: %w", err)
	}
	if !generation.IsKnownGood(previousStatus) {
		return fmt.Errorf("previous generation %q is not known-good", previousSpec.GenerationID)
	}
	if bootstrapped {
		if _, ok := selectedKubernetes(previousSpec.Sysexts); !ok {
			return fmt.Errorf("bootstrapped node current generation has no Kubernetes sysext to preserve")
		}
	}
	return nil
}

func mergeKernelCommandLine(previous, required []string) []string {
	merged := make([]string, 0, len(previous)+len(required))
	seen := make(map[string]bool, len(previous)+len(required))
	for _, options := range [][]string{previous, required} {
		for _, option := range options {
			option = strings.TrimSpace(option)
			if option == "" || seen[option] {
				continue
			}
			seen[option] = true
			merged = append(merged, option)
		}
	}
	return merged
}

func StagePreservedAssets(root string, plan HostUpgradePlan) error {
	for _, asset := range plan.PreservedAssets {
		source, err := rootedPath(root, asset.SourcePath)
		if err != nil {
			return err
		}
		target, err := rootedPath(root, asset.TargetPath)
		if err != nil {
			return err
		}
		if err := os.RemoveAll(target); err != nil {
			return fmt.Errorf("clear preserved %s %q target: %w", asset.Kind, asset.Name, err)
		}
		if asset.Directory {
			if err := copyDirectory(source, target); err != nil {
				return fmt.Errorf("stage preserved %s %q: %w", asset.Kind, asset.Name, err)
			}
			got, err := generation.DigestDirectory(target)
			if err != nil {
				return fmt.Errorf("digest staged preserved %s %q: %w", asset.Kind, asset.Name, err)
			}
			if got != asset.SHA256 {
				return fmt.Errorf("staged preserved %s %q SHA-256 mismatch", asset.Kind, asset.Name)
			}
			continue
		}
		if err := copyFile(source, target); err != nil {
			return fmt.Errorf("stage preserved %s %q: %w", asset.Kind, asset.Name, err)
		}
		if err := verifyFileSHA256(target, asset.SHA256); err != nil {
			return fmt.Errorf("verify staged preserved %s %q: %w", asset.Kind, asset.Name, err)
		}
	}
	return nil
}

func StageBundledAssets(root string, plan HostUpgradePlan) error {
	for _, asset := range plan.BundledAssets {
		if !filepath.IsAbs(asset.SourcePath) {
			return fmt.Errorf("bundled %s %q source path must be absolute", asset.Kind, asset.Name)
		}
		target, err := rootedPath(root, asset.TargetPath)
		if err != nil {
			return err
		}
		if err := os.RemoveAll(target); err != nil {
			return fmt.Errorf("clear bundled %s %q target: %w", asset.Kind, asset.Name, err)
		}
		if err := copyFile(filepath.Clean(asset.SourcePath), target); err != nil {
			return fmt.Errorf("stage bundled %s %q: %w", asset.Kind, asset.Name, err)
		}
		if err := verifyFileSHA256(target, asset.SHA256); err != nil {
			return fmt.Errorf("verify bundled %s %q: %w", asset.Kind, asset.Name, err)
		}
	}
	return nil
}

func upgradeSysexts(payload Payload, previous generation.GenerationSpec, generationID string, root generation.RootSelection, bootstrapped bool) ([]generation.ExtensionRef, []PreservedAsset, []BundledAsset, error) {
	previousKubernetes, hasPreviousKubernetes := selectedKubernetes(previous.Sysexts)
	if bootstrapped && !hasPreviousKubernetes {
		return nil, nil, nil, fmt.Errorf("bootstrapped node current generation has no Kubernetes sysext to preserve")
	}
	if hasPreviousKubernetes {
		if err := generation.ValidatePair(root, previousKubernetes); err != nil {
			return nil, nil, nil, fmt.Errorf("preserved Kubernetes sysext is incompatible with upgraded runtime: %w", err)
		}
	}

	refs := make([]generation.ExtensionRef, 0, len(previous.Sysexts))
	assets := make([]PreservedAsset, 0, len(previous.Sysexts))
	bundled := make([]BundledAsset, 0, 1)
	for _, ref := range previous.Sysexts {
		if ref.Name == EndpointAdvertiserName {
			if len(bundled) != 0 {
				return nil, nil, nil, fmt.Errorf("current generation contains multiple %s sysexts", EndpointAdvertiserName)
			}
			target := filepath.Join(generation.GenerationRecordsDir, generationID, "sysext", EndpointAdvertiserName+".raw")
			replacement, err := payload.EndpointAdvertiserExtensionRef(target)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("replace bundled %s sysext: %w", EndpointAdvertiserName, err)
			}
			if err := generation.ValidatePair(root, replacement); err != nil {
				return nil, nil, nil, fmt.Errorf("bundled %s sysext is incompatible with upgraded runtime: %w", EndpointAdvertiserName, err)
			}
			refs = append(refs, replacement)
			bundled = append(bundled, BundledAsset{
				Kind:       "sysext",
				Name:       EndpointAdvertiserName,
				SourcePath: payload.ComponentPath(payload.EndpointAdvertiser),
				TargetPath: replacement.Path,
				SHA256:     replacement.SHA256,
			})
			continue
		}
		path, err := rehomeGenerationPath(previous.GenerationID, generationID, ref.Path, "sysext")
		if err != nil {
			return nil, nil, nil, fmt.Errorf("preserve sysext %q: %w", ref.Name, err)
		}
		assets = append(assets, PreservedAsset{Kind: "sysext", Name: ref.Name, SourcePath: ref.Path, TargetPath: path, SHA256: ref.SHA256})
		ref.Path = path
		refs = append(refs, ref)
	}
	return refs, assets, bundled, nil
}

func rehomeConfexts(previous generation.GenerationSpec, generationID string) ([]generation.GeneratedConfext, []PreservedAsset, error) {
	refs := make([]generation.GeneratedConfext, 0, len(previous.Confexts))
	assets := make([]PreservedAsset, 0, len(previous.Confexts))
	for _, ref := range previous.Confexts {
		path, err := rehomeGenerationPath(previous.GenerationID, generationID, ref.Path, "confext")
		if err != nil {
			return nil, nil, fmt.Errorf("preserve confext %q: %w", ref.Name, err)
		}
		assets = append(assets, PreservedAsset{Kind: "confext", Name: ref.Name, SourcePath: ref.Path, TargetPath: path, Directory: true, SHA256: ref.SHA256})
		ref.Path = path
		refs = append(refs, ref)
	}
	return refs, assets, nil
}

func rehomeGenerationPath(previousID string, generationID string, value string, kind string) (string, error) {
	value = "/" + strings.TrimPrefix(strings.TrimSpace(value), "/")
	previousPrefix := generation.GenerationRecordsDir + "/" + previousID + "/" + kind
	nextPrefix := generation.GenerationRecordsDir + "/" + generationID + "/" + kind
	if value == previousPrefix {
		return nextPrefix, nil
	}
	if strings.HasPrefix(value, previousPrefix+"/") {
		return nextPrefix + strings.TrimPrefix(value, previousPrefix), nil
	}
	base := strings.TrimSpace(value)
	if base == "" || base == "/" {
		return "", fmt.Errorf("path is required")
	}
	name := base[strings.LastIndex(base, "/")+1:]
	if name == "" || name == "." || name == ".." {
		return "", fmt.Errorf("path %q does not name an artifact", value)
	}
	return nextPrefix + "/" + name, nil
}

func selectedKubernetes(refs []generation.ExtensionRef) (generation.ExtensionRef, bool) {
	for _, ref := range refs {
		if ref.Name == "kubernetes" {
			return ref, true
		}
	}
	return generation.ExtensionRef{}, false
}

func rootedPath(root string, absolutePath string) (string, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return "", fmt.Errorf("target root is required")
	}
	absolutePath = filepath.ToSlash(filepath.Clean("/" + strings.TrimPrefix(strings.TrimSpace(absolutePath), "/")))
	if absolutePath == "/" {
		return filepath.Clean(root), nil
	}
	return filepath.Join(filepath.Clean(root), filepath.FromSlash(strings.TrimPrefix(absolutePath, "/"))), nil
}

func copyDirectory(source string, target string) error {
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", source)
	}
	return filepath.WalkDir(source, func(path string, dirent os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		targetPath := filepath.Join(target, rel)
		info, err := dirent.Info()
		if err != nil {
			return err
		}
		if dirent.IsDir() {
			return os.MkdirAll(targetPath, info.Mode().Perm())
		}
		if info.Mode()&os.ModeSymlink != 0 {
			linkTarget, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(linkTarget, targetPath); err != nil {
				return err
			}
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file", path)
		}
		return copyFile(path, targetPath)
	})
}

func copyFile(source string, target string) error {
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", source)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func verifyFileSHA256(path string, want string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != want {
		return fmt.Errorf("SHA-256 mismatch")
	}
	return nil
}
