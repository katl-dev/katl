package vmtest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	runtime := config.Runtime
	runtime.RequireVMTestAgent = true
	if err := PrepareInstalledRuntime(result, runtime); err != nil {
		return RunningInstalledRuntimeNode{}, err
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
		return RunningInstalledRuntimeNode{}, err
	}
	result.VSock = plan.VSock
	if err := prepareVM(plan, vm); err != nil {
		return RunningInstalledRuntimeNode{}, err
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
		return RunningInstalledRuntimeNode{}, err
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
		<-done
		return RunningInstalledRuntimeNode{}, err
	}
	if qemuDone {
		return RunningInstalledRuntimeNode{}, fmt.Errorf("qemu exited after serial signal before installed runtime node %q could be used", name)
	}
	if err := runner.checkAgent(runCtx, result, vm); err != nil {
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
