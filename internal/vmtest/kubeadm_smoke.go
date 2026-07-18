package vmtest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	DefaultKubeadmConfigPath       = "/etc/katl/kubeadm/control-plane/config.yaml"
	DefaultProjectedKubernetesPath = "/var/lib/katl/kubernetes/etc-kubernetes"
)

type KubeadmAPISmokeConfig struct {
	Runtime        InstalledRuntimeConfig
	Smoke          KubeadmAPISmokePlan
	AgentConnector KubeadmSmokeAgentConnector
}

type KubeadmSmokeAgentSession interface {
	GuestAgentClient
	Close() error
}

type KubeadmSmokeAgentConnector func(ctx context.Context, plan VSockPlan, transcript string) (KubeadmSmokeAgentSession, error)

type KubeadmAPISmokePlan struct {
	ConfigPath            string
	ProjectedSource       string
	ReadyTimeout          time.Duration
	ReadyPollInterval     time.Duration
	AgentConnectTimeout   time.Duration
	AgentConnectInterval  time.Duration
	KubeadmTimeout        time.Duration
	APIServerTimeout      time.Duration
	APIServerPollInterval time.Duration
	CommandTimeout        time.Duration
	DiagnosticTimeout     time.Duration
}

func RunInstalledKubeadmAPISmoke(ctx context.Context, result Result, config KubeadmAPISmokeConfig, runner VMRunner) Result {
	runtime := config.Runtime
	runtime.RequireVMTestAgent = true
	if err := PrepareInstalledRuntime(result, runtime); err != nil {
		return finishVM(result, "kubeadm-api-smoke", StatusFailed, err.Error(), result.Started, runnerTime())
	}
	vm := runtime.VM
	vm.Phase = "kubeadm-api-smoke"
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
			return errors.New("libvirt domain exited after serial signal before kubeadm API smoke")
		}
		session, err := connectKubeadmSmokeAgent(handle.ctx, config, config.Smoke, handle.Result.VSock, handle.Result.Artifacts.VSockTranscript)
		if err != nil {
			return err
		}
		defer session.Close()

		guest := NewGuestControl(handle.Result, session)
		if err := RunKubeadmAPISmoke(handle.ctx, guest, config.Smoke); err != nil {
			collectKubeadmSmokeDiagnostics(handle.ctx, handle.Result, config, session)
			return err
		}
		return nil
	})
}

func RunKubeadmAPISmoke(ctx context.Context, guest *GuestControl, plan KubeadmAPISmokePlan) error {
	if guest == nil {
		return errors.New("guest control is required")
	}
	plan = normalizeKubeadmAPISmokePlan(plan)
	if err := waitKubeadmReady(ctx, guest, plan); err != nil {
		return err
	}
	if err := RunKatlcSmoke(ctx, guest); err != nil {
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
	if !kubernetesProjectionSourceMatches(source, plan.ProjectedSource) {
		return fmt.Errorf("/etc/kubernetes is backed by %q, want %q", source, plan.ProjectedSource)
	}
	for _, command := range []GuestCommandRequest{
		{Name: "test", Argv: []string{"test", "-x", "/usr/bin/kubeadm"}},
		{Name: "test", Argv: []string{"test", "-x", "/usr/bin/kubelet"}},
		{Name: "test", Argv: []string{"test", "-x", "/usr/bin/kubectl"}},
		{Name: "test", Argv: []string{"test", "-x", "/usr/bin/crictl"}},
		{Name: "systemctl", Argv: []string{"systemctl", "is-active", "--quiet", "containerd.service"}},
		visibilityCommand("networkctl-status", []string{"networkctl", "status", "--all"}, 512<<10),
		visibilityCommand("resolvectl-status", []string{"resolvectl", "status"}, 512<<10),
		visibilityCommand("ip-route", []string{"ip", "route"}, 128<<10),
		{Name: "crictl-info", Argv: []string{"crictl", "info"}, SensitiveOutput: true},
	} {
		if _, err := guest.RunCommand(ctx, command); err != nil {
			return err
		}
	}
	if err := waitGuestRegistryDNS(ctx, guest, plan); err != nil {
		return err
	}
	if _, err := guest.RunCommand(ctx, GuestCommandRequest{
		Name:        "kubeadm-init",
		Argv:        []string{"kubeadm", "init", "--config", plan.ConfigPath, "--skip-token-print", "--skip-phases=addon/coredns,addon/kube-proxy"},
		Timeout:     plan.KubeadmTimeout,
		StdoutLimit: 1024 << 10,
		StderrLimit: 1024 << 10,
	}); err != nil {
		return err
	}
	for _, path := range []string{
		"/etc/kubernetes/admin.conf",
		"/etc/kubernetes/manifests/kube-apiserver.yaml",
		"/etc/kubernetes/manifests/kube-controller-manager.yaml",
		"/etc/kubernetes/manifests/kube-scheduler.yaml",
		"/etc/kubernetes/manifests/etcd.yaml",
	} {
		if _, err := guest.RunCommand(ctx, GuestCommandRequest{Name: filepath.Base(path), Argv: []string{"test", "-f", path}}); err != nil {
			return err
		}
	}
	if err := waitKubeAPIServer(ctx, guest, plan); err != nil {
		return err
	}
	if _, err := guest.RunCommand(ctx, GuestCommandRequest{
		Name:    "kubectl-readyz",
		Argv:    []string{"kubectl", "--kubeconfig", "/etc/kubernetes/admin.conf", "get", "--raw=/readyz"},
		Timeout: plan.CommandTimeout,
	}); err != nil {
		return err
	}
	return nil
}

func visibilityCommand(name string, argv []string, stdoutLimit uint32) GuestCommandRequest {
	return GuestCommandRequest{
		Name:         name,
		Argv:         argv,
		StdoutLimit:  stdoutLimit,
		StderrLimit:  128 << 10,
		AllowFailure: true,
	}
}

func kubernetesProjectionSourceMatches(source string, projected string) bool {
	source = strings.TrimSpace(source)
	projected = strings.TrimSpace(projected)
	if source == projected {
		return true
	}
	statePath, ok := strings.CutPrefix(projected, "/var")
	if !ok || statePath == "" {
		return false
	}
	return strings.HasSuffix(source, "["+statePath+"]")
}

func waitKubeadmReady(ctx context.Context, guest *GuestControl, plan KubeadmAPISmokePlan) error {
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

func waitGuestRegistryDNS(ctx context.Context, guest *GuestControl, plan KubeadmAPISmokePlan) error {
	timeout := plan.ReadyTimeout
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	interval := plan.ReadyPollInterval
	if interval == 0 {
		interval = 2 * time.Second
	}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		if _, err := guest.RunCommand(ctx, GuestCommandRequest{
			Name:    "registry-dns",
			Argv:    []string{"getent", "hosts", "registry.k8s.io"},
			Timeout: plan.CommandTimeout,
		}); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("wait for guest DNS resolution of registry.k8s.io: %w", lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

func waitKubeAPIServer(ctx context.Context, guest *GuestControl, plan KubeadmAPISmokePlan) error {
	timeout := plan.APIServerTimeout
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	interval := plan.APIServerPollInterval
	if interval == 0 {
		interval = 2 * time.Second
	}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		record, err := guest.RunCommand(ctx, GuestCommandRequest{
			Name:    "kube-apiserver-running",
			Argv:    []string{"crictl", "ps", "--name", "kube-apiserver", "--state", "Running", "-q"},
			Timeout: plan.CommandTimeout,
		})
		if err == nil {
			output, readErr := readCommandStdout(record)
			if readErr != nil {
				return readErr
			}
			if strings.TrimSpace(output) != "" {
				return nil
			}
			lastErr = errors.New("kube-apiserver container is not running")
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("wait for kube-apiserver running: %w", lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

func normalizeKubeadmAPISmokePlan(plan KubeadmAPISmokePlan) KubeadmAPISmokePlan {
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
	if plan.KubeadmTimeout == 0 {
		plan.KubeadmTimeout = 5 * time.Minute
	}
	if plan.APIServerTimeout == 0 {
		plan.APIServerTimeout = 2 * time.Minute
	}
	if plan.APIServerPollInterval == 0 {
		plan.APIServerPollInterval = 2 * time.Second
	}
	if plan.CommandTimeout == 0 {
		plan.CommandTimeout = 30 * time.Second
	}
	if plan.DiagnosticTimeout == 0 {
		plan.DiagnosticTimeout = 30 * time.Second
	}
	return plan
}

func kubeadmSmokeDiagnostics(plan KubeadmAPISmokePlan) GuestDiagnostics {
	plan = normalizeKubeadmAPISmokePlan(plan)
	return GuestDiagnostics{
		Timeout: plan.DiagnosticTimeout,
		Commands: []GuestCommandRequest{
			{Name: "ready-target-status", Argv: []string{"systemctl", "status", "katl-kubeadm-ready.target"}},
			{Name: "containerd-status", Argv: []string{"systemctl", "status", "containerd.service"}},
			{Name: "kubelet-status", Argv: []string{"systemctl", "status", "kubelet.service"}},
			{Name: "networkd-status", Argv: []string{"systemctl", "status", "systemd-networkd.service"}},
			visibilityCommand("networkctl-status", []string{"networkctl", "status", "--all"}, 512<<10),
			visibilityCommand("resolvectl-status", []string{"resolvectl", "status"}, 512<<10),
			visibilityCommand("ip-route", []string{"ip", "route"}, 128<<10),
			{Name: "crictl-ps", Argv: []string{"crictl", "ps", "-a"}},
			{Name: "etc-kubernetes-mount", Argv: []string{"findmnt", "--target", "/etc/kubernetes", "--output", "SOURCE,TARGET,FSTYPE,OPTIONS"}},
		},
		Files: []GuestFileRequest{
			{Name: "node-metadata", Path: "/etc/katl/node.json"},
			{Name: "kubeadm-config", Path: plan.ConfigPath},
			{Name: "networkd-vmtest-dhcp", Path: "/etc/systemd/network/80-katl-vmtest-dhcp.network"},
		},
		Journals: []GuestJournalRequest{
			{Name: "runtime-handoff", Units: []string{"katl-kubeadm-ready.target", "katl-generation-activate.service", "katl-runtime-handoff-status.service", "containerd.service", "kubelet.service"}},
		},
	}
}

func collectKubeadmSmokeDiagnostics(ctx context.Context, result Result, config KubeadmAPISmokeConfig, fallback KubeadmSmokeAgentSession) {
	smoke := normalizeKubeadmAPISmokePlan(config.Smoke)
	diagCtx, cancel := context.WithTimeout(ctx, smoke.DiagnosticTimeout)
	defer cancel()
	session, err := connectKubeadmSmokeAgent(diagCtx, config, smoke, result.VSock, result.Artifacts.VSockTranscript)
	if err == nil {
		defer session.Close()
		NewGuestControl(result, session).CollectDiagnostics(diagCtx, kubeadmSmokeDiagnostics(smoke))
		return
	}
	if fallback != nil {
		NewGuestControl(result, fallback).CollectDiagnostics(diagCtx, kubeadmSmokeDiagnostics(smoke))
	}
}

func connectKubeadmSmokeAgent(ctx context.Context, config KubeadmAPISmokeConfig, smoke KubeadmAPISmokePlan, plan VSockPlan, transcript string) (KubeadmSmokeAgentSession, error) {
	if !plan.Enabled {
		return nil, errors.New("kubeadm API smoke requires vmtest agent vsock")
	}
	smoke = normalizeKubeadmAPISmokePlan(smoke)
	connector := config.AgentConnector
	if connector == nil {
		connector = func(ctx context.Context, plan VSockPlan, transcript string) (KubeadmSmokeAgentSession, error) {
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

func waitForSerialSignal(ctx context.Context, done <-chan struct{}, wait func() error, serialLog string, expect string, interval time.Duration) (bool, error) {
	if interval == 0 {
		interval = 250 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if serialHas(serialLog, expect) {
			select {
			case <-done:
				return true, nil
			default:
			}
			return false, nil
		}
		select {
		case <-done:
			err := wait()
			if serialHas(serialLog, expect) {
				return true, nil
			}
			if err == nil {
				err = errors.New("libvirt domain exited before serial signal appeared")
			}
			return true, fmt.Errorf("libvirt domain exited before serial signal %q appeared: %w", expect, err)
		case <-ctx.Done():
			return false, fmt.Errorf("libvirt domain timed out waiting for serial signal %q", expect)
		case <-ticker.C:
		}
	}
}

func readCommandStdout(record GuestCommandArtifact) (string, error) {
	if record.Stdout == "" {
		return "", errors.New("command stdout artifact is unavailable")
	}
	data, err := os.ReadFile(record.Stdout)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

var _ KubeadmSmokeAgentSession = (*AgentClient)(nil)
var _ io.Closer = (*AgentClient)(nil)
