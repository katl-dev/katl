package vmtest

import (
	"context"
	"errors"
	"fmt"
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
	RequireVMTestAgent bool
	Expect             string
	VM                 VMConfig
}

func RunInstalledRuntime(ctx context.Context, result Result, config InstalledRuntimeConfig, runner VMRunner) Result {
	if err := PrepareInstalledRuntime(result, config); err != nil {
		return finishVM(result, "runtime", StatusFailed, err.Error(), result.Started, runnerTime())
	}
	vm := config.VM
	vm.Phase = "runtime"
	vm.Expect = first(vm.Expect, config.Expect, "Katl state projection ready")
	vm.Boot = VMBoot{
		Image:         config.Disk,
		ImageFormat:   diskFormat(config.DiskFormat),
		ImageSnapshot: true,
	}
	if config.RequireVMTestAgent {
		vm.Boot.EFITree = runtimeESPPath(result)
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
	esp := runtimeESPPath(result)
	if err := copyDir(config.ESPArtifacts, esp); err != nil {
		return err
	}
	if config.RequireVMTestAgent {
		if err := InjectESPOption(esp, "katl.vmtest_agent=1"); err != nil {
			return err
		}
	}
	return CheckESP(esp)
}

func runtimeESPPath(result Result) string {
	return filepath.Join(result.RunDir, "esp")
}

func InjectESPOption(root string, option string) error {
	option = strings.TrimSpace(option)
	if option == "" || strings.ContainsAny(option, " \t\n\r") {
		return fmt.Errorf("loader option %q must not contain whitespace", option)
	}
	entries := filepath.Join(root, "loader", "entries")
	var changed int
	err := filepath.WalkDir(entries, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || filepath.Ext(path) != ".conf" {
			return nil
		}
		if err := injectLoaderOption(path, option); err != nil {
			return err
		}
		changed++
		return nil
	})
	if err != nil {
		return err
	}
	if changed == 0 {
		return errors.New("ESP artifacts contain no loader entries")
	}
	return nil
}

func injectLoaderOption(path string, option string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		if !strings.HasPrefix(line, "options ") {
			continue
		}
		fields := strings.Fields(line)
		for _, field := range fields[1:] {
			if field == option {
				return nil
			}
		}
		lines[i] = line + " " + option
		return os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644)
	}
	return fmt.Errorf("loader entry %s has no options line", path)
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
