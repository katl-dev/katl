package installer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zariel/katl/internal/installer/discovery"
	"github.com/zariel/katl/internal/installer/disk"
	"github.com/zariel/katl/internal/installer/generation"
	"github.com/zariel/katl/internal/installer/katlosimage"
	"github.com/zariel/katl/internal/installer/kubeadmconfig"
	"github.com/zariel/katl/internal/installer/manifest"
	installstatus "github.com/zariel/katl/internal/installer/status"
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
	ManifestPath      string
	StateDir          string
	TargetRoot        string
	BootRoot          string
	Commands          CommandRunner
	Store             StateStore
	Manifest          manifest.Manifest
	LoaderRecord      *generation.Record
	KatlosImage       *katlosimage.Payload
	KatlosResolver    KatlosImageResolver
	Discovery         discovery.DiscoverySource
	HardwareFacts     discovery.HardwareFacts
	RootProfile       manifest.RootDiskProfile
	DiskLayout        *disk.DiskLayoutPlan
	RootSlotPlan      *disk.RootSlotWritePlan
	RootSlotTarget    disk.RootSlotDevice
	RootSlotOpener    disk.RootSlotDeviceOpener
	RootSlotInstaller disk.RootSlotInstaller
	CurrentRootSlot   disk.RootSlot
	RootPartitionUUID string
	GenerationID      string
	KubeadmConfigs    map[string]kubeadmconfig.Plan
	IdentityRandom    io.Reader
	Completed         []StepID
	Chown             func(path string, uid int, gid int) error
	InputMode         string
	InputSource       string
	RequestDigest     string
	PreviousStatus    *installstatus.Record
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
	decoded, err := manifest.Decode(bytes.NewReader(data))
	if err != nil {
		return err
	}
	install.Manifest = decoded
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
	if install.KatlosResolver != nil {
		payload, err := install.KatlosResolver.ResolveKatlosImage(ctx, install.Manifest.KatlosImage)
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
	generationID := strings.TrimSpace(install.GenerationID)
	if generationID == "" {
		generationID = payload.Index.Version
	}
	request, err := payload.FirstInstallRequest(katlosimage.FirstInstallRequest{
		GenerationID:      generationID,
		RootSlot:          string(rootPlan.Slot),
		RootPartitionUUID: install.RootPartitionUUID,
		UKIPath:           "/efi/EFI/Linux/katl-" + generationID + ".efi",
		CreatedAt:         timeNow(),
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
		AllowDestructive:  install.Manifest.Install.AllowDestructiveInstall,
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
	var target string
	for _, ref := range install.LoaderRecord.Sysexts {
		if ref.Name == "kubernetes" {
			target = ref.Path
			break
		}
	}
	if target == "" {
		return fmt.Errorf("Kubernetes sysext record is required")
	}
	path, err := targetPathForAbsolute(install.TargetRoot, target)
	if err != nil {
		return err
	}
	if err := copyVerifiedComponent(install.KatlosImage.ComponentPath(install.KatlosImage.Kubernetes), path, install.KatlosImage.Kubernetes); err != nil {
		return err
	}
	return recordStep(ctx, install, InstallExtensions)
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
		if _, err := generation.WriteInstallIdentity(generation.InstallIdentityRequest{
			TargetRoot: install.TargetRoot,
			BootRoot:   bootRoot,
			Identity:   request,
			Loader:     generation.LoaderRequest{Record: *install.LoaderRecord},
		}); err != nil {
			return err
		}
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
		TargetRoot:     install.TargetRoot,
		Manifest:       install.Manifest,
		KubeadmConfigs: install.KubeadmConfigs,
		Record:         *install.LoaderRecord,
		Chown:          install.Chown,
	})
	if err != nil {
		return err
	}
	install.LoaderRecord = &result.Record
	return recordStep(ctx, install, WriteInstallRecord)
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
	if install.Commands != nil {
		if err := install.Commands.Run(ctx, "sync"); err != nil {
			return fmt.Errorf("sync target writes: %w", err)
		}
	}
	return recordStep(ctx, install, Reboot)
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
	record.KatlosImage = installstatus.ImageFromManifest(install.Manifest)
	record.TargetDiskStableID = targetDiskStableID(install.Manifest.Install.TargetDisk)
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
