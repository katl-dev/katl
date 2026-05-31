package installer

import (
	"context"
	"reflect"
	"testing"
)

func TestDefaultPlanOrder(t *testing.T) {
	want := []StepID{
		DiscoverInstallerInput,
		WaitForLocalConfig,
		LoadManifest,
		SelectNode,
		CollectHardwareFacts,
		VerifyTrust,
		PlanInstall,
		PrepareDisk,
		CreatePartitions,
		FormatFilesystems,
		MountTarget,
		InstallRootSlot,
		InstallBootArtifacts,
		InstallExtensions,
		InstallSeed,
		InstallMountUnits,
		WriteInstallRecord,
		VerifyTarget,
		Reboot,
	}

	if got := DefaultPlan().IDs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("DefaultPlan IDs = %#v, want %#v", got, want)
	}
}

func TestPreseededManifestPlanSkipsLocalConfigWait(t *testing.T) {
	want := []StepID{
		DiscoverInstallerInput,
		LoadManifest,
		SelectNode,
		CollectHardwareFacts,
		VerifyTrust,
		PlanInstall,
		PrepareDisk,
		CreatePartitions,
		FormatFilesystems,
		MountTarget,
		InstallRootSlot,
		InstallBootArtifacts,
		InstallExtensions,
		InstallSeed,
		InstallMountUnits,
		WriteInstallRecord,
		VerifyTarget,
		Reboot,
	}

	if got := PreseededManifestPlan().IDs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("PreseededManifestPlan IDs = %#v, want %#v", got, want)
	}
}

func TestRunnerRecordsCheckpointsWithoutCommands(t *testing.T) {
	store := &MemoryStateStore{}
	commands := &NoopCommandRunner{}
	install := &Context{
		ManifestPath: "/etc/katl/install.json",
		StateDir:     t.TempDir(),
		Commands:     commands,
		Store:        store,
	}

	if err := NewRunner(DefaultPlan(), install).Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	want := DefaultPlan().IDs()
	if !reflect.DeepEqual(install.Completed, want) {
		t.Fatalf("completed steps = %#v, want %#v", install.Completed, want)
	}
	if len(store.Checkpoints) != len(want) {
		t.Fatalf("checkpoint count = %d, want %d", len(store.Checkpoints), len(want))
	}
	if got := store.Checkpoints[len(store.Checkpoints)-1].CompletedSteps; !reflect.DeepEqual(got, want) {
		t.Fatalf("final checkpoint completed steps = %#v, want %#v", got, want)
	}
	if len(commands.Calls) != 0 {
		t.Fatalf("command runner was called during scaffold run: %#v", commands.Calls)
	}
}
