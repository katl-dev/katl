package installer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/installer/discovery"
	"github.com/katl-dev/katl/internal/installer/disk"
	"github.com/katl-dev/katl/internal/installer/generation"
	"github.com/katl-dev/katl/internal/installer/katlosimage"
	"github.com/katl-dev/katl/internal/installer/kubeadmconfig"
	"github.com/katl-dev/katl/internal/installer/kubernetesbundle"
	"github.com/katl-dev/katl/internal/installer/manifest"
	installstatus "github.com/katl-dev/katl/internal/installer/status"
)

type StepID string

const (
	DiscoverInstallerInput StepID = "DiscoverInstallerInput"
	WaitForLocalConfig     StepID = "WaitForLocalConfig"
	LoadManifest           StepID = "LoadManifest"
	SelectNode             StepID = "SelectNode"
	CollectHardwareFacts   StepID = "CollectHardwareFacts"
	VerifyTrust            StepID = "VerifyTrust"
	PlanInstall            StepID = "PlanInstall"
	PrepareDisk            StepID = "PrepareDisk"
	CreatePartitions       StepID = "CreatePartitions"
	FormatFilesystems      StepID = "FormatFilesystems"
	MountTarget            StepID = "MountTarget"
	InstallRootSlot        StepID = "InstallRootSlot"
	InstallBootArtifacts   StepID = "InstallBootArtifacts"
	InstallExtensions      StepID = "InstallExtensions"
	InstallSeed            StepID = "InstallSeed"
	InstallMountUnits      StepID = "InstallMountUnits"
	WriteInstallRecord     StepID = "WriteInstallRecord"
	VerifyTarget           StepID = "VerifyTarget"
	Reboot                 StepID = "Reboot"
)

type Context struct {
	ManifestPath          string
	StateDir              string
	TargetRoot            string
	BootRoot              string
	Commands              CommandRunner
	Store                 StateStore
	Manifest              manifest.Manifest
	LoaderRecord          *generation.Record
	KatlosImage           *katlosimage.Payload
	KatlosResolver        KatlosImageResolver
	MediaKatlosResolver   KatlosImageResolver
	DefaultKatlosImage    manifest.KatlosImage
	KatlosImageFromMedia  bool
	Discovery             discovery.DiscoverySource
	HardwareFacts         discovery.HardwareFacts
	RootProfile           manifest.RootDiskProfile
	DiskLayout            *disk.DiskLayoutPlan
	RootSlotPlan          *disk.RootSlotWritePlan
	RootSlotTarget        disk.RootSlotDevice
	RootSlotOpener        disk.RootSlotDeviceOpener
	RootSlotInstaller     disk.RootSlotInstaller
	CurrentRootSlot       disk.RootSlot
	RootPartitionUUID     string
	GenerationID          string
	KubeadmConfigs        map[string]kubeadmconfig.Plan
	IdentityRandom        io.Reader
	Completed             []StepID
	Chown                 func(path string, uid int, gid int) error
	InputMode             string
	InputSource           string
	RequestDigest         string
	BundleDigest          string
	SourceDigest          string
	NodeMaterialDigest    string
	InstallMaterialDigest string
	PreviousStatus        *installstatus.Record
	ReportStep            func(StepID)
}

type Step interface {
	ID() StepID
	Run(context.Context, *Context) error
}

type KatlosImageResolver interface {
	ResolveKatlosImage(context.Context, manifest.KatlosImage) (katlosimage.Payload, error)
}

var ErrInstallRefused = errors.New("install refused")

type Plan []Step

func DefaultPlan() Plan {
	return NewPlan(PlanOptions{})
}

type PlanOptions struct {
	PreseededManifest bool
}

func NewPlan(options PlanOptions) Plan {
	plan := Plan{
		stubStep{id: DiscoverInstallerInput},
	}

	if !options.PreseededManifest {
		plan = append(plan, stubStep{id: WaitForLocalConfig})
	}

	plan = append(plan,
		loadManifestStep{},
		stubStep{id: SelectNode},
		collectHardwareFactsStep{},
		verifyKatlosImageStep{},
		planInstallStep{},
		prepareDiskStep{},
		createPartitionsStep{},
		formatFilesystemsStep{},
		mountTargetStep{},
		installRootSlotStep{},
		installBootArtifactsStep{},
		installExtensionsStep{},
		installSeedStep{},
		installMountUnitsStep{},
		writeInstallRecordStep{},
		verifyTargetStep{},
		rebootStep{},
	)

	return plan
}

func PreseededManifestPlan() Plan {
	return NewPlan(PlanOptions{PreseededManifest: true})
}

func (p Plan) IDs() []StepID {
	ids := make([]StepID, 0, len(p))
	for _, step := range p {
		ids = append(ids, step.ID())
	}
	return ids
}

type Runner struct {
	plan Plan
	ctx  *Context
}

func NewRunner(plan Plan, ctx *Context) Runner {
	return Runner{plan: plan, ctx: ctx}
}

func (r Runner) Run(ctx context.Context) error {
	if r.ctx == nil {
		return fmt.Errorf("installer context is required")
	}
	if r.ctx.Commands == nil {
		return fmt.Errorf("command runner is required")
	}
	if r.ctx.Store == nil {
		return fmt.Errorf("state store is required")
	}
	if err := loadPreviousStatus(ctx, r.ctx); err != nil {
		return err
	}

	for _, step := range r.plan {
		if r.ctx.ReportStep != nil {
			r.ctx.ReportStep(step.ID())
		}
		if err := step.Run(ctx, r.ctx); err != nil {
			if statusErr := recordFailure(ctx, r.ctx, step.ID(), err); statusErr != nil {
				return fmt.Errorf("%s: %w", step.ID(), errors.Join(err, fmt.Errorf("record failure status: %w", statusErr)))
			}
			return fmt.Errorf("%s: %w", step.ID(), err)
		}
	}

	return nil
}

type loadManifestStep struct{}

func (loadManifestStep) ID() StepID {
	return LoadManifest
}

func (loadManifestStep) Run(ctx context.Context, install *Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if install.ManifestPath == "" {
		return fmt.Errorf("manifest path is required")
	}
	data, err := os.ReadFile(install.ManifestPath)
	if err != nil {
		return fmt.Errorf("open manifest: %w", err)
	}
	install.RequestDigest = installstatus.Digest(data)
	if install.InputSource == "" {
		install.InputSource = install.ManifestPath
	}
	decoded, defaulted, err := manifest.DecodeWithDefaultImage(bytes.NewReader(data), install.DefaultKatlosImage)
	if err != nil {
		return err
	}
	install.Manifest = decoded
	install.KatlosImageFromMedia = defaulted || (!manifest.KatlosImageEmpty(install.DefaultKatlosImage) && decoded.KatlosImage == install.DefaultKatlosImage)
	digest, err := installstatus.DigestManifest(decoded)
	if err != nil {
		return err
	}
	install.RequestDigest = digest
	if err := refuseChangedInterruptedRequest(install); err != nil {
		return err
	}
	return recordStep(ctx, install, LoadManifest)
}

type collectHardwareFactsStep struct{}

func (collectHardwareFactsStep) ID() StepID {
	return CollectHardwareFacts
}

func (collectHardwareFactsStep) Run(ctx context.Context, install *Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if install.Discovery != nil {
		facts, err := install.Discovery.Discover(ctx)
		if err != nil {
			return err
		}
		install.HardwareFacts = facts
	}
	return recordStep(ctx, install, CollectHardwareFacts)
}

type verifyKatlosImageStep struct{}

func (verifyKatlosImageStep) ID() StepID {
	return VerifyTrust
}

func (verifyKatlosImageStep) Run(ctx context.Context, install *Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	resolver := install.KatlosResolver
	if install.KatlosImageFromMedia && install.MediaKatlosResolver != nil {
		resolver = install.MediaKatlosResolver
	}
	if resolver != nil {
		payload, err := resolver.ResolveKatlosImage(ctx, install.Manifest.KatlosImage)
		if err != nil {
			return err
		}
		install.KatlosImage = &payload
	}
	return recordStep(ctx, install, VerifyTrust)
}

type planInstallStep struct{}

func (planInstallStep) ID() StepID {
	return PlanInstall
}

func (planInstallStep) Run(ctx context.Context, install *Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if install.KatlosImage != nil && len(install.HardwareFacts.BlockDevices) > 0 {
		if err := planInstall(install); err != nil {
			return err
		}
	}
	return recordStep(ctx, install, PlanInstall)
}

func planInstall(install *Context) error {
	profile := install.RootProfile
	layoutRequest, err := manifest.BuildDiskLayoutRequest(install.Manifest, profile, runtimeRootSizeMiB(install.KatlosImage.Runtime.SizeBytes))
	if err != nil {
		return err
	}
	layout, err := disk.PlanDiskLayout(install.HardwareFacts, layoutRequest)
	if err != nil {
		return err
	}
	rootPlan, err := disk.PlanRootSlotWrite(layout, disk.RootSlotWriteRequest{
		RuntimeArtifact: install.KatlosImage.RuntimeArtifact(),
		CurrentSlot:     install.CurrentRootSlot,
	})
	if err != nil {
		return err
	}
	install.DiskLayout = &layout
	install.RootSlotPlan = &rootPlan

	if strings.TrimSpace(install.RootPartitionUUID) != "" {
		record, err := firstInstallRecordFromImage(*install.KatlosImage, rootPlan, install)
		if err != nil {
			return err
		}
		install.LoaderRecord = &record
	}
	return nil
}

func firstInstallRecordFromImage(payload katlosimage.Payload, rootPlan disk.RootSlotWritePlan, install *Context) (generation.Record, error) {
	generationID := "0"
	request, err := payload.FirstInstallRequest(katlosimage.FirstInstallRequest{
		GenerationID:             generationID,
		RootSlot:                 string(rootPlan.Slot),
		RootPartitionUUID:        install.RootPartitionUUID,
		UKIPath:                  "/efi/EFI/Linux/katl-" + generationID + ".efi",
		CreatedAt:                timeNow(),
		EnableEndpointAdvertiser: install.Manifest.Node.ControlPlaneEndpoint != nil,
	})
	if err != nil {
		return generation.Record{}, err
	}
	record := generation.Record{
		APIVersion:     generation.APIVersion,
		Kind:           generation.Kind,
		GenerationID:   request.GenerationID,
		RuntimeVersion: request.RuntimeVersion,
		Root: generation.RootSelection{
			Slot:                  request.RootSlot,
			PartitionUUID:         request.RootPartitionUUID,
			RuntimeVersion:        request.RuntimeVersion,
			RuntimeInterface:      request.RuntimeInterface,
			Architecture:          request.RuntimeArchitecture,
			RuntimeArtifactSHA256: request.RuntimeArtifactSHA256,
		},
		Boot:              generation.BootSelection{UKIPath: request.UKIPath},
		Sysexts:           request.Sysexts,
		KernelCommandLine: request.KernelCommandLine,
		CreatedAt:         request.CreatedAt,
		BootState:         "pending",
		HealthState:       "unknown",
	}
	for _, sysext := range record.Sysexts {
		if err := generation.ValidatePair(record.Root, sysext); err != nil {
			return generation.Record{}, err
		}
	}
	return record, nil
}

func runtimeRootSizeMiB(sizeBytes int64) uint64 {
	if sizeBytes <= 0 {
		return 0
	}
	const mib = 1024 * 1024
	return uint64((sizeBytes + mib - 1) / mib)
}

type prepareDiskStep struct{}

func (prepareDiskStep) ID() StepID {
	return PrepareDisk
}

func (prepareDiskStep) Run(ctx context.Context, install *Context) error {
	if err := executeDiskGroup(ctx, install, disk.PrepareOperations); err != nil {
		return err
	}
	return recordStep(ctx, install, PrepareDisk)
}

type createPartitionsStep struct{}

func (createPartitionsStep) ID() StepID {
	return CreatePartitions
}

func (createPartitionsStep) Run(ctx context.Context, install *Context) error {
	if err := executeDiskGroup(ctx, install, disk.PartitionOperations); err != nil {
		return err
	}
	return recordStep(ctx, install, CreatePartitions)
}

type formatFilesystemsStep struct{}

func (formatFilesystemsStep) ID() StepID {
	return FormatFilesystems
}

func (formatFilesystemsStep) Run(ctx context.Context, install *Context) error {
	if err := executeDiskGroup(ctx, install, disk.FormatOperations); err != nil {
		return err
	}
	return recordStep(ctx, install, FormatFilesystems)
}

type mountTargetStep struct{}

func (mountTargetStep) ID() StepID {
	return MountTarget
}

func (mountTargetStep) Run(ctx context.Context, install *Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	result, err := executeDiskGroupResult(ctx, install, disk.MountOperations)
	if err != nil {
		return err
	}
	if result.Boot != nil && strings.TrimSpace(result.Boot.RootPartitionUUID) != "" {
		install.RootPartitionUUID = result.Boot.RootPartitionUUID
	}
	if install.KatlosImage != nil && install.RootSlotPlan != nil && install.LoaderRecord == nil {
		if strings.TrimSpace(install.RootPartitionUUID) == "" {
			commands, ok := install.Commands.(disk.OutputCommandRunner)
			if !ok {
				return fmt.Errorf("command runner must support output to read root partition UUID")
			}
			uuid, err := disk.ReadPartUUID(ctx, commands, install.RootSlotPlan.TargetPartition.GPTLabel)
			if err != nil {
				return err
			}
			install.RootPartitionUUID = uuid
		}
		record, err := firstInstallRecordFromImage(*install.KatlosImage, *install.RootSlotPlan, install)
		if err != nil {
			return err
		}
		install.LoaderRecord = &record
	}
	return recordStep(ctx, install, MountTarget)
}

func executeDiskGroup(ctx context.Context, install *Context, group disk.DiskOperationGroup) error {
	_, err := executeDiskGroupResult(ctx, install, group)
	return err
}

func executeDiskGroupResult(ctx context.Context, install *Context, group disk.DiskOperationGroup) (disk.DiskExecutionResult, error) {
	select {
	case <-ctx.Done():
		return disk.DiskExecutionResult{}, ctx.Err()
	default:
	}
	if install.DiskLayout == nil {
		return disk.DiskExecutionResult{}, nil
	}
	executor := disk.DiskExecutor{Commands: install.Commands}
	result, err := executor.ExecuteGroup(ctx, diskExecutionRequest(install), group)
	if err != nil {
		return disk.DiskExecutionResult{}, err
	}
	return result, nil
}

func diskExecutionRequest(install *Context) disk.DiskExecutionRequest {
	return disk.DiskExecutionRequest{
		Plan:              *install.DiskLayout,
		AllowDestructive:  install.Manifest.Install.WipeTarget,
		TargetMountPrefix: install.TargetRoot,
	}
}

type installRootSlotStep struct{}

func (installRootSlotStep) ID() StepID {
	return InstallRootSlot
}

func (installRootSlotStep) Run(ctx context.Context, install *Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if install.KatlosImage == nil && install.RootSlotPlan == nil {
		return recordStep(ctx, install, InstallRootSlot)
	}
	if install.KatlosImage == nil {
		return fmt.Errorf("KatlOS image payload is required to install root slot")
	}
	if install.RootSlotPlan == nil {
		return fmt.Errorf("root slot plan is required")
	}
	target := install.RootSlotTarget
	var closeTarget func() error
	if target == nil && install.RootSlotOpener != nil {
		opened, err := install.RootSlotOpener.OpenRootSlotDevice(ctx, install.RootSlotPlan.TargetPartition)
		if err != nil {
			return err
		}
		target = opened
		if closer, ok := opened.(io.Closer); ok {
			closeTarget = closer.Close
		}
	}
	if target == nil {
		return fmt.Errorf("root slot target is required")
	}
	if closeTarget != nil {
		defer closeTarget()
	}
	source, err := os.Open(install.KatlosImage.ComponentPath(install.KatlosImage.Runtime))
	if err != nil {
		return fmt.Errorf("open runtime root component: %w", err)
	}
	defer source.Close()
	installer := install.RootSlotInstaller
	if installer == nil {
		installer = func(context.Context, disk.RootSlotInstallRequest) (disk.RootSlotInstallResult, error) {
			return disk.WriteRootSlot(disk.RootSlotInstallRequest{
				Plan:     *install.RootSlotPlan,
				Artifact: source,
				Target:   target,
			})
		}
	}
	if _, err := installer(ctx, disk.RootSlotInstallRequest{
		Plan:     *install.RootSlotPlan,
		Artifact: source,
		Target:   target,
	}); err != nil {
		return err
	}
	return recordStep(ctx, install, InstallRootSlot)
}

type installBootArtifactsStep struct{}

func (installBootArtifactsStep) ID() StepID {
	return InstallBootArtifacts
}

func (installBootArtifactsStep) Run(ctx context.Context, install *Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if install.KatlosImage == nil {
		return recordStep(ctx, install, InstallBootArtifacts)
	}
	if install.LoaderRecord == nil {
		return fmt.Errorf("loader generation record is required to install boot artifacts")
	}
	target, err := targetPathForAbsolute(install.TargetRoot, install.LoaderRecord.Boot.UKIPath)
	if err != nil {
		return err
	}
	if err := copyVerifiedComponent(install.KatlosImage.ComponentPath(install.KatlosImage.Boot), target, install.KatlosImage.Boot); err != nil {
		return err
	}
	return recordStep(ctx, install, InstallBootArtifacts)
}

type installExtensionsStep struct{}

func (installExtensionsStep) ID() StepID {
	return InstallExtensions
}

func (installExtensionsStep) Run(ctx context.Context, install *Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if install.KatlosImage == nil {
		return recordStep(ctx, install, InstallExtensions)
	}
	if install.LoaderRecord == nil {
		return fmt.Errorf("loader generation record is required to install extensions")
	}
	if install.Manifest.Node.SystemRole != "control-plane" {
		return recordStep(ctx, install, InstallExtensions)
	}
	artifact, err := endpointAdvertiserArtifact(*install.KatlosImage)
	if err != nil {
		return err
	}
	cacheTarget, err := targetPathForAbsolute(install.TargetRoot, artifact.Extension.Path)
	if err != nil {
		return err
	}
	if err := copyVerifiedComponent(install.KatlosImage.ComponentPath(install.KatlosImage.EndpointAdvertiser), cacheTarget, install.KatlosImage.EndpointAdvertiser); err != nil {
		return err
	}
	if err := writeEndpointAdvertiserArtifact(install.TargetRoot, artifact); err != nil {
		return err
	}
	for _, extension := range install.LoaderRecord.Sysexts {
		if extension.Name != katlosimage.EndpointAdvertiserName {
			continue
		}
		target, err := targetPathForAbsolute(install.TargetRoot, extension.Path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("create endpoint advertiser sysext directory: %w", err)
		}
		if err := linkOrCopyInstalledArtifact(cacheTarget, target); err != nil {
			return err
		}
	}
	return recordStep(ctx, install, InstallExtensions)
}

func linkOrCopyInstalledArtifact(source, target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	sourceInfo, err := os.Stat(source)
	if err != nil {
		return err
	}
	if targetInfo, err := os.Stat(target); err == nil {
		if os.SameFile(sourceInfo, targetInfo) {
			return nil
		}
		if err := os.Remove(target); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Link(source, target); err == nil {
		return nil
	}
	sourceFile, err := os.Open(source)
	if err != nil {
		return err
	}
	defer sourceFile.Close()
	targetFile, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(targetFile, sourceFile)
	closeErr := targetFile.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func targetPathForAbsolute(root string, path string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", fmt.Errorf("target root is required")
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("target path %q must be absolute", path)
	}
	clean := filepath.Clean(path)
	if clean == string(filepath.Separator) {
		return "", fmt.Errorf("target path must not be root")
	}
	return filepath.Join(root, strings.TrimPrefix(clean, string(filepath.Separator))), nil
}

func copyVerifiedComponent(source string, target string, component katlosimage.Component) error {
	info, err := os.Stat(source)
	if err != nil {
		return fmt.Errorf("stat %s component: %w", component.Name, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s component is not a regular file", component.Name)
	}
	if info.Size() != component.SizeBytes {
		return fmt.Errorf("%s component size %d does not match index %d", component.Name, info.Size(), component.SizeBytes)
	}
	src, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open %s component: %w", component.Name, err)
	}
	defer src.Close()
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("create %s target directory: %w", component.Name, err)
	}
	dst, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("open %s target: %w", component.Name, err)
	}
	hash := sha256.New()
	written, copyErr := io.Copy(dst, io.TeeReader(src, hash))
	closeErr := dst.Close()
	if copyErr != nil {
		return fmt.Errorf("copy %s component: %w", component.Name, copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close %s target: %w", component.Name, closeErr)
	}
	if written != component.SizeBytes {
		return fmt.Errorf("copy %s component wrote %d bytes, want %d", component.Name, written, component.SizeBytes)
	}
	got := hex.EncodeToString(hash.Sum(nil))
	if got != component.SHA256 {
		return fmt.Errorf("%s component digest %s does not match index %s", component.Name, got, component.SHA256)
	}
	return nil
}

type installSeedStep struct{}

func (installSeedStep) ID() StepID {
	return InstallSeed
}

func (installSeedStep) Run(ctx context.Context, install *Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if install.TargetRoot == "" {
		return fmt.Errorf("target root is required")
	}
	request := generation.IdentityRequest{
		AuthorizedKeys: install.Manifest.Node.Identity.SSH.AuthorizedKeys,
		Random:         install.IdentityRandom,
	}
	if install.LoaderRecord != nil {
		bootRoot := install.BootRoot
		if bootRoot == "" {
			bootRoot = filepath.Join(install.TargetRoot, "efi")
		}
		identity, err := generation.WriteInstallIdentity(generation.InstallIdentityRequest{
			TargetRoot: install.TargetRoot,
			BootRoot:   bootRoot,
			Identity:   request,
			Loader:     generation.LoaderRequest{Record: *install.LoaderRecord},
		})
		if err != nil {
			return err
		}
		entryPath, err := bootRelativePath(bootRoot, identity.EntryPath)
		if err != nil {
			return err
		}
		install.LoaderRecord.Boot.LoaderEntryPath = entryPath
	} else if _, err := generation.WriteIdentity(install.TargetRoot, request); err != nil {
		return err
	}
	return recordStep(ctx, install, InstallSeed)
}

type installMountUnitsStep struct{}

func (installMountUnitsStep) ID() StepID {
	return InstallMountUnits
}

func (installMountUnitsStep) Run(ctx context.Context, install *Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if install.LoaderRecord == nil {
		return fmt.Errorf("loader generation record is required to install mount units")
	}
	if _, err := generation.WriteState(install.TargetRoot, generation.StateRequest{
		PartitionUUID: install.LoaderRecord.Root.PartitionUUID,
	}); err != nil {
		return err
	}
	return recordStep(ctx, install, InstallMountUnits)
}

type writeInstallRecordStep struct{}

func (writeInstallRecordStep) ID() StepID {
	return WriteInstallRecord
}

func (writeInstallRecordStep) Run(ctx context.Context, install *Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if install.LoaderRecord == nil {
		return fmt.Errorf("loader generation record is required to materialize generated confext")
	}
	result, err := MaterializeInstallRecord(InstallRecordRequest{
		TargetRoot:        install.TargetRoot,
		Manifest:          install.Manifest,
		KubeadmConfigs:    install.KubeadmConfigs,
		KubernetesVersion: installedKubernetesPayloadVersion(install),
		Record:            *install.LoaderRecord,
		Chown:             install.Chown,
	})
	if err != nil {
		return err
	}
	install.LoaderRecord = &result.Record
	if err := writeInstalledManifest(install.TargetRoot, install.Manifest); err != nil {
		return err
	}
	if err := writeInitialBootSelection(install.TargetRoot, result.Record); err != nil {
		return err
	}
	if _, err := WriteClusterIntent(ClusterIntentRequest{
		TargetRoot:         install.TargetRoot,
		Manifest:           install.Manifest,
		KubeadmConfigs:     install.KubeadmConfigs,
		KubernetesVersion:  installedKubernetesPayloadVersion(install),
		GenerationID:       result.Record.GenerationID,
		RequestDigest:      install.RequestDigest,
		InstalledAt:        result.Record.CreatedAt,
		TargetDiskStableID: targetDiskStableID(install.Manifest.Install.TargetDisk),
	}); err != nil {
		return err
	}
	return recordStep(ctx, install, WriteInstallRecord)
}

func writeInstalledManifest(targetRoot string, installManifest manifest.Manifest) error {
	target := filepath.Join(filepath.Clean(targetRoot), "var/lib/katl/install/manifest.json")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("create install manifest parent: %w", err)
	}
	data, err := json.MarshalIndent(installManifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encode install manifest: %w", err)
	}
	if err := os.WriteFile(target, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write install manifest: %w", err)
	}
	return nil
}

func writeInitialBootSelection(targetRoot string, record generation.Record) error {
	entry := strings.TrimSpace(record.Boot.LoaderEntryPath)
	if entry == "" {
		return fmt.Errorf("loader entry path is required for boot selection")
	}
	now := timeNow()
	if !record.CreatedAt.IsZero() {
		now = record.CreatedAt
	}
	return generation.WriteBootSelection(targetRoot, generation.BootSelectionRecord{
		APIVersion:            generation.APIVersion,
		Kind:                  generation.BootSelectionKind,
		DefaultGenerationID:   record.GenerationID,
		BootedGenerationID:    record.GenerationID,
		Generation0FallbackID: record.GenerationID,
		DefaultBootEntry:      entry,
		BootedBootEntry:       entry,
		UpdatedAt:             now,
	})
}

type verifyTargetStep struct{}

func (verifyTargetStep) ID() StepID {
	return VerifyTarget
}

func (verifyTargetStep) Run(ctx context.Context, install *Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if install.DiskLayout == nil {
		return recordStep(ctx, install, VerifyTarget)
	}
	if install.Discovery == nil {
		return fmt.Errorf("discovery source is required to verify target layout")
	}
	facts, err := install.Discovery.Discover(ctx)
	if err != nil {
		return err
	}
	install.HardwareFacts = facts
	if err := disk.ValidateAppliedLayoutAt(facts, *install.DiskLayout, install.TargetRoot); err != nil {
		return err
	}
	return recordStep(ctx, install, VerifyTarget)
}

type rebootStep struct{}

func (rebootStep) ID() StepID {
	return Reboot
}

func (rebootStep) Run(ctx context.Context, install *Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if err := recordStep(ctx, install, Reboot); err != nil {
		return fmt.Errorf("persist reboot status: %w", err)
	}
	if err := install.Commands.Run(ctx, "sync"); err != nil {
		return fmt.Errorf("sync install state: %w", err)
	}
	// Leave the handoff API alive briefly after persisting reboot-requested so
	// operators can observe the terminal state before the installer disappears.
	// A transient timer keeps the delay outside the installer state machine.
	if err := install.Commands.Run(ctx, "systemd-run", "--unit=katl-installer-reboot", "--on-active=2s", "systemctl", "--no-block", "reboot"); err != nil {
		return fmt.Errorf("request system reboot: %w", err)
	}
	return nil
}

type stubStep struct {
	id StepID
}

func (s stubStep) ID() StepID {
	return s.id
}

func (s stubStep) Run(ctx context.Context, install *Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	return recordStep(ctx, install, s.id)
}

func recordStep(ctx context.Context, install *Context, id StepID) error {
	install.Completed = append(install.Completed, id)
	if err := install.Store.SaveCheckpoint(ctx, Checkpoint{
		CurrentStep:    id,
		CompletedSteps: append([]StepID(nil), install.Completed...),
	}); err != nil {
		return err
	}
	record := statusFromContext(install, statusForStep(id), id, nil)
	if err := install.Store.SaveStatus(ctx, record); err != nil {
		return err
	}
	return writeTargetStatus(ctx, install, id, record)
}

func recordFailure(ctx context.Context, install *Context, id StepID, err error) error {
	record := statusFromContext(install, failureState(install, id, err), id, err)
	if saveErr := install.Store.SaveStatus(ctx, record); saveErr != nil {
		return saveErr
	}
	return writeTargetStatus(ctx, install, id, record)
}

func statusFromContext(install *Context, state string, current StepID, err error) installstatus.Record {
	record := installstatus.New(state, timeNow())
	record.CurrentStep = string(current)
	record.CompletedSteps = stepStrings(install.Completed)
	record.InputMode = install.InputMode
	record.InputSource = installstatus.RedactSource(install.InputSource)
	record.RequestDigest = install.RequestDigest
	record.BundleDigest = install.BundleDigest
	record.SourceDigest = install.SourceDigest
	record.NodeMaterialDigest = install.NodeMaterialDigest
	record.InstallMaterialDigest = install.InstallMaterialDigest
	record.KatlosImage = installstatus.ImageFromManifest(install.Manifest)
	record.TargetDiskStableID = targetDiskStableID(install.Manifest.Install.TargetDisk)
	record.WipeTargetAccepted = install.Manifest.Install.WipeTarget
	if install.LoaderRecord != nil {
		record.SelectedRootSlot = install.LoaderRecord.Root.Slot
		record.InstalledGeneration = install.LoaderRecord.GenerationID
		record.BootArtifactVersion = install.LoaderRecord.Boot.UKIPath
	}
	if err != nil {
		record.LastError = installstatus.RedactError(err)
		record.RefusalReason = record.LastError
		if state == installstatus.StateFailedBeforeMutation || state == installstatus.StateInstallRefused {
			record.RetryHint = "fix input or environment and rerun before disk mutation"
		} else {
			record.RetryHint = "inspect target state before rerun or repair"
			record.DestructiveMutation = true
		}
	}
	return record
}

func statusForStep(id StepID) string {
	if id == WaitForLocalConfig {
		return installstatus.StateWaitingForConfig
	}
	if id == Reboot {
		return installstatus.StateRebootRequested
	}
	return installstatus.StateRunning
}

func failureState(install *Context, id StepID, err error) string {
	if errors.Is(err, ErrInstallRefused) {
		return installstatus.StateInstallRefused
	}
	if !mutationStarted(install.Completed, id) {
		return installstatus.StateFailedBeforeMutation
	}
	return installstatus.StateFailedAfterMutation
}

func mutationStarted(completed []StepID, current StepID) bool {
	if current == PrepareDisk {
		return true
	}
	for _, step := range completed {
		if step == PrepareDisk || step == CreatePartitions || step == FormatFilesystems || step == MountTarget || step == InstallRootSlot {
			return true
		}
	}
	return false
}

func writeTargetStatus(ctx context.Context, install *Context, current StepID, record installstatus.Record) error {
	if !targetStatusReady(install, current) {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	path, err := installstatus.RuntimeStatusPath(install.TargetRoot)
	if err != nil {
		return err
	}
	return installstatus.WriteFile(path, record)
}

func targetStatusReady(install *Context, current StepID) bool {
	if install == nil || install.TargetRoot == "" {
		return false
	}
	if current == MountTarget {
		return true
	}
	for _, step := range install.Completed {
		if step == MountTarget {
			return true
		}
	}
	return false
}

func loadPreviousStatus(ctx context.Context, install *Context) error {
	if install.PreviousStatus != nil {
		return nil
	}
	record, err := install.Store.LoadStatus(ctx)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("load previous install status: %w", err)
	}
	install.PreviousStatus = &record
	return nil
}

func refuseChangedInterruptedRequest(install *Context) error {
	if install.PreviousStatus == nil || !install.PreviousStatus.DestructiveMutation {
		return nil
	}
	previousDigest := install.PreviousStatus.RequestDigest
	if previousDigest == "" || previousDigest == install.RequestDigest {
		return nil
	}
	return fmt.Errorf("%w: previous destructive install request digest %s does not match current request digest %s", ErrInstallRefused, previousDigest, install.RequestDigest)
}

func stepStrings(steps []StepID) []string {
	out := make([]string, 0, len(steps))
	for _, step := range steps {
		out = append(out, string(step))
	}
	return out
}

func targetDiskStableID(selector manifest.DiskSelector) string {
	switch {
	case selector.ByID != "":
		return selector.ByID
	case selector.WWN != "":
		return "wwn:" + selector.WWN
	case selector.Serial != "":
		return "serial:" + selector.Serial
	default:
		return ""
	}
}

func timeNow() time.Time {
	return time.Now().UTC()
}

func installedKubernetesPayloadVersion(install *Context) string {
	return firstNonEmpty(installedKubernetesVersionFromBootstrapBundle(install), installedKubernetesVersionFromBootstrapCatalog(install), installedKubernetesVersionFromKubeadm(install), installedKubernetesVersionFromRecord(install))
}

func installedKubernetesVersionFromBootstrapBundle(install *Context) string {
	if install == nil || install.Manifest.Node.Bootstrap == nil {
		return ""
	}
	payloadVersion, err := kubernetesbundle.PayloadVersionFromRef(install.Manifest.Node.Bootstrap.KubernetesBundle)
	if err != nil {
		return ""
	}
	return payloadVersion
}

func installedKubernetesVersionFromBootstrapCatalog(install *Context) string {
	if install == nil || install.Manifest.Node.Bootstrap == nil {
		return ""
	}
	version := strings.TrimSpace(install.Manifest.Node.Bootstrap.KubernetesCatalogRef)
	if strings.HasPrefix(version, "v") && strings.Count(version, ".") == 2 {
		return version
	}
	return ""
}

func installedKubernetesVersionFromKubeadm(install *Context) string {
	if install == nil {
		return ""
	}
	ref := strings.TrimSpace(install.Manifest.Node.Kubernetes.Kubeadm.ConfigRef)
	if ref == "" {
		return ""
	}
	plan, ok := install.KubeadmConfigs[ref]
	if !ok {
		return ""
	}
	for _, document := range plan.Documents {
		if version := strings.TrimSpace(document.KubernetesVersion); version != "" {
			return version
		}
	}
	return ""
}

func installedKubernetesVersionFromRecord(install *Context) string {
	if install == nil || install.LoaderRecord == nil {
		return ""
	}
	return selectedKubernetesPayloadVersion(*install.LoaderRecord)
}

func bootRelativePath(bootRoot string, path string) (string, error) {
	bootRoot = filepath.Clean(bootRoot)
	path = filepath.Clean(path)
	rel, err := filepath.Rel(bootRoot, path)
	if err != nil {
		return "", fmt.Errorf("resolve loader entry path: %w", err)
	}
	rel = filepath.ToSlash(rel)
	if rel == "." || strings.HasPrefix(rel, "../") || strings.HasPrefix(rel, "/") {
		return "", fmt.Errorf("loader entry path %s is not under boot root %s", path, bootRoot)
	}
	return rel, nil
}
