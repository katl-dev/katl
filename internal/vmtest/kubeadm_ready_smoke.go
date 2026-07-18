package vmtest

import (
	"context"
	"errors"
	"fmt"
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
	vm.Expect = first(vm.Expect, runtime.Expect, runtimeBootSignal)
	vm.Boot = VMBoot{
		Image:         runtime.Disk,
		ImageFormat:   diskFormat(runtime.DiskFormat),
		ImageSnapshot: true,
	}
	vm.VSock.Enabled = true

	return runner.RunWithVM(ctx, result, vm, func(handle *VMHandle) error {
		domainDone, err := handle.WaitForSerialSignal(vm.Expect, vm.PollInterval)
		if err != nil {
			return err
		}
		if domainDone {
			return errors.New("libvirt domain exited after serial signal before kubeadm-ready smoke")
		}
		session, err := connectKubeadmReadySmokeAgent(handle.ctx, config, config.Smoke, handle.Result.VSock, handle.Result.Artifacts.VSockTranscript)
		if err != nil {
			return err
		}
		defer session.Close()

		guest := NewGuestControl(handle.Result, session)
		if err := RunKubeadmReadySmoke(handle.ctx, guest, config.Smoke); err != nil {
			collectKubeadmReadySmokeDiagnostics(handle.ctx, handle.Result, config, session)
			return err
		}
		return nil
	})
}

func RunKubeadmReadySmoke(ctx context.Context, guest *GuestControl, plan KubeadmReadySmokePlan) error {
	if guest == nil {
		return errors.New("guest control is required")
	}
	plan = normalizeKubeadmReadySmokePlan(plan)
	if err := waitKubeadmReadySmoke(ctx, guest, plan); err != nil {
		return err
	}
	if err := RunKatlcSmoke(ctx, guest); err != nil {
		return err
	}
	return nil
}

func waitKubeadmReadySmoke(ctx context.Context, guest *GuestControl, plan KubeadmReadySmokePlan) error {
	deadline := time.Now().Add(plan.ReadyTimeout)
	var lastErr error
	for {
		if _, err := guest.Systemctl(ctx, "start", "katl-kubeadm-ready.target"); err != nil {
			lastErr = err
		} else if _, err := guest.Systemctl(ctx, "is-active", "--quiet", "katl-kubeadm-ready.target"); err == nil {
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
			{Name: "sysext-status", Argv: []string{"systemctl", "status", "systemd-sysext.service"}},
			{Name: "confext-status", Argv: []string{"systemctl", "status", "systemd-confext.service"}},
			{Name: "var-mount-status", Argv: []string{"systemctl", "status", "var.mount"}},
			{Name: "etc-kubernetes-mount-status", Argv: []string{"systemctl", "status", "etc-kubernetes.mount"}},
			{Name: "extension-listing", Argv: []string{"find", "/run/extensions", "/run/confexts", "-maxdepth", "2", "-type", "l", "-o", "-type", "f"}},
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
			{Name: "runtime-handoff", Units: []string{"katl-kubeadm-ready.target", "katl-generation-activate.service", "katl-runtime-handoff-status.service", "systemd-sysext.service", "systemd-confext.service", "var.mount", "etc-kubernetes.mount", "containerd.service", "kubelet.service"}},
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
