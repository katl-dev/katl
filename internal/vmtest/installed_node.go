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
	if err := ensureInstalledRuntimeNodeDirs(result); err != nil {
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
	vm.Expect = first(vm.Expect, runtime.Expect, runtimeBootSignal)
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
	result.DomainName = plan.DomainName
	result.MACAddress = plan.MACAddress
	result.VSock = plan.VSock
	if err := prepareVM(plan, vm); err != nil {
		return fail(err)
	}

	runCtx := ctx
	var timeoutCancel context.CancelFunc
	if vm.Timeout > 0 {
		runCtx, timeoutCancel = context.WithTimeout(ctx, vm.Timeout)
	}
	runCtx, cancel := context.WithCancelCause(runCtx)
	stop := func() {
		cancel(errVMRunComplete)
		if timeoutCancel != nil {
			timeoutCancel()
		}
	}
	failStop := func() {
		cancel(errVMRunFailed)
		if timeoutCancel != nil {
			timeoutCancel()
		}
	}
	file, err := os.OpenFile(plan.SerialLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		failStop()
		return fail(err)
	}
	executor := runner.Executor
	var preservation *DomainPreservation
	if executor == nil {
		executor, preservation = defaultVMExecutor(result, plan)
	}
	done := make(chan error, 1)
	go func() {
		defer file.Close()
		done <- executor.Run(runCtx, first(plan.VirshPath, plan.CommandPath), plan.Args, file)
	}()
	domainDone, err := waitForSerialSignal(runCtx, done, plan.SerialLog, vm.Expect, vm.PollInterval)
	if err != nil {
		failStop()
		if !domainDone {
			<-done
		}
		result = runner.withDebugTarget(debugFailedResult(result), plan, preservation)
		return fail(err)
	}
	if domainDone {
		result = runner.withDebugTarget(debugFailedResult(result), plan, preservation)
		return fail(fmt.Errorf("libvirt domain exited after serial signal before installed runtime node %q could be used", name))
	}
	if err := runner.checkAgent(runCtx, result, vm); err != nil {
		failStop()
		<-done
		result = runner.withDebugTarget(debugFailedResult(result), plan, preservation)
		return fail(err)
	}
	if plan.MACAddress != "" {
		lease, err := WaitLibvirtLease(runCtx, plan.VirshPath, plan.LibvirtURI, plan.LibvirtNetwork, plan.MACAddress, 30*time.Second)
		if err != nil {
			failStop()
			<-done
			result = runner.withDebugTarget(debugFailedResult(result), plan, preservation)
			return fail(err)
		}
		result.IPAddress = lease.IPAddress
		if err := writeJSON(result.Artifacts.LibvirtLease, lease); err != nil {
			failStop()
			<-done
			result = runner.withDebugTarget(debugFailedResult(result), plan, preservation)
			return fail(fmt.Errorf("write libvirt lease artifact: %w", err))
		}
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
	if err := ensureInstalledRuntimeNodeDirs(result); err != nil {
		return err
	}
	if err := writeJSON(result.Artifacts.Scenario, scenarioRecord{
		Scenario: Scenario{
			Name: result.ScenarioName,
			Keep: result.Keep,
			KVM:  result.KVM,
		},
		Result: result,
	}); err != nil {
		return err
	}
	return writeJSON(result.Artifacts.Result, result)
}

func ensureInstalledRuntimeNodeDirs(result Result) error {
	if err := os.MkdirAll(result.VMDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(result.DiskDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(result.ManifestDir, 0o755); err != nil {
		return err
	}
	return nil
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
	result.ScenarioName = nodeScenarioName(parent.ScenarioName, name)
	result.RunID = parent.RunID + "-" + name
	result.RunDir = runDir
	result.VMDir = filepath.Join(runDir, "vm")
	result.DiskDir = filepath.Join(runDir, "disks")
	result.ManifestDir = filepath.Join(runDir, "manifests")
	result.Artifacts = pathsFor(runDir)
	result.VSock = VSockPlan{}
	result.Phases = nil
	if parent.Debug != nil {
		debug := *parent.Debug
		debug.Targets = nil
		result.Debug = &debug
	}
	return result
}

func nodeScenarioName(parent, name string) string {
	if parent == "" {
		return name
	}
	return parent + "/" + name
}
