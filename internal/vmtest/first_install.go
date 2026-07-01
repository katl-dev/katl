package vmtest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/zariel/katl/internal/installer"
	"github.com/zariel/katl/internal/installer/handoff"
)

type FirstInstallConfig struct {
	Installer       InstallerBootConfig
	Runtime         InstalledRuntimeConfig
	UseInstalledESP bool
	ESPExtractor    InstalledESPExtractor
	Manifest        []byte
	ManifestPath    string
	ConfigBundle    string
	SelectedNode    string
	GuestHandoff    bool
	PreseedManifest bool
	HandoffToken    string
	HandoffURL      string
	HandoffPoster   func(context.Context, string, string, []byte) (int, string, error)
	TargetDisk      DiskFixture
	DiskRunner      DiskRunner
	PreseedRunner   DiskRunner
	InstallerRunner VMRunner
	RuntimeRunner   VMRunner
}

type InstalledESPExtractor func(context.Context, DiskPlan, string) (string, error)

const (
	guestHandoffSignal         = "katlos-install waiting for config at "
	guestHandoffAcceptedSignal = "katlos-install handoff accepted manifest="
	installerCompletedSignal   = "katlos-install completed manifest="
	bundleCompletedSignal      = "katlos-install completed bundle="
)

type handoffLog struct {
	URL          string `json:"url"`
	PostURL      string `json:"postUrl,omitempty"`
	Token        string `json:"token,omitempty"`
	ManifestPath string `json:"manifestPath"`
	Announcement string `json:"announcement,omitempty"`
	GuestAddress string `json:"guestAddress,omitempty"`
	DomainName   string `json:"domainName,omitempty"`
	SerialLog    string `json:"serialLog,omitempty"`
	SerialTail   string `json:"serialTail,omitempty"`
	StatusCode   int    `json:"statusCode,omitempty"`
	Body         string `json:"body,omitempty"`
}

func RunFirstInstall(ctx context.Context, runner Runner, scenario Scenario, config FirstInstallConfig) (Result, error) {
	scenario = withTarget(scenario, config.TargetDisk)
	result, err := runner.Plan(scenario)
	if err != nil {
		return Result{}, err
	}
	result.start(runner.time())
	if err := CreateDisks(ctx, diskExec(config.DiskRunner), result.Disks); err != nil {
		return failFirst(runner, scenario, result, "prepare-fixtures", err)
	}
	manifest, err := loadManifest(config)
	if err != nil {
		return failFirst(runner, scenario, result, "install-manifest", err)
	}
	if err := writeManifest(result, manifest); err != nil {
		return failFirst(runner, scenario, result, "install-manifest", err)
	}
	if err := copyFirstInstallSingleImageProof(result, config); err != nil {
		return failFirst(runner, scenario, result, "single-image-proof", err)
	}
	if config.PreseedManifest {
		preseed, err := writePreseedMedia(ctx, result, config, manifest)
		if err != nil {
			return failFirst(runner, scenario, result, "preseed", err)
		}
		config.Installer.VM.PreseedImage = preseed.Image
		config.Installer.VM.EFIDiskImage = true
		config.Installer.VM.MediaRunner = config.PreseedRunner
		if config.Installer.Expect == "" && config.Installer.VM.Expect == "" {
			config.Installer.Expect = firstInstallCompletedSignal(config)
		}
	}
	if config.GuestHandoff {
		preseed, err := writeGuestHandoffSeedMedia(ctx, result, config, manifest)
		if err != nil {
			return failFirst(runner, scenario, result, "guest-handoff-seed", err)
		}
		config.Installer.VM.PreseedImage = preseed.Image
		config.Installer.VM.MediaRunner = config.PreseedRunner
		config, err = configureGuestHandoff(result, config, manifest)
		if err != nil {
			return failFirst(runner, scenario, result, "guest-handoff", err)
		}
	}

	config.Installer.VM.SerialHooks = append(config.Installer.VM.SerialHooks, firstInstallFailureHooks()...)
	result = BootInstaller(ctx, result, config.Installer, config.InstallerRunner)
	if err := copyArtifact(result.Artifacts.LaunchCommand, result.Artifacts.InstallerLaunchCommand); err != nil {
		return failFirst(runner, scenario, result, "installer", err)
	}
	if result.Status != StatusPassed {
		if err := runner.Write(scenario, result); err != nil {
			return result, err
		}
		return result, nil
	}
	if config.GuestHandoff {
		if err := requireGuestHandoff(result); err != nil {
			return failFirst(runner, scenario, result, "guest-handoff", err)
		}
		now := runner.time()
		result.addPhase("guest-handoff", StatusPassed, "", now, now)
	} else if config.PreseedManifest {
		if err := requirePreseedInstallerEvidence(result); err != nil {
			return failFirst(runner, scenario, result, "preseed", err)
		}
		now := runner.time()
		result.addPhase("preseed", StatusPassed, "", now, now)
	} else {
		if err := deliverHandoff(ctx, result, config, manifest); err != nil {
			return failFirst(runner, scenario, result, "local-handoff", err)
		}
		now := runner.time()
		result.addPhase("local-handoff", StatusPassed, "", now, now)
	}
	if config.UseInstalledESP {
		esp, err := extractInstalledESP(ctx, result, config)
		if err != nil {
			return failFirst(runner, scenario, result, "installed-esp", err)
		}
		config.Runtime.ESPArtifacts = esp
		now := runner.time()
		result.addPhase("installed-esp", StatusPassed, "", now, now)
	}

	runtime, err := runtimeConfig(result, config.Runtime)
	if err != nil {
		return failFirst(runner, scenario, result, "runtime", err)
	}
	disks := result.Disks
	bootResult := result
	bootResult.Disks = nil
	result = RunInstalledRuntime(ctx, bootResult, runtime, config.RuntimeRunner)
	result.Disks = disks
	if err := copyArtifact(result.Artifacts.LaunchCommand, result.Artifacts.RuntimeLaunchCommand); err != nil {
		return failFirst(runner, scenario, result, "runtime", err)
	}
	if result.Status == StatusPassed {
		if err := CleanupDisks(result); err != nil {
			return failFirst(runner, scenario, result, "cleanup", err)
		}
	}
	if err := runner.Write(scenario, result); err != nil {
		return result, err
	}
	return result, nil
}

func copyFirstInstallSingleImageProof(result Result, config FirstInstallConfig) error {
	if strings.TrimSpace(config.ManifestPath) == "" {
		return nil
	}
	source := filepath.Join(filepath.Dir(config.ManifestPath), "single-image-proof.json")
	if _, err := os.Stat(source); err != nil {
		return err
	}
	return copyRequiredFile(source, result.Artifacts.SingleImageProof, 0o600)
}

func firstInstallFailureHooks() []SerialHook {
	return []SerialHook{
		{
			Name:   "katlos-install-service-failed",
			Signal: "katlos-install.service: Failed with result",
			Run: func(_ context.Context, event SerialHookEvent) error {
				return fmt.Errorf("installer service failed; serial tail:\n%s", serialTail(event.Plan.SerialLog, 20, 6000))
			},
		},
		{
			Name:   "initrd-switch-root-failed",
			Signal: "initrd-switch-root.service: Failed with result",
			Run: func(_ context.Context, event SerialHookEvent) error {
				if strings.Contains(event.SerialText, installerCompletedSignal) {
					return nil
				}
				return fmt.Errorf("installer initrd attempted switch-root and failed; serial tail:\n%s", serialTail(event.Plan.SerialLog, 20, 6000))
			},
		},
	}
}

func withTarget(scenario Scenario, target DiskFixture) Scenario {
	if len(scenario.Disks) > 0 {
		return scenario
	}
	if target.Name == "" {
		target = TargetDisk("root", string(DiskQCOW2), "32G")
	}
	scenario.Disks = []DiskFixture{target}
	return scenario
}

func diskExec(runner DiskRunner) DiskRunner {
	if runner != nil {
		return runner
	}
	return ExecDiskRunner{}
}

func loadManifest(config FirstInstallConfig) ([]byte, error) {
	if len(config.Manifest) > 0 {
		return append([]byte(nil), config.Manifest...), nil
	}
	if config.ManifestPath == "" {
		return nil, errors.New("install manifest is required")
	}
	data, err := os.ReadFile(config.ManifestPath)
	if err != nil {
		return nil, fmt.Errorf("read install manifest: %w", err)
	}
	return data, nil
}

func configureGuestHandoff(result Result, config FirstInstallConfig, manifest []byte) (FirstInstallConfig, error) {
	if config.Installer.Expect == "" && config.Installer.VM.Expect == "" {
		config.Installer.Expect = firstInstallCompletedSignal(config)
	}
	config.Installer.VM.SerialHooks = append(config.Installer.VM.SerialHooks, SerialHook{
		Name:   "installer-guest-handoff",
		Signal: guestHandoffSignal,
		Run: func(ctx context.Context, event SerialHookEvent) error {
			return deliverGuestHandoff(ctx, result, config, manifest, event, event.SerialText)
		},
	})
	return config, nil
}

func requireGuestHandoff(result Result) error {
	if _, err := os.Stat(result.Artifacts.HandoffResponse); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errors.New("guest handoff response artifact is missing")
		}
		return fmt.Errorf("stat guest handoff response: %w", err)
	}
	return nil
}

func requirePreseedInstallerEvidence(result Result) error {
	serial, err := os.ReadFile(result.Artifacts.InstallerSerial)
	if err != nil {
		return fmt.Errorf("read installer serial for preseed evidence: %w", err)
	}
	text := string(serial)
	signals := []string{
		"katl input: mounted seed device",
		"katl input: copied",
		"inputMode=offline-media",
	}
	if strings.Contains(text, "bundlePath=/run/katl/preseed/config.katlcfg") {
		signals = append(signals, "bundlePath=/run/katl/preseed/config.katlcfg")
	} else {
		signals = append(signals, "manifestPath=/run/katl/preseed/install-manifest.json")
	}
	for _, signal := range signals {
		if !strings.Contains(text, signal) {
			return fmt.Errorf("installer serial missing preseed signal %q", signal)
		}
	}
	return nil
}

func firstInstallCompletedSignal(config FirstInstallConfig) string {
	if strings.TrimSpace(config.ConfigBundle) != "" {
		return bundleCompletedSignal
	}
	return installerCompletedSignal
}

func writeManifest(result Result, manifest []byte) error {
	if err := os.MkdirAll(filepath.Dir(result.Artifacts.InstallManifest), 0o755); err != nil {
		return err
	}
	return os.WriteFile(result.Artifacts.InstallManifest, manifest, 0o600)
}

type preseedMedia struct {
	Dir   string
	Image string
}

func writePreseedMedia(ctx context.Context, result Result, config FirstInstallConfig, manifest []byte) (preseedMedia, error) {
	dir := filepath.Join(result.Artifacts.ManifestsDir, "preseed")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return preseedMedia{}, err
	}
	input := struct {
		ManifestPath string `json:"manifestPath,omitempty"`
		BundlePath   string `json:"bundlePath,omitempty"`
		NodeName     string `json:"nodeName,omitempty"`
		InstallMode  string `json:"installMode"`
	}{InstallMode: "auto"}
	if strings.TrimSpace(config.ConfigBundle) != "" {
		input.BundlePath = "/run/katl/preseed/config.katlcfg"
		input.NodeName = strings.TrimSpace(config.SelectedNode)
		if input.NodeName == "" {
			return preseedMedia{}, errors.New("selected node is required for preseed config bundle")
		}
		if err := copyRequiredFile(config.ConfigBundle, filepath.Join(dir, "config.katlcfg"), 0o600); err != nil {
			return preseedMedia{}, fmt.Errorf("copy preseed config bundle: %w", err)
		}
	} else {
		input.ManifestPath = "/run/katl/preseed/install-manifest.json"
	}
	if err := writeJSON(filepath.Join(dir, "install-input.json"), input); err != nil {
		return preseedMedia{}, err
	}
	if input.ManifestPath != "" {
		if err := os.WriteFile(filepath.Join(dir, "install-manifest.json"), manifest, 0o600); err != nil {
			return preseedMedia{}, err
		}
	}
	if err := copyPreseedLocalRef(config, result, manifest, dir); err != nil {
		return preseedMedia{}, err
	}
	if input.ManifestPath != "" {
		if err := copyPreseedKubeadmDirs(config, result, dir); err != nil {
			return preseedMedia{}, err
		}
	}
	image := filepath.Join(result.Artifacts.ManifestsDir, "preseed.img")
	if err := createPreseedImage(ctx, dir, image, config.PreseedRunner); err != nil {
		return preseedMedia{}, err
	}
	return preseedMedia{Dir: dir, Image: image}, nil
}

func writeGuestHandoffSeedMedia(ctx context.Context, result Result, config FirstInstallConfig, manifest []byte) (preseedMedia, error) {
	dir := filepath.Join(result.Artifacts.ManifestsDir, "handoff-seed")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return preseedMedia{}, err
	}
	networkDir := filepath.Join(dir, "etc/systemd/network")
	if err := os.MkdirAll(networkDir, 0o755); err != nil {
		return preseedMedia{}, err
	}
	network := "[Match]\nName=en*\n\n[Network]\nDHCP=yes\n"
	if err := os.WriteFile(filepath.Join(networkDir, "80-katl-vmtest-installer-dhcp.network"), []byte(network), 0o644); err != nil {
		return preseedMedia{}, err
	}
	if err := copyPreseedLocalRef(config, result, manifest, dir); err != nil {
		return preseedMedia{}, err
	}
	if strings.TrimSpace(config.ConfigBundle) == "" {
		if err := copyPreseedKubeadmDirs(config, result, dir); err != nil {
			return preseedMedia{}, err
		}
	}
	image := filepath.Join(result.Artifacts.ManifestsDir, "handoff-seed.img")
	if err := createPreseedImage(ctx, dir, image, config.PreseedRunner); err != nil {
		return preseedMedia{}, err
	}
	return preseedMedia{Dir: dir, Image: image}, nil
}

func copyPreseedLocalRef(config FirstInstallConfig, result Result, manifest []byte, preseedDir string) error {
	var input struct {
		KatlosImage struct {
			LocalRef string `json:"localRef"`
		} `json:"katlosImage"`
	}
	if err := json.Unmarshal(manifest, &input); err != nil {
		return fmt.Errorf("decode preseed install manifest: %w", err)
	}
	localRef := strings.TrimSpace(input.KatlosImage.LocalRef)
	if localRef == "" {
		return nil
	}
	if filepath.IsAbs(localRef) || filepath.Clean(localRef) != localRef || localRef == "." || strings.HasPrefix(localRef, "../") || strings.Contains(localRef, "/../") {
		return fmt.Errorf("preseed KatlOS image localRef %q must be a clean relative path", localRef)
	}
	manifestRoot := filepath.Dir(result.Artifacts.InstallManifest)
	if config.ManifestPath != "" {
		manifestRoot = filepath.Dir(config.ManifestPath)
	}
	src := filepath.Join(manifestRoot, filepath.FromSlash(localRef))
	dst := filepath.Join(preseedDir, filepath.FromSlash(localRef))
	if err := copyRequiredFile(src, dst, 0o600); err != nil {
		return fmt.Errorf("copy preseed KatlOS image localRef: %w", err)
	}
	return nil
}

func copyPreseedKubeadmDirs(config FirstInstallConfig, result Result, preseedDir string) error {
	manifestRoot := filepath.Dir(result.Artifacts.InstallManifest)
	if config.ManifestPath != "" {
		manifestRoot = filepath.Dir(config.ManifestPath)
	}
	for _, name := range []string{installer.KubeadmConfigObjectsDir, installer.KubeadmConfigFilesDir} {
		src := filepath.Join(manifestRoot, name)
		dst := filepath.Join(preseedDir, name)
		if err := copyOptionalDir(src, dst); err != nil {
			return fmt.Errorf("copy preseed %s: %w", name, err)
		}
	}
	return nil
}

func copyRequiredFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func copyOptionalDir(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", src)
	}
	return filepath.WalkDir(src, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file", path)
		}
		return copyRequiredFile(path, target, info.Mode().Perm())
	})
}

func createPreseedImage(ctx context.Context, dir, image string, runner DiskRunner) error {
	return createFATImage(ctx, dir, image, "KATLSEED", runner)
}

func createFATImage(ctx context.Context, dir, image, label string, runner DiskRunner) error {
	size, err := fatImageSize(dir)
	if err != nil {
		return err
	}
	if runner == nil {
		runner = ExecDiskRunner{}
	}
	if err := runner.Run(ctx, "truncate", "-s", strconv.FormatInt(size, 10), image); err != nil {
		return fmt.Errorf("create FAT image file: %w", err)
	}
	if err := runner.Run(ctx, "mformat", "-i", image, "-F", "-v", label, "::"); err != nil {
		return fmt.Errorf("format FAT image: %w", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read FAT image source dir: %w", err)
	}
	args := []string{"-i", image, "-s"}
	for _, entry := range entries {
		args = append(args, filepath.Join(dir, entry.Name()))
	}
	args = append(args, "::/")
	if err := runner.Run(ctx, "mcopy", args...); err != nil {
		return fmt.Errorf("copy files into FAT image: %w", err)
	}
	return nil
}

func fatImageSize(dir string) (int64, error) {
	var payload int64
	if err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		payload += info.Size()
		return nil
	}); err != nil {
		return 0, fmt.Errorf("measure FAT image source dir: %w", err)
	}
	const (
		minSize = int64(8 * 1024 * 1024)
		slack   = int64(64 * 1024 * 1024)
		mb      = int64(1024 * 1024)
	)
	size := payload + slack
	if size < minSize {
		size = minSize
	}
	if rem := size % mb; rem != 0 {
		size += mb - rem
	}
	return size, nil
}

func extractInstalledESP(ctx context.Context, result Result, config FirstInstallConfig) (string, error) {
	target, err := firstTargetDisk(result)
	if err != nil {
		return "", err
	}
	extractor := config.ESPExtractor
	if extractor == nil {
		extractor = ExtractInstalledESPArtifacts
	}
	return extractor(ctx, target, result.Artifacts.InstalledESP)
}

func firstTargetDisk(result Result) (DiskPlan, error) {
	for _, disk := range result.Disks {
		if disk.Kind == DiskTarget || disk.Kind == DiskSnapshot {
			return disk, nil
		}
	}
	return DiskPlan{}, errors.New("first install result has no target disk")
}

func ExtractInstalledESPArtifacts(ctx context.Context, disk DiskPlan, outputDir string) (string, error) {
	if strings.TrimSpace(outputDir) == "" {
		return "", errors.New("installed ESP output directory is required")
	}
	if strings.TrimSpace(disk.HostPath) == "" {
		return "", errors.New("installed runtime disk path is required")
	}
	if _, err := os.Stat(disk.HostPath); err != nil {
		return "", fmt.Errorf("installed runtime disk not found: %w", err)
	}
	format := diskFormat(disk.Format)
	switch format {
	case DiskRaw, DiskQCOW2:
	default:
		return "", fmt.Errorf("installed runtime disk format %q is unsupported", format)
	}
	stateDir := filepath.Join(filepath.Dir(outputDir), "installed-esp-extract")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return "", err
	}
	rawDisk := disk.HostPath
	if format == DiskQCOW2 {
		rawDisk = filepath.Join(stateDir, "installed-runtime.raw")
		if err := runTool(ctx, "qemu-img", "convert", "-f", string(DiskQCOW2), "-O", string(DiskRaw), disk.HostPath, rawDisk); err != nil {
			return "", fmt.Errorf("convert installed runtime disk: %w", err)
		}
	}
	tableJSON, err := runToolOutput(ctx, "sfdisk", "--json", rawDisk)
	if err != nil {
		return "", fmt.Errorf("inspect installed runtime disk partitions: %w", err)
	}
	partition, sectorSize, err := installedESPPartition(tableJSON)
	if err != nil {
		return "", err
	}
	offset := partition.Start * sectorSize
	if err := os.RemoveAll(outputDir); err != nil {
		return "", err
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return "", err
	}
	if err := runTool(ctx, "mcopy", "-s", "-i", fmt.Sprintf("%s@@%d", rawDisk, offset), "::*", outputDir+string(os.PathSeparator)); err != nil {
		return "", fmt.Errorf("copy installed ESP artifacts: %w", err)
	}
	if err := checkExtractedESPArtifacts(outputDir); err != nil {
		return "", err
	}
	return outputDir, nil
}

type installedESPPartitionRecord struct {
	Start int64
	Size  int64
}

func installedESPPartition(data []byte) (installedESPPartitionRecord, int64, error) {
	var record struct {
		PartitionTable struct {
			SectorSize int64 `json:"sectorsize"`
			Partitions []struct {
				Name  string `json:"name"`
				Type  string `json:"type"`
				Start int64  `json:"start"`
				Size  int64  `json:"size"`
			} `json:"partitions"`
		} `json:"partitiontable"`
	}
	if err := json.Unmarshal(data, &record); err != nil {
		return installedESPPartitionRecord{}, 0, fmt.Errorf("decode installed disk partition table: %w", err)
	}
	sectorSize := record.PartitionTable.SectorSize
	if sectorSize == 0 {
		sectorSize = 512
	}
	if sectorSize < 0 {
		return installedESPPartitionRecord{}, 0, errors.New("disk sector size is invalid")
	}
	for _, partition := range record.PartitionTable.Partitions {
		if partition.Name != "KATL_ESP" && strings.ToLower(partition.Type) != "c12a7328-f81f-11d2-ba4b-00a0c93ec93b" {
			continue
		}
		if partition.Start < 0 || partition.Size <= 0 {
			return installedESPPartitionRecord{}, 0, errors.New("ESP partition geometry is invalid")
		}
		return installedESPPartitionRecord{Start: partition.Start, Size: partition.Size}, sectorSize, nil
	}
	return installedESPPartitionRecord{}, 0, errors.New("installed disk has no KATL_ESP partition")
}

func checkExtractedESPArtifacts(root string) error {
	entriesDir := filepath.Join(root, "loader", "entries")
	entries, err := os.ReadDir(entriesDir)
	if err != nil {
		return fmt.Errorf("extracted ESP missing loader/entries: %w", err)
	}
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), ".conf") {
			return nil
		}
	}
	return errors.New("extracted ESP contains no loader entries")
}

func runTool(ctx context.Context, name string, args ...string) error {
	output, err := runToolOutput(ctx, name, args...)
	if err != nil {
		return fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, output)
	}
	return nil
}

func runToolOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	return cmd.CombinedOutput()
}

func deliverGuestHandoff(ctx context.Context, result Result, config FirstInstallConfig, manifest []byte, event SerialHookEvent, serialText string) error {
	announcement, url, token, err := parseHandoffAnnouncement(serialText)
	if err != nil {
		return err
	}
	postURL := strings.TrimSpace(config.HandoffURL)
	if postURL == "" {
		postURL = url
	}
	postURL = handoffPostURL(postURL, config)
	request := handoffLog{
		URL:          url,
		PostURL:      postURL,
		Token:        token,
		ManifestPath: result.Artifacts.InstallManifest,
		Announcement: announcement,
		GuestAddress: handoffGuestAddress(url),
		DomainName:   event.Plan.DomainName,
		SerialLog:    event.Plan.SerialLog,
		SerialTail:   serialTail(event.Plan.SerialLog, 12, 4000),
	}
	if err := writeJSON(result.Artifacts.HandoffRequest, request); err != nil {
		return err
	}
	status, body, err := postHandoff(ctx, config, postURL, token, manifest)
	if err != nil {
		return fmt.Errorf("guest handoff post failed: %w; %s", err, handoffContext(request))
	}
	if err := writeHandoff(result, url, status, body); err != nil {
		return fmt.Errorf("%w; %s", err, handoffContext(request))
	}
	return nil
}

func handoffGuestAddress(rawURL string) string {
	parsed, err := neturl.Parse(rawURL)
	if err != nil {
		return ""
	}
	return parsed.Hostname()
}

func handoffContext(log handoffLog) string {
	parts := []string{
		"guest=" + first(log.GuestAddress, log.URL),
	}
	if log.DomainName != "" {
		parts = append(parts, "domain="+log.DomainName)
	}
	if log.SerialLog != "" {
		parts = append(parts, "serial="+log.SerialLog)
	}
	if log.SerialTail != "" {
		parts = append(parts, "serial tail:\n"+log.SerialTail)
	}
	return strings.Join(parts, "; ")
}

func parseHandoffAnnouncement(serialText string) (announcement string, url string, token string, err error) {
	for _, line := range strings.Split(serialText, "\n") {
		line = strings.TrimSpace(line)
		if strings.Contains(line, guestHandoffSignal) {
			announcement = line
		}
	}
	if announcement == "" {
		return "", "", "", errors.New("handoff announcement not found in installer serial log")
	}
	start := strings.Index(announcement, guestHandoffSignal)
	if start < 0 {
		return "", "", "", fmt.Errorf("could not parse handoff announcement: %s", announcement)
	}
	payload := strings.TrimSpace(announcement[start+len(guestHandoffSignal):])
	const tokenPrefix = " token="
	tokenStart := strings.LastIndex(payload, tokenPrefix)
	if tokenStart < 0 {
		return "", "", "", fmt.Errorf("could not parse handoff token from announcement: %s", announcement)
	}
	url = strings.TrimSpace(payload[:tokenStart])
	token = strings.TrimSpace(payload[tokenStart+len(tokenPrefix):])
	if url == "" || token == "" {
		return "", "", "", fmt.Errorf("could not parse handoff URL/token from announcement: %s", announcement)
	}
	return announcement, url, token, nil
}

func deliverHandoff(ctx context.Context, result Result, config FirstInstallConfig, manifest []byte) error {
	url := config.HandoffURL
	token := config.HandoffToken
	var announcement string
	var handler http.Handler
	if url == "" {
		server, err := handoff.NewHandoffServer(token, nil)
		if err != nil {
			return err
		}
		handler = server.Handler()
		url = handoffPostURL("http://vmtest.local/v1/install", config)
		token = server.Token()
		announcement = server.Announcement("http://vmtest.local")
	}
	if token == "" {
		return errors.New("handoff token is required")
	}

	request := handoffLog{
		URL:          url,
		Token:        token,
		ManifestPath: result.Artifacts.InstallManifest,
		Announcement: announcement,
	}
	if err := writeJSON(result.Artifacts.HandoffRequest, request); err != nil {
		return err
	}

	if handler != nil {
		status, body, err := postLocal(ctx, handler, url, token, config, manifest)
		if err != nil {
			return err
		}
		return writeHandoff(result, url, status, body)
	}
	status, body, err := postHandoff(ctx, config, url, token, manifest)
	if err != nil {
		return err
	}
	return writeHandoff(result, url, status, body)
}

func postHandoff(ctx context.Context, config FirstInstallConfig, url, token string, manifest []byte) (int, string, error) {
	payload, contentType, err := handoffPayload(config, manifest)
	if err != nil {
		return 0, "", err
	}
	url = handoffPostURL(url, config)
	if config.HandoffPoster != nil {
		return config.HandoffPoster(ctx, url, token, payload)
	}
	return postRemote(ctx, url, token, payload, contentType)
}

func postLocal(ctx context.Context, handler http.Handler, url, token string, config FirstInstallConfig, manifest []byte) (int, string, error) {
	payload, contentType, err := handoffPayload(config, manifest)
	if err != nil {
		return 0, "", err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return 0, "", err
	}
	httpRequest.Header.Set("Content-Type", contentType)
	httpRequest.Header.Set("X-Katl-Install-Token", token)
	response := &responseCapture{header: http.Header{}}
	handler.ServeHTTP(response, httpRequest)
	if response.status == 0 {
		response.status = http.StatusOK
	}
	return response.status, response.body.String(), nil
}

func postRemote(ctx context.Context, url, token string, payload []byte, contentType string) (int, string, error) {
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return 0, "", err
	}
	httpRequest.Header.Set("Content-Type", contentType)
	httpRequest.Header.Set("X-Katl-Install-Token", token)
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Do(httpRequest)
	if err != nil {
		return 0, "", fmt.Errorf("post handoff manifest: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return 0, "", fmt.Errorf("read handoff response: %w", err)
	}
	return response.StatusCode, string(body), nil
}

func handoffPayload(config FirstInstallConfig, manifest []byte) ([]byte, string, error) {
	if strings.TrimSpace(config.ConfigBundle) == "" {
		return manifest, "application/json", nil
	}
	data, err := os.ReadFile(config.ConfigBundle)
	if err != nil {
		return nil, "", fmt.Errorf("read handoff config bundle: %w", err)
	}
	return data, "application/vnd.katl.config.bundle.v1", nil
}

func handoffPostURL(raw string, config FirstInstallConfig) string {
	if strings.TrimSpace(config.ConfigBundle) == "" {
		return raw
	}
	parsed, err := neturl.Parse(raw)
	if err != nil {
		return raw
	}
	base := strings.TrimRight(filepath.ToSlash(filepath.Dir(parsed.Path)), "/")
	if parsed.Path == "" || parsed.Path == "/" || base == "." {
		base = ""
	}
	parsed.Path = base + "/config-bundle"
	query := parsed.Query()
	if node := strings.TrimSpace(config.SelectedNode); node != "" {
		query.Set("node", node)
	}
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func writeHandoff(result Result, url string, statusCode int, body string) error {
	log := handoffLog{
		URL:          url,
		ManifestPath: result.Artifacts.InstallManifest,
		StatusCode:   statusCode,
		Body:         body,
	}
	if err := writeJSON(result.Artifacts.HandoffResponse, log); err != nil {
		return err
	}
	if statusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("handoff failed: status=%d body=%s", statusCode, body)
	}
	return nil
}

func runtimeConfig(result Result, config InstalledRuntimeConfig) (InstalledRuntimeConfig, error) {
	if config.Disk != "" {
		return config, nil
	}
	for _, disk := range result.Disks {
		if disk.Kind == DiskTarget || disk.Kind == DiskSnapshot {
			config.Disk = disk.HostPath
			config.DiskFormat = disk.Format
			return config, nil
		}
	}
	return config, errors.New("target disk fixture is required")
}

func failFirst(runner Runner, scenario Scenario, result Result, phase string, err error) (Result, error) {
	now := runner.time()
	result.finish(StatusFailed, err.Error(), now)
	if len(result.Phases) > 0 {
		result.Phases[len(result.Phases)-1].Name = phase
	}
	if writeErr := runner.Write(scenario, result); writeErr != nil {
		return result, writeErr
	}
	return result, nil
}

func copyArtifact(src, dst string) error {
	if src == "" || dst == "" {
		return nil
	}
	data, err := os.ReadFile(src)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o644)
}

type responseCapture struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (r *responseCapture) Header() http.Header {
	return r.header
}

func (r *responseCapture) Write(data []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.body.Write(data)
}

func (r *responseCapture) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
}
