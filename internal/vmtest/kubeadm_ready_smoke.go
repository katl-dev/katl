package vmtest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

type KubeadmReadySmokeConfig struct {
	Runtime        InstalledRuntimeConfig
	Smoke          KubeadmReadySmokePlan
	AgentConnector KubeadmReadySmokeAgentConnector
}

type KubeadmReadySmokeAgentSession interface {
	GuestAgentClient
	Close() error
}

type KubeadmReadySmokeAgentConnector func(ctx context.Context, plan VSockPlan, transcript string) (KubeadmReadySmokeAgentSession, error)

type KubeadmReadySmokePlan struct {
	ConfigPath           string
	ProjectedSource      string
	ReadyTimeout         time.Duration
	ReadyPollInterval    time.Duration
	AgentConnectTimeout  time.Duration
	AgentConnectInterval time.Duration
	CommandTimeout       time.Duration
	DiagnosticTimeout    time.Duration
}

func RunInstalledKubeadmReadySmoke(ctx context.Context, result Result, config KubeadmReadySmokeConfig, runner VMRunner) Result {
	runtime := config.Runtime
	runtime.RequireVMTestAgent = true
	if err := PrepareInstalledRuntime(result, runtime); err != nil {
		return finishVM(result, "kubeadm-ready-smoke", StatusFailed, err.Error(), result.Started, runnerTime())
	}
	vm := runtime.VM
	vm.Phase = "kubeadm-ready-smoke"
	vm.Expect = first(vm.Expect, runtime.Expect, "Katl state projection ready")
	vm.Boot = VMBoot{
		Image:         runtime.Disk,
		ImageFormat:   diskFormat(runtime.DiskFormat),
		ImageSnapshot: true,
		EFITree:       runtimeESPPath(result),
	}
	vm.VSock.Enabled = true

	started := time.Now().UTC()
	plan, err := planVM(result, vm, runner.probe)
	if err != nil {
		return finishVM(result, "kubeadm-ready-smoke", StatusFailed, err.Error(), started, time.Now().UTC())
	}
	result.VSock = plan.VSock
	if err := prepareVM(plan, vm); err != nil {
		return finishVM(result, "kubeadm-ready-smoke", StatusFailed, err.Error(), started, time.Now().UTC())
	}
	if vm.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, vm.Timeout)
		defer cancel()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	serial, err := os.OpenFile(plan.SerialLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return finishVM(result, "kubeadm-ready-smoke", StatusFailed, err.Error(), started, time.Now().UTC())
	}
	defer serial.Close()

	executor := runner.Executor
	if executor == nil {
		executor = ExecVMExecutor{}
	}
	done := make(chan error, 1)
	go func() {
		done <- executor.Run(ctx, plan.QEMUPath, plan.Args, serial)
	}()
	qemuDone, err := waitForSerialSignal(ctx, done, plan.SerialLog, vm.Expect, vm.PollInterval)
	if err != nil {
		if !qemuDone {
			cancel()
			<-done
		}
		return finishVM(result, "kubeadm-ready-smoke", StatusFailed, err.Error(), started, time.Now().UTC())
	}
	if qemuDone {
		return finishVM(result, "kubeadm-ready-smoke", StatusFailed, "qemu exited after serial signal before kubeadm-ready smoke", started, time.Now().UTC())
	}

	session, err := connectKubeadmReadySmokeAgent(ctx, config, config.Smoke, result.VSock, result.Artifacts.VSockTranscript)
	if err != nil {
		cancel()
		<-done
		return finishVM(result, "kubeadm-ready-smoke", StatusFailed, err.Error(), started, time.Now().UTC())
	}
	defer session.Close()

	guest := NewGuestControl(result, session)
	if err := RunKubeadmReadySmoke(ctx, guest, config.Smoke); err != nil {
		collectKubeadmReadySmokeDiagnostics(ctx, result, config, session)
		cancel()
		<-done
		return finishVM(result, "kubeadm-ready-smoke", StatusFailed, err.Error(), started, time.Now().UTC())
	}
	cancel()
	<-done
	return finishVM(result, "kubeadm-ready-smoke", StatusPassed, "", started, time.Now().UTC())
}

func RunKubeadmReadySmoke(ctx context.Context, guest *GuestControl, plan KubeadmReadySmokePlan) error {
	if guest == nil {
		return errors.New("guest control is required")
	}
	plan = normalizeKubeadmReadySmokePlan(plan)
	if err := waitKubeadmReadySmoke(ctx, guest, plan); err != nil {
		return err
	}
	if _, err := guest.RunCommand(ctx, GuestCommandRequest{Name: "kubeadm-config", Argv: []string{"test", "-f", plan.ConfigPath}}); err != nil {
		return err
	}
	mount, err := guest.Findmnt(ctx, "--noheadings", "--target", "/etc/kubernetes", "--output", "SOURCE")
	if err != nil {
		return err
	}
	source, err := readCommandStdout(mount)
	if err != nil {
		return err
	}
	if strings.TrimSpace(source) != plan.ProjectedSource {
		return fmt.Errorf("/etc/kubernetes is backed by %q, want %q", strings.TrimSpace(source), plan.ProjectedSource)
	}
	for _, command := range []GuestCommandRequest{
		{Name: "test", Argv: []string{"test", "-x", "/usr/bin/kubeadm"}},
		{Name: "test", Argv: []string{"test", "-x", "/usr/bin/kubelet"}},
		{Name: "test", Argv: []string{"test", "-x", "/usr/bin/kubectl"}},
		{Name: "test", Argv: []string{"test", "-x", "/usr/bin/crictl"}},
		{Name: "systemctl", Argv: []string{"systemctl", "is-active", "--quiet", "containerd.service"}},
		{Name: "crictl-info", Argv: []string{"crictl", "info"}, Timeout: plan.CommandTimeout, SensitiveOutput: true},
	} {
		if _, err := guest.RunCommand(ctx, command); err != nil {
			return err
		}
	}
	return nil
}

func waitKubeadmReadySmoke(ctx context.Context, guest *GuestControl, plan KubeadmReadySmokePlan) error {
	deadline := time.Now().Add(plan.ReadyTimeout)
	var lastErr error
	for {
		if _, err := guest.Systemctl(ctx, "is-active", "--quiet", "katl-kubeadm-ready.target"); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("wait for katl-kubeadm-ready.target: %w", lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(plan.ReadyPollInterval):
		}
	}
}

func normalizeKubeadmReadySmokePlan(plan KubeadmReadySmokePlan) KubeadmReadySmokePlan {
	if plan.ConfigPath == "" {
		plan.ConfigPath = DefaultKubeadmConfigPath
	}
	if plan.ProjectedSource == "" {
		plan.ProjectedSource = DefaultProjectedKubernetesPath
	}
	if plan.ReadyTimeout == 0 {
		plan.ReadyTimeout = 2 * time.Minute
	}
	if plan.ReadyPollInterval == 0 {
		plan.ReadyPollInterval = 2 * time.Second
	}
	if plan.AgentConnectTimeout == 0 {
		plan.AgentConnectTimeout = 30 * time.Second
	}
	if plan.AgentConnectInterval == 0 {
		plan.AgentConnectInterval = 250 * time.Millisecond
	}
	if plan.CommandTimeout == 0 {
		plan.CommandTimeout = 30 * time.Second
	}
	if plan.DiagnosticTimeout == 0 {
		plan.DiagnosticTimeout = 30 * time.Second
	}
	return plan
}

func kubeadmReadySmokeDiagnostics(plan KubeadmReadySmokePlan) GuestDiagnostics {
	plan = normalizeKubeadmReadySmokePlan(plan)
	return GuestDiagnostics{
		Timeout: plan.DiagnosticTimeout,
		Commands: []GuestCommandRequest{
			{Name: "ready-target-status", Argv: []string{"systemctl", "status", "katl-kubeadm-ready.target"}},
			{Name: "containerd-status", Argv: []string{"systemctl", "status", "containerd.service"}},
			{Name: "kubelet-status", Argv: []string{"systemctl", "status", "kubelet.service"}},
			{Name: "crictl-ps", Argv: []string{"crictl", "ps", "-a"}},
			{Name: "etc-kubernetes-mount", Argv: []string{"findmnt", "--target", "/etc/kubernetes", "--output", "SOURCE,TARGET,FSTYPE,OPTIONS"}},
		},
		Files: []GuestFileRequest{
			{Name: "node-metadata", Path: "/etc/katl/node.json"},
			{Name: "kubeadm-config", Path: plan.ConfigPath},
		},
		Journals: []GuestJournalRequest{
			{Name: "runtime-handoff", Units: []string{"katl-kubeadm-ready.target", "katl-generation-activate.service", "katl-runtime-handoff-status.service", "containerd.service", "kubelet.service"}},
		},
	}
}

func collectKubeadmReadySmokeDiagnostics(ctx context.Context, result Result, config KubeadmReadySmokeConfig, fallback KubeadmReadySmokeAgentSession) {
	smoke := normalizeKubeadmReadySmokePlan(config.Smoke)
	diagCtx, cancel := context.WithTimeout(ctx, smoke.DiagnosticTimeout)
	defer cancel()
	session, err := connectKubeadmReadySmokeAgent(diagCtx, config, smoke, result.VSock, result.Artifacts.VSockTranscript)
	if err == nil {
		defer session.Close()
		NewGuestControl(result, session).CollectDiagnostics(diagCtx, kubeadmReadySmokeDiagnostics(smoke))
		return
	}
	if fallback != nil {
		NewGuestControl(result, fallback).CollectDiagnostics(diagCtx, kubeadmReadySmokeDiagnostics(smoke))
	}
}

func connectKubeadmReadySmokeAgent(ctx context.Context, config KubeadmReadySmokeConfig, smoke KubeadmReadySmokePlan, plan VSockPlan, transcript string) (KubeadmReadySmokeAgentSession, error) {
	if !plan.Enabled {
		return nil, errors.New("kubeadm-ready smoke requires vmtest agent vsock")
	}
	smoke = normalizeKubeadmReadySmokePlan(smoke)
	connector := config.AgentConnector
	if connector == nil {
		connector = func(ctx context.Context, plan VSockPlan, transcript string) (KubeadmReadySmokeAgentSession, error) {
			return DialAgent(ctx, plan.GuestCID, plan.Port, transcript)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, smoke.AgentConnectTimeout)
	defer cancel()
	var lastErr error
	for {
		session, err := connector(ctx, plan, transcript)
		if err == nil {
			return session, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return nil, fmt.Errorf("connect vmtest agent: %w", lastErr)
			}
			return nil, fmt.Errorf("connect vmtest agent: %w", ctx.Err())
		case <-time.After(smoke.AgentConnectInterval):
		}
	}
}
