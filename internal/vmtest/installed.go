package vmtest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type InstalledRuntimeConfig struct {
	Disk               string
	DiskFormat         DiskFormat
	ESPArtifacts       string
	FixtureManifest    string
	NodeMetadata       string
	RequireVMTestAgent bool
	Expect             string
	VM                 VMConfig
}

var runtimeConsoleOptions = []string{
	"console=ttyS0,115200n8",
	"systemd.log_target=console",
	"loglevel=6",
}

const runtimeBootSignal = "Katl runtime reached systemd userspace"
const runtimeKernelBootSignal = "katl.generation="
const runtimeDebugShellOption = "katl.vmtest_debug_shell=1"

type installedRuntimeRecord struct {
	APIVersion         string                         `json:"apiVersion"`
	Kind               string                         `json:"kind"`
	Disk               string                         `json:"disk"`
	DiskFormat         string                         `json:"diskFormat"`
	ESPArtifacts       string                         `json:"espArtifacts"`
	RequireVMTestAgent bool                           `json:"requireVMTestAgent"`
	FixtureManifest    string                         `json:"fixtureManifest,omitempty"`
	Fixture            *installedRuntimeFixtureRecord `json:"fixture,omitempty"`
	NodeMetadata       string                         `json:"nodeMetadata,omitempty"`
}

type installedRuntimeFixtureRecord struct {
	APIVersion   string                       `json:"apiVersion"`
	Kind         string                       `json:"kind"`
	NodeName     string                       `json:"nodeName,omitempty"`
	SystemRole   string                       `json:"systemRole,omitempty"`
	Disk         installedRuntimeFixtureDisk  `json:"disk"`
	ESPArtifacts installedRuntimeFixtureESP   `json:"espArtifacts"`
	NodeMetadata *installedRuntimeFixtureFile `json:"nodeMetadata,omitempty"`
}

type installedRuntimeFixtureDisk struct {
	Path   string `json:"path"`
	Format string `json:"format"`
	SHA256 string `json:"sha256"`
}

type installedRuntimeFixtureESP struct {
	Path       string `json:"path"`
	TreeSHA256 string `json:"treeSHA256"`
}

type installedRuntimeFixtureFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

func RunInstalledRuntime(ctx context.Context, result Result, config InstalledRuntimeConfig, runner VMRunner) Result {
	if err := PrepareInstalledRuntime(result, config); err != nil {
		return finishVM(result, "runtime", StatusFailed, err.Error(), result.Started, runnerTime())
	}
	vm := config.VM
	vm.Phase = "runtime"
	defaultSignal := runtimeBootSignal
	if config.RequireVMTestAgent || vm.Agent.RequireHealth {
		defaultSignal = runtimeKernelBootSignal
	}
	vm.Expect = first(vm.Expect, config.Expect, defaultSignal)
	vm.Boot = VMBoot{
		Image:         config.Disk,
		ImageFormat:   diskFormat(config.DiskFormat),
		ImageSnapshot: true,
	}
	if config.RequireVMTestAgent || vm.Agent.RequireHealth {
		vm.VSock.Enabled = true
	}
	return runner.Run(ctx, result, vm)
}

func PrepareInstalledRuntime(result Result, config InstalledRuntimeConfig) error {
	if config.Disk == "" {
		return errors.New("installed runtime disk is required")
	}
	if _, err := os.Stat(config.Disk); err != nil {
		return fmt.Errorf("installed runtime disk not found: %w", err)
	}
	if config.ESPArtifacts == "" {
		return errors.New("ESP artifacts directory is required")
	}
	if err := writeInstalledRuntimeRecord(result, config); err != nil {
		return err
	}
	esp := runtimeESPPath(result)
	if err := copyDir(config.ESPArtifacts, esp); err != nil {
		return err
	}
	return CheckESP(esp)
}

func writeInstalledRuntimeRecord(result Result, config InstalledRuntimeConfig) error {
	if err := os.MkdirAll(filepath.Dir(result.Artifacts.InstalledRuntime), 0o755); err != nil {
		return err
	}
	fixtureManifest := strings.TrimSpace(config.FixtureManifest)
	fixture, err := readInstalledRuntimeFixture(fixtureManifest)
	if err != nil {
		return err
	}
	nodeMetadata := strings.TrimSpace(config.NodeMetadata)
	if fixture != nil && strings.TrimSpace(nodeMetadata) == "" && fixture.NodeMetadata != nil {
		nodeMetadata, err = fixtureRelativePath(fixtureManifest, fixture.NodeMetadata.Path)
		if err != nil {
			return err
		}
	}
	if fixture != nil {
		if err := validateInstalledRuntimeFixture(fixtureManifest, *fixture, config, nodeMetadata); err != nil {
			return err
		}
	}
	record := installedRuntimeRecord{
		APIVersion:         "katl.dev/v1alpha1",
		Kind:               "InstalledRuntimeVMTestInput",
		Disk:               config.Disk,
		DiskFormat:         string(diskFormat(config.DiskFormat)),
		ESPArtifacts:       config.ESPArtifacts,
		RequireVMTestAgent: config.RequireVMTestAgent,
		FixtureManifest:    fixtureManifest,
		Fixture:            fixture,
		NodeMetadata:       nodeMetadata,
	}
	return writeJSON(result.Artifacts.InstalledRuntime, record)
}

func readInstalledRuntimeFixture(path string) (*installedRuntimeFixtureRecord, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read installed runtime fixture manifest: %w", err)
	}
	var record installedRuntimeFixtureRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, fmt.Errorf("decode installed runtime fixture manifest: %w", err)
	}
	if record.APIVersion != "katl.dev/v1alpha1" || record.Kind != "InstalledRuntimeVMTestFixture" {
		return nil, fmt.Errorf("installed runtime fixture manifest has apiVersion=%q kind=%q", record.APIVersion, record.Kind)
	}
	if strings.TrimSpace(record.Disk.Path) == "" || strings.TrimSpace(record.Disk.Format) == "" || strings.TrimSpace(record.Disk.SHA256) == "" {
		return nil, errors.New("installed runtime fixture manifest disk binding is incomplete")
	}
	if strings.TrimSpace(record.ESPArtifacts.Path) == "" || strings.TrimSpace(record.ESPArtifacts.TreeSHA256) == "" {
		return nil, errors.New("installed runtime fixture manifest ESP binding is incomplete")
	}
	if record.NodeMetadata != nil && (strings.TrimSpace(record.NodeMetadata.Path) == "" || strings.TrimSpace(record.NodeMetadata.SHA256) == "") {
		return nil, errors.New("installed runtime fixture manifest node metadata binding is incomplete")
	}
	return &record, nil
}

func validateInstalledRuntimeFixture(manifestPath string, fixture installedRuntimeFixtureRecord, config InstalledRuntimeConfig, nodeMetadata string) error {
	diskPath, err := fixtureRelativePath(manifestPath, fixture.Disk.Path)
	if err != nil {
		return err
	}
	configDisk, err := cleanAbs(config.Disk)
	if err != nil {
		return err
	}
	if diskPath != configDisk {
		return fmt.Errorf("installed runtime fixture disk path %s does not match %s", diskPath, configDisk)
	}
	if fixture.Disk.Format != string(diskFormat(config.DiskFormat)) {
		return fmt.Errorf("installed runtime fixture disk format %q does not match %q", fixture.Disk.Format, diskFormat(config.DiskFormat))
	}
	diskSHA, err := fileSHA256(config.Disk)
	if err != nil {
		return fmt.Errorf("hash installed runtime disk: %w", err)
	}
	if diskSHA != fixture.Disk.SHA256 {
		return fmt.Errorf("installed runtime fixture disk sha256 does not match %s", config.Disk)
	}

	espPath, err := fixtureRelativePath(manifestPath, fixture.ESPArtifacts.Path)
	if err != nil {
		return err
	}
	configESP, err := cleanAbs(config.ESPArtifacts)
	if err != nil {
		return err
	}
	if espPath != configESP {
		return fmt.Errorf("installed runtime fixture ESP path %s does not match %s", espPath, configESP)
	}
	espSHA, err := espTreeSHA256(config.ESPArtifacts)
	if err != nil {
		return fmt.Errorf("hash installed runtime ESP artifacts: %w", err)
	}
	if espSHA != fixture.ESPArtifacts.TreeSHA256 {
		return fmt.Errorf("installed runtime fixture ESP treeSHA256 does not match %s", config.ESPArtifacts)
	}

	if strings.TrimSpace(nodeMetadata) != "" {
		if fixture.NodeMetadata == nil {
			return errors.New("installed runtime fixture manifest node metadata binding is required")
		}
		metadataPath, err := fixtureRelativePath(manifestPath, fixture.NodeMetadata.Path)
		if err != nil {
			return err
		}
		configMetadata, err := cleanAbs(nodeMetadata)
		if err != nil {
			return err
		}
		if metadataPath != configMetadata {
			return fmt.Errorf("installed runtime fixture node metadata path %s does not match %s", metadataPath, configMetadata)
		}
		metadataSHA, err := fileSHA256(nodeMetadata)
		if err != nil {
			return fmt.Errorf("hash installed runtime node metadata: %w", err)
		}
		if metadataSHA != fixture.NodeMetadata.SHA256 {
			return fmt.Errorf("installed runtime fixture node metadata sha256 does not match %s", nodeMetadata)
		}
	}
	return nil
}

func fixtureRelativePath(manifestPath string, value string) (string, error) {
	if filepath.IsAbs(value) {
		return cleanAbs(value)
	}
	return cleanAbs(filepath.Join(filepath.Dir(manifestPath), value))
}

func cleanAbs(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

func fileSHA256(path string) (string, error) {
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

func espTreeSHA256(root string) (string, error) {
	hash := sha256.New()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		mode := fmt.Sprintf("%o", info.Mode().Perm())
		if entry.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("unsupported non-regular entry: %s", rel)
		}
		if entry.IsDir() {
			_, _ = fmt.Fprintf(hash, "dir %s %s\n", mode, rel)
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("unsupported non-regular entry: %s", rel)
		}
		fileSHA, err := fileSHA256(path)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(hash, "file %s %s %s\n", mode, fileSHA, rel)
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func runtimeESPPath(result Result) string {
	return filepath.Join(result.RunDir, "esp")
}

func CheckESP(root string) error {
	entries := filepath.Join(root, "loader", "entries")
	info, err := os.Stat(entries)
	if err != nil {
		return fmt.Errorf("ESP artifacts missing loader/entries: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("ESP loader entries path is not a directory: %s", entries)
	}
	var found bool
	var content strings.Builder
	err = filepath.WalkDir(entries, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || filepath.Ext(path) != ".conf" {
			return nil
		}
		found = true
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		content.Write(data)
		content.WriteByte('\n')
		return nil
	})
	if err != nil {
		return err
	}
	if !found {
		return errors.New("ESP artifacts contain no loader entries")
	}
	text := content.String()
	for _, want := range []string{
		"root=PARTUUID=",
		"rootfstype=squashfs",
		"katl.generation=",
		"systemd.machine_id=",
	} {
		if !strings.Contains(text, want) {
			return fmt.Errorf("loader entries missing %s", want)
		}
	}
	if !hasRO(text) {
		return errors.New("loader entries missing ro option")
	}
	if strings.Contains(text, "root=gpt-auto") || strings.Contains(text, "root=LABEL=") || strings.Contains(text, "root=UUID=") {
		return errors.New("loader entries rely on root auto-discovery")
	}
	return nil
}

func hasRO(text string) bool {
	for _, field := range strings.Fields(text) {
		if field == "ro" {
			return true
		}
	}
	return false
}

func copyDir(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("source is not a directory: %s", src)
	}
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	return filepath.WalkDir(src, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}

func diskFormat(format DiskFormat) DiskFormat {
	if format == "" {
		return DiskRaw
	}
	return format
}

func runnerTime() time.Time {
	return time.Now().UTC()
}
