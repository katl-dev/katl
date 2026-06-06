package vmtest

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type VMBoot struct {
	UKI           string
	EFITree       string
	Image         string
	ImageFormat   DiskFormat
	ImageSnapshot bool
}

type VMConfig struct {
	Boot         VMBoot
	PreseedDir   string
	QEMUPath     string
	OVMFCode     string
	OVMFVars     string
	KVM          KVMPolicy
	RAMMiB       int
	CPUs         int
	Phase        string
	Expect       string
	Timeout      time.Duration
	PollInterval time.Duration
	Network      VMNetworkConfig
	HostForwards []HostForward
	SerialHooks  []SerialHook
	VSock        VSockConfig
	Agent        AgentControlConfig
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
	VMNetworkUser   VMNetworkMode = "user"
	VMNetworkBridge VMNetworkMode = "bridge"
)

type VMNetworkConfig struct {
	Mode   VMNetworkMode
	Bridge string
	Helper string
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

type VMPlan struct {
	QEMUPath       string
	Args           []string
	Accel          string
	SerialLog      string
	CommandFile    string
	OVMFVars       string
	OVMFVarsSource string
	EFITree        string
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

type VMRunner struct {
	Executor       VMExecutor
	AgentConnector func(ctx context.Context, plan VSockPlan, transcript string) (AgentHealthClient, error)
	probe          probe
}

func PlanVM(result Result, config VMConfig) (VMPlan, error) {
	return planVM(result, config, systemProbe())
}

func RunVM(ctx context.Context, result Result, config VMConfig) Result {
	return VMRunner{probe: systemProbe()}.Run(ctx, result, config)
}

func (r VMRunner) Run(ctx context.Context, result Result, config VMConfig) Result {
	started := time.Now().UTC()
	plan, err := planVM(result, config, r.probe)
	if err != nil {
		return finishVM(result, phaseName(config), StatusFailed, err.Error(), started, time.Now().UTC())
	}
	result.VSock = plan.VSock
	if err := prepareVM(plan, config); err != nil {
		return finishVM(result, phaseName(config), StatusFailed, err.Error(), started, time.Now().UTC())
	}
	if config.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, config.Timeout)
		defer cancel()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	file, err := os.OpenFile(plan.SerialLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return finishVM(result, phaseName(config), StatusFailed, err.Error(), started, time.Now().UTC())
	}
	defer file.Close()
	executor := r.Executor
	if executor == nil {
		executor = defaultVMExecutor(result)
	}
	done := make(chan error, 1)
	go func() {
		done <- executor.Run(ctx, plan.QEMUPath, plan.Args, file)
	}()
	if config.Expect != "" {
		return r.waitSerial(ctx, cancel, done, result, config, plan, started)
	}
	if err := <-done; err != nil {
		summary := fmt.Sprintf("qemu exited: %v", err)
		if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
			summary = "qemu timed out"
		}
		return finishVM(result, phaseName(config), StatusFailed, summary, started, time.Now().UTC())
	}
	return finishVM(result, phaseName(config), StatusPassed, "", started, time.Now().UTC())
}

func defaultVMExecutor(result Result) VMExecutor {
	return ExecVMExecutor{TempDir: filepath.Join(result.QEMUDir, "tmp")}
}

func (r VMRunner) waitSerial(ctx context.Context, cancel context.CancelFunc, done <-chan error, result Result, config VMConfig, plan VMPlan, started time.Time) Result {
	interval := config.PollInterval
	if interval == 0 {
		interval = 250 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	hooksRun := make([]bool, len(config.SerialHooks))
	for {
		serialText := readSerial(plan.SerialLog)
		if err := r.runSerialHooks(ctx, result, config, plan, serialText, hooksRun); err != nil {
			cancel()
			<-done
			return finishVM(result, phaseName(config), StatusFailed, err.Error(), started, time.Now().UTC())
		}
		if strings.Contains(serialText, config.Expect) {
			if err := r.checkAgent(ctx, result, config); err != nil {
				cancel()
				<-done
				return finishVM(result, phaseName(config), StatusFailed, err.Error(), started, time.Now().UTC())
			}
			cancel()
			<-done
			return finishVM(result, phaseName(config), StatusPassed, "", started, time.Now().UTC())
		}
		select {
		case err := <-done:
			serialText := readSerial(plan.SerialLog)
			if hookErr := r.runSerialHooks(ctx, result, config, plan, serialText, hooksRun); hookErr != nil {
				return finishVM(result, phaseName(config), StatusFailed, hookErr.Error(), started, time.Now().UTC())
			}
			if strings.Contains(serialText, config.Expect) {
				if checkErr := r.checkAgent(ctx, result, config); checkErr != nil {
					return finishVM(result, phaseName(config), StatusFailed, checkErr.Error(), started, time.Now().UTC())
				}
				return finishVM(result, phaseName(config), StatusPassed, "", started, time.Now().UTC())
			}
			if err == nil {
				err = errors.New("qemu exited before serial signal appeared")
			}
			return finishVM(result, phaseName(config), StatusFailed, fmt.Sprintf("qemu exited before serial signal %q appeared: %v", config.Expect, err), started, time.Now().UTC())
		case <-ctx.Done():
			cancel()
			<-done
			return finishVM(result, phaseName(config), StatusFailed, "qemu timed out", started, time.Now().UTC())
		case <-ticker.C:
		}
	}
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
	qemu := config.QEMUPath
	if qemu == "" {
		found, err := probe.lookPath("qemu-system-x86_64")
		if err != nil {
			return VMPlan{}, PrereqError{Missing: []MissingPrerequisite{{
				Name:   "qemu-system-x86_64",
				Detail: "not found in PATH",
			}}}
		}
		qemu = found
	}
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
	accel, err := qemuAccel(config.KVM, probe)
	if err != nil {
		return VMPlan{}, err
	}
	network, err := qemuNetdev(config.Network, config.HostForwards, probe)
	if err != nil {
		return VMPlan{}, err
	}
	serial := result.Artifacts.InstallerSerial
	if runtimeSerialPhase(config.Phase) {
		serial = result.Artifacts.RuntimeSerial
	}
	args := []string{
		"-machine", "q35,accel=" + accel,
		"-cpu", "max",
		"-smp", strconv.Itoa(config.CPUs),
		"-m", strconv.Itoa(config.RAMMiB),
		"-display", "none",
		"-monitor", "none",
		"-serial", "file:" + serial,
		"-drive", "if=pflash,format=raw,readonly=on,file=" + config.OVMFCode,
		"-drive", "if=pflash,format=raw,file=" + filepath.Join(result.QEMUDir, "OVMF_VARS.fd"),
	}
	driveArgs, efiTree, err := vmDrives(result, config)
	if err != nil {
		return VMPlan{}, err
	}
	args = append(args, driveArgs...)
	vsock, err := planVSock(result.RunID, config.VSock, qemu, probe)
	if err != nil {
		return VMPlan{}, err
	}
	args = append(args,
		"-device", "virtio-rng-pci",
		"-netdev", network,
		"-device", "virtio-net-pci,netdev=net0",
	)
	if vsock.Enabled {
		args = append(args, "-device", vsock.Device)
		result.VSock = vsock
	}
	return VMPlan{
		QEMUPath:       qemu,
		Args:           args,
		Accel:          accel,
		SerialLog:      serial,
		CommandFile:    result.Artifacts.QEMUCommand,
		OVMFVars:       filepath.Join(result.QEMUDir, "OVMF_VARS.fd"),
		OVMFVarsSource: config.OVMFVars,
		EFITree:        efiTree,
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
	if err := copyFile(plan.OVMFVarsSource, plan.OVMFVars, 0o600); err != nil {
		return err
	}
	if config.Boot.UKI != "" {
		bootPath := filepath.Join(plan.EFITree, "EFI", "BOOT", "BOOTX64.EFI")
		if err := copyFile(config.Boot.UKI, bootPath, 0o644); err != nil {
			return err
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
	return os.WriteFile(plan.CommandFile, []byte(commandLine(plan.QEMUPath, plan.Args)+"\n"), 0o644)
}

func normalizeVM(config VMConfig) VMConfig {
	if config.KVM == "" {
		config.KVM = KVMAuto
	}
	if config.RAMMiB == 0 {
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

func qemuAccel(policy KVMPolicy, probe probe) (string, error) {
	switch policy {
	case KVMOff:
		return "tcg", nil
	case KVMOn:
		if err := probe.access("/dev/kvm"); err != nil {
			return "", fmt.Errorf("/dev/kvm required by KVM policy on: %w", err)
		}
		return "kvm", nil
	default:
		if err := probe.access("/dev/kvm"); err == nil {
			return "kvm", nil
		}
		return "tcg", nil
	}
}

func vmDrives(result Result, config VMConfig) ([]string, string, error) {
	boot := config.Boot
	var args []string
	index := 0
	add := func(spec string) {
		args = append(args, "-drive", fmt.Sprintf("if=virtio,index=%d,%s", index, spec))
		index++
	}
	efiTree := filepath.Join(result.QEMUDir, "efi")
	if boot.UKI != "" && boot.EFITree != "" {
		return nil, "", errors.New("VM boot requires at most one of UKI or EFI tree")
	}
	if boot.UKI != "" {
		add("format=raw,file=fat:rw:" + efiTree)
		if boot.Image != "" {
			add(imageSpec(boot))
		}
	} else if boot.EFITree != "" {
		add("format=raw,file=fat:rw:" + boot.EFITree)
		if boot.Image == "" {
			return nil, "", errors.New("VM boot from EFI tree requires disk image")
		}
		add(imageSpec(boot))
	} else {
		if boot.Image == "" {
			return nil, "", errors.New("VM boot requires UKI or disk image")
		}
		add(imageSpec(boot))
	}
	for _, disk := range result.Disks {
		id := fmt.Sprintf("katldisk%d", index)
		args = append(args,
			"-drive", fmt.Sprintf("if=none,id=%s,format=%s,file=%s", id, disk.Format, disk.HostPath),
			"-device", fmt.Sprintf("virtio-blk-pci,drive=%s,serial=katl-%s", id, clean(disk.Name)),
		)
		index++
	}
	if config.PreseedDir != "" {
		id := fmt.Sprintf("katlseed%d", index)
		args = append(args,
			"-drive", fmt.Sprintf("if=none,id=%s,format=raw,file=fat:rw:%s", id, config.PreseedDir),
			"-device", fmt.Sprintf("virtio-blk-pci,drive=%s,serial=katl-seed", id),
		)
	}
	return args, efiTree, nil
}

func imageSpec(boot VMBoot) string {
	spec := fmt.Sprintf("format=%s,file=%s", boot.ImageFormat, boot.Image)
	if boot.ImageSnapshot {
		spec += ",snapshot=on"
	}
	return spec
}

func qemuNetdev(network VMNetworkConfig, forwards []HostForward, probe probe) (string, error) {
	switch network.Mode {
	case "", VMNetworkUser:
		spec := "user,id=net0"
		for _, forward := range forwards {
			spec += fmt.Sprintf(",hostfwd=tcp:127.0.0.1:%d-:%d", forward.HostPort, forward.GuestPort)
		}
		return spec, nil
	case VMNetworkBridge:
		if len(forwards) > 0 {
			return "", errors.New("host forwards require user-mode networking")
		}
		bridge := strings.TrimSpace(first(network.Bridge, probe.env("KATL_VMTEST_BRIDGE")))
		if bridge == "" {
			return "", errors.New("bridge networking requires VMNetworkConfig.Bridge or KATL_VMTEST_BRIDGE")
		}
		if err := validateBridgeName(bridge); err != nil {
			return "", err
		}
		spec := "bridge,id=net0,br=" + bridge
		helper := strings.TrimSpace(first(network.Helper, probe.env("KATL_QEMU_BRIDGE_HELPER")))
		if helper != "" {
			if strings.ContainsAny(helper, " \t\n\r,") {
				return "", fmt.Errorf("bridge helper path %q contains unsupported whitespace or comma", helper)
			}
			spec += ",helper=" + helper
		}
		return spec, nil
	default:
		return "", fmt.Errorf("VM network mode %q is unsupported", network.Mode)
	}
}

func validateBridgeName(value string) error {
	if value == "" {
		return errors.New("bridge name is required")
	}
	if len(value) > 15 {
		return fmt.Errorf("bridge name %q is longer than 15 characters", value)
	}
	for _, r := range value {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.'
		if !ok {
			return fmt.Errorf("bridge name %q contains unsupported character %q", value, r)
		}
	}
	return nil
}

var cidReservations = struct {
	sync.Mutex
	used map[uint32]string
}{used: map[uint32]string{}}

func planVSock(runID string, config VSockConfig, qemu string, probe probe) (VSockPlan, error) {
	if !config.Enabled {
		return VSockPlan{}, nil
	}
	if err := probe.access("/dev/vhost-vsock"); err != nil {
		return VSockPlan{}, fmt.Errorf("/dev/vhost-vsock required for vmtest vsock: %w", err)
	}
	if err := checkQEMUVSock(qemu, probe); err != nil {
		return VSockPlan{}, err
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
		Device:   fmt.Sprintf("vhost-vsock-pci,id=vsock0,guest-cid=%d", cid),
	}, nil
}

func checkQEMUVSock(qemu string, probe probe) error {
	output, err := probe.output(qemu, "-device", "vhost-vsock-pci,help")
	if err != nil {
		return fmt.Errorf("QEMU does not expose vhost-vsock-pci: %w", err)
	}
	if !strings.Contains(string(output), "vhost-vsock-pci") && !strings.Contains(string(output), "guest-cid") {
		return fmt.Errorf("QEMU vhost-vsock-pci help output did not describe guest-cid")
	}
	return nil
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
	return "qemu"
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
