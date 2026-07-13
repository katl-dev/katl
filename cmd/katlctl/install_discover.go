package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/katl-dev/katl/internal/installer/handoff"
	"github.com/spf13/cobra"
)

type installDiscoverOptions struct {
	timeout time.Duration
	output  string
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
	opts := installDiscoverOptions{timeout: 3 * time.Second, output: "json"}
	cmd := &cobra.Command{
		Use:   "discover",
		Short: "Find waiting KatlOS installers and inspect their disks",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runInstallDiscover(ctx, opts, stdout, stderr)
		},
	}
	cmd.Flags().DurationVar(&opts.timeout, "timeout", opts.timeout, "overall local-network discovery timeout")
	cmd.Flags().StringVar(&opts.output, "output", opts.output, "output format: json")
	return cmd
}

func runInstallDiscover(ctx context.Context, opts installDiscoverOptions, stdout, stderr io.Writer) error {
	_ = stderr
	if opts.output != "json" {
		return fmt.Errorf("--output = %q, want json", opts.output)
	}
	installers, err := discoverInstallers(ctx, opts.timeout)
	if err != nil {
		return err
	}
	report := installDiscoveryReport{APIVersion: "katl.dev/v1alpha1", Kind: "InstallerDiscovery", Installers: installers}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("encode installer discovery: %w", err)
	}
	_, err = stdout.Write(append(data, '\n'))
	return err
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
