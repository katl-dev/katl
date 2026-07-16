package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/installer/configbundle"
	"github.com/katl-dev/katl/internal/installer/handoff"
	installstatus "github.com/katl-dev/katl/internal/installer/status"
	"github.com/spf13/cobra"
)

const (
	maxInstallBundleSize   = 64 << 20
	maxInstallResponseSize = 1 << 20
	installApplyCreator    = "katlctl install apply"
)

type installApplyOptions struct {
	endpoint   string
	sourcePath string
	nodeName   string
	noWait     bool
	timeout    time.Duration
	output     string
}

type installStatusOptions struct {
	endpoint string
	timeout  time.Duration
	output   string
}

type installHandoffReport struct {
	APIVersion   string                `json:"apiVersion"`
	Kind         string                `json:"kind"`
	Endpoint     string                `json:"endpoint"`
	SelectedNode string                `json:"selectedNode,omitempty"`
	Handoff      handoff.HandoffStatus `json:"handoff"`
}

func newInstallCommand(ctx context.Context, stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{Use: "install", Short: "KatlOS installer handoff operations"}
	cmd.AddCommand(newInstallDiscoverCommand(ctx, stdout, stderr))
	cmd.AddCommand(newInstallApplyCommand(ctx, stdout, stderr))
	cmd.AddCommand(newInstallStatusCommand(ctx, stdout, stderr))
	return cmd
}

func newInstallApplyCommand(ctx context.Context, stdout, stderr io.Writer) *cobra.Command {
	opts := installApplyOptions{timeout: 30 * time.Minute, output: "json"}
	cmd := &cobra.Command{
		Use:   "apply SOURCE",
		Short: "Compile a cluster config and apply it to a waiting KatlOS installer",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			opts.sourcePath = args[0]
			return runInstallApply(ctx, opts, stdout, stderr)
		},
	}
	cmd.Flags().StringVar(&opts.endpoint, "endpoint", "", "installer base URL; discovers a unique waiting installer when omitted")
	cmd.Flags().StringVar(&opts.nodeName, "node", "", "node name or configured address; inferred when the config contains one node")
	cmd.Flags().BoolVar(&opts.noWait, "no-wait", false, "return after the installer accepts the bundle")
	cmd.Flags().DurationVar(&opts.timeout, "timeout", opts.timeout, "overall handoff and install wait timeout")
	cmd.Flags().StringVar(&opts.output, "output", opts.output, "output format: json")
	return cmd
}

func newInstallStatusCommand(ctx context.Context, stdout, stderr io.Writer) *cobra.Command {
	opts := installStatusOptions{timeout: 15 * time.Second, output: "json"}
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Report a waiting or running KatlOS installer",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runInstallStatus(ctx, opts, stdout, stderr)
		},
	}
	cmd.Flags().StringVar(&opts.endpoint, "endpoint", "", "installer base URL; discovers a unique waiting installer when omitted")
	cmd.Flags().DurationVar(&opts.timeout, "timeout", opts.timeout, "status request timeout")
	cmd.Flags().StringVar(&opts.output, "output", opts.output, "output format: json")
	return cmd
}

func runInstallApply(ctx context.Context, opts installApplyOptions, stdout, stderr io.Writer) error {
	if opts.output != "json" {
		return fmt.Errorf("--output = %q, want json", opts.output)
	}
	if opts.timeout <= 0 {
		return fmt.Errorf("--timeout must be positive")
	}
	if strings.TrimSpace(opts.sourcePath) == "" {
		return fmt.Errorf("source config path is required")
	}
	archive, result, err := configbundle.BuildArchive(configbundle.BuildRequest{
		SourcePath:     opts.sourcePath,
		KatlctlVersion: version,
		KatlctlCommit:  commit,
		CreatedBy:      installApplyCreator,
	})
	if err != nil {
		return fmt.Errorf("compile cluster config: %w", err)
	}
	if len(archive) > maxInstallBundleSize {
		return fmt.Errorf("compiled config bundle size %d exceeds %d bytes", len(archive), maxInstallBundleSize)
	}
	nodeName, err := resolveInstallNode(archive, result.Digest, opts.nodeName)
	if err != nil {
		return err
	}
	selected, err := configbundle.ReadSelectedNode(bytes.NewReader(archive), configbundle.ReadOptions{
		ExpectedDigest:          result.Digest,
		NodeName:                nodeName,
		AllowMissingKatlosImage: true,
	})
	if err != nil {
		return fmt.Errorf("select node from compiled cluster config: %w", err)
	}
	endpointHint, err := installEndpointHint(opts.endpoint, opts.nodeName, nodeName)
	if err != nil {
		return err
	}
	endpoint, err := resolveInstallerEndpoint(ctx, endpointHint, opts.timeout)
	if err != nil {
		return err
	}

	waitCtx, cancel := context.WithTimeout(ctx, opts.timeout)
	defer cancel()
	client := &http.Client{Timeout: requestTimeout(opts.timeout)}
	before, err := fetchInstallStatus(waitCtx, client, endpoint)
	if err != nil {
		return err
	}
	if before.State != handoff.HandoffWaiting {
		return fmt.Errorf("installer is not accepting config: state=%s selectedNode=%s", before.State, before.SelectedNode)
	}

	accepted, err := submitInstallBundle(waitCtx, client, endpoint, archive, selected.BundleDigest, selected.Node.Name)
	if err != nil {
		return err
	}
	report := newInstallHandoffReport(endpoint, selected.Node.Name, accepted)
	if opts.noWait {
		return writeInstallReport(stdout, report)
	}
	status, err := waitForInstall(waitCtx, client, endpoint, accepted, stderr)
	if err != nil {
		return err
	}
	report.Handoff = status
	if err := writeInstallReport(stdout, report); err != nil {
		return err
	}
	if installFailed(status.InstallStatus.State) {
		return fmt.Errorf("installer finished in %s: %s", status.InstallStatus.State, installFailure(status.InstallStatus))
	}
	return nil
}

func installEndpointHint(endpoint, selector, nodeName string) (string, error) {
	if endpoint = strings.TrimSpace(endpoint); endpoint != "" {
		return normalizeInstallerEndpoint(endpoint)
	}
	selector = strings.TrimSpace(selector)
	if selector != "" && selector != nodeName {
		return normalizeInstallerAddress(selector)
	}
	return "", nil
}

func resolveInstallNode(archive []byte, digest, selector string) (string, error) {
	bundle, err := configbundle.ReadBundle(bytes.NewReader(archive), digest)
	if err != nil {
		return "", fmt.Errorf("read compiled cluster config: %w", err)
	}
	return selectInstallNode(bundle.Manifest, selector)
}

func selectInstallNode(manifest configbundle.BundleManifest, selector string) (string, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		if len(manifest.Nodes) == 1 {
			return manifest.Nodes[0].Name, nil
		}
		return "", fmt.Errorf("--node is required because the cluster config contains %d nodes (%s)", len(manifest.Nodes), installNodeChoices(manifest))
	}
	for _, node := range manifest.Nodes {
		if node.Name == selector {
			return node.Name, nil
		}
	}
	var matches []string
	for _, node := range manifest.Cluster.BootstrapInventory.Nodes {
		if strings.TrimSpace(node.Address) == selector {
			matches = append(matches, node.Name)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		sort.Strings(matches)
		return "", fmt.Errorf("--node address %q matches multiple nodes (%s); use a node name", selector, strings.Join(matches, ", "))
	}
	return "", fmt.Errorf("--node %q does not match a configured node name or address; choose %s", selector, installNodeChoices(manifest))
}

func installNodeChoices(manifest configbundle.BundleManifest) string {
	addresses := make(map[string]string, len(manifest.Cluster.BootstrapInventory.Nodes))
	for _, node := range manifest.Cluster.BootstrapInventory.Nodes {
		addresses[node.Name] = strings.TrimSpace(node.Address)
	}
	choices := make([]string, 0, len(manifest.Nodes))
	for _, node := range manifest.Nodes {
		choice := node.Name
		if address := addresses[node.Name]; address != "" {
			choice += " (" + address + ")"
		}
		choices = append(choices, choice)
	}
	sort.Strings(choices)
	return strings.Join(choices, ", ")
}

func runInstallStatus(ctx context.Context, opts installStatusOptions, stdout, stderr io.Writer) error {
	_ = stderr
	if opts.output != "json" {
		return fmt.Errorf("--output = %q, want json", opts.output)
	}
	endpoint, err := resolveInstallerEndpoint(ctx, opts.endpoint, opts.timeout)
	if err != nil {
		return err
	}
	if opts.timeout <= 0 {
		return fmt.Errorf("--timeout must be positive")
	}
	requestCtx, cancel := context.WithTimeout(ctx, opts.timeout)
	defer cancel()
	status, err := fetchInstallStatus(requestCtx, &http.Client{Timeout: requestTimeout(opts.timeout)}, endpoint)
	if err != nil {
		return err
	}
	return writeInstallReport(stdout, newInstallHandoffReport(endpoint, status.SelectedNode, status))
}

func normalizeInstallerEndpoint(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("--endpoint is required")
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "", fmt.Errorf("--endpoint is invalid: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("--endpoint scheme must be http or https")
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("--endpoint host is required")
	}
	if parsed.User != nil {
		return "", fmt.Errorf("--endpoint must not contain user information")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("--endpoint must not contain a query or fragment")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return "", fmt.Errorf("--endpoint must be a base URL without a path")
	}
	parsed.Path = ""
	return strings.TrimRight(parsed.String(), "/"), nil
}

func normalizeInstallerAddress(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("address is required")
	}
	if strings.Contains(value, "://") {
		return normalizeInstallerEndpoint(value)
	}
	if ip := net.ParseIP(value); ip != nil {
		return normalizeInstallerEndpoint("http://" + net.JoinHostPort(ip.String(), "8080"))
	}
	if host, port, err := net.SplitHostPort(value); err == nil && host != "" && port != "" {
		return normalizeInstallerEndpoint("http://" + value)
	}
	if strings.Contains(value, ":") {
		return "", fmt.Errorf("address %q must be an IP, hostname, host:port, or HTTP base URL", value)
	}
	return normalizeInstallerEndpoint("http://" + net.JoinHostPort(value, "8080"))
}

func fetchInstallStatus(ctx context.Context, client *http.Client, endpoint string) (handoff.HandoffStatus, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/v1/status", nil)
	if err != nil {
		return handoff.HandoffStatus{}, fmt.Errorf("create installer status request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	return doInstallRequest(client, req, "read installer status")
}

func submitInstallBundle(ctx context.Context, client *http.Client, endpoint string, archive []byte, digest, node string) (handoff.HandoffStatus, error) {
	requestURL, err := url.Parse(endpoint + "/v1/config-bundle")
	if err != nil {
		return handoff.HandoffStatus{}, fmt.Errorf("create installer handoff URL: %w", err)
	}
	query := requestURL.Query()
	query.Set("node", node)
	query.Set("digest", digest)
	requestURL.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL.String(), bytes.NewReader(archive))
	if err != nil {
		return handoff.HandoffStatus{}, fmt.Errorf("create installer handoff request: %w", err)
	}
	req.ContentLength = int64(len(archive))
	req.Header.Set("Content-Type", "application/vnd.katl.config.bundle.v1")
	req.Header.Set("Accept", "application/json")
	return doInstallRequest(client, req, "submit installer config bundle")
}

func doInstallRequest(client *http.Client, req *http.Request, action string) (handoff.HandoffStatus, error) {
	resp, err := client.Do(req)
	if err != nil {
		return handoff.HandoffStatus{}, fmt.Errorf("%s: %w", action, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxInstallResponseSize+1))
	if err != nil {
		return handoff.HandoffStatus{}, fmt.Errorf("%s response: %w", action, err)
	}
	if len(data) > maxInstallResponseSize {
		return handoff.HandoffStatus{}, fmt.Errorf("%s response exceeds %d bytes", action, maxInstallResponseSize)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message := strings.TrimSpace(string(data))
		if message == "" {
			message = http.StatusText(resp.StatusCode)
		}
		return handoff.HandoffStatus{}, fmt.Errorf("%s: HTTP %d: %s", action, resp.StatusCode, message)
	}
	var status handoff.HandoffStatus
	if err := json.NewDecoder(bytes.NewReader(data)).Decode(&status); err != nil {
		return handoff.HandoffStatus{}, fmt.Errorf("%s response is invalid: %w", action, err)
	}
	if strings.TrimSpace(string(status.State)) == "" || strings.TrimSpace(status.InstallStatus.State) == "" {
		return handoff.HandoffStatus{}, fmt.Errorf("%s response is missing installer state", action)
	}
	return status, nil
}

func waitForInstall(ctx context.Context, client *http.Client, endpoint string, initial handoff.HandoffStatus, stderr io.Writer) (handoff.HandoffStatus, error) {
	last := initial
	lastProgress := ""
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		progress := last.InstallStatus.State + "/" + last.InstallStatus.CurrentStep
		if progress != lastProgress {
			fmt.Fprintf(stderr, "katlctl install state=%s step=%s\n", last.InstallStatus.State, last.InstallStatus.CurrentStep)
			lastProgress = progress
		}
		if installTerminal(last.InstallStatus.State) {
			return last, nil
		}
		select {
		case <-ctx.Done():
			return handoff.HandoffStatus{}, fmt.Errorf("wait for installer after state=%s step=%s: %w", last.InstallStatus.State, last.InstallStatus.CurrentStep, ctx.Err())
		case <-ticker.C:
			status, err := fetchInstallStatus(ctx, client, endpoint)
			if err != nil {
				return handoff.HandoffStatus{}, fmt.Errorf("installer status became unavailable after state=%s step=%s: %w", last.InstallStatus.State, last.InstallStatus.CurrentStep, err)
			}
			last = status
		}
	}
}

func installTerminal(state string) bool {
	switch state {
	case installstatus.StateRebootRequested,
		installstatus.StateInstallRefused,
		installstatus.StateFailedBeforeMutation,
		installstatus.StateFailedAfterMutation:
		return true
	default:
		return false
	}
}

func installFailed(state string) bool {
	return installTerminal(state) && state != installstatus.StateRebootRequested
}

func installFailure(status installstatus.Record) string {
	for _, value := range []string{status.RefusalReason, status.LastError, status.RetryHint} {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return "inspect installer status and console"
}

func requestTimeout(overall time.Duration) time.Duration {
	const maximum = 15 * time.Second
	if overall < maximum {
		return overall
	}
	return maximum
}

func newInstallHandoffReport(endpoint, node string, status handoff.HandoffStatus) installHandoffReport {
	return installHandoffReport{
		APIVersion:   installstatus.APIVersion,
		Kind:         "InstallHandoffReport",
		Endpoint:     endpoint,
		SelectedNode: node,
		Handoff:      status,
	}
}

func writeInstallReport(stdout io.Writer, report installHandoffReport) error {
	report.Handoff.InstallStatus.BundleDigest = ""
	report.Handoff.InstallStatus.SourceDigest = ""
	report.Handoff.InstallStatus.NodeMaterialDigest = ""
	report.Handoff.InstallStatus.InstallMaterialDigest = ""
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal installer handoff report: %w", err)
	}
	_, err = stdout.Write(append(data, '\n'))
	return err
}
