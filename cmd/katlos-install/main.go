package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/installer"
	"github.com/katl-dev/katl/internal/installer/configbundle"
	"github.com/katl-dev/katl/internal/installer/discovery"
	"github.com/katl-dev/katl/internal/installer/disk"
	"github.com/katl-dev/katl/internal/installer/handoff"
	"github.com/katl-dev/katl/internal/installer/installmedia"
	"github.com/katl-dev/katl/internal/installer/katlosimage"
	"github.com/katl-dev/katl/internal/installer/kubeadmconfig"
	installstatus "github.com/katl-dev/katl/internal/installer/status"
)

var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "katlos-install: %v\n", err)
		os.Exit(1)
	}
}

func runManifest(ctx context.Context, manifestPath, stateDir, inputMode, inputSource string, stdout io.Writer) error {
	if manifestPath == "" {
		return fmt.Errorf("--manifest is required unless --list-states, --version, --apply-input, or --boot is set")
	}
	if stdout == nil {
		stdout = io.Discard
	}
	if strings.TrimSpace(inputSource) == "" {
		inputSource = manifestPath
	}

	install, err := manifestRunnerContext(manifestPath, stateDir, inputMode, inputSource)
	if err != nil {
		return err
	}
	runner := installer.NewRunner(installer.PreseededManifestPlan(), install)

	if err := runner.Run(ctx); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "katlos-install completed manifest=%s\n", manifestPath)
	return nil
}

func manifestRunnerContext(manifestPath, stateDir, inputMode, inputSource string) (*installer.Context, error) {
	mediaRoot, err := manifestMediaRoot(manifestPath)
	if err != nil {
		return nil, err
	}
	kubeadmConfigs, err := loadKubeadmConfigs(mediaRoot)
	if err != nil {
		return nil, err
	}
	commands := installer.NewExecCommandRunner()
	media, _, err := loadInstallMedia()
	if err != nil {
		return nil, err
	}
	return &installer.Context{
		ManifestPath: manifestPath,
		StateDir:     stateDir,
		TargetRoot:   "/mnt/target",
		Commands:     commands,
		Store:        installer.NewFileStateStore(stateDir),
		KatlosResolver: katlosimage.Resolver{
			MediaRoot: mediaRoot,
			WorkDir:   filepath.Join(stateDir, "katlos-image"),
			Commands:  commands,
		},
		MediaKatlosResolver: katlosimage.Resolver{
			MediaRoot: media.Root,
			WorkDir:   filepath.Join(stateDir, "katlos-image"),
			Commands:  commands,
		},
		DefaultKatlosImage: media.Image,
		Discovery:          discovery.NewCommandDiscoverySource(commands),
		RootSlotOpener:     disk.FileRootSlotDeviceOpener{},
		IdentityRandom:     rand.Reader,
		Chown:              os.Chown,
		KubeadmConfigs:     kubeadmConfigs,
		InputMode:          inputMode,
		InputSource:        inputSource,
	}, nil
}

func runBundle(ctx context.Context, bundlePath, selectedNode, expectedDigest, stateDir, inputMode, inputSource string, stdout io.Writer) error {
	if strings.TrimSpace(bundlePath) == "" {
		return fmt.Errorf("--bundle is required")
	}
	if stdout == nil {
		stdout = io.Discard
	}
	if strings.TrimSpace(inputSource) == "" {
		inputSource = bundlePath
	}
	media, _, err := loadInstallMedia()
	if err != nil {
		return err
	}
	selected, err := configbundle.ReadSelectedNodeFile(bundlePath, configbundle.ReadOptions{
		ExpectedDigest:     expectedDigest,
		NodeName:           selectedNode,
		DefaultKatlosImage: media.Image,
	})
	if err != nil {
		return err
	}
	manifestPath, err := writeBundleInstallManifest(stateDir, selected.InstallManifest)
	if err != nil {
		return err
	}
	install, err := bundleRunnerContext(bundlePath, manifestPath, stateDir, inputMode, inputSource, selected)
	if err != nil {
		return err
	}
	runner := installer.NewRunner(installer.PreseededManifestPlan(), install)
	if err := runner.Run(ctx); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "katlos-install completed bundle=%s node=%s\n", bundlePath, selected.Node.Name)
	return nil
}

func bundleRunnerContext(bundlePath, manifestPath, stateDir, inputMode, inputSource string, selected configbundle.SelectedNodeMaterial) (*installer.Context, error) {
	mediaRoot, err := manifestMediaRoot(bundlePath)
	if err != nil {
		return nil, err
	}
	commands := installer.NewExecCommandRunner()
	media, _, err := loadInstallMedia()
	if err != nil {
		return nil, err
	}
	return &installer.Context{
		ManifestPath: manifestPath,
		StateDir:     stateDir,
		TargetRoot:   "/mnt/target",
		Commands:     commands,
		Store:        installer.NewFileStateStore(stateDir),
		KatlosResolver: katlosimage.Resolver{
			MediaRoot: mediaRoot,
			WorkDir:   filepath.Join(stateDir, "katlos-image"),
			Commands:  commands,
		},
		MediaKatlosResolver: katlosimage.Resolver{
			MediaRoot: media.Root,
			WorkDir:   filepath.Join(stateDir, "katlos-image"),
			Commands:  commands,
		},
		DefaultKatlosImage:    media.Image,
		KatlosImageFromMedia:  selected.KatlosImageFromMedia,
		Discovery:             discovery.NewCommandDiscoverySource(commands),
		RootSlotOpener:        disk.FileRootSlotDeviceOpener{},
		IdentityRandom:        rand.Reader,
		Chown:                 os.Chown,
		KubeadmConfigs:        selected.KubeadmConfigs,
		InputMode:             inputMode,
		InputSource:           inputSource,
		BundleDigest:          selected.BundleDigest,
		SourceDigest:          selected.SourceDigest,
		NodeMaterialDigest:    selected.NodeMaterialDigest,
		InstallMaterialDigest: selected.InstallMaterialDigest,
	}, nil
}

func writeBundleInstallManifest(stateDir string, installManifest any) (string, error) {
	path := filepath.Join(stateDir, "input", "install-manifest.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("create bundle manifest dir: %w", err)
	}
	data, err := json.MarshalIndent(installManifest, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode bundle install material: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return "", fmt.Errorf("write bundle install material: %w", err)
	}
	return path, nil
}

func manifestMediaRoot(manifestPath string) (string, error) {
	path, err := filepath.Abs(manifestPath)
	if err != nil {
		return "", fmt.Errorf("resolve manifest path: %w", err)
	}
	return filepath.Dir(path), nil
}

func loadInstallMedia() (installmedia.Media, bool, error) {
	root := strings.TrimSpace(os.Getenv("KATL_INSTALL_MEDIA_ROOT"))
	if root == "" {
		root = filepath.Join(installer.DefaultMediaMount, "katl")
	}
	return installmedia.Load(root)
}

func loadKubeadmConfigs(mediaRoot string) (map[string]kubeadmconfig.Plan, error) {
	objectDir := filepath.Join(mediaRoot, installer.KubeadmConfigObjectsDir)
	entries, err := os.ReadDir(objectDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read kubeadm config object dir: %w", err)
	}
	configs := make(map[string]kubeadmconfig.Plan)
	for _, entry := range entries {
		if entry.IsDir() || !isKubeadmConfigObjectFile(entry.Name()) {
			continue
		}
		path := filepath.Join(objectDir, entry.Name())
		file, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("open kubeadm config object %s: %w", path, err)
		}
		object, err := kubeadmconfig.Decode(file)
		closeErr := file.Close()
		if err != nil {
			return nil, fmt.Errorf("decode kubeadm config object %s: %w", path, err)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close kubeadm config object %s: %w", path, closeErr)
		}
		if _, exists := configs[object.Metadata.Name]; exists {
			return nil, fmt.Errorf("duplicate kubeadm config %q", object.Metadata.Name)
		}
		plan, err := kubeadmconfig.Resolve(kubeadmconfig.ResolveRequest{RepoRoot: mediaRoot, Object: object})
		if err != nil {
			return nil, fmt.Errorf("resolve kubeadm config %q: %w", object.Metadata.Name, err)
		}
		configs[object.Metadata.Name] = plan
	}
	return configs, nil
}

func isKubeadmConfigObjectFile(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".yaml", ".yml", ".json":
		return true
	default:
		return false
	}
}

func runBoot(ctx context.Context, runDir, etcDir, handoffAddr string, stdout io.Writer) error {
	return runBootWithHandoff(ctx, runDir, etcDir, handoffAddr, stdout, runHandoff)
}

func runBootWithHandoff(ctx context.Context, runDir, etcDir, handoffAddr string, stdout io.Writer, handoffRunner func(context.Context, string, string, io.Writer) error) error {
	input, err := bootInput(runDir, etcDir)
	if err != nil {
		return err
	}
	for _, log := range input.Logs {
		fmt.Fprintf(stdout, "katlos-install input: %s\n", log)
	}
	inputMode := bootInputMode(input)
	manifestURL := redactURL(input.ManifestURL)
	bundleURL := redactURL(input.BundleURL)
	fmt.Fprintf(stdout, "katlos-install mode: action=%s installMode=%s manifestPath=%s manifestURL=%s bundlePath=%s bundleURL=%s node=%s inputMode=%s\n", input.Action, input.InstallMode, input.ManifestPath, manifestURL, input.BundlePath, bundleURL, input.NodeName, inputMode)

	switch input.Action {
	case installer.InstallActionHoldForDebug:
		fmt.Fprintln(stdout, "katlos-install debug hold active")
		<-ctx.Done()
		return ctx.Err()
	case installer.InstallActionWaitForConfig:
		return handoffRunner(ctx, runDir, handoffAddr, stdout)
	case installer.InstallActionRun:
		if input.BundleURL != "" {
			bundlePath, err := fetchBundleURL(ctx, input.BundleURL, input.BundleSHA256, runDir)
			if err != nil {
				return err
			}
			fmt.Fprintf(stdout, "katlos-install downloaded bundle url=%s path=%s\n", bundleURL, bundlePath)
			return runBundle(ctx, bundlePath, input.NodeName, input.BundleDigest, filepath.Join(runDir, "state"), inputMode, bundleURL, stdout)
		}
		if input.BundlePath != "" {
			return runBundle(ctx, input.BundlePath, input.NodeName, input.BundleDigest, filepath.Join(runDir, "state"), inputMode, input.BundlePath, stdout)
		}
		if input.ManifestURL != "" {
			manifestPath, err := fetchManifestURL(ctx, input.ManifestURL, input.ManifestSHA256, runDir)
			if err != nil {
				return err
			}
			fmt.Fprintf(stdout, "katlos-install downloaded manifest url=%s path=%s\n", manifestURL, manifestPath)
			return runManifest(ctx, manifestPath, filepath.Join(runDir, "state"), inputMode, manifestURL, stdout)
		}
		return runManifest(ctx, input.ManifestPath, filepath.Join(runDir, "state"), inputMode, input.ManifestPath, stdout)
	default:
		return fmt.Errorf("unsupported install action %q", input.Action)
	}
}

func fetchBundleURL(ctx context.Context, bundleURL, wantSHA256, runDir string) (string, error) {
	body, err := fetchURLWithSHA256(ctx, bundleURL, wantSHA256, "bundle", 64<<20, false)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return "", fmt.Errorf("create run dir: %w", err)
	}
	path := filepath.Join(runDir, "config.katlcfg")
	tmp, err := os.CreateTemp(runDir, ".config-bundle-*.katlcfg")
	if err != nil {
		return "", fmt.Errorf("create bundle temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("write bundle temp file: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("chmod bundle temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close bundle temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return "", fmt.Errorf("install fetched bundle: %w", err)
	}
	return path, nil
}

func fetchManifestURL(ctx context.Context, manifestURL, wantSHA256, runDir string) (string, error) {
	body, err := fetchURLWithSHA256(ctx, manifestURL, wantSHA256, "manifest", 1<<20, true)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return "", fmt.Errorf("create run dir: %w", err)
	}
	path := filepath.Join(runDir, "install-manifest.json")
	tmp, err := os.CreateTemp(runDir, ".install-manifest-*.json")
	if err != nil {
		return "", fmt.Errorf("create manifest temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("write manifest temp file: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("chmod manifest temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close manifest temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return "", fmt.Errorf("install fetched manifest: %w", err)
	}
	return path, nil
}

func fetchURLWithSHA256(ctx context.Context, rawURL, wantSHA256, label string, limit int64, requireDigest bool) ([]byte, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, fmt.Errorf("parse %s URL: invalid URL", label)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("%s URL must use http or https", label)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("%s URL missing host", label)
	}
	wantSHA256 = strings.ToLower(strings.TrimSpace(wantSHA256))
	if requireDigest && wantSHA256 == "" {
		return nil, fmt.Errorf("%s URL requires %sSHA256", label, label)
	}
	if wantSHA256 != "" {
		if len(wantSHA256) != 64 {
			return nil, fmt.Errorf("%sSHA256 must be 64 hexadecimal characters", label)
		}
		if _, err := hex.DecodeString(wantSHA256); err != nil {
			return nil, fmt.Errorf("%sSHA256 is not valid hex: %w", label, err)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("create %s request for %s: invalid URL", label, redactURL(rawURL))
	}
	client := http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s URL %s: request failed", label, redactURL(rawURL))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s URL: unexpected HTTP status %s", label, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read %s URL response: %w", label, err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("%s URL response exceeds %d bytes", label, limit)
	}
	gotSHA256 := sha256.Sum256(body)
	if got := hex.EncodeToString(gotSHA256[:]); wantSHA256 != "" && got != wantSHA256 {
		return nil, fmt.Errorf("%s URL digest mismatch: got %s want %s", label, got, wantSHA256)
	}
	return body, nil
}

func redactURL(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "<invalid-url>"
	}
	hadQuery := parsed.RawQuery != ""
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	if hadQuery {
		return parsed.String() + "?<redacted>"
	}
	return parsed.String()
}

func bootInputMode(input installer.BootInput) string {
	source := input.SelectedSources["manifestPath"]
	if input.SelectedSources["bundlePath"] != "" {
		source = input.SelectedSources["bundlePath"]
	}
	switch source {
	case installer.InputSourceRunKatl, installer.InputSourceEtcKatl, installer.InputSourceEmbeddedMedia, installer.InputSourceLocalFile:
		return installstatus.InputModeOfflineMedia
	default:
		return installstatus.InputModePXEPreseed
	}
}

func bootInput(runDir, etcDir string) (installer.BootInput, error) {
	var request installer.BootInputRequest
	request.KernelCmdline = readText("/proc/cmdline")
	addInputFile(&request, installer.InputSourceEtcKatl, filepath.Join(etcDir, "install-input.json"))
	addManifestFiles(&request, installer.InputSourceEtcKatl, etcDir)
	addBundleFiles(&request, installer.InputSourceEtcKatl, etcDir)
	addInputFile(&request, installer.InputSourceRunKatl, filepath.Join(runDir, "install-input.json"))
	addManifestFiles(&request, installer.InputSourceRunKatl, runDir)
	addBundleFiles(&request, installer.InputSourceRunKatl, runDir)
	return installer.DiscoverBootInput(request)
}

func addInputFile(request *installer.BootInputRequest, source installer.InputSource, path string) {
	data, ok := readFile(path)
	if !ok {
		return
	}
	request.Files = append(request.Files, installer.BootInputFile{
		Source:  source,
		Path:    path,
		Content: data,
	})
}

func addManifestFile(request *installer.BootInputRequest, source installer.InputSource, path string) {
	data, ok := readFile(path)
	if !ok {
		return
	}
	request.Manifest = data
	request.Files = append(request.Files, installer.BootInputFile{
		Source:  source,
		Path:    path + ".input",
		Content: []byte(fmt.Sprintf(`{"manifestPath":%q}`, path)),
	})
}

func addManifestFiles(request *installer.BootInputRequest, source installer.InputSource, dir string) {
	for _, name := range []string{"install-manifest.json", "install-manifest.yml", "install-manifest.yaml"} {
		addManifestFile(request, source, filepath.Join(dir, name))
	}
}

func addBundleFile(request *installer.BootInputRequest, source installer.InputSource, path string) {
	if _, ok := readFile(path); !ok {
		return
	}
	request.Files = append(request.Files, installer.BootInputFile{
		Source:  source,
		Path:    path + ".input",
		Content: []byte(fmt.Sprintf(`{"bundlePath":%q}`, path)),
	})
}

func addBundleFiles(request *installer.BootInputRequest, source installer.InputSource, dir string) {
	for _, name := range []string{"config.katlcfg", "cluster.katlcfg"} {
		addBundleFile(request, source, filepath.Join(dir, name))
	}
}

func readText(path string) string {
	data, ok := readFile(path)
	if !ok {
		return ""
	}
	return string(data)
}

func readFile(path string) ([]byte, bool) {
	data, err := os.ReadFile(path)
	return data, err == nil
}

func runHandoff(ctx context.Context, runDir, addr string, stdout io.Writer) error {
	media, _, err := loadInstallMedia()
	if err != nil {
		return err
	}
	server, err := handoff.NewHandoffServerWithDefaultImage("", nil, media.Image)
	if err != nil {
		return err
	}
	server.SetStatusReader(func() (installstatus.Record, error) {
		return installstatus.ReadFile(filepath.Join(runDir, "state", "status.json"))
	})
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen for handoff: %w", err)
	}
	defer listener.Close()

	httpServer := &http.Server{Handler: server.Handler()}
	errc := make(chan error, 1)
	go func() {
		if err := httpServer.Serve(listener); err != nil && err != http.ErrServerClosed {
			errc <- err
			return
		}
		errc <- nil
	}()
	defer httpServer.Shutdown(context.Background())

	fmt.Fprintln(stdout, "katlos-install waiting for handoff announcement address")
	baseURL, err := waitHandoffAnnouncementBaseURL(ctx, listener.Addr())
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, server.Announcement(baseURL))
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		if bundle := server.Bundle(); len(bundle.Data) > 0 {
			bundlePath := filepath.Join(runDir, "config.katlcfg")
			if err := os.MkdirAll(runDir, 0o755); err != nil {
				return fmt.Errorf("create handoff dir: %w", err)
			}
			if err := os.WriteFile(bundlePath, bundle.Data, 0o600); err != nil {
				return fmt.Errorf("write handoff config bundle: %w", err)
			}
			fmt.Fprintf(stdout, "katlos-install handoff accepted bundle=%s node=%s\n", bundlePath, bundle.NodeName)
			err := runBundle(ctx, bundlePath, bundle.NodeName, "", filepath.Join(runDir, "state"), installstatus.InputModeLocalHandoff, bundlePath, stdout)
			waitForHandoffStatusObservation(ctx)
			return err
		}
		if len(server.Manifest()) > 0 {
			manifestPath := filepath.Join(runDir, "install-manifest.json")
			if err := os.MkdirAll(runDir, 0o755); err != nil {
				return fmt.Errorf("create handoff dir: %w", err)
			}
			if err := os.WriteFile(manifestPath, server.Manifest(), 0o600); err != nil {
				return fmt.Errorf("write handoff manifest: %w", err)
			}
			if err := materializeHandoffPayloads(manifestPath, runDir, stdout); err != nil {
				return err
			}
			fmt.Fprintf(stdout, "katlos-install handoff accepted manifest=%s\n", manifestPath)
			err := runManifest(ctx, manifestPath, filepath.Join(runDir, "state"), installstatus.InputModeLocalHandoff, manifestPath, stdout)
			waitForHandoffStatusObservation(ctx)
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-errc:
			return err
		case <-ticker.C:
		}
	}
}

func waitForHandoffStatusObservation(ctx context.Context) {
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

func handoffAnnouncementBaseURL(addr net.Addr) (string, error) {
	return handoffAnnouncementBaseURLWithHost(addr, handoffAnnouncementHost)
}

func waitHandoffAnnouncementBaseURL(ctx context.Context, addr net.Addr) (string, error) {
	return waitHandoffAnnouncementBaseURLWithHost(ctx, addr, 30*time.Second, 250*time.Millisecond, handoffAnnouncementHost)
}

func waitHandoffAnnouncementBaseURLWithHost(ctx context.Context, addr net.Addr, timeout, interval time.Duration, detectHost func() (string, error)) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	deadline, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var lastErr error
	for {
		baseURL, err := handoffAnnouncementBaseURLWithHost(addr, detectHost)
		if err == nil {
			return baseURL, nil
		}
		lastErr = err
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", ctx.Err()
		case <-deadline.Done():
			timer.Stop()
			return "", fmt.Errorf("wait for handoff announcement address: %w", lastErr)
		case <-timer.C:
		}
	}
}

func handoffAnnouncementBaseURLWithHost(addr net.Addr, detectHost func() (string, error)) (string, error) {
	tcpAddr, ok := addr.(*net.TCPAddr)
	if !ok {
		return "", fmt.Errorf("handoff listener has unexpected address: %s", addr)
	}
	host := tcpAddr.IP.String()
	if tcpAddr.IP == nil || tcpAddr.IP.IsUnspecified() {
		detected, err := detectHost()
		if err != nil {
			return "", err
		}
		host = detected
	}
	return "http://" + net.JoinHostPort(host, fmt.Sprintf("%d", tcpAddr.Port)), nil
}

func handoffAnnouncementHost() (string, error) {
	if ip, ok := outboundIP(); ok {
		return ip.String(), nil
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "", fmt.Errorf("discover handoff announcement address: %w", err)
	}
	for _, addr := range addrs {
		ip := interfaceIP(addr)
		if handoffAnnouncementIP(ip) {
			return ip.String(), nil
		}
	}
	return "", errors.New("discover handoff announcement address: no non-loopback interface address found")
}

func outboundIP() (net.IP, bool) {
	conn, err := net.Dial("udp", "192.0.2.1:9")
	if err != nil {
		return nil, false
	}
	defer conn.Close()
	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok || !handoffAnnouncementIP(addr.IP) {
		return nil, false
	}
	return addr.IP, true
}

func interfaceIP(addr net.Addr) net.IP {
	switch value := addr.(type) {
	case *net.IPNet:
		return value.IP
	case *net.IPAddr:
		return value.IP
	default:
		return nil
	}
}

func handoffAnnouncementIP(ip net.IP) bool {
	return ip != nil && !ip.IsUnspecified() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast()
}

func materializeHandoffPayloads(manifestPath, runDir string, stdout io.Writer) error {
	copied, err := installer.CopyManifestPayloads(manifestPath, filepath.Join(runDir, "preseed"), runDir)
	if err != nil {
		return fmt.Errorf("materialize handoff payloads: %w", err)
	}
	for _, payload := range copied {
		fmt.Fprintf(stdout, "katlos-install handoff copied %s to %s\n", payload.Source, payload.Destination)
	}
	return nil
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("katlos-install", flag.ContinueOnError)
	flags.SetOutput(stderr)

	manifestPath := flags.String("manifest", "", "path to install manifest")
	bundlePath := flags.String("bundle", "", "path to Katl config bundle")
	nodeName := flags.String("node", "", "selected node name for config bundle")
	bundleDigest := flags.String("bundle-digest", "", "expected config bundle digest")
	stateDir := flags.String("state-dir", "/var/lib/katl/install", "installer state directory")
	listStates := flags.Bool("list-states", false, "print the installer state order and exit")
	showVersion := flags.Bool("version", false, "print build metadata and exit")
	applyInput := flags.Bool("apply-input", false, "copy preseeded installer input and exit")
	boot := flags.Bool("boot", false, "run installer boot entrypoint")
	preseedDir := flags.String("preseed-dir", "", "additional installer preseed directory")
	seedWait := flags.Duration("seed-wait", 15*time.Second, "time to wait for installer seed devices")
	runDir := flags.String("run-dir", "/run/katl", "runtime installer input directory")
	etcDir := flags.String("etc-dir", "/etc/katl", "persistent installer input directory")
	handoffAddr := flags.String("handoff-addr", "0.0.0.0:8080", "installer handoff listen address")

	if err := flags.Parse(args); err != nil {
		return err
	}

	if *showVersion {
		fmt.Fprintf(stdout, "katlos-install version=%s commit=%s date=%s\n", version, commit, date)
		return nil
	}

	if *applyInput {
		preseedDirs := installer.DefaultPreseedDirs()
		if strings.TrimSpace(*preseedDir) != "" {
			preseedDirs = append([]string{strings.TrimSpace(*preseedDir)}, preseedDirs...)
		}
		return installer.ApplyInput(installer.InputApplyRequest{
			Context:      ctx,
			PreseedDirs:  preseedDirs,
			MediaDevices: installer.DefaultMediaDevices,
			MediaMount:   installer.DefaultMediaMount,
			SeedDevices:  installer.DefaultSeedDevices,
			SeedMount:    installer.DefaultSeedMount,
			SeedWait:     *seedWait,
			Commands:     installer.NewExecCommandRunner(),
			RunDir:       *runDir,
			EtcDir:       *etcDir,
			Stdout:       stdout,
		})
	}

	if *boot {
		return runBoot(ctx, *runDir, *etcDir, *handoffAddr, stdout)
	}

	plan := installer.DefaultPlan()
	if *listStates {
		for _, id := range plan.IDs() {
			fmt.Fprintln(stdout, id)
		}
		return nil
	}

	if strings.TrimSpace(*bundlePath) != "" {
		return runBundle(ctx, strings.TrimSpace(*bundlePath), strings.TrimSpace(*nodeName), strings.TrimSpace(*bundleDigest), *stateDir, installstatus.InputModePXEPreseed, strings.TrimSpace(*bundlePath), stdout)
	}
	return runManifest(ctx, strings.TrimSpace(*manifestPath), *stateDir, installstatus.InputModePXEPreseed, strings.TrimSpace(*manifestPath), stdout)
}
