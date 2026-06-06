package vmtest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type InstalledRuntimeNodeConfig struct {
	Name    string
	Runtime InstalledRuntimeConfig
}

type RunningInstalledRuntimeNode struct {
	Name   string
	Result Result
	VSock  VSockPlan

	cancel context.CancelFunc
	done   <-chan error
}

func StartInstalledRuntimeNode(ctx context.Context, parent Result, config InstalledRuntimeNodeConfig, runner VMRunner) (RunningInstalledRuntimeNode, error) {
	name := clean(strings.TrimSpace(config.Name))
	if name == "" {
		return RunningInstalledRuntimeNode{}, errors.New("installed runtime node name is required")
	}
	result, err := PlannedInstalledRuntimeNodeResult(parent, name)
	if err != nil {
		return RunningInstalledRuntimeNode{}, err
	}
	started := time.Now().UTC()
	fail := func(err error) (RunningInstalledRuntimeNode, error) {
		if writeErr := writeInstalledRuntimeNodeResult(result, StatusFailed, err.Error(), started); writeErr != nil {
			return RunningInstalledRuntimeNode{}, fmt.Errorf("%w; write installed runtime node result: %v", err, writeErr)
		}
		return RunningInstalledRuntimeNode{}, err
	}
	runtime := config.Runtime
	runtime.RequireVMTestAgent = true
	if err := PrepareInstalledRuntime(result, runtime); err != nil {
		return fail(err)
	}
	vm := runtime.VM
	vm.Phase = "runtime"
	vm.Expect = first(vm.Expect, runtime.Expect, "Katl state projection ready")
	vm.Boot = VMBoot{
		Image:         runtime.Disk,
		ImageFormat:   diskFormat(runtime.DiskFormat),
		ImageSnapshot: true,
		EFITree:       runtimeESPPath(result),
	}
	vm.VSock.Enabled = true
	vm.Agent.RequireHealth = true

	plan, err := planVM(result, vm, runner.probe)
	if err != nil {
		return fail(err)
	}
	result.VSock = plan.VSock
	if err := prepareVM(plan, vm); err != nil {
		return fail(err)
	}

	runCtx := ctx
	var timeoutCancel context.CancelFunc
	if vm.Timeout > 0 {
		runCtx, timeoutCancel = context.WithTimeout(ctx, vm.Timeout)
	}
	runCtx, cancel := context.WithCancel(runCtx)
	stop := func() {
		cancel()
		if timeoutCancel != nil {
			timeoutCancel()
		}
	}
	file, err := os.OpenFile(plan.SerialLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		stop()
		return fail(err)
	}
	executor := runner.Executor
	if executor == nil {
		executor = defaultVMExecutor(result)
	}
	done := make(chan error, 1)
	go func() {
		defer file.Close()
		done <- executor.Run(runCtx, plan.QEMUPath, plan.Args, file)
	}()
	qemuDone, err := waitForSerialSignal(runCtx, done, plan.SerialLog, vm.Expect, vm.PollInterval)
	if err != nil {
		stop()
		if !qemuDone {
			<-done
		}
		return fail(err)
	}
	if qemuDone {
		return fail(fmt.Errorf("qemu exited after serial signal before installed runtime node %q could be used", name))
	}
	if err := runner.checkAgent(runCtx, result, vm); err != nil {
		stop()
		<-done
		return fail(err)
	}
	if err := writeInstalledRuntimeNodeResult(result, StatusPassed, "", started); err != nil {
		stop()
		<-done
		return RunningInstalledRuntimeNode{}, err
	}
	return RunningInstalledRuntimeNode{
		Name:   name,
		Result: result,
		VSock:  plan.VSock,
		cancel: stop,
		done:   done,
	}, nil
}

func PlannedInstalledRuntimeNodeResult(parent Result, name string) (Result, error) {
	name = clean(strings.TrimSpace(name))
	if name == "" {
		return Result{}, errors.New("installed runtime node name is required")
	}
	return nodeResult(parent, name), nil
}

func writeInstalledRuntimeNodeResult(result Result, status Status, failure string, started time.Time) error {
	finished := time.Now().UTC()
	result.Status = status
	result.Started = started
	result.Finished = finished
	if !started.IsZero() {
		result.DurationMS = finished.Sub(started).Milliseconds()
	}
	result.FailureSummary = failure
	result.addPhase("installed-runtime-node-start", status, failure, started, finished)
	if err := os.MkdirAll(result.QEMUDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(result.DiskDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(result.ManifestDir, 0o755); err != nil {
		return err
	}
	return writeJSON(result.Artifacts.Result, result)
}

func (n RunningInstalledRuntimeNode) Stop() error {
	if n.cancel == nil || n.done == nil {
		return nil
	}
	n.cancel()
	return <-n.done
}

func nodeResult(parent Result, name string) Result {
	runDir := filepath.Join(parent.RunDir, "nodes", name)
	result := parent
	result.RunID = parent.RunID + "-" + name
	result.RunDir = runDir
	result.QEMUDir = filepath.Join(runDir, "qemu")
	result.DiskDir = filepath.Join(runDir, "disks")
	result.ManifestDir = filepath.Join(runDir, "manifests")
	result.Artifacts = pathsFor(runDir)
	result.VSock = VSockPlan{}
	result.Phases = nil
	return result
}
