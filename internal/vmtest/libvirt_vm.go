package vmtest

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

type VMBoot struct {
	UKI           string
	ISO           string
	DiskFirst     bool
	Kernel        string
	Initrd        string
	CommandLine   []string
	EFITree       string
	EFIImage      string
	Image         string
	ImageFormat   DiskFormat
	ImageSnapshot bool
}

type VMConfig struct {
	Boot              VMBoot
	EFIDiskImage      bool
	PreseedDir        string
	PreseedImage      string
	MediaRunner       DiskRunner
	CommandPath       string
	VirshPath         string
	ScriptPath        string
	ImageTool         string
	LibvirtURI        string
	LibvirtNetwork    string
	OVMFCode          string
	OVMFVars          string
	PreserveNVRAM     bool
	KVM               KVMPolicy
	RAMMiB            int
	CPUs              int
	Phase             string
	Expect            string
	Timeout           time.Duration
	SerialIdleTimeout time.Duration
	PollInterval      time.Duration
	Network           VMNetworkConfig
	HostForwards      []HostForward
	SerialHooks       []SerialHook
	VSock             VSockConfig
	Agent             AgentControlConfig
	DomainMetadata    string
	PersistentSerial  bool
}

type SerialHook struct {
	Name   string
	Signal string
	Run    func(context.Context, SerialHookEvent) error
}

type SerialHookEvent struct {
	Result     Result
	Config     VMConfig
	Plan       VMPlan
	SerialText string
}

type VMNetworkMode string

const (
	VMNetworkUser VMNetworkMode = "user"
)

type VMNetworkConfig struct {
	Mode VMNetworkMode
	MAC  string
}

type HostForward struct {
	HostPort  int
	GuestPort int
}

type VSockConfig struct {
	Enabled  bool
	GuestCID uint32
	Port     uint32
}

type VSockPlan struct {
	Enabled  bool   `json:"enabled,omitempty"`
	GuestCID uint32 `json:"guestCid,omitempty"`
	Port     uint32 `json:"port,omitempty"`
	Device   string `json:"device,omitempty"`
}

type AgentControlConfig struct {
	RequireHealth bool
	Timeout       time.Duration
}

const defaultSerialIdleTimeout = 45 * time.Second

type VMPlan struct {
	CommandPath    string
	VirshPath      string
	ScriptPath     string
	Args           []string
	Accel          string
	DomainName     string
	MACAddress     string
	DomainXML      string
	DomainXMLFile  string
	LibvirtURI     string
	LibvirtNetwork string
	SerialLog      string
	CommandFile    string
	OVMFVars       string
	OVMFVarsSource string
	EFITree        string
	EFIImage       string
	PreseedImage   string
	PreseedDir     string
	DomainDisks    []libvirtDisk
	VSock          VSockPlan
}

type VMExecutor interface {
	Run(ctx context.Context, name string, args []string, serial io.Writer) error
}

type AgentHealthClient interface {
	Health(ctx context.Context) error
	Close() error
}

type ExecVMExecutor struct {
	TempDir string
}

func (e ExecVMExecutor) Run(ctx context.Context, name string, args []string, serial io.Writer) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = serial
	cmd.Stderr = serial
	if e.TempDir != "" {
		if err := os.MkdirAll(e.TempDir, 0o755); err != nil {
			return err
		}
		cmd.Env = append(os.Environ(), "TMPDIR="+e.TempDir)
	}
	return cmd.Run()
}

type LibvirtVMExecutor struct {
	TempDir           string
	VirshPath         string
	ScriptPath        string
	URI               string
	DomainName        string
	DomainXMLFile     string
	PollInterval      time.Duration
	CleanupTimeout    time.Duration
	PreserveOnFailure bool
	PreserveNVRAM     bool
	Preservation      *DomainPreservation
}

type DomainPreservation struct {
	Preserved bool
	Reason    string
}

var (
	errVMRunComplete = errors.New("vmtest VM run complete")
	errVMRunFailed   = errors.New("vmtest VM run failed")
)

func (e LibvirtVMExecutor) Run(ctx context.Context, _ string, _ []string, serial io.Writer) (runErr error) {
	if e.TempDir != "" {
		if err := os.MkdirAll(e.TempDir, 0o755); err != nil {
			return err
		}
	}
	if err := e.virsh(ctx, "define", e.DomainXMLFile); err != nil {
		return fmt.Errorf("define libvirt domain %q: %w", e.DomainName, err)
	}
	defined := true
	started := false
	defer func() {
		if defined && !e.preserveDomain(ctx, runErr, started) {
			args := []string{"undefine", e.DomainName}
			if e.PreserveNVRAM {
				args = append(args, "--keep-nvram")
			} else {
				args = append(args, "--nvram")
			}
			_ = e.cleanupVirsh(args...)
		}
	}()
	if err := e.virsh(ctx, "start", e.DomainName); err != nil {
		return fmt.Errorf("start libvirt domain %q: %w", e.DomainName, err)
	}
	started = true
	defer func() {
		if started && !e.preserveDomain(ctx, runErr, started) {
			_ = e.cleanupVirsh("destroy", e.DomainName)
		}
	}()
	consoleCtx, stopConsole := context.WithCancel(ctx)
	consoleDone, err := e.startConsoleCapture(consoleCtx, serial)
	if err != nil {
		stopConsole()
		return err
	}
	defer func() {
		stopConsole()
		if consoleDone != nil {
			select {
			case <-consoleDone:
			case <-time.After(e.cleanupTimeout()):
			}
		}
	}()
	interval := e.PollInterval
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		state, err := e.virshOutput(ctx, "domstate", e.DomainName)
		if err == nil {
			switch strings.TrimSpace(state) {
			case "shut off":
				if consoleDone != nil {
					select {
					case <-consoleDone:
						consoleDone = nil
					case <-time.After(e.cleanupTimeout()):
					}
				}
				return nil
			case "crashed":
				return errors.New("libvirt domain crashed")
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err, ok := <-consoleDone:
			if !ok {
				consoleDone = nil
				continue
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				return fmt.Errorf("libvirt console capture for %q exited: %w", e.DomainName, err)
			}
			consoleDone = nil
		case <-ticker.C:
		}
	}
}

func (e LibvirtVMExecutor) preserveDomain(ctx context.Context, runErr error, started bool) bool {
	if !e.PreserveOnFailure || runErr == nil || !started {
		e.recordPreservation(false, "")
		return false
	}
	if errors.Is(context.Cause(ctx), errVMRunComplete) {
		e.recordPreservation(false, "")
		return false
	}
	stateCtx, cancel := context.WithTimeout(context.Background(), e.cleanupTimeout())
	defer cancel()
	state, err := e.virshOutput(stateCtx, "domstate", e.DomainName)
	if err != nil {
		e.recordPreservation(false, "libvirt domain state could not be confirmed: "+err.Error())
		return false
	}
	switch strings.TrimSpace(state) {
	case "running", "paused", "idle", "in shutdown", "pmsuspended":
		e.recordPreservation(true, "debug-on-failure preserved live libvirt domain")
		return true
	default:
		e.recordPreservation(false, "libvirt domain is not live: "+strings.TrimSpace(state))
		return false
	}
}

func (e LibvirtVMExecutor) recordPreservation(preserved bool, reason string) {
	if e.Preservation == nil {
		return
	}
	if preserved || e.Preservation.Reason == "" {
		e.Preservation.Preserved = preserved
		e.Preservation.Reason = reason
	}
}

func (e LibvirtVMExecutor) cleanupVirsh(args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), e.cleanupTimeout())
	defer cancel()
	return e.virsh(ctx, args...)
}

func (e LibvirtVMExecutor) cleanupTimeout() time.Duration {
	timeout := e.CleanupTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return timeout
}

func (e LibvirtVMExecutor) virsh(ctx context.Context, args ...string) error {
	_, err := e.virshOutput(ctx, args...)
	return err
}

func (e LibvirtVMExecutor) virshOutput(ctx context.Context, args ...string) (string, error) {
	cmd := e.virshCommand(ctx, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", cmd.Path, strings.Join(cmd.Args[1:], " "), err, strings.TrimSpace(string(output)))
	}
	return string(output), nil
}

func (e LibvirtVMExecutor) startConsoleCapture(ctx context.Context, serial io.Writer) (<-chan error, error) {
	if serial == nil {
		return nil, nil
	}
	cmd := e.consoleCommand(ctx)
	cmd.Stdout = serial
	cmd.Stderr = serial
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start libvirt console capture for %q: %w", e.DomainName, err)
	}
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
		close(done)
	}()
	return done, nil
}

func (e LibvirtVMExecutor) consoleCommand(ctx context.Context) *exec.Cmd {
	virsh, virshArgs := e.virshInvocation("console", e.DomainName, "--force")
	parts := make([]string, 0, len(virshArgs)+1)
	parts = append(parts, shellQuote(virsh))
	for _, arg := range virshArgs {
		parts = append(parts, shellQuote(arg))
	}
	script := e.ScriptPath
	if script == "" {
		script = "script"
	}
	cmd := exec.CommandContext(ctx, script, "--return", "--flush", "--quiet", "--command", strings.Join(parts, " "), "/dev/null")
	if e.TempDir != "" {
		cmd.Env = append(os.Environ(), "TMPDIR="+e.TempDir)
	}
	configureProcessGroupCancel(cmd)
	return cmd
}

func (e LibvirtVMExecutor) virshCommand(ctx context.Context, args ...string) *exec.Cmd {
	virsh, fullArgs := e.virshInvocation(args...)
	cmd := exec.CommandContext(ctx, virsh, fullArgs...)
	if e.TempDir != "" {
		cmd.Env = append(os.Environ(), "TMPDIR="+e.TempDir)
	}
	configureProcessGroupCancel(cmd)
	return cmd
}

func configureProcessGroupCancel(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 100 * time.Millisecond
}

func (e LibvirtVMExecutor) virshInvocation(args ...string) (string, []string) {
	virsh := e.VirshPath
	if virsh == "" {
		virsh = "virsh"
	}
	fullArgs := []string{}
	if e.URI != "" {
		fullArgs = append(fullArgs, "-c", e.URI)
	}
	fullArgs = append(fullArgs, args...)
	return virsh, fullArgs
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

type VMRunner struct {
	Executor       VMExecutor
	AgentConnector func(ctx context.Context, plan VSockPlan, transcript string) (AgentHealthClient, error)
	probe          probe
}

type VMHandle struct {
	Result  Result
	Config  VMConfig
	Plan    VMPlan
	Started time.Time

	ctx           context.Context
	cancel        context.CancelCauseFunc
	timeoutCancel context.CancelFunc
	done          <-chan struct{}
	waitMu        sync.Mutex
	waitErr       error
	runner        VMRunner
	preservation  *DomainPreservation
}

func PlanVM(result Result, config VMConfig) (VMPlan, error) {
	return planVM(result, config, systemProbe())
}

func RunVM(ctx context.Context, result Result, config VMConfig) Result {
	return VMRunner{probe: systemProbe()}.Run(ctx, result, config)
}

func (r VMRunner) Run(ctx context.Context, result Result, config VMConfig) Result {
	return r.RunWithVM(ctx, result, config, func(vm *VMHandle) error {
		if vm.Config.Expect != "" {
			return vm.WaitForExpectedSerial()
		}
		if err := vm.Wait(); err != nil {
			return errors.New(libvirtDomainExitSummary(vm.ctx, err, vm.Plan.SerialLog))
		}
		return nil
	})
}

func (r VMRunner) RunWithVM(ctx context.Context, result Result, config VMConfig, run func(*VMHandle) error) Result {
	handle, setupResult, ok := r.startVM(ctx, result, config)
	if !ok {
		return setupResult
	}
	if run == nil {
		handle.StopFailure()
		_ = handle.Wait()
		return handle.Fail("vmtest VM handler is required")
	}
	if err := run(handle); err != nil {
		handle.StopFailure()
		_ = handle.Wait()
		return handle.Fail(err.Error())
	}
	handle.StopSuccess()
	_ = handle.Wait()
	return handle.Pass()
}

func (r VMRunner) startVM(ctx context.Context, result Result, config VMConfig) (*VMHandle, Result, bool) {
	started := time.Now().UTC()
	plan, err := planVM(result, config, r.probe)
	if err != nil {
		return nil, finishVM(result, phaseName(config), StatusFailed, err.Error(), started, time.Now().UTC()), false
	}
	result.DomainName = plan.DomainName
	result.MACAddress = plan.MACAddress
	result.VSock = plan.VSock
	if err := prepareVM(plan, config); err != nil {
		releaseVSock(result, plan, nil)
		return nil, finishVM(result, phaseName(config), StatusFailed, err.Error(), started, time.Now().UTC()), false
	}
	var timeoutCancel context.CancelFunc
	if config.Timeout > 0 {
		ctx, timeoutCancel = context.WithTimeout(ctx, config.Timeout)
	}
	runCtx, cancel := context.WithCancelCause(ctx)
	file, err := os.OpenFile(plan.SerialLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		cancel(errVMRunFailed)
		if timeoutCancel != nil {
			timeoutCancel()
		}
		releaseVSock(result, plan, nil)
		return nil, finishVM(result, phaseName(config), StatusFailed, err.Error(), started, time.Now().UTC()), false
	}
	executor := r.Executor
	defaultExecutor := executor == nil
	var preservation *DomainPreservation
	if executor == nil {
		executor, preservation = defaultVMExecutor(result, plan, config)
	}
	if defaultExecutor && config.Expect != "" && config.SerialIdleTimeout == 0 {
		config.SerialIdleTimeout = defaultSerialIdleTimeout
	}
	if defaultExecutor {
		go tailLiveSerial(runCtx, plan.SerialLog, os.Stderr, 100*time.Millisecond)
	}
	done := make(chan struct{})
	handle := &VMHandle{
		Result:        result,
		Config:        config,
		Plan:          plan,
		Started:       started,
		ctx:           runCtx,
		cancel:        cancel,
		timeoutCancel: timeoutCancel,
		done:          done,
		runner:        r,
		preservation:  preservation,
	}
	go func() {
		defer file.Close()
		err := executor.Run(runCtx, first(plan.VirshPath, plan.CommandPath), plan.Args, io.Writer(file))
		handle.waitMu.Lock()
		handle.waitErr = err
		handle.waitMu.Unlock()
		close(done)
	}()
	return handle, result, true
}

func (h *VMHandle) Wait() error {
	if h.done == nil {
		return nil
	}
	<-h.done
	h.waitMu.Lock()
	defer h.waitMu.Unlock()
	return h.waitErr
}

func (h *VMHandle) StopSuccess() {
	h.stop(errVMRunComplete)
}

func (h *VMHandle) StopFailure() {
	h.stop(errVMRunFailed)
}

func (h *VMHandle) stop(cause error) {
	if h.cancel != nil {
		h.cancel(cause)
	}
	if h.timeoutCancel != nil {
		h.timeoutCancel()
	}
}

func (h *VMHandle) WaitForSerialSignal(expect string, interval time.Duration) (bool, error) {
	return waitForSerialSignal(h.ctx, h.done, h.Wait, h.Plan.SerialLog, expect, interval)
}

func (h *VMHandle) CheckAgent() error {
	return h.runner.checkAgent(h.ctx, h.Result, h.Config)
}

func (h *VMHandle) Fail(failure string) Result {
	result := finishVM(h.Result, phaseName(h.Config), StatusFailed, failure, h.Started, time.Now().UTC())
	result = h.DebugResult(result)
	releaseVSock(h.Result, h.Plan, h.preservation)
	return result
}

func (h *VMHandle) Pass() Result {
	releaseVSock(h.Result, h.Plan, h.preservation)
	return finishVM(h.Result, phaseName(h.Config), StatusPassed, "", h.Started, time.Now().UTC())
}

func (h *VMHandle) DebugFailedResult() Result {
	result := h.Result
	result.Status = StatusFailed
	return h.DebugResult(result)
}

func (h *VMHandle) DebugResult(result Result) Result {
	return h.runner.withDebugTarget(result, h.Plan, h.preservation)
}

func (h *VMHandle) WaitForExpectedSerial() error {
	interval := h.Config.PollInterval
	if interval == 0 {
		interval = 250 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	hooksRun := make([]bool, len(h.Config.SerialHooks))
	lastSerialLen := -1
	lastSerialProgress := time.Now().UTC()
	for {
		serialText := readSerial(h.Plan.SerialLog)
		if len(serialText) != lastSerialLen {
			lastSerialLen = len(serialText)
			lastSerialProgress = time.Now().UTC()
		}
		if err := h.runner.runSerialHooks(h.ctx, h.Result, h.Config, h.Plan, serialText, hooksRun); err != nil {
			return err
		}
		if strings.Contains(serialText, h.Config.Expect) {
			if err := h.CheckAgent(); err != nil {
				return err
			}
			return nil
		}
		select {
		case <-h.done:
			err := h.Wait()
			serialText := readSerial(h.Plan.SerialLog)
			if hookErr := h.runner.runSerialHooks(h.ctx, h.Result, h.Config, h.Plan, serialText, hooksRun); hookErr != nil {
				return hookErr
			}
			if strings.Contains(serialText, h.Config.Expect) {
				if checkErr := h.CheckAgent(); checkErr != nil {
					return checkErr
				}
				return nil
			}
			if err == nil {
				err = errors.New("libvirt domain exited before serial signal appeared")
			}
			return fmt.Errorf("libvirt domain exited before serial signal %q appeared: %v", h.Config.Expect, err)
		case <-h.ctx.Done():
			return errors.New(libvirtTimeoutSummary(h.Plan.SerialLog))
		case <-ticker.C:
			if h.Config.SerialIdleTimeout > 0 && time.Since(lastSerialProgress) >= h.Config.SerialIdleTimeout {
				return errors.New(libvirtSerialIdleSummary(h.Plan.SerialLog, h.Config.SerialIdleTimeout))
			}
		}
	}
}

func defaultVMExecutor(result Result, plan VMPlan, config VMConfig) (VMExecutor, *DomainPreservation) {
	var preservation *DomainPreservation
	preserve := result.Debug != nil && result.Debug.OnFailure
	if preserve {
		preservation = &DomainPreservation{}
	}
	return LibvirtVMExecutor{
		TempDir:           filepath.Join(result.VMDir, "tmp"),
		VirshPath:         plan.VirshPath,
		ScriptPath:        plan.ScriptPath,
		URI:               plan.LibvirtURI,
		DomainName:        plan.DomainName,
		DomainXMLFile:     plan.DomainXMLFile,
		PreserveOnFailure: preserve,
		PreserveNVRAM:     config.PreserveNVRAM,
		Preservation:      preservation,
	}, preservation
}

func tailLiveSerial(ctx context.Context, path string, out io.Writer, interval time.Duration) {
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	var offset int64
	if info, err := os.Stat(path); err == nil {
		offset = info.Size()
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := copySerialFromOffset(path, out, &offset); err != nil && !errors.Is(err, os.ErrNotExist) {
			_, _ = fmt.Fprintf(out, "\nvmtest live serial tail error: %v\n", err)
		}
		select {
		case <-ctx.Done():
			_ = copySerialFromOffset(path, out, &offset)
			return
		case <-ticker.C:
		}
	}
}

func copySerialFromOffset(path string, out io.Writer, offset *int64) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.Seek(*offset, io.SeekStart); err != nil {
		return err
	}
	n, err := io.Copy(out, file)
	*offset += n
	return err
}

func libvirtDomainExitSummary(ctx context.Context, err error, serialLog string) string {
	summary := fmt.Sprintf("libvirt domain exited: %v", err)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		summary = libvirtTimeoutSummary(serialLog)
	}
	return summary
}

func libvirtTimeoutSummary(serialLog string) string {
	const prefix = "libvirt domain timed out"
	tail := serialTail(serialLog, 12, 4000)
	if tail == "" {
		return prefix
	}
	return prefix + "; serial tail:\n" + tail
}

func libvirtSerialIdleSummary(serialLog string, idle time.Duration) string {
	prefix := fmt.Sprintf("libvirt domain serial idle timed out after %s", idle)
	tail := serialTail(serialLog, 12, 4000)
	if tail == "" {
		return prefix
	}
	return prefix + "; serial tail:\n" + tail
}

func (r VMRunner) withDebugTarget(result Result, plan VMPlan, preservation *DomainPreservation) Result {
	if result.Debug == nil || !result.Debug.OnFailure || result.Status == StatusPassed || plan.DomainName == "" {
		return result
	}
	target := DebugTarget{
		DomainName:     plan.DomainName,
		LibvirtURI:     plan.LibvirtURI,
		SerialLog:      plan.SerialLog,
		ConsoleCommand: consoleCommandLine(plan.LibvirtURI, plan.DomainName),
		CleanupCommand: cleanupCommandLine(result.Artifacts.Result),
		ShellMode:      "serial-root",
		VSock:          plan.VSock,
	}
	if preservation != nil {
		target.Preserved = preservation.Preserved
		target.Reason = preservation.Reason
	}
	if target.Reason == "" {
		target.Reason = "debug-on-failure requested after VM failure"
	}
	result.Debug.Targets = append(result.Debug.Targets, target)
	return result
}

func debugMetadata(enabled bool) *DebugMetadata {
	if !enabled {
		return nil
	}
	return &DebugMetadata{OnFailure: true, Shell: true}
}

func consoleCommandLine(uri, domain string) string {
	args := []string{"virsh"}
	if strings.TrimSpace(uri) != "" {
		args = append(args, "-c", uri)
	}
	args = append(args, "console", domain, "--force")
	return shellCommand(args...)
}

func cleanupCommandLine(resultPath string) string {
	return shellCommand("scripts/vmtest-clean", resultPath)
}

func shellCommand(args ...string) string {
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		quoted = append(quoted, shellQuote(arg))
	}
	return strings.Join(quoted, " ")
}

func serialTail(path string, maxLines int, maxBytes int) string {
	if maxLines <= 0 || maxBytes <= 0 {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	text := strings.TrimSpace(string(data))
	if text == "" {
		return ""
	}
	lines := strings.Split(text, "\n")
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	tail := strings.TrimSpace(strings.Join(lines, "\n"))
	if len(tail) <= maxBytes {
		return tail
	}
	return tail[len(tail)-maxBytes:]
}

func (r VMRunner) runSerialHooks(ctx context.Context, result Result, config VMConfig, plan VMPlan, serialText string, hooksRun []bool) error {
	for i, hook := range config.SerialHooks {
		if hooksRun[i] || hook.Signal == "" || !strings.Contains(serialText, hook.Signal) {
			continue
		}
		if hook.Run == nil {
			return fmt.Errorf("serial hook %q has no runner", serialHookName(hook))
		}
		if err := hook.Run(ctx, SerialHookEvent{
			Result:     result,
			Config:     config,
			Plan:       plan,
			SerialText: serialText,
		}); err != nil {
			return fmt.Errorf("serial hook %q failed: %w", serialHookName(hook), err)
		}
		hooksRun[i] = true
	}
	return nil
}

func serialHookName(hook SerialHook) string {
	if hook.Name != "" {
		return hook.Name
	}
	return hook.Signal
}

func (r VMRunner) checkAgent(ctx context.Context, result Result, config VMConfig) error {
	if !config.Agent.RequireHealth {
		return nil
	}
	if !result.VSock.Enabled {
		return errors.New("vmtest agent health requires vsock")
	}
	timeout := config.Agent.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	agentCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	connector := r.AgentConnector
	if connector == nil {
		connector = func(ctx context.Context, plan VSockPlan, transcript string) (AgentHealthClient, error) {
			client, err := DialAgent(ctx, plan.GuestCID, plan.Port, transcript)
			if err != nil {
				return nil, err
			}
			return agentHealthClient{client: client}, nil
		}
	}
	poll := config.PollInterval
	if poll == 0 {
		poll = 250 * time.Millisecond
	}
	var lastErr error
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		client, err := connector(agentCtx, result.VSock, result.Artifacts.VSockTranscript)
		if err != nil {
			lastErr = fmt.Errorf("connect vmtest agent: %w", err)
		} else {
			if err := client.Health(agentCtx); err != nil {
				lastErr = fmt.Errorf("vmtest agent health failed: %w", err)
			} else {
				return client.Close()
			}
			_ = client.Close()
		}
		select {
		case <-agentCtx.Done():
			if lastErr != nil {
				return lastErr
			}
			return agentCtx.Err()
		case <-ticker.C:
		}
	}
}

type agentHealthClient struct {
	client *AgentClient
}

func (c agentHealthClient) Health(ctx context.Context) error {
	_, err := c.client.Health(ctx)
	return err
}

func (c agentHealthClient) Close() error {
	return c.client.Close()
}

func planVM(result Result, config VMConfig, probe probe) (VMPlan, error) {
	probe = probe.withDefaults()
	config = normalizeVM(config)
	virsh := config.VirshPath
	if virsh == "" {
		found, err := probe.lookPath("virsh")
		if err != nil {
			return VMPlan{}, PrereqError{Missing: []MissingPrerequisite{{
				Name:   "virsh",
				Detail: "not found in PATH",
			}}}
		}
		virsh = found
	}
	script := config.ScriptPath
	if script == "" {
		found, err := probe.lookPath("script")
		if err != nil {
			return VMPlan{}, PrereqError{Missing: []MissingPrerequisite{{
				Name:   "script",
				Detail: "not found in PATH",
			}}}
		}
		script = found
	}
	libvirtURI := first(config.LibvirtURI, probe.env("KATL_VMTEST_LIBVIRT_URI"), "qemu:///system")
	libvirtNetwork := first(config.LibvirtNetwork, probe.env("KATL_VMTEST_LIBVIRT_NETWORK"), "default")
	directKernel := config.Boot.Kernel != ""
	if directKernel {
		if _, err := probe.stat(config.Boot.Kernel); err != nil {
			return VMPlan{}, fmt.Errorf("VM kernel not readable: %w", err)
		}
		if config.Boot.Initrd != "" {
			if _, err := probe.stat(config.Boot.Initrd); err != nil {
				return VMPlan{}, fmt.Errorf("VM initrd not readable: %w", err)
			}
		}
	} else {
		if config.OVMFCode == "" {
			config.OVMFCode = probe.env("KATL_OVMF_CODE")
		}
		if config.OVMFVars == "" {
			config.OVMFVars = probe.env("KATL_OVMF_VARS")
		}
		if config.OVMFCode == "" || config.OVMFVars == "" {
			return VMPlan{}, errors.New("OVMF firmware is required: set OVMFCode/OVMFVars or KATL_OVMF_CODE/KATL_OVMF_VARS")
		}
		if _, err := probe.stat(config.OVMFCode); err != nil {
			return VMPlan{}, fmt.Errorf("OVMF code not readable: %w", err)
		}
		if _, err := probe.stat(config.OVMFVars); err != nil {
			return VMPlan{}, fmt.Errorf("OVMF vars not readable: %w", err)
		}
		if config.Boot.ISO != "" {
			if _, err := probe.stat(config.Boot.ISO); err != nil {
				return VMPlan{}, fmt.Errorf("VM ISO not readable: %w", err)
			}
		}
	}
	accel, err := libvirtAccel(config.KVM, probe)
	if err != nil {
		return VMPlan{}, err
	}
	if len(config.HostForwards) > 0 {
		return VMPlan{}, errors.New("host forwards are not supported by libvirt VM execution")
	}
	serial := result.Artifacts.InstallerSerial
	if runtimeSerialPhase(config.Phase) {
		serial = result.Artifacts.RuntimeSerial
	}
	disks, efiTree, efiImage, preseedImage, preseedDir, err := vmDomainDisks(result, config)
	if err != nil {
		return VMPlan{}, err
	}
	vsock, err := planVSock(result.RunID, config.VSock, probe)
	if err != nil {
		return VMPlan{}, err
	}
	domainName := "katl-" + clean(result.RunID)
	macAddress := strings.TrimSpace(config.Network.MAC)
	domainXML, err := libvirtDomainXML(libvirtDomain{
		Name:             domainName,
		Accel:            accel,
		RAMMiB:           config.RAMMiB,
		CPUs:             config.CPUs,
		OVMFCode:         firstPath(!directKernel, config.OVMFCode),
		OVMFVars:         firstPath(!directKernel, filepath.Join(result.VMDir, "OVMF_VARS.fd")),
		Kernel:           config.Boot.Kernel,
		Initrd:           config.Boot.Initrd,
		CommandLine:      strings.Join(config.Boot.CommandLine, " "),
		SerialLog:        serial,
		Network:          libvirtNetwork,
		MACAddress:       macAddress,
		Disks:            disks,
		VSock:            vsock,
		Metadata:         first(config.DomainMetadata, "katl/vmtest"),
		PersistentSerial: config.PersistentSerial,
	})
	if err != nil {
		return VMPlan{}, fmt.Errorf("marshal libvirt domain XML: %w", err)
	}
	xmlFile := result.Artifacts.DomainXML
	args := []string{"-c", libvirtURI, "define", xmlFile}
	return VMPlan{
		CommandPath:    virsh,
		VirshPath:      virsh,
		ScriptPath:     script,
		Args:           args,
		Accel:          accel,
		DomainName:     domainName,
		MACAddress:     macAddress,
		DomainXML:      domainXML,
		DomainXMLFile:  xmlFile,
		LibvirtURI:     libvirtURI,
		LibvirtNetwork: libvirtNetwork,
		SerialLog:      serial,
		CommandFile:    result.Artifacts.LaunchCommand,
		OVMFVars:       firstPath(!directKernel, filepath.Join(result.VMDir, "OVMF_VARS.fd")),
		OVMFVarsSource: firstPath(!directKernel, config.OVMFVars),
		EFITree:        efiTree,
		EFIImage:       efiImage,
		PreseedImage:   preseedImage,
		PreseedDir:     preseedDir,
		DomainDisks:    disks,
		VSock:          vsock,
	}, nil
}

func prepareVM(plan VMPlan, config VMConfig) error {
	if err := os.MkdirAll(filepath.Dir(plan.SerialLog), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(plan.CommandFile), 0o755); err != nil {
		return err
	}
	if plan.OVMFVarsSource != "" {
		if sameFilePath(plan.OVMFVarsSource, plan.OVMFVars) {
			info, err := os.Stat(plan.OVMFVarsSource)
			if err != nil {
				return fmt.Errorf("stat OVMF vars image: %w", err)
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("OVMF vars image %s is not a regular file", plan.OVMFVarsSource)
			}
		} else if err := copyFile(plan.OVMFVarsSource, plan.OVMFVars, 0o600); err != nil {
			return err
		}
	}
	if config.Boot.UKI != "" {
		bootPath := filepath.Join(plan.EFITree, "EFI", "BOOT", "BOOTX64.EFI")
		if err := copyFile(config.Boot.UKI, bootPath, 0o644); err != nil {
			return err
		}
	}
	if plan.EFIImage != "" {
		if config.Boot.EFIImage != "" {
			info, err := os.Stat(config.Boot.EFIImage)
			if err != nil {
				return fmt.Errorf("stat VM EFI image: %w", err)
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("VM EFI image %s is not a regular file", config.Boot.EFIImage)
			}
		} else {
			if err := createFATImage(context.Background(), plan.EFITree, plan.EFIImage, "KATLEFI", config.MediaRunner); err != nil {
				return err
			}
		}
	}
	if config.PreseedDir != "" {
		info, err := os.Stat(config.PreseedDir)
		if err != nil {
			return fmt.Errorf("stat VM preseed dir: %w", err)
		}
		if !info.IsDir() {
			return fmt.Errorf("VM preseed path %s is not a directory", config.PreseedDir)
		}
	}
	if plan.PreseedDir != "" && plan.PreseedImage != "" {
		if err := createFATImage(context.Background(), plan.PreseedDir, plan.PreseedImage, "KATLSEED", config.MediaRunner); err != nil {
			return err
		}
	}
	for _, disk := range plan.DomainDisks {
		if disk.BackingPath == "" {
			continue
		}
		imageTool := first(config.ImageTool, os.Getenv("KATL_VMTEST_IMAGE_TOOL"), "qemu-img")
		if err := runTool(context.Background(), imageTool, "create", "-f", string(DiskQCOW2), "-F", string(disk.BackingFormat), "-b", disk.BackingPath, disk.Path); err != nil {
			return err
		}
	}
	if config.PreseedImage != "" {
		info, err := os.Stat(config.PreseedImage)
		if err != nil {
			return fmt.Errorf("stat VM preseed image: %w", err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("VM preseed image %s is not a regular file", config.PreseedImage)
		}
	}
	if err := os.WriteFile(plan.DomainXMLFile, []byte(plan.DomainXML), 0o644); err != nil {
		return err
	}
	return os.WriteFile(plan.CommandFile, []byte(commandLine(plan.VirshPath, plan.Args)+"\n"), 0o644)
}

// PrepareVM materializes a planned VM's firmware state, generated boot media,
// domain XML, and launch command without defining or starting the domain.
func PrepareVM(plan VMPlan, config VMConfig) error {
	return prepareVM(plan, config)
}

func normalizeVM(config VMConfig) VMConfig {
	if config.KVM == "" {
		config.KVM = KVMAuto
	}
	if config.RAMMiB == 0 {
		// Keep vmtest guests small; if kubeadm or boot tests fail, check guest OOM logs before raising this.
		config.RAMMiB = 2048
	}
	if config.CPUs == 0 {
		config.CPUs = 2
	}
	if config.Network.Mode == "" {
		config.Network.Mode = VMNetworkUser
	}
	if config.Boot.ImageFormat == "" {
		config.Boot.ImageFormat = DiskRaw
	}
	if len(config.Boot.CommandLine) == 0 && config.Boot.Kernel != "" {
		config.Boot.CommandLine = []string{
			"console=ttyS0,115200n8",
			"systemd.log_target=console",
			"loglevel=6",
		}
	}
	if config.VSock.Enabled && config.VSock.Port == 0 {
		config.VSock.Port = 10240
	}
	return config
}

func serialHas(path, text string) bool {
	return strings.Contains(readSerial(path), text)
}

func readSerial(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

func libvirtAccel(policy KVMPolicy, probe probe) (string, error) {
	switch policy {
	case KVMOff:
		return "qemu", nil
	case KVMOn:
		if err := probe.access("/dev/kvm"); err != nil {
			return "", fmt.Errorf("/dev/kvm required by KVM policy on: %w", err)
		}
		return "kvm", nil
	default:
		if err := probe.access("/dev/kvm"); err == nil {
			return "kvm", nil
		}
		return "qemu", nil
	}
}

type libvirtDisk struct {
	Path          string
	Format        DiskFormat
	Target        string
	Serial        string
	BackingPath   string
	BackingFormat DiskFormat
	Device        string
	Bus           string
	ReadOnly      bool
	BootOrder     int
}

type libvirtDomain struct {
	Name             string
	Accel            string
	RAMMiB           int
	CPUs             int
	OVMFCode         string
	OVMFVars         string
	Kernel           string
	Initrd           string
	CommandLine      string
	SerialLog        string
	Network          string
	MACAddress       string
	Disks            []libvirtDisk
	VSock            VSockPlan
	Metadata         string
	PersistentSerial bool
}

type domainXML struct {
	XMLName  xml.Name       `xml:"domain"`
	Type     string         `xml:"type,attr"`
	Name     string         `xml:"name"`
	Metadata domainMetadata `xml:"metadata"`
	Memory   domainMemory   `xml:"memory"`
	VCPU     int            `xml:"vcpu"`
	OS       domainOS       `xml:"os"`
	Features domainFeatures `xml:"features"`
	Devices  domainDevices  `xml:"devices"`
}

type domainMetadata struct {
	VMTest domainVMTest `xml:"https://katlos.io/xmlns/vmtest/1 vmtest"`
}

type domainVMTest struct {
	Value string `xml:",chardata"`
}

type domainMemory struct {
	Unit  string `xml:"unit,attr"`
	Value int    `xml:",chardata"`
}

type domainOS struct {
	Type    domainOSType  `xml:"type"`
	Loader  *domainLoader `xml:"loader,omitempty"`
	NVRAM   string        `xml:"nvram,omitempty"`
	Kernel  string        `xml:"kernel,omitempty"`
	Initrd  string        `xml:"initrd,omitempty"`
	Cmdline string        `xml:"cmdline,omitempty"`
}

type domainOSType struct {
	Arch    string `xml:"arch,attr"`
	Machine string `xml:"machine,attr"`
	Value   string `xml:",chardata"`
}

type domainLoader struct {
	ReadOnly string `xml:"readonly,attr"`
	Type     string `xml:"type,attr"`
	Path     string `xml:",chardata"`
}

type domainFeatures struct {
	ACPI struct{} `xml:"acpi"`
	APIC struct{} `xml:"apic"`
}

type domainDevices struct {
	RNG       domainRNG       `xml:"rng"`
	Interface domainInterface `xml:"interface"`
	Disks     []domainDisk    `xml:"disk"`
	Serial    domainSerial    `xml:"serial"`
	Console   domainConsole   `xml:"console"`
	VSock     *domainVSock    `xml:"vsock,omitempty"`
}

type domainRNG struct {
	Model   string           `xml:"model,attr"`
	Backend domainRNGBackend `xml:"backend"`
}

type domainRNGBackend struct {
	Model string `xml:"model,attr"`
	Path  string `xml:",chardata"`
}

type domainInterface struct {
	Type   string                `xml:"type,attr"`
	MAC    *domainInterfaceMAC   `xml:"mac,omitempty"`
	Source domainInterfaceSource `xml:"source"`
	Model  domainInterfaceModel  `xml:"model"`
}

type domainInterfaceMAC struct {
	Address string `xml:"address,attr"`
}

type domainInterfaceSource struct {
	Network string `xml:"network,attr"`
}

type domainInterfaceModel struct {
	Type string `xml:"type,attr"`
}

type domainDisk struct {
	Type     string           `xml:"type,attr"`
	Device   string           `xml:"device,attr"`
	Driver   domainDiskDriver `xml:"driver"`
	Source   domainDiskSource `xml:"source"`
	Target   domainDiskTarget `xml:"target"`
	Serial   string           `xml:"serial,omitempty"`
	ReadOnly *struct{}        `xml:"readonly,omitempty"`
	Boot     *domainBoot      `xml:"boot,omitempty"`
}

type domainBoot struct {
	Order int `xml:"order,attr"`
}

type domainDiskDriver struct {
	Name  string `xml:"name,attr"`
	Type  string `xml:"type,attr"`
	Cache string `xml:"cache,attr,omitempty"`
}

type domainDiskSource struct {
	File string `xml:"file,attr"`
}

type domainDiskTarget struct {
	Dev string `xml:"dev,attr"`
	Bus string `xml:"bus,attr"`
}

type domainSerial struct {
	Type   string              `xml:"type,attr"`
	Source *domainSerialSource `xml:"source,omitempty"`
	Log    *domainSerialLog    `xml:"log,omitempty"`
	Target domainSerialTarget  `xml:"target"`
}

type domainSerialLog struct {
	File   string `xml:"file,attr"`
	Append string `xml:"append,attr"`
}

type domainSerialSource struct {
	Path string `xml:"path,attr"`
}

type domainSerialTarget struct {
	Port int `xml:"port,attr"`
}

type domainConsole struct {
	Type   string              `xml:"type,attr"`
	Source *domainSerialSource `xml:"source,omitempty"`
	Target domainConsoleTarget `xml:"target"`
}

type domainConsoleTarget struct {
	Type string `xml:"type,attr"`
	Port int    `xml:"port,attr"`
}

type domainVSock struct {
	Model string         `xml:"model,attr"`
	CID   domainVSockCID `xml:"cid"`
}

type domainVSockCID struct {
	Auto    string `xml:"auto,attr"`
	Address uint32 `xml:"address,attr"`
}

func vmDomainDisks(result Result, config VMConfig) ([]libvirtDisk, string, string, string, string, error) {
	boot := config.Boot
	var disks []libvirtDisk
	efiTree := filepath.Join(result.VMDir, "efi")
	efiImage := ""
	preseedImage := ""
	preseedDir := ""
	nextTarget := byte('a')
	var diskErr error
	add := func(path string, format DiskFormat, serial string, snapshot bool) {
		if diskErr != nil {
			return
		}
		target := "vd" + string(nextTarget)
		disk := libvirtDisk{
			Path:   path,
			Format: format,
			Target: target,
			Serial: serial,
		}
		if snapshot {
			snapshotPath := filepath.Join(result.VMDir, target+".snapshot.qcow2")
			if sameFilePath(path, snapshotPath) {
				diskErr = fmt.Errorf("VM disk snapshot %s would use itself as backing file", snapshotPath)
				return
			}
			disk.BackingPath = path
			disk.BackingFormat = format
			disk.Format = DiskQCOW2
			disk.Path = snapshotPath
		}
		disks = append(disks, libvirtDisk{
			Path:          disk.Path,
			Format:        disk.Format,
			Target:        disk.Target,
			Serial:        disk.Serial,
			BackingPath:   disk.BackingPath,
			BackingFormat: disk.BackingFormat,
		})
		nextTarget++
	}
	bootModes := 0
	for _, value := range []string{boot.UKI, boot.ISO, boot.EFITree, boot.EFIImage, boot.Kernel} {
		if value != "" {
			bootModes++
		}
	}
	if bootModes > 1 {
		return nil, "", "", "", "", errors.New("VM boot requires at most one of ISO, UKI, EFI tree, EFI image, or kernel")
	}
	if boot.Kernel != "" {
		if boot.Image != "" {
			add(boot.Image, boot.ImageFormat, "katl-boot", boot.ImageSnapshot)
		}
	} else if boot.ISO != "" {
		isoBootOrder := 1
		if boot.DiskFirst {
			isoBootOrder = 2
		}
		disks = append(disks, libvirtDisk{
			Path:      boot.ISO,
			Format:    DiskRaw,
			Target:    "sda",
			Device:    "cdrom",
			Bus:       "sata",
			ReadOnly:  true,
			BootOrder: isoBootOrder,
		})
		if boot.Image != "" {
			add(boot.Image, boot.ImageFormat, "katl-boot", boot.ImageSnapshot)
		}
	} else if boot.EFIImage != "" {
		efiImage = boot.EFIImage
		add(efiImage, DiskRaw, "katl-efi", false)
		if boot.Image == "" {
			return nil, "", "", "", "", errors.New("VM boot from EFI image requires disk image")
		}
		add(boot.Image, boot.ImageFormat, "katl-boot", boot.ImageSnapshot)
	} else if boot.UKI != "" {
		efiImage = filepath.Join(result.VMDir, "efi.img")
		add(efiImage, DiskRaw, "katl-efi", false)
		if boot.Image != "" {
			add(boot.Image, boot.ImageFormat, "katl-boot", boot.ImageSnapshot)
		}
	} else if boot.EFITree != "" {
		efiTree = boot.EFITree
		efiImage = filepath.Join(result.VMDir, "efi.img")
		add(efiImage, DiskRaw, "katl-efi", false)
		if boot.Image == "" {
			return nil, "", "", "", "", errors.New("VM boot from EFI tree requires disk image")
		}
		add(boot.Image, boot.ImageFormat, "katl-boot", boot.ImageSnapshot)
	} else {
		if boot.Image == "" {
			return nil, "", "", "", "", errors.New("VM boot requires UKI or disk image")
		}
		add(boot.Image, boot.ImageFormat, "katl-boot", boot.ImageSnapshot)
	}
	for _, disk := range result.Disks {
		add(disk.HostPath, disk.Format, "katl-"+clean(disk.Name), false)
	}
	if config.PreseedImage != "" {
		add(config.PreseedImage, DiskRaw, "katl-seed", false)
	} else if config.PreseedDir != "" {
		preseedDir = config.PreseedDir
		preseedImage = filepath.Join(result.VMDir, "preseed.img")
		add(preseedImage, DiskRaw, "katl-seed", false)
	}
	if diskErr != nil {
		return nil, "", "", "", "", diskErr
	}
	if boot.ISO != "" && boot.DiskFirst {
		for i := range disks {
			if disks[i].Device == "" {
				disks[i].BootOrder = 1
				break
			}
		}
	}
	return disks, efiTree, efiImage, preseedImage, preseedDir, nil
}

func libvirtDomainXML(domain libvirtDomain) (string, error) {
	doc := domainXML{
		Type:     domain.Accel,
		Name:     domain.Name,
		Metadata: domainMetadata{VMTest: domainVMTest{Value: domain.Metadata}},
		Memory:   domainMemory{Unit: "MiB", Value: domain.RAMMiB},
		VCPU:     domain.CPUs,
		OS: domainOS{
			Type: domainOSType{Arch: "x86_64", Machine: "q35", Value: "hvm"},
		},
		Features: domainFeatures{},
		Devices: domainDevices{
			RNG: domainRNG{
				Model:   "virtio",
				Backend: domainRNGBackend{Model: "random", Path: "/dev/urandom"},
			},
			Interface: domainInterface{
				Type:   "network",
				Source: domainInterfaceSource{Network: domain.Network},
				Model:  domainInterfaceModel{Type: "virtio"},
			},
			Serial: domainSerial{
				Type:   "pty",
				Target: domainSerialTarget{Port: 0},
			},
			Console: domainConsole{
				Type:   "pty",
				Target: domainConsoleTarget{Type: "serial", Port: 0},
			},
		},
	}
	if domain.Kernel != "" {
		doc.OS.Kernel = domain.Kernel
		doc.OS.Initrd = domain.Initrd
		doc.OS.Cmdline = domain.CommandLine
	} else {
		doc.OS.Loader = &domainLoader{ReadOnly: "yes", Type: "pflash", Path: domain.OVMFCode}
		doc.OS.NVRAM = domain.OVMFVars
	}
	if strings.TrimSpace(domain.MACAddress) != "" {
		doc.Devices.Interface.MAC = &domainInterfaceMAC{Address: strings.TrimSpace(domain.MACAddress)}
	}
	if domain.PersistentSerial {
		doc.Devices.Serial.Log = &domainSerialLog{File: domain.SerialLog, Append: "on"}
	}
	for _, disk := range domain.Disks {
		device := disk.Device
		if device == "" {
			device = "disk"
		}
		bus := disk.Bus
		if bus == "" {
			bus = "virtio"
		}
		xmlDisk := domainDisk{
			Type:   "file",
			Device: device,
			Driver: domainDiskDriver{
				Name: "qemu",
				Type: string(disk.Format),
			},
			Source: domainDiskSource{File: disk.Path},
			Target: domainDiskTarget{Dev: disk.Target, Bus: bus},
			Serial: disk.Serial,
		}
		if disk.ReadOnly {
			xmlDisk.ReadOnly = &struct{}{}
		}
		if disk.BootOrder > 0 {
			xmlDisk.Boot = &domainBoot{Order: disk.BootOrder}
		}
		doc.Devices.Disks = append(doc.Devices.Disks, xmlDisk)
	}
	if domain.VSock.Enabled {
		doc.Devices.VSock = &domainVSock{
			Model: "virtio",
			CID:   domainVSockCID{Auto: "no", Address: domain.VSock.GuestCID},
		}
	}
	data, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}
	return string(data) + "\n", nil
}

func firstPath(ok bool, path string) string {
	if !ok {
		return ""
	}
	return path
}

var cidReservations = struct {
	sync.Mutex
	used map[uint32]string
}{used: map[uint32]string{}}

func planVSock(runID string, config VSockConfig, probe probe) (VSockPlan, error) {
	if !config.Enabled {
		return VSockPlan{}, nil
	}
	if err := probe.access("/dev/vhost-vsock"); err != nil {
		return VSockPlan{}, fmt.Errorf("/dev/vhost-vsock required for vmtest vsock: %w", err)
	}
	cid, err := reserveCID(runID, config.GuestCID)
	if err != nil {
		return VSockPlan{}, err
	}
	port := config.Port
	if port == 0 {
		port = 10240
	}
	return VSockPlan{
		Enabled:  true,
		GuestCID: cid,
		Port:     port,
		Device:   fmt.Sprintf("virtio-vsock,cid=%d", cid),
	}, nil
}

func reserveCID(runID string, requested uint32) (uint32, error) {
	if requested != 0 {
		return reserveExactCID(runID, requested)
	}
	base := cidForRun(runID)
	for offset := uint32(0); offset < 256; offset++ {
		cid := base + offset
		if cid < 1024 {
			continue
		}
		if reserved, err := reserveExactCID(runID, cid); err == nil {
			return reserved, nil
		}
	}
	return 0, fmt.Errorf("no free vmtest vsock guest CID for run %q", runID)
}

func reserveExactCID(runID string, cid uint32) (uint32, error) {
	if cid < 3 {
		return 0, fmt.Errorf("vsock guest CID %d is reserved", cid)
	}
	cidReservations.Lock()
	defer cidReservations.Unlock()
	if owner, ok := cidReservations.used[cid]; ok && owner != runID {
		return 0, fmt.Errorf("vsock guest CID %d already reserved by %s", cid, owner)
	}
	cidReservations.used[cid] = runID
	return cid, nil
}

func releaseVSock(result Result, plan VMPlan, preservation *DomainPreservation) {
	if !plan.VSock.Enabled {
		return
	}
	if preservation != nil && preservation.Preserved {
		return
	}
	releaseCID(result.RunID, plan.VSock.GuestCID)
}

func releaseCID(runID string, cid uint32) {
	cidReservations.Lock()
	defer cidReservations.Unlock()
	if owner, ok := cidReservations.used[cid]; ok && owner == runID {
		delete(cidReservations.used, cid)
	}
}

func cidForRun(runID string) uint32 {
	hash := sha256.Sum256([]byte(runID))
	value := binary.BigEndian.Uint32(hash[:4])
	return 1024 + value%60000
}

func finishVM(result Result, phase string, status Status, failure string, started, finished time.Time) Result {
	result.Status = status
	result.FailureSummary = failure
	if result.Started.IsZero() {
		result.Started = started
	}
	result.Finished = finished
	result.DurationMS = finished.Sub(result.Started).Milliseconds()
	result.addPhase(phase, status, failure, started, finished)
	return result
}

func phaseName(config VMConfig) string {
	if config.Phase != "" {
		return config.Phase
	}
	return "vm"
}

func runtimeSerialPhase(phase string) bool {
	return phase == "runtime" || phase == "kubeadm-api-smoke"
}

func copyFile(src, dst string, mode os.FileMode) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, data, mode)
}

func sameFilePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

func commandLine(name string, args []string) string {
	parts := append([]string{name}, args...)
	quoted := make([]string, 0, len(parts))
	for _, part := range parts {
		quoted = append(quoted, quoteArg(part))
	}
	return strings.Join(quoted, " ")
}

func quoteArg(arg string) string {
	if arg == "" {
		return "''"
	}
	if strings.IndexFunc(arg, func(r rune) bool {
		return r == '\'' || r == ' ' || r == '\t' || r == '\n'
	}) == -1 {
		return arg
	}
	return "'" + strings.ReplaceAll(arg, "'", "'\\''") + "'"
}
