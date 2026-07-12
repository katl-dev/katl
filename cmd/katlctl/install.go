package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
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
	maxInstallTokenSize    = 4 << 10
)

type installApplyOptions struct {
	endpoint   string
	token      string
	tokenFile  string
	bundlePath string
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
	cmd.AddCommand(newInstallApplyCommand(ctx, stdout, stderr))
	cmd.AddCommand(newInstallStatusCommand(ctx, stdout, stderr))
	return cmd
}

func newInstallApplyCommand(ctx context.Context, stdout, stderr io.Writer) *cobra.Command {
	opts := installApplyOptions{timeout: 30 * time.Minute, output: "json"}
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Submit one config bundle to a waiting KatlOS installer",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runInstallApply(ctx, opts, stdout, stderr)
		},
	}
	cmd.Flags().StringVar(&opts.endpoint, "endpoint", "", "installer base URL such as http://192.0.2.10:8080")
	cmd.Flags().StringVar(&opts.token, "token", "", "one-time installer token; --token-file avoids shell history exposure")
	cmd.Flags().StringVar(&opts.tokenFile, "token-file", "", "protected file containing the one-time installer token")
	cmd.Flags().StringVar(&opts.bundlePath, "config-bundle", "", "Katl config bundle")
	cmd.Flags().StringVar(&opts.nodeName, "node", "", "node to select from the config bundle")
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
	cmd.Flags().StringVar(&opts.endpoint, "endpoint", "", "installer base URL such as http://192.0.2.10:8080")
	cmd.Flags().DurationVar(&opts.timeout, "timeout", opts.timeout, "status request timeout")
	cmd.Flags().StringVar(&opts.output, "output", opts.output, "output format: json")
	return cmd
}

func runInstallApply(ctx context.Context, opts installApplyOptions, stdout, stderr io.Writer) error {
	if opts.output != "json" {
		return fmt.Errorf("--output = %q, want json", opts.output)
	}
	endpoint, err := normalizeInstallerEndpoint(opts.endpoint)
	if err != nil {
		return err
	}
	if opts.timeout <= 0 {
		return fmt.Errorf("--timeout must be positive")
	}
	token, err := readInstallToken(opts.token, opts.tokenFile)
	if err != nil {
		return err
	}
	if strings.TrimSpace(opts.bundlePath) == "" {
		return fmt.Errorf("--config-bundle is required")
	}
	if strings.TrimSpace(opts.nodeName) == "" {
		return fmt.Errorf("--node is required")
	}
	if err := validateInstallBundleSize(opts.bundlePath); err != nil {
		return err
	}
	selected, err := configbundle.ReadSelectedNodeFile(opts.bundlePath, configbundle.ReadOptions{
		NodeName:                opts.nodeName,
		AllowMissingKatlosImage: true,
	})
	if err != nil {
		return fmt.Errorf("validate config bundle: %w", err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, opts.timeout)
	defer cancel()
	client := &http.Client{Timeout: requestTimeout(opts.timeout)}
	before, err := fetchInstallStatus(waitCtx, client, endpoint)
	if err != nil {
		return redactInstallError(err, token)
	}
	if before.State != handoff.HandoffWaiting {
		return fmt.Errorf("installer is not accepting config: state=%s selectedNode=%s", before.State, before.SelectedNode)
	}

	accepted, err := submitInstallBundle(waitCtx, client, endpoint, token, opts.bundlePath, selected.BundleDigest, selected.Node.Name)
	if err != nil {
		return redactInstallError(err, token)
	}
	report := newInstallHandoffReport(endpoint, selected.Node.Name, accepted)
	if opts.noWait {
		return writeInstallReport(stdout, report)
	}
	status, err := waitForInstall(waitCtx, client, endpoint, accepted, stderr)
	if err != nil {
		return redactInstallError(err, token)
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

func runInstallStatus(ctx context.Context, opts installStatusOptions, stdout, stderr io.Writer) error {
	_ = stderr
	if opts.output != "json" {
		return fmt.Errorf("--output = %q, want json", opts.output)
	}
	endpoint, err := normalizeInstallerEndpoint(opts.endpoint)
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

func readInstallToken(value, path string) (string, error) {
	value = strings.TrimSpace(value)
	path = strings.TrimSpace(path)
	if (value == "") == (path == "") {
		return "", fmt.Errorf("exactly one of --token or --token-file is required")
	}
	if path != "" {
		file, err := os.Open(path)
		if err != nil {
			return "", fmt.Errorf("open installer token file: %w", err)
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			return "", fmt.Errorf("stat installer token file: %w", err)
		}
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("installer token file must be a regular file")
		}
		if info.Mode().Perm()&0o077 != 0 {
			return "", fmt.Errorf("installer token file permissions must not allow group or other access")
		}
		data, err := io.ReadAll(io.LimitReader(file, maxInstallTokenSize+1))
		if err != nil {
			return "", fmt.Errorf("read installer token file: %w", err)
		}
		if len(data) > maxInstallTokenSize {
			return "", fmt.Errorf("installer token file exceeds %d bytes", maxInstallTokenSize)
		}
		value = strings.TrimSpace(string(data))
	}
	if value == "" {
		return "", fmt.Errorf("installer token is empty")
	}
	if strings.ContainsAny(value, "\r\n") {
		return "", fmt.Errorf("installer token must be one line")
	}
	return value, nil
}

func validateInstallBundleSize(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat config bundle: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("config bundle must be a regular file")
	}
	if info.Size() <= 0 {
		return fmt.Errorf("config bundle is empty")
	}
	if info.Size() > maxInstallBundleSize {
		return fmt.Errorf("config bundle size %d exceeds %d bytes", info.Size(), maxInstallBundleSize)
	}
	return nil
}

func fetchInstallStatus(ctx context.Context, client *http.Client, endpoint string) (handoff.HandoffStatus, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/v1/status", nil)
	if err != nil {
		return handoff.HandoffStatus{}, fmt.Errorf("create installer status request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	return doInstallRequest(client, req, "read installer status")
}

func submitInstallBundle(ctx context.Context, client *http.Client, endpoint, token, path, digest, node string) (handoff.HandoffStatus, error) {
	file, err := os.Open(path)
	if err != nil {
		return handoff.HandoffStatus{}, fmt.Errorf("open config bundle: %w", err)
	}
	defer file.Close()
	requestURL, err := url.Parse(endpoint + "/v1/config-bundle")
	if err != nil {
		return handoff.HandoffStatus{}, fmt.Errorf("create installer handoff URL: %w", err)
	}
	query := requestURL.Query()
	query.Set("node", node)
	query.Set("digest", digest)
	requestURL.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL.String(), file)
	if err != nil {
		return handoff.HandoffStatus{}, fmt.Errorf("create installer handoff request: %w", err)
	}
	if info, statErr := file.Stat(); statErr == nil {
		req.ContentLength = info.Size()
	}
	req.Header.Set("Authorization", "Bearer "+token)
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

func redactInstallError(err error, token string) error {
	if err == nil || strings.TrimSpace(token) == "" {
		return err
	}
	return fmt.Errorf("%s", strings.ReplaceAll(err.Error(), token, "<redacted>"))
}
