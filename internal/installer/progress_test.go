package installer

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/installer/disk"
	installstatus "github.com/katl-dev/katl/internal/installer/status"
)

func TestInstallProgressLiveness(t *testing.T) {
	store := NewFileStateStore(t.TempDir())
	install := &Context{Store: store, DiskLayout: &disk.DiskLayoutPlan{TargetDiskPath: "/dev/vda"}}
	if err := install.startProgress(context.Background(), FormatFilesystems); err != nil {
		t.Fatal(err)
	}
	defer install.stopProgress()
	if err := install.reportDiskOperation(disk.DiskOperation{Name: "format data", Args: []string{"--discard=no", "/dev/vdb"}}); err != nil {
		t.Fatal(err)
	}
	first, err := store.LoadStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.Progress == nil || first.Progress.Operation != "format data" || first.Progress.Target != "/dev/vdb" {
		t.Fatalf("progress = %+v", first.Progress)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		current, err := store.LoadStatus(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if current.UpdatedAt.After(first.UpdatedAt) {
			if current.Progress == nil || !current.Progress.StartedAt.Equal(first.Progress.StartedAt) || current.Progress.Target != "/dev/vdb" {
				t.Fatalf("heartbeat lost operation: %+v", current)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no progress heartbeat during operation")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := install.stopProgress(); err != nil {
		t.Fatal(err)
	}
	terminal := installstatus.New(installstatus.StateFailedAfterMutation, time.Now())
	terminal.LastError = "device disconnected"
	if err := store.SaveStatus(context.Background(), terminal); err != nil {
		t.Fatal(err)
	}
	// Completion joins the writer; its last persisted state must be terminal.
	current, err := store.LoadStatus(context.Background())
	if err != nil || current.Progress != nil || current.State != terminal.State {
		t.Fatalf("terminal status = %+v, %v", current, err)
	}
}

func TestProgressSummary(t *testing.T) {
	started := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	progress := installstatus.Progress{Operation: "format data", Target: "/dev/vdb", StartedAt: started}
	if got := progress.Summary(started.Add(65 * time.Second)); got != "format data · /dev/vdb · 1m5s elapsed" {
		t.Fatal(got)
	}
	if got := progress.Summary(started.Add(-time.Minute)); strings.Contains(got, "-") {
		t.Fatal(got)
	}
}

func TestProgressMarksDiskChanges(t *testing.T) {
	for _, scenario := range []struct {
		step      StepID
		completed []StepID
		changed   bool
	}{
		{PlanInstall, nil, false},
		{PrepareDisk, nil, true},
		{InstallExtensions, []StepID{PrepareDisk, CreatePartitions, InstallRootSlot}, true},
	} {
		t.Run(string(scenario.step), func(t *testing.T) {
			store := NewFileStateStore(t.TempDir())
			install := &Context{Store: store, Completed: scenario.completed}
			if err := install.startProgress(context.Background(), scenario.step); err != nil {
				t.Fatal(err)
			}
			defer install.stopProgress()
			record, err := store.LoadStatus(context.Background())
			if err != nil || record.DestructiveMutation != scenario.changed {
				t.Fatalf("mutation status = %+v, %v", record, err)
			}
		})
	}
}
