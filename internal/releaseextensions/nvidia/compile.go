package nvidia

import (
	_ "embed"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/katl-dev/katl/internal/kernelmodule"
	"github.com/katl-dev/katl/internal/releaseextensions"
)

//go:embed katl-nvidia-devices.service
var devicesUnit []byte

type Input struct {
	Target      kernelmodule.Target
	Version     string
	Sources     map[string]string
	Destination string
	Work        string
	Stdout      io.Writer
	Stderr      io.Writer
}

func Compile(input Input) (kernelmodule.Contract, error) {
	if len(input.Sources) != 2 || input.Sources["source"] == "" || input.Sources["driver"] == "" {
		return kernelmodule.Contract{}, fmt.Errorf("NVIDIA recipe requires source and driver archives")
	}
	sourceDir := filepath.Join(input.Work, "source")
	if err := os.Mkdir(sourceDir, 0o755); err != nil {
		return kernelmodule.Contract{}, err
	}
	// Both verified archives are unpacked in a private build tree. The
	// runfile is extracted only; its installer never touches the host.
	command := exec.Command("tar", "--extract", "--gzip", "--file", input.Sources["source"], "--directory", sourceDir, "--strip-components=1", "--no-same-owner", "--no-same-permissions")
	command.Stdout, command.Stderr = input.Stdout, input.Stderr
	if err := command.Run(); err != nil {
		return kernelmodule.Contract{}, fmt.Errorf("extract NVIDIA module source: %w", err)
	}
	driverDir := filepath.Join(input.Work, "driver")
	command = exec.Command("sh", input.Sources["driver"], "--extract-only", "--target", driverDir)
	command.Stdout, command.Stderr = input.Stdout, input.Stderr
	if err := command.Run(); err != nil {
		return kernelmodule.Contract{}, fmt.Errorf("extract NVIDIA driver distribution: %w", err)
	}
	if err := validateVersions(sourceDir, driverDir, input.Version); err != nil {
		return kernelmodule.Contract{}, err
	}
	kdir := filepath.Join("/usr/src/kernels", input.Target.Release)
	command = exec.Command("make", "-C", sourceDir, fmt.Sprintf("-j%d", min(runtime.NumCPU(), 8)),
		"SYSSRC="+kdir, "SYSOUT="+kdir, "KERNEL_UNAME="+input.Target.Release,
		"NV_EXCLUDE_KERNEL_MODULES=nvidia-peermem", "modules")
	command.Env = append(os.Environ(), "SOURCE_DATE_EPOCH=315532800")
	command.Stdout, command.Stderr = input.Stdout, input.Stderr
	if err := command.Run(); err != nil {
		return kernelmodule.Contract{}, fmt.Errorf("compile NVIDIA: %w", err)
	}
	contract, err := installModules(filepath.Join(sourceDir, "kernel-open"), input.Destination, input.Target, input.Version)
	if err != nil {
		return contract, err
	}
	if err := installUserspace(driverDir, input.Destination, input.Version); err != nil {
		return contract, err
	}
	if err := installDeviceService(input.Destination); err != nil {
		return contract, err
	}
	return contract, nil
}

func validateVersions(source, driver, version string) error {
	build, err := os.ReadFile(filepath.Join(source, "version.mk"))
	if err != nil {
		return err
	}
	if !strings.Contains(string(build), "NVIDIA_VERSION = "+version+"\n") {
		return fmt.Errorf("NVIDIA module source does not match recipe version %s", version)
	}
	manifest, err := os.ReadFile(filepath.Join(driver, ".manifest"))
	if err != nil {
		return err
	}
	lines := strings.SplitN(string(manifest), "\n", 3)
	if len(lines) < 2 || lines[1] != version {
		return fmt.Errorf("NVIDIA driver distribution does not match recipe version %s", version)
	}
	return nil
}

func installModules(source, destination string, target kernelmodule.Target, version string) (kernelmodule.Contract, error) {
	contract := kernelmodule.Contract{Target: target}
	for _, name := range []string{"nvidia", "nvidia-modeset", "nvidia-uvm", "nvidia-drm"} {
		path := filepath.Join(source, name+".ko")
		for field, expected := range map[string]string{
			"name":     strings.ReplaceAll(name, "-", "_"),
			"version":  version,
			"vermagic": target.Release,
		} {
			value, err := exec.Command("modinfo", "-F", field, path).Output()
			parts := strings.Fields(string(value))
			if err != nil || len(parts) == 0 || parts[0] != expected {
				return contract, fmt.Errorf("built module %s has unexpected %s: %s", name, field, value)
			}
		}
		relative := filepath.Join("usr/lib/modules", target.Release, "extra/katl", name+".ko")
		output := filepath.Join(destination, relative)
		if err := releaseextensions.CopyRegular(path, output, 0o644); err != nil {
			return contract, err
		}
		digest, err := releaseextensions.SHA256(output)
		if err != nil {
			return contract, err
		}
		contract.Modules = append(contract.Modules, kernelmodule.Module{
			Name: name, Path: relative, SHA256: digest,
			Required: name == "nvidia" || name == "nvidia-uvm",
		})
	}
	return contract, contract.Validate()
}

func installUserspace(source, destination, version string) error {
	libdir := filepath.Join(destination, "usr/lib64")
	for _, library := range []struct {
		name   string
		soname string
	}{
		{"libcuda", "1"},
		{"libnvidia-ml", "1"},
		{"libnvidia-ptxjitcompiler", "1"},
		{"libnvidia-nvvm", "4"},
		{"libnvidia-cfg", "1"},
		{"libnvidia-encode", "1"},
		{"libnvcuvid", "1"},
		{"libnvidia-opticalflow", "1"},
	} {
		file := library.name + ".so." + version
		if err := releaseextensions.CopyRegular(filepath.Join(source, file), filepath.Join(libdir, file), 0o755); err != nil {
			return err
		}
		soname := library.name + ".so." + library.soname
		if err := os.Symlink(file, filepath.Join(libdir, soname)); err != nil {
			return err
		}
		if err := os.Symlink(soname, filepath.Join(libdir, library.name+".so")); err != nil {
			return err
		}
	}
	for _, file := range []string{"libnvidia-nvvm70.so.4", "libnvidia-gpucomp.so." + version} {
		if err := releaseextensions.CopyRegular(filepath.Join(source, file), filepath.Join(libdir, file), 0o755); err != nil {
			return err
		}
	}
	for _, file := range []string{"nvidia-smi", "nvidia-modprobe"} {
		if err := releaseextensions.CopyRegular(filepath.Join(source, file), filepath.Join(destination, "usr/bin", file), 0o755); err != nil {
			return err
		}
	}
	for _, file := range []string{"gsp_tu10x.bin", "ucodes_tu10x.bin", "gsp_ga10x.bin", "ucodes_ga10x.bin"} {
		if err := releaseextensions.CopyRegular(filepath.Join(source, "firmware", file), filepath.Join(destination, "usr/lib/firmware/nvidia", version, file), 0o644); err != nil {
			return err
		}
	}
	return releaseextensions.CopyRegular(filepath.Join(source, "LICENSE"), filepath.Join(destination, "usr/share/licenses/katl-nvidia/LICENSE"), 0o644)
}

func installDeviceService(destination string) error {
	modprobe := filepath.Join(destination, "usr/lib/modprobe.d")
	if err := os.MkdirAll(modprobe, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(modprobe, "katl-nvidia.conf"), []byte("blacklist nouveau\n"), 0o644); err != nil {
		return err
	}
	units := filepath.Join(destination, "usr/lib/systemd/system")
	if err := os.MkdirAll(units, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(units, "katl-nvidia-devices.service"), devicesUnit, 0o644); err != nil {
		return err
	}
	// The selected extension owns this requirement. Removing the extension
	// removes both the service and its boot-health dependency.
	requires := filepath.Join(units, "katl-boot-health.service.requires")
	if err := os.MkdirAll(requires, 0o755); err != nil {
		return err
	}
	return os.Symlink("../katl-nvidia-devices.service", filepath.Join(requires, "katl-nvidia-devices.service"))
}
