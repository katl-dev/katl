package vmtest

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
)

type InstalledRuntimeNodeConfig struct {
	Name    string
	Runtime InstalledRuntimeConfig
}

type RunningInstalledRuntimeNode struct {
	Name   string
	Result Result
	VSock  VSockPlan

	handle *VMHandle
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
	vm.Expect = first(vm.Expect, runtime.Expect, runtimeKernelBootSignal)
	vm.Boot = VMBoot{
		Image:         runtime.Disk,
		ImageFormat:   diskFormat(runtime.DiskFormat),
		ImageSnapshot: true,
		ImageSerial:   "katl-root",
	}
	vm.VSock.Enabled = true
	vm.Agent.RequireHealth = true

	handle, setupResult, ok := runner.startVM(ctx, result, vm)
	if !ok {
		result = setupResult
		return fail(errors.New(setupResult.FailureSummary))
	}
	result = handle.Result
	failWithDebug := func(err error) (RunningInstalledRuntimeNode, error) {
		handle.StopFailure()
		_ = handle.Wait()
		result = handle.DebugFailedResult()
		return fail(err)
	}
	domainDone, err := handle.WaitForSerialSignal(vm.Expect, vm.PollInterval)
	if err != nil {
		return failWithDebug(err)
	}
	if domainDone {
		return failWithDebug(fmt.Errorf("libvirt domain exited after serial signal before installed runtime node %q could be used", name))
	}
	if err := handle.CheckAgent(); err != nil {
		return failWithDebug(err)
	}
	if handle.Plan.MACAddress != "" {
		lease, err := WaitLibvirtLease(handle.ctx, handle.Plan.VirshPath, handle.Plan.LibvirtURI, handle.Plan.LibvirtNetwork, handle.Plan.MACAddress, 30*time.Second)
		if err != nil {
			return failWithDebug(err)
		}
		// The network reserves the planned address for the MAC, so another
		// address means the reservation did not reach the guest.
		if planned := vm.Network.Address; planned != "" && lease.IPAddress != planned {
			return failWithDebug(fmt.Errorf("installed runtime node %q was given %s by libvirt network %s, not its planned address %s", name, lease.IPAddress, handle.Plan.LibvirtNetwork, planned))
		}
		result.IPAddress = lease.IPAddress
		if err := writeJSON(result.Artifacts.LibvirtLease, lease); err != nil {
			return failWithDebug(fmt.Errorf("write libvirt lease artifact: %w", err))
		}
		// Callers use the address as the node's management endpoint. Neither
		// the lease nor vmtest agent health orders it after katlc-agent binds:
		// a reserved address is leased at once, and katlc-agent.service is
		// Type=simple, so systemd reports it active before it listens.
		address, err := netip.ParseAddr(lease.IPAddress)
		if err != nil {
			return failWithDebug(fmt.Errorf("libvirt lease address %q: %w", lease.IPAddress, err))
		}
		management := inventory.ManagementAddress{Host: inventory.HostFromAddr(address)}
		if err := waitManagementEndpoint(handle.ctx, name, management.Endpoint().AddrPort(), managementStartupTimeout, runner.managementDial); err != nil {
			return failWithDebug(err)
		}
	}
	// The node is returned only after the boot has settled, so callers never
	// act on a guest that is still starting sshd, mounts or the boot trial.
	if err := runner.waitBootComplete(handle.ctx, result, handle.Plan.VSock); err != nil {
		return failWithDebug(err)
	}
	if err := writeInstalledRuntimeNodeResult(result, StatusPassed, "", started); err != nil {
		handle.StopSuccess()
		_ = handle.Wait()
		return RunningInstalledRuntimeNode{}, err
	}
	return RunningInstalledRuntimeNode{
		Name:   name,
		Result: result,
		VSock:  handle.Plan.VSock,
		handle: handle,
	}, nil
}

const (
	managementStartupTimeout = time.Minute
	bootSettleTimeout        = 2 * time.Minute
)

func (r VMRunner) waitBootComplete(ctx context.Context, result Result, plan VSockPlan) error {
	ctx, cancel := context.WithTimeout(ctx, bootSettleTimeout)
	defer cancel()
	client, err := r.connectGuest(ctx, plan, result.Artifacts.VSockTranscript)
	if err != nil {
		return fmt.Errorf("connect guest control to wait for boot completion: %w", err)
	}
	defer client.Close()
	return NewGuestControl(result, client).WaitBootComplete(ctx)
}

// WaitManagementEndpoint waits until the node's katlc agent accepts TCP
// connections at endpoint, as katlctl first observes it.
func WaitManagementEndpoint(ctx context.Context, node string, endpoint netip.AddrPort, timeout time.Duration) error {
	return waitManagementEndpoint(ctx, node, endpoint, timeout, nil)
}

type managementDialFunc func(context.Context, string, string) (net.Conn, error)

func waitManagementEndpoint(ctx context.Context, node string, endpoint netip.AddrPort, timeout time.Duration, dial managementDialFunc) error {
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	for {
		dialCtx, dialCancel := context.WithTimeout(ctx, 2*time.Second)
		conn, err := dial(dialCtx, "tcp", endpoint.String())
		dialCancel()
		if err == nil {
			return conn.Close()
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s did not accept management connections on %s: %w", node, endpoint, lastErr)
		case <-ticker.C:
		}
	}
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
	if n.handle == nil {
		return nil
	}
	n.handle.StopSuccess()
	return n.handle.Wait()
}

func (n RunningInstalledRuntimeNode) StopFailure(failure string) error {
	if n.handle == nil {
		return nil
	}
	n.handle.StopFailure()
	err := n.handle.Wait()
	result := n.handle.DebugFailedResult()
	if strings.TrimSpace(failure) == "" {
		failure = "parent scenario failed"
	}
	if writeErr := writeInstalledRuntimeNodeResult(result, StatusFailed, failure, n.handle.Started); writeErr != nil {
		if err != nil {
			return fmt.Errorf("%w; write installed runtime node result: %v", err, writeErr)
		}
		return writeErr
	}
	return err
}

// PowerCycle cuts power to the node and starts it again, then returns the node
// as it is addressed on its new boot. Copies of the node taken earlier keep the
// previous vsock CID and must be replaced by the returned value.
func (n RunningInstalledRuntimeNode) PowerCycle(ctx context.Context) (RunningInstalledRuntimeNode, error) {
	if n.handle == nil {
		return n, fmt.Errorf("installed runtime node %q has no running VM", n.Name)
	}
	if err := n.handle.PowerCycle(ctx); err != nil {
		return n, err
	}
	n.VSock = n.handle.Plan.VSock
	n.Result.VSock = n.VSock
	return n, nil
}

func (n RunningInstalledRuntimeNode) WaitForPoweroff(ctx context.Context) error {
	if n.handle == nil || n.handle.done == nil {
		return nil
	}
	select {
	case <-n.handle.done:
		return n.handle.Wait()
	case <-ctx.Done():
		return fmt.Errorf("wait for installed runtime node %q to power off: %w", n.Name, ctx.Err())
	}
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
