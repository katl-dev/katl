package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/installer/handoff"
	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/spf13/cobra"
)

type installDiscoverOptions struct {
	timeout    time.Duration
	configInit configInitOptions
}

type discoveredInstaller struct {
	Endpoint string                `json:"endpoint"`
	Status   handoff.HandoffStatus `json:"status"`
}

type installDiscoveryReport struct {
	APIVersion string                `json:"apiVersion"`
	Kind       string                `json:"kind"`
	Installers []discoveredInstaller `json:"installers"`
}

var installerInterfaceAddrs = net.InterfaceAddrs
var installerDiscoveryProbe = func(ctx context.Context, endpoint string, timeout time.Duration) (handoff.HandoffStatus, error) {
	return fetchInstallStatus(ctx, &http.Client{Timeout: timeout}, endpoint)
}

func newInstallDiscoverCommand(ctx context.Context, stdout, stderr io.Writer) *cobra.Command {
	opts := installDiscoverOptions{timeout: 3 * time.Second, configInit: defaultConfigInitOptions()}
	cmd := &cobra.Command{
		Use:   "discover [CLUSTER_CONFIG]",
		Short: "Find waiting KatlOS installers and optionally create a ClusterConfig",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) == 1 {
				opts.configInit.outputPath = args[0]
			}
			return runInstallDiscover(ctx, opts, stdout, stderr)
		},
	}
	cmd.Flags().DurationVar(&opts.timeout, "timeout", opts.timeout, "overall local-network discovery timeout")
	addConfigInitFlags(cmd, &opts.configInit)
	return cmd
}

func runInstallDiscover(ctx context.Context, opts installDiscoverOptions, stdout, stderr io.Writer) error {
	installers, err := discoverInstallers(ctx, opts.timeout)
	if err != nil {
		return err
	}
	if strings.TrimSpace(opts.configInit.outputPath) != "" {
		opts.configInit.nodes, err = discoveredInitNodes(installers)
		if err != nil {
			return err
		}
		return runConfigInit(opts.configInit, stdout, stderr)
	}
	report := installDiscoveryReport{APIVersion: "katl.dev/v1alpha1", Kind: "InstallerDiscovery", Installers: installers}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("encode installer discovery: %w", err)
	}
	_, err = stdout.Write(append(data, '\n'))
	return err
}

func discoveredInitNodes(installers []discoveredInstaller) (initNodeSpecs, error) {
	waiting := make([]discoveredInstaller, 0, len(installers))
	for _, installer := range installers {
		if installer.Status.State == handoff.HandoffWaiting {
			waiting = append(waiting, installer)
		}
	}
	if len(waiting) == 0 {
		return nil, fmt.Errorf("no waiting KatlOS installer was discovered; boot the installation media and retry")
	}

	nodes := make(initNodeSpecs, 0, len(waiting))
	worker := 1
	for i, installer := range waiting {
		address, err := installerAddress(installer.Endpoint)
		if err != nil {
			return nil, err
		}
		disk, err := discoveredTargetDisk(installer)
		if err != nil {
			return nil, err
		}
		name := "cp-1"
		role := inventory.RoleControlPlane
		if i > 0 {
			name = fmt.Sprintf("worker-%d", worker)
			role = inventory.RoleWorker
			worker++
		}
		nodes = append(nodes, initNodeSpec{name: name, role: role, address: address, disk: disk})
	}
	return nodes, nil
}

func installerAddress(endpoint string) (string, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Hostname() == "" {
		return "", fmt.Errorf("discovered installer endpoint %q has no usable address", endpoint)
	}
	return parsed.Hostname(), nil
}

func discoveredTargetDisk(installer discoveredInstaller) (manifest.DiskSelector, error) {
	selectable := make([]handoff.HandoffDisk, 0, len(installer.Status.Disks))
	for _, disk := range installer.Status.Disks {
		if disk.Selectable {
			selectable = append(selectable, disk)
		}
	}
	if len(selectable) != 1 {
		paths := make([]string, 0, len(selectable))
		for _, disk := range selectable {
			paths = append(paths, disk.Path)
		}
		detail := "none"
		if len(paths) > 0 {
			detail = strings.Join(paths, ", ")
		}
		return manifest.DiskSelector{}, fmt.Errorf("installer %s reports %d selectable disks (%s); create the ClusterConfig with katlctl config init and choose the intended stable disk", installer.Endpoint, len(selectable), detail)
	}
	disk := selectable[0]
	if len(disk.ByID) > 0 {
		byID := append([]string(nil), disk.ByID...)
		sort.Strings(byID)
		return manifest.DiskSelector{ByID: byID[0]}, nil
	}
	if strings.TrimSpace(disk.WWN) != "" {
		return manifest.DiskSelector{WWN: disk.WWN}, nil
	}
	if strings.TrimSpace(disk.Serial) != "" {
		return manifest.DiskSelector{Serial: disk.Serial}, nil
	}
	return manifest.DiskSelector{}, fmt.Errorf("installer %s marked %s selectable without a stable disk identity", installer.Endpoint, disk.Path)
}

func resolveInstallerEndpoint(ctx context.Context, value string, timeout time.Duration) (string, error) {
	if strings.TrimSpace(value) != "" {
		return normalizeInstallerEndpoint(value)
	}
	if timeout <= 0 || timeout > 3*time.Second {
		timeout = 3 * time.Second
	}
	installers, err := discoverInstallers(ctx, timeout)
	if err != nil {
		return "", err
	}
	waiting := make([]string, 0, len(installers))
	for _, installer := range installers {
		if installer.Status.State == handoff.HandoffWaiting {
			waiting = append(waiting, installer.Endpoint)
		}
	}
	switch len(waiting) {
	case 0:
		return "", fmt.Errorf("no waiting KatlOS installer was discovered; run katlctl install discover or pass --endpoint")
	case 1:
		return waiting[0], nil
	default:
		return "", fmt.Errorf("discovered %d waiting KatlOS installers (%s); select one with --endpoint", len(waiting), strings.Join(waiting, ", "))
	}
}

func discoverInstallers(ctx context.Context, timeout time.Duration) ([]discoveredInstaller, error) {
	if timeout <= 0 {
		return nil, fmt.Errorf("--timeout must be positive")
	}
	candidates, err := localInstallerCandidates()
	if err != nil {
		return nil, err
	}
	discoveryCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	requestTimeout := 750 * time.Millisecond
	if timeout < requestTimeout {
		requestTimeout = timeout
	}
	jobs := make(chan string)
	results := make(chan discoveredInstaller, len(candidates))
	workers := 64
	if len(candidates) < workers {
		workers = len(candidates)
	}
	var group sync.WaitGroup
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			for endpoint := range jobs {
				status, err := installerDiscoveryProbe(discoveryCtx, endpoint, requestTimeout)
				if err == nil {
					results <- discoveredInstaller{Endpoint: endpoint, Status: status}
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, endpoint := range candidates {
			select {
			case jobs <- endpoint:
			case <-discoveryCtx.Done():
				return
			}
		}
	}()
	done := make(chan struct{})
	go func() {
		group.Wait()
		close(results)
		close(done)
	}()
	var installers []discoveredInstaller
	for result := range results {
		installers = append(installers, result)
	}
	<-done
	sort.Slice(installers, func(i, j int) bool { return installers[i].Endpoint < installers[j].Endpoint })
	return installers, nil
}

func localInstallerCandidates() ([]string, error) {
	addrs, err := installerInterfaceAddrs()
	if err != nil {
		return nil, fmt.Errorf("list local network addresses: %w", err)
	}
	seen := map[string]struct{}{}
	for _, addr := range addrs {
		ip, _, err := net.ParseCIDR(addr.String())
		if err != nil || ip == nil || ip.IsLoopback() || ip.To4() == nil {
			continue
		}
		ipv4 := ip.To4()
		for host := 1; host < 255; host++ {
			candidate := net.IPv4(ipv4[0], ipv4[1], ipv4[2], byte(host)).String()
			seen["http://"+net.JoinHostPort(candidate, "8080")] = struct{}{}
		}
	}
	if len(seen) == 0 {
		return nil, fmt.Errorf("no non-loopback IPv4 network is available for installer discovery; pass --endpoint")
	}
	candidates := make([]string, 0, len(seen))
	for endpoint := range seen {
		candidates = append(candidates, endpoint)
	}
	sort.Strings(candidates)
	return candidates, nil
}
