package installer

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/katl-dev/katl/internal/installer/disk"
	installstatus "github.com/katl-dev/katl/internal/installer/status"
)

type installProgress struct {
	mu     sync.Mutex
	record installstatus.Record
	err    error
	cancel context.CancelFunc
	done   chan struct{}
}

func (install *Context) startProgress(ctx context.Context, step StepID) error {
	record := statusFromContext(install, installstatus.StateRunning, step, nil)
	record.Progress = &installstatus.Progress{Operation: string(step), StartedAt: timeNow()}
	if install.DiskLayout != nil {
		record.Progress.Target = install.DiskLayout.TargetDiskPath
	}
	if err := install.Store.SaveStatus(ctx, record); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	progress := &installProgress{record: record, cancel: cancel, done: make(chan struct{})}
	install.progress = progress
	go func() {
		defer close(progress.done)
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				progress.mu.Lock()
				progress.record.UpdatedAt = timeNow()
				if progress.err == nil {
					progress.err = install.Store.SaveStatus(context.WithoutCancel(ctx), progress.record)
				}
				progress.mu.Unlock()
			}
		}
	}()
	return nil
}

func (install *Context) stopProgress() error {
	progress := install.progress
	if progress == nil {
		return nil
	}
	// Join the writer before publishing completion or failure, so a heartbeat
	// cannot replace a terminal status with an older in-progress snapshot.
	progress.cancel()
	<-progress.done
	install.progress = nil
	return progress.err
}

func (install *Context) reportDiskOperation(operation disk.DiskOperation) error {
	progress := install.progress
	if progress == nil {
		return nil
	}
	progress.mu.Lock()
	defer progress.mu.Unlock()
	if progress.err != nil {
		return progress.err
	}
	target := install.DiskLayout.TargetDiskPath
	for _, argument := range operation.Args {
		if strings.HasPrefix(argument, "/dev/") {
			target = argument
		}
	}
	progress.record.Progress = &installstatus.Progress{Operation: operation.Name, Target: target, StartedAt: timeNow()}
	progress.record.UpdatedAt = timeNow()
	progress.err = install.Store.SaveStatus(context.Background(), progress.record)
	return progress.err
}
