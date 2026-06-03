package installer

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/zariel/katl/internal/installer/generation"
	"github.com/zariel/katl/internal/installer/manifest"
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
	ManifestPath   string
	StateDir       string
	TargetRoot     string
	BootRoot       string
	Commands       CommandRunner
	Store          StateStore
	Manifest       manifest.Manifest
	LoaderRecord   *generation.Record
	IdentityRandom io.Reader
	Completed      []StepID
}

type Step interface {
	ID() StepID
	Run(context.Context, *Context) error
}

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
		stubStep{id: CollectHardwareFacts},
		stubStep{id: VerifyTrust},
		stubStep{id: PlanInstall},
		stubStep{id: PrepareDisk},
		stubStep{id: CreatePartitions},
		stubStep{id: FormatFilesystems},
		stubStep{id: MountTarget},
		stubStep{id: InstallRootSlot},
		stubStep{id: InstallBootArtifacts},
		stubStep{id: InstallExtensions},
		installSeedStep{},
		stubStep{id: InstallMountUnits},
		stubStep{id: WriteInstallRecord},
		stubStep{id: VerifyTarget},
		stubStep{id: Reboot},
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

	for _, step := range r.plan {
		if err := step.Run(ctx, r.ctx); err != nil {
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
	file, err := os.Open(install.ManifestPath)
	if err != nil {
		return fmt.Errorf("open manifest: %w", err)
	}
	defer file.Close()
	decoded, err := manifest.Decode(file)
	if err != nil {
		return err
	}
	install.Manifest = decoded
	return recordStep(ctx, install, LoadManifest)
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
	return install.Store.SaveCheckpoint(ctx, Checkpoint{
		CurrentStep:    id,
		CompletedSteps: append([]StepID(nil), install.Completed...),
	})
}
