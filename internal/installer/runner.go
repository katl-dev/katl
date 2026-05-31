package installer

import (
	"context"
	"fmt"
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
	ManifestPath string
	StateDir     string
	Commands     CommandRunner
	Store        StateStore
	Completed    []StepID
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
		stubStep{id: LoadManifest},
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
		stubStep{id: InstallSeed},
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

	install.Completed = append(install.Completed, s.id)
	return install.Store.SaveCheckpoint(ctx, Checkpoint{
		CurrentStep:    s.id,
		CompletedSteps: append([]StepID(nil), install.Completed...),
	})
}
