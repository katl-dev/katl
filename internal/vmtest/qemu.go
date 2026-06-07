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
	"time"
)

type VMBoot struct {
	UKI           string
	Kernel        string
	Initrd        string
	CommandLine   []string
	EFITree       string
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
	QEMUPath          string
	VirshPath         string
	ImageTool         string
	LibvirtURI        string
	LibvirtNetwork    string
	OVMFCode          string
	OVMFVars          string
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

const defaultSerialIdleTimeout = 45 * time.Second

type VMPlan struct {
	QEMUPath       string
	VirshPath      string
	Args           []string
	Accel          string
	DomainName     string
	DomainXML      string
	DomainXMLFile  string
	LibvirtURI     string
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
	TempDir        string
	VirshPath      string
	URI            string
	DomainName     string
	DomainXMLFile  string
	PollInterval   time.Duration
	CleanupTimeout time.Duration
}

func (e LibvirtVMExecutor) Run(ctx context.Context, _ string, _ []string, _ io.Writer) error {
	if e.TempDir != "" {
		if err := os.MkdirAll(e.TempDir, 0o755); err != nil {
			return err
		}
	}
	if err := e.virsh(ctx, "define", e.DomainXMLFile); err != nil {
		return fmt.Errorf("define libvirt domain %q: %w", e.DomainName, err)
	}
	defined := true
	defer func() {
		if defined {
			_ = e.cleanupVirsh("undefine", e.DomainName, "--nvram")
		}
	}()
	if err := e.virsh(ctx, "start", e.DomainName); err != nil {
		return fmt.Errorf("start libvirt domain %q: %w", e.DomainName, err)
	}
	defer func() {
		_ = e.cleanupVirsh("destroy", e.DomainName)
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
				return nil
			case "crashed":
				return errors.New("libvirt domain crashed")
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (e LibvirtVMExecutor) cleanupVirsh(args ...string) error {
	timeout := e.CleanupTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return e.virsh(ctx, args...)
}

func (e LibvirtVMExecutor) virsh(ctx context.Context, args ...string) error {
	_, err := e.virshOutput(ctx, args...)
	return err
}

func (e LibvirtVMExecutor) virshOutput(ctx context.Context, args ...string) (string, error) {
	virsh := e.VirshPath
	if virsh == "" {
		virsh = "virsh"
	}
	fullArgs := []string{}
	if e.URI != "" {
		fullArgs = append(fullArgs, "-c", e.URI)
	}
	fullArgs = append(fullArgs, args...)
	cmd := exec.CommandContext(ctx, virsh, fullArgs...)
	if e.TempDir != "" {
		cmd.Env = append(os.Environ(), "TMPDIR="+e.TempDir)
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", virsh, strings.Join(fullArgs, " "), err, strings.TrimSpace(string(output)))
	}
	return string(output), nil
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
	defaultExecutor := executor == nil
	if executor == nil {
		executor = defaultVMExecutor(result, plan)
	}
	if defaultExecutor && config.Expect != "" && config.SerialIdleTimeout == 0 {
		config.SerialIdleTimeout = defaultSerialIdleTimeout
	}
	if defaultExecutor {
		go tailLiveSerial(ctx, plan.SerialLog, os.Stderr, 100*time.Millisecond)
	}
	serial := io.Writer(file)
	done := make(chan error, 1)
	go func() {
		done <- executor.Run(ctx, first(plan.VirshPath, plan.QEMUPath), plan.Args, serial)
	}()
	if config.Expect != "" {
		return r.waitSerial(ctx, cancel, done, result, config, plan, started)
	}
	if err := <-done; err != nil {
		summary := fmt.Sprintf("libvirt domain exited: %v", err)
		if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
			summary = qemuTimeoutSummary(plan.SerialLog)
		}
		return finishVM(result, phaseName(config), StatusFailed, summary, started, time.Now().UTC())
	}
	return finishVM(result, phaseName(config), StatusPassed, "", started, time.Now().UTC())
}

func defaultVMExecutor(result Result, plan VMPlan) VMExecutor {
	return LibvirtVMExecutor{
		TempDir:       filepath.Join(result.QEMUDir, "tmp"),
		VirshPath:     plan.VirshPath,
		URI:           plan.LibvirtURI,
		DomainName:    plan.DomainName,
		DomainXMLFile: plan.DomainXMLFile,
	}
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

func (r VMRunner) waitSerial(ctx context.Context, cancel context.CancelFunc, done <-chan error, result Result, config VMConfig, plan VMPlan, started time.Time) Result {
	interval := config.PollInterval
	if interval == 0 {
		interval = 250 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	hooksRun := make([]bool, len(config.SerialHooks))
	lastSerialLen := -1
	lastSerialProgress := time.Now().UTC()
	for {
		serialText := readSerial(plan.SerialLog)
		if len(serialText) != lastSerialLen {
			lastSerialLen = len(serialText)
			lastSerialProgress = time.Now().UTC()
		}
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
				err = errors.New("libvirt domain exited before serial signal appeared")
			}
			return finishVM(result, phaseName(config), StatusFailed, fmt.Sprintf("libvirt domain exited before serial signal %q appeared: %v", config.Expect, err), started, time.Now().UTC())
		case <-ctx.Done():
			cancel()
			<-done
			return finishVM(result, phaseName(config), StatusFailed, qemuTimeoutSummary(plan.SerialLog), started, time.Now().UTC())
		case <-ticker.C:
			if config.SerialIdleTimeout > 0 && time.Since(lastSerialProgress) >= config.SerialIdleTimeout {
				cancel()
				<-done
				return finishVM(result, phaseName(config), StatusFailed, qemuSerialIdleSummary(plan.SerialLog, config.SerialIdleTimeout), started, time.Now().UTC())
			}
		}
	}
}

func qemuTimeoutSummary(serialLog string) string {
	const prefix = "libvirt domain timed out"
	tail := serialTail(serialLog, 12, 4000)
	if tail == "" {
		return prefix
	}
	return prefix + "; serial tail:\n" + tail
}

func qemuSerialIdleSummary(serialLog string, idle time.Duration) string {
	prefix := fmt.Sprintf("libvirt domain serial idle timed out after %s", idle)
	tail := serialTail(serialLog, 12, 4000)
	if tail == "" {
		return prefix
	}
	return prefix + "; serial tail:\n" + tail
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
	}
	accel, err := qemuAccel(config.KVM, probe)
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
	domainXML, err := libvirtDomainXML(libvirtDomain{
		Name:        domainName,
		Accel:       accel,
		RAMMiB:      config.RAMMiB,
		CPUs:        config.CPUs,
		OVMFCode:    firstPath(!directKernel, config.OVMFCode),
		OVMFVars:    firstPath(!directKernel, filepath.Join(result.QEMUDir, "OVMF_VARS.fd")),
		Kernel:      config.Boot.Kernel,
		Initrd:      config.Boot.Initrd,
		CommandLine: strings.Join(config.Boot.CommandLine, " "),
		SerialLog:   serial,
		Network:     libvirtNetwork,
		Disks:       disks,
		VSock:       vsock,
	})
	if err != nil {
		return VMPlan{}, fmt.Errorf("marshal libvirt domain XML: %w", err)
	}
	xmlFile := filepath.Join(result.QEMUDir, "domain.xml")
	args := []string{"-c", libvirtURI, "define", xmlFile}
	return VMPlan{
		QEMUPath:       virsh,
		VirshPath:      virsh,
		Args:           args,
		Accel:          accel,
		DomainName:     domainName,
		DomainXML:      domainXML,
		DomainXMLFile:  xmlFile,
		LibvirtURI:     libvirtURI,
		SerialLog:      serial,
		CommandFile:    result.Artifacts.QEMUCommand,
		OVMFVars:       firstPath(!directKernel, filepath.Join(result.QEMUDir, "OVMF_VARS.fd")),
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
		if err := copyFile(plan.OVMFVarsSource, plan.OVMFVars, 0o600); err != nil {
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
		if err := createFATImage(context.Background(), plan.EFITree, plan.EFIImage, "KATLEFI", config.MediaRunner); err != nil {
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

func qemuAccel(policy KVMPolicy, probe probe) (string, error) {
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
}

type libvirtDomain struct {
	Name        string
	Accel       string
	RAMMiB      int
	CPUs        int
	OVMFCode    string
	OVMFVars    string
	Kernel      string
	Initrd      string
	CommandLine string
	SerialLog   string
	Network     string
	Disks       []libvirtDisk
	VSock       VSockPlan
}

type domainXML struct {
	XMLName  xml.Name       `xml:"domain"`
	Type     string         `xml:"type,attr"`
	Name     string         `xml:"name"`
	Memory   domainMemory   `xml:"memory"`
	VCPU     int            `xml:"vcpu"`
	OS       domainOS       `xml:"os"`
	Features domainFeatures `xml:"features"`
	Devices  domainDevices  `xml:"devices"`
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
	Source domainInterfaceSource `xml:"source"`
	Model  domainInterfaceModel  `xml:"model"`
}

type domainInterfaceSource struct {
	Network string `xml:"network,attr"`
}

type domainInterfaceModel struct {
	Type string `xml:"type,attr"`
}

type domainDisk struct {
	Type   string           `xml:"type,attr"`
	Device string           `xml:"device,attr"`
	Driver domainDiskDriver `xml:"driver"`
	Source domainDiskSource `xml:"source"`
	Target domainDiskTarget `xml:"target"`
	Serial string           `xml:"serial,omitempty"`
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
	Type   string             `xml:"type,attr"`
	Source domainSerialSource `xml:"source"`
	Target domainSerialTarget `xml:"target"`
}

type domainSerialSource struct {
	Path string `xml:"path,attr"`
}

type domainSerialTarget struct {
	Port int `xml:"port,attr"`
}

type domainConsole struct {
	Type   string              `xml:"type,attr"`
	Source domainSerialSource  `xml:"source"`
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
	efiTree := filepath.Join(result.QEMUDir, "efi")
	efiImage := ""
	preseedImage := ""
	preseedDir := ""
	nextTarget := byte('a')
	add := func(path string, format DiskFormat, serial string, snapshot bool) {
		target := "vd" + string(nextTarget)
		disk := libvirtDisk{
			Path:   path,
			Format: format,
			Target: target,
			Serial: serial,
		}
		if snapshot {
			disk.BackingPath = path
			disk.BackingFormat = format
			disk.Format = DiskQCOW2
			disk.Path = filepath.Join(result.QEMUDir, target+".snapshot.qcow2")
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
	for _, value := range []string{boot.UKI, boot.EFITree, boot.Kernel} {
		if value != "" {
			bootModes++
		}
	}
	if bootModes > 1 {
		return nil, "", "", "", "", errors.New("VM boot requires at most one of UKI, EFI tree, or kernel")
	}
	if boot.Kernel != "" {
		if boot.Image != "" {
			add(boot.Image, boot.ImageFormat, "katl-boot", boot.ImageSnapshot)
		}
	} else if boot.UKI != "" {
		efiImage = filepath.Join(result.QEMUDir, "efi.img")
		add(efiImage, DiskRaw, "katl-efi", false)
		if boot.Image != "" {
			add(boot.Image, boot.ImageFormat, "katl-boot", boot.ImageSnapshot)
		}
	} else if boot.EFITree != "" {
		efiTree = boot.EFITree
		efiImage = filepath.Join(result.QEMUDir, "efi.img")
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
		preseedImage = filepath.Join(result.QEMUDir, "preseed.img")
		add(preseedImage, DiskRaw, "katl-seed", false)
	}
	return disks, efiTree, efiImage, preseedImage, preseedDir, nil
}

func libvirtDomainXML(domain libvirtDomain) (string, error) {
	doc := domainXML{
		Type:   domain.Accel,
		Name:   domain.Name,
		Memory: domainMemory{Unit: "MiB", Value: domain.RAMMiB},
		VCPU:   domain.CPUs,
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
				Type:   "file",
				Source: domainSerialSource{Path: domain.SerialLog},
				Target: domainSerialTarget{Port: 0},
			},
			Console: domainConsole{
				Type:   "file",
				Source: domainSerialSource{Path: domain.SerialLog},
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
	for _, disk := range domain.Disks {
		xmlDisk := domainDisk{
			Type:   "file",
			Device: "disk",
			Driver: domainDiskDriver{
				Name: "qemu",
				Type: string(disk.Format),
			},
			Source: domainDiskSource{File: disk.Path},
			Target: domainDiskTarget{Dev: disk.Target, Bus: "virtio"},
			Serial: disk.Serial,
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
