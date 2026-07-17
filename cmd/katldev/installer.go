package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/vmtest"
	"github.com/spf13/cobra"
)

const (
	installerStateAPIVersion = "katl.dev/v1alpha1"
	installerStateKind       = "KatlDevInstallerVM"
	installerDomainMetadata  = "katl/katldev-installer"
	vmtestMetadataURI        = "https://katlos.io/xmlns/vmtest/1"
)

type installerOptions struct {
	MemoryMiB int
	CPUs      int
	DiskSize  string
	Timeout   time.Duration
	NoBuild   bool
}

type installerState struct {
	APIVersion   string    `json:"apiVersion"`
	Kind         string    `json:"kind"`
	RepoRoot     string    `json:"repoRoot"`
	Revision     string    `json:"revision,omitempty"`
	Dirty        bool      `json:"dirty,omitempty"`
	DomainName   string    `json:"domainName"`
	LibvirtURI   string    `json:"libvirtURI"`
	Network      string    `json:"network"`
	MACAddress   string    `json:"macAddress"`
	IPAddress    string    `json:"ipAddress,omitempty"`
	Endpoint     string    `json:"endpoint,omitempty"`
	InstallerISO string    `json:"installerISO"`
	ISOSHA256    string    `json:"isoSHA256"`
	TargetDisk   string    `json:"targetDisk"`
	SerialLog    string    `json:"serialLog"`
	DomainXML    string    `json:"domainXML"`
	MemoryMiB    int       `json:"memoryMiB"`
	CPUs         int       `json:"cpus"`
	DiskSize     string    `json:"diskSize"`
	StartedAt    time.Time `json:"startedAt"`
}

type installerManager struct {
	repoRoot string
	stdout   io.Writer
	stderr   io.Writer
	client   *http.Client
}

func newInstallerCommand(ctx context.Context, stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "installer",
		Short: "Manage a persistent KatlOS installer VM for manual UX testing",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newInstallerStartCommand(ctx, stdout, stderr, false))
	cmd.AddCommand(newInstallerStartCommand(ctx, stdout, stderr, true))
	cmd.AddCommand(newInstallerStatusCommand(ctx, stdout, stderr))
	cmd.AddCommand(newInstallerStopCommand(ctx, stdout, stderr))
	cmd.AddCommand(newInstallerConsoleCommand(ctx, stdout, stderr))
	return cmd
}

func newInstallerStartCommand(ctx context.Context, stdout, stderr io.Writer, reset bool) *cobra.Command {
	opts := installerOptions{MemoryMiB: 4096, CPUs: 2, DiskSize: "32G", Timeout: 3 * time.Minute}
	name := "start"
	short := "Start or resume the persistent installer VM"
	if reset {
		name = "reset"
		short = "Wipe the managed VM and boot a fresh installer"
	}
	cmd := &cobra.Command{
		Use:   name,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			manager, err := loadInstallerManager(stdout, stderr)
			if err != nil {
				return err
			}
			return manager.start(ctx, opts, reset)
		},
	}
	cmd.Flags().IntVar(&opts.MemoryMiB, "memory", opts.MemoryMiB, "guest memory in MiB")
	cmd.Flags().IntVar(&opts.CPUs, "cpus", opts.CPUs, "guest virtual CPUs")
	cmd.Flags().StringVar(&opts.DiskSize, "disk-size", opts.DiskSize, "fresh target disk size")
	cmd.Flags().DurationVar(&opts.Timeout, "timeout", opts.Timeout, "installer DHCP and readiness timeout")
	cmd.Flags().BoolVar(&opts.NoBuild, "no-build", false, "use the existing installer ISO without checking the current checkout")
	return cmd
}

func newInstallerStatusCommand(ctx context.Context, stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the managed VM, network endpoint, and installer state",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			manager, err := loadInstallerManager(stdout, stderr)
			if err != nil {
				return err
			}
			return manager.status(ctx)
		},
	}
}

func newInstallerStopCommand(ctx context.Context, stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Power off the managed VM while keeping its disk",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			manager, err := loadInstallerManager(stdout, stderr)
			if err != nil {
				return err
			}
			return manager.stop(ctx)
		},
	}
}

func newInstallerConsoleCommand(ctx context.Context, stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "console",
		Short: "Attach to the managed VM serial console",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			manager, err := loadInstallerManager(stdout, stderr)
			if err != nil {
				return err
			}
			return manager.console(ctx)
		},
	}
}

func loadInstallerManager(stdout, stderr io.Writer) (installerManager, error) {
	repoRoot, err := repositoryRoot()
	if err != nil {
		return installerManager{}, err
	}
	return installerManager{
		repoRoot: repoRoot,
		stdout:   stdout,
		stderr:   stderr,
		client:   &http.Client{Timeout: 2 * time.Second},
	}, nil
}

func repositoryRoot() (string, error) {
	output, err := exec.Command("git", "rev-parse", "--show-toplevel").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("find Katl checkout: %w: %s", err, strings.TrimSpace(string(output)))
	}
	root := strings.TrimSpace(string(output))
	if root == "" {
		return "", errors.New("find Katl checkout: git returned an empty path")
	}
	return filepath.Abs(root)
}

func (manager installerManager) start(ctx context.Context, opts installerOptions, reset bool) error {
	if err := validateInstallerOptions(opts); err != nil {
		return err
	}
	state, stateErr := manager.readState()
	if stateErr != nil && !errors.Is(stateErr, os.ErrNotExist) {
		return stateErr
	}
	if !reset && stateErr == nil {
		exists, err := manager.domainExists(ctx, state.LibvirtURI, state.DomainName)
		if err != nil {
			return err
		}
		if exists {
			if err := manager.requireOwnedDomain(ctx, state.LibvirtURI, state.DomainName); err != nil {
				return err
			}
			return manager.resume(ctx, state, opts.Timeout)
		}
		fmt.Fprintln(manager.stderr, "katldev installer: recorded domain is missing; recreating it")
	}

	iso, digest, err := manager.prepareInstaller(ctx, opts.NoBuild)
	if err != nil {
		return err
	}
	if err := manager.removeManagedVM(ctx, state); err != nil {
		return err
	}
	if err := os.RemoveAll(manager.stateRoot()); err != nil {
		return fmt.Errorf("remove previous installer VM state: %w", err)
	}

	state, err = manager.create(ctx, opts, iso, digest)
	if err != nil {
		return err
	}
	state, err = manager.waitReady(ctx, state, opts.Timeout)
	if err != nil {
		return err
	}
	return manager.printReady(state)
}

func validateInstallerOptions(opts installerOptions) error {
	if opts.MemoryMiB < 1024 {
		return errors.New("--memory must be at least 1024 MiB")
	}
	if opts.CPUs < 1 {
		return errors.New("--cpus must be positive")
	}
	if strings.TrimSpace(opts.DiskSize) == "" {
		return errors.New("--disk-size is required")
	}
	if opts.Timeout <= 0 {
		return errors.New("--timeout must be positive")
	}
	return nil
}

func (manager installerManager) prepareInstaller(ctx context.Context, noBuild bool) (string, string, error) {
	if !noBuild {
		fmt.Fprintln(manager.stderr, "katldev installer: checking current checkout installer artifacts")
		command := exec.CommandContext(ctx, filepath.Join(manager.repoRoot, "scripts", "mkosi"), "build-installer-iso")
		command.Dir = manager.repoRoot
		command.Stdout = manager.stderr
		command.Stderr = manager.stderr
		if err := command.Run(); err != nil {
			return "", "", fmt.Errorf("build current installer ISO: %w", err)
		}
	}
	iso := filepath.Join(manager.repoRoot, "_build", "mkosi", "katl-installer.iso")
	digest, err := sha256File(iso)
	if err != nil {
		if noBuild && errors.Is(err, os.ErrNotExist) {
			return "", "", fmt.Errorf("existing installer ISO is unavailable; rerun without --no-build: %w", err)
		}
		return "", "", fmt.Errorf("read installer ISO: %w", err)
	}
	return iso, digest, nil
}

func (manager installerManager) create(ctx context.Context, opts installerOptions, iso, digest string) (installerState, error) {
	runID, mac := installerIdentity(manager.repoRoot)
	stateRoot := manager.stateRoot()
	scenario := vmtest.Scenario{
		Name:      "katldev installer",
		RunID:     runID,
		StateRoot: stateRoot,
		Keep:      vmtest.KeepAlways,
		KVM:       vmtest.KVMAuto,
		Disks:     []vmtest.DiskFixture{vmtest.TargetDisk("root", string(vmtest.DiskQCOW2), opts.DiskSize)},
	}
	runner := vmtest.NewRunner(vmtest.Options{StateRoot: stateRoot, Keep: vmtest.KeepAlways, KVM: vmtest.KVMAuto, RunID: runID})
	result, err := runner.Plan(scenario)
	if err != nil {
		return installerState{}, fmt.Errorf("plan installer VM: %w", err)
	}
	if err := vmtest.CreateDisks(ctx, vmtest.ExecDiskRunner{}, result.Disks); err != nil {
		return installerState{}, fmt.Errorf("create installer VM disk: %w", err)
	}
	uri := firstNonEmpty(os.Getenv("KATL_VMTEST_LIBVIRT_URI"), "qemu:///system")
	network := firstNonEmpty(os.Getenv("KATL_VMTEST_LIBVIRT_NETWORK"), "default")
	config := vmtest.VMConfig{
		Boot:             vmtest.VMBoot{ISO: iso, DiskFirst: true},
		VirshPath:        firstNonEmpty(os.Getenv("KATL_VMTEST_VIRSH"), "virsh"),
		ImageTool:        firstNonEmpty(os.Getenv("KATL_VMTEST_IMAGE_TOOL"), "qemu-img"),
		LibvirtURI:       uri,
		LibvirtNetwork:   network,
		OVMFCode:         os.Getenv("KATL_OVMF_CODE"),
		OVMFVars:         os.Getenv("KATL_OVMF_VARS"),
		PreserveNVRAM:    true,
		KVM:              vmtest.KVMAuto,
		RAMMiB:           opts.MemoryMiB,
		CPUs:             opts.CPUs,
		Phase:            "installer",
		Network:          vmtest.VMNetworkConfig{Mode: vmtest.VMNetworkUser, MAC: mac},
		DomainMetadata:   installerDomainMetadata,
		PersistentSerial: true,
	}
	plan, err := vmtest.PlanVM(result, config)
	if err != nil {
		return installerState{}, fmt.Errorf("plan libvirt installer VM: %w", err)
	}
	if err := vmtest.PrepareVM(plan, config); err != nil {
		return installerState{}, fmt.Errorf("prepare libvirt installer VM: %w", err)
	}
	serial, err := os.OpenFile(plan.SerialLog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return installerState{}, fmt.Errorf("prepare persistent serial log: %w", err)
	}
	if err := serial.Close(); err != nil {
		return installerState{}, fmt.Errorf("prepare persistent serial log: %w", err)
	}
	if _, err := manager.virsh(ctx, uri, "define", plan.DomainXMLFile); err != nil {
		return installerState{}, err
	}
	if _, err := manager.virsh(ctx, uri, "start", plan.DomainName); err != nil {
		return installerState{}, err
	}
	revision, dirty := checkoutRevision(manager.repoRoot)
	state := installerState{
		APIVersion:   installerStateAPIVersion,
		Kind:         installerStateKind,
		RepoRoot:     manager.repoRoot,
		Revision:     revision,
		Dirty:        dirty,
		DomainName:   plan.DomainName,
		LibvirtURI:   uri,
		Network:      network,
		MACAddress:   mac,
		InstallerISO: iso,
		ISOSHA256:    digest,
		TargetDisk:   result.Disks[0].HostPath,
		SerialLog:    plan.SerialLog,
		DomainXML:    plan.DomainXMLFile,
		MemoryMiB:    opts.MemoryMiB,
		CPUs:         opts.CPUs,
		DiskSize:     opts.DiskSize,
		StartedAt:    time.Now().UTC(),
	}
	if err := manager.writeState(state); err != nil {
		return installerState{}, err
	}
	return state, nil
}

func (manager installerManager) resume(ctx context.Context, state installerState, timeout time.Duration) error {
	domainState, err := manager.domainState(ctx, state.LibvirtURI, state.DomainName)
	if err != nil {
		return err
	}
	switch domainState {
	case "running", "idle", "in shutdown":
		if status, statusErr := manager.installerStatus(ctx, state.Endpoint); statusErr == nil {
			if installerAcceptingConfig(status) {
				return manager.printReady(state)
			}
			return manager.printCurrent(ctx, state, domainState)
		}
		if state.IPAddress != "" && tcpReachable(ctx, net.JoinHostPort(state.IPAddress, "9443"), 500*time.Millisecond) {
			return manager.printCurrent(ctx, state, domainState)
		}
		return manager.waitResumed(ctx, state, timeout)
	case "paused":
		if _, err := manager.virsh(ctx, state.LibvirtURI, "resume", state.DomainName); err != nil {
			return err
		}
	case "shut off", "crashed", "pmsuspended":
		if _, err := manager.virsh(ctx, state.LibvirtURI, "start", state.DomainName); err != nil {
			return err
		}
	default:
		return fmt.Errorf("installer VM %s is in unsupported state %q", state.DomainName, domainState)
	}
	return manager.waitResumed(ctx, state, timeout)
}

func (manager installerManager) waitResumed(ctx context.Context, state installerState, timeout time.Duration) error {
	readyCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	lastAddress := state.IPAddress
	for {
		lease, leaseErr := manager.discoverLease(readyCtx, state)
		if leaseErr == nil {
			lastAddress = lease.IPAddress
			endpoint := "http://" + net.JoinHostPort(lease.IPAddress, "8080")
			if status, err := manager.installerStatus(readyCtx, endpoint); err == nil {
				state.IPAddress = lease.IPAddress
				state.Endpoint = endpoint
				if err := manager.writeState(state); err != nil {
					return err
				}
				if installerAcceptingConfig(status) {
					return manager.printReady(state)
				}
				return manager.printCurrent(readyCtx, state, "running")
			}
			if tcpReachable(readyCtx, net.JoinHostPort(lease.IPAddress, "9443"), 500*time.Millisecond) {
				state.IPAddress = lease.IPAddress
				state.Endpoint = endpoint
				if err := manager.writeState(state); err != nil {
					return err
				}
				return manager.printCurrent(readyCtx, state, "running")
			}
		}
		select {
		case <-readyCtx.Done():
			return fmt.Errorf("VM is running%s but neither the installer nor KatlOS management API became reachable within %s; inspect %s or run katldev installer console", addressDetail(lastAddress), timeout, state.SerialLog)
		case <-ticker.C:
		}
	}
}

func (manager installerManager) waitReady(ctx context.Context, state installerState, timeout time.Duration) (installerState, error) {
	readyCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var lastErr error
	lastAddress := ""
	for {
		lease, leaseErr := manager.discoverLease(readyCtx, state)
		if leaseErr == nil {
			lastAddress = lease.IPAddress
			endpoint := "http://" + net.JoinHostPort(lease.IPAddress, "8080")
			status, statusErr := manager.installerStatus(readyCtx, endpoint)
			if statusErr == nil {
				if !installerAcceptingConfig(status) {
					return state, fmt.Errorf("installer API is reachable but is not waiting for config: state=%s", status)
				}
				state.IPAddress = lease.IPAddress
				state.Endpoint = endpoint
				if err := manager.writeState(state); err != nil {
					return state, err
				}
				return state, nil
			}
			lastErr = statusErr
		} else {
			lastErr = leaseErr
		}
		select {
		case <-readyCtx.Done():
			return state, fmt.Errorf("VM is running%s but the installer API did not become ready within %s: %w; inspect %s or run katldev installer console", addressDetail(lastAddress), timeout, lastErr, state.SerialLog)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (manager installerManager) discoverLease(ctx context.Context, state installerState) (vmtest.LibvirtLease, error) {
	return vmtest.DiscoverLibvirtLease(ctx, firstNonEmpty(os.Getenv("KATL_VMTEST_VIRSH"), "virsh"), state.LibvirtURI, state.Network, state.MACAddress)
}

func addressDetail(address string) string {
	if strings.TrimSpace(address) == "" {
		return " without a discovered address"
	}
	return " at " + address
}

func installerAcceptingConfig(state string) bool {
	switch strings.TrimSpace(state) {
	case "waiting", "waiting-for-config":
		return true
	default:
		return false
	}
}

func tcpReachable(ctx context.Context, address string, timeout time.Duration) bool {
	dialer := net.Dialer{Timeout: timeout}
	connection, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return false
	}
	_ = connection.Close()
	return true
}

func (manager installerManager) installerStatus(ctx context.Context, endpoint string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(endpoint, "/")+"/v1/status", nil)
	if err != nil {
		return "", err
	}
	response, err := manager.client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("installer status returned %s", response.Status)
	}
	var status struct {
		State string `json:"state"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&status); err != nil {
		return "", err
	}
	return strings.TrimSpace(status.State), nil
}

func (manager installerManager) status(ctx context.Context) error {
	state, err := manager.readState()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errors.New("installer VM does not exist; run katldev installer start")
		}
		return err
	}
	exists, err := manager.domainExists(ctx, state.LibvirtURI, state.DomainName)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("installer VM %s is missing; run katldev installer reset", state.DomainName)
	}
	if err := manager.requireOwnedDomain(ctx, state.LibvirtURI, state.DomainName); err != nil {
		return err
	}
	domainState, err := manager.domainState(ctx, state.LibvirtURI, state.DomainName)
	if err != nil {
		return err
	}
	return manager.printCurrent(ctx, state, domainState)
}

func (manager installerManager) printCurrent(ctx context.Context, state installerState, domainState string) error {
	installerState := "unreachable"
	if state.Endpoint != "" && domainState == "running" {
		if current, err := manager.installerStatus(ctx, state.Endpoint); err == nil {
			installerState = current
		} else if state.IPAddress != "" && tcpReachable(ctx, net.JoinHostPort(state.IPAddress, "9443"), 500*time.Millisecond) {
			installerState = "not active (KatlOS management API is reachable)"
		}
	}
	fmt.Fprintf(manager.stdout, "VM: %s (%s)\n", state.DomainName, domainState)
	fmt.Fprintf(manager.stdout, "Address: %s\n", firstNonEmpty(state.IPAddress, "not assigned"))
	fmt.Fprintf(manager.stdout, "Endpoint: %s\n", firstNonEmpty(state.Endpoint, "not assigned"))
	fmt.Fprintf(manager.stdout, "Installer: %s\n", installerState)
	fmt.Fprintf(manager.stdout, "Checkout: %s%s\n", firstNonEmpty(state.Revision, "unknown"), dirtySuffix(state.Dirty))
	fmt.Fprintf(manager.stdout, "Serial log: %s\n", state.SerialLog)
	if domainState == "running" {
		fmt.Fprintln(manager.stdout, "Console: katldev installer console")
	}
	fmt.Fprintln(manager.stdout, "Fresh installer: katldev installer reset")
	return nil
}

func (manager installerManager) printReady(state installerState) error {
	configPath := filepath.Join(manager.repoRoot, "_build", "katldev", "cluster.yaml")
	fmt.Fprintln(manager.stdout, "KatlOS installer VM is ready.")
	fmt.Fprintf(manager.stdout, "VM: %s\n", state.DomainName)
	fmt.Fprintf(manager.stdout, "Endpoint: %s\n", state.Endpoint)
	fmt.Fprintf(manager.stdout, "Target disk: /dev/disk/by-id/virtio-katl-root\n")
	fmt.Fprintln(manager.stdout)
	fmt.Fprintln(manager.stdout, "Try the operator flow:")
	fmt.Fprintf(manager.stdout, "  katlctl config init %s --installer %s\n", configPath, state.Endpoint)
	fmt.Fprintf(manager.stdout, "  katlctl install apply --config %s --endpoint %s\n", configPath, state.Endpoint)
	fmt.Fprintln(manager.stdout)
	fmt.Fprintln(manager.stdout, "Console: katldev installer console")
	fmt.Fprintln(manager.stdout, "Reset:   katldev installer reset")
	return nil
}

func (manager installerManager) stop(ctx context.Context) error {
	state, err := manager.readState()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errors.New("installer VM does not exist; run katldev installer start")
		}
		return err
	}
	exists, err := manager.domainExists(ctx, state.LibvirtURI, state.DomainName)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("installer VM %s is already absent", state.DomainName)
	}
	if err := manager.requireOwnedDomain(ctx, state.LibvirtURI, state.DomainName); err != nil {
		return err
	}
	domainState, err := manager.domainState(ctx, state.LibvirtURI, state.DomainName)
	if err != nil {
		return err
	}
	if domainState != "shut off" && domainState != "crashed" {
		if _, err := manager.virsh(ctx, state.LibvirtURI, "destroy", state.DomainName); err != nil {
			return err
		}
	}
	fmt.Fprintf(manager.stdout, "Stopped %s. Resume it with katldev installer start or wipe it with katldev installer reset.\n", state.DomainName)
	return nil
}

func (manager installerManager) console(ctx context.Context) error {
	state, err := manager.readState()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errors.New("installer VM does not exist; run katldev installer start")
		}
		return err
	}
	virsh := firstNonEmpty(os.Getenv("KATL_VMTEST_VIRSH"), "virsh")
	args := []string{"-c", state.LibvirtURI, "console", state.DomainName, "--force"}
	command := exec.CommandContext(ctx, virsh, args...)
	command.Stdin = os.Stdin
	command.Stdout = manager.stdout
	command.Stderr = manager.stderr
	return command.Run()
}

func (manager installerManager) removeManagedVM(ctx context.Context, state installerState) error {
	uri := firstNonEmpty(state.LibvirtURI, os.Getenv("KATL_VMTEST_LIBVIRT_URI"), "qemu:///system")
	domain := state.DomainName
	if domain == "" {
		runID, _ := installerIdentity(manager.repoRoot)
		domain = "katl-" + runID
	}
	exists, err := manager.domainExists(ctx, uri, domain)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if err := manager.requireOwnedDomain(ctx, uri, domain); err != nil {
		return err
	}
	domainState, err := manager.domainState(ctx, uri, domain)
	if err != nil {
		return err
	}
	if domainState != "shut off" && domainState != "crashed" {
		if _, err := manager.virsh(ctx, uri, "destroy", domain); err != nil {
			return err
		}
	}
	if _, err := manager.virsh(ctx, uri, "undefine", domain, "--nvram"); err != nil {
		return err
	}
	return nil
}

func (manager installerManager) domainExists(ctx context.Context, uri, domain string) (bool, error) {
	output, err := manager.virsh(ctx, uri, "list", "--all", "--name")
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(output), "\n") {
		if strings.TrimSpace(line) == domain {
			return true, nil
		}
	}
	return false, nil
}

func (manager installerManager) domainState(ctx context.Context, uri, domain string) (string, error) {
	output, err := manager.virsh(ctx, uri, "domstate", domain)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

func (manager installerManager) requireOwnedDomain(ctx context.Context, uri, domain string) error {
	output, err := manager.virsh(ctx, uri, "metadata", domain, "--uri", vmtestMetadataURI)
	if err != nil {
		return fmt.Errorf("refusing to manage libvirt domain %s without Katl developer ownership metadata: %w", domain, err)
	}
	owner, err := domainOwner(output)
	if err != nil {
		return fmt.Errorf("decode libvirt domain %s ownership metadata: %w", domain, err)
	}
	if owner != installerDomainMetadata {
		return fmt.Errorf("refusing to manage libvirt domain %s owned by %q", domain, owner)
	}
	return nil
}

func domainOwner(data []byte) (string, error) {
	var metadata struct {
		Value string `xml:",chardata"`
	}
	if err := xml.Unmarshal(data, &metadata); err != nil {
		return "", err
	}
	return strings.TrimSpace(metadata.Value), nil
}

func (manager installerManager) virsh(ctx context.Context, uri string, args ...string) ([]byte, error) {
	virsh := firstNonEmpty(os.Getenv("KATL_VMTEST_VIRSH"), "virsh")
	fullArgs := []string{"-c", uri}
	fullArgs = append(fullArgs, args...)
	output, err := exec.CommandContext(ctx, virsh, fullArgs...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("virsh %s: %w: %s", strings.Join(fullArgs, " "), err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

func (manager installerManager) stateRoot() string {
	return filepath.Join(manager.repoRoot, "_build", "katldev", "installer")
}

func (manager installerManager) statePath() string {
	return filepath.Join(manager.stateRoot(), "state.json")
}

func (manager installerManager) readState() (installerState, error) {
	data, err := os.ReadFile(manager.statePath())
	if err != nil {
		return installerState{}, err
	}
	var state installerState
	if err := json.Unmarshal(data, &state); err != nil {
		return installerState{}, fmt.Errorf("decode %s: %w", manager.statePath(), err)
	}
	if state.APIVersion != installerStateAPIVersion || state.Kind != installerStateKind || state.RepoRoot != manager.repoRoot || state.DomainName == "" {
		return installerState{}, fmt.Errorf("%s is not valid state for this checkout", manager.statePath())
	}
	return state, nil
}

func (manager installerManager) writeState(state installerState) error {
	if err := os.MkdirAll(manager.stateRoot(), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temporary := manager.statePath() + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(temporary, manager.statePath()); err != nil {
		return err
	}
	return nil
}

func installerIdentity(repoRoot string) (string, string) {
	digest := sha256.Sum256([]byte(repoRoot))
	runID := "dev-installer-" + hex.EncodeToString(digest[:4])
	mac := fmt.Sprintf("52:54:00:%02x:%02x:%02x", digest[4], digest[5], digest[6])
	return runID, mac
}

func sha256File(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func checkoutRevision(repoRoot string) (string, bool) {
	revisionOutput, revisionErr := exec.Command("git", "-C", repoRoot, "rev-parse", "--short=12", "HEAD").Output()
	statusOutput, statusErr := exec.Command("git", "-C", repoRoot, "status", "--porcelain", "--untracked-files=no").Output()
	revision := ""
	if revisionErr == nil {
		revision = strings.TrimSpace(string(revisionOutput))
	}
	return revision, statusErr == nil && strings.TrimSpace(string(statusOutput)) != ""
}

func dirtySuffix(dirty bool) string {
	if dirty {
		return " (with local changes)"
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
