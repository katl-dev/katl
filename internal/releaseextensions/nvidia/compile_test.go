package nvidia

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNVIDIADistributionVersion(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		source string
		driver string
		valid  bool
	}{
		{"matching", "NVIDIA_VERSION = 615.71.09\n", "NVIDIA Driver\n615.71.09\n", true},
		{"different source", "NVIDIA_VERSION = 615.71.08\n", "NVIDIA Driver\n615.71.09\n", false},
		{"different driver", "NVIDIA_VERSION = 615.71.09\n", "NVIDIA Driver\n615.71.08\n", false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			source, driver := t.TempDir(), t.TempDir()
			if err := os.WriteFile(filepath.Join(source, "version.mk"), []byte(scenario.source), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(driver, ".manifest"), []byte(scenario.driver), 0o644); err != nil {
				t.Fatal(err)
			}
			err := validateVersions(source, driver, "615.71.09")
			if (err == nil) != scenario.valid {
				t.Fatalf("version validation = %v, expected valid = %t", err, scenario.valid)
			}
		})
	}
}

func TestNVIDIAUserspacePayload(t *testing.T) {
	source, destination := t.TempDir(), t.TempDir()
	for _, file := range []string{
		"libcuda.so.615.71.09", "libnvidia-ml.so.615.71.09",
		"libnvidia-ptxjitcompiler.so.615.71.09", "libnvidia-nvvm.so.615.71.09",
		"libnvidia-cfg.so.615.71.09", "libnvidia-encode.so.615.71.09",
		"libnvcuvid.so.615.71.09", "libnvidia-opticalflow.so.615.71.09",
		"libnvidia-nvvm70.so.4", "libnvidia-gpucomp.so.615.71.09",
		"nvidia-smi", "nvidia-modprobe", "LICENSE",
		"firmware/gsp_tu10x.bin", "firmware/ucodes_tu10x.bin",
		"firmware/gsp_ga10x.bin", "firmware/ucodes_ga10x.bin",
	} {
		path := filepath.Join(source, file)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(file), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := installUserspace(source, destination, "615.71.09"); err != nil {
		t.Fatal(err)
	}
	for path, expected := range map[string]string{
		"usr/lib64/libcuda.so.615.71.09":                     "libcuda.so.615.71.09",
		"usr/lib64/libnvidia-ml.so.615.71.09":                "libnvidia-ml.so.615.71.09",
		"usr/lib64/libnvidia-ptxjitcompiler.so.615.71.09":    "libnvidia-ptxjitcompiler.so.615.71.09",
		"usr/bin/nvidia-smi":                                 "nvidia-smi",
		"usr/lib/firmware/nvidia/615.71.09/gsp_tu10x.bin":    "firmware/gsp_tu10x.bin",
		"usr/lib/firmware/nvidia/615.71.09/ucodes_ga10x.bin": "firmware/ucodes_ga10x.bin",
		"usr/share/licenses/katl-nvidia/LICENSE":             "LICENSE",
	} {
		data, err := os.ReadFile(filepath.Join(destination, path))
		if err != nil || string(data) != expected {
			t.Fatalf("payload %s = %q, %v", path, data, err)
		}
	}
	for path, target := range map[string]string{
		"usr/lib64/libcuda.so.1":      "libcuda.so.615.71.09",
		"usr/lib64/libcuda.so":        "libcuda.so.1",
		"usr/lib64/libnvidia-ml.so.1": "libnvidia-ml.so.615.71.09",
	} {
		actual, err := os.Readlink(filepath.Join(destination, path))
		if err != nil || actual != target {
			t.Fatalf("payload symlink %s = %q, %v", path, actual, err)
		}
	}
	if err := os.Remove(filepath.Join(source, "firmware/gsp_ga10x.bin")); err != nil {
		t.Fatal(err)
	}
	if err := installUserspace(source, t.TempDir(), "615.71.09"); err == nil {
		t.Fatal("accepted driver distribution without Ampere GSP firmware")
	}
}

func TestNVIDIADeviceService(t *testing.T) {
	destination := t.TempDir()
	if err := installDeviceService(destination); err != nil {
		t.Fatal(err)
	}
	unit := filepath.Join(destination, "usr/lib/systemd/system/katl-nvidia-devices.service")
	data, err := os.ReadFile(unit)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		"ExecStart=/usr/bin/nvidia-modprobe -c 0",
		"ExecStart=/usr/bin/nvidia-modprobe -u",
		"Before=katl-boot-health.service",
	} {
		if !strings.Contains(string(data), line) {
			t.Fatalf("device service omits %q", line)
		}
	}
	link := filepath.Join(destination, "usr/lib/systemd/system/katl-boot-health.service.requires/katl-nvidia-devices.service")
	if target, err := os.Readlink(link); err != nil || target != "../katl-nvidia-devices.service" {
		t.Fatalf("boot-health dependency = %q, %v", target, err)
	}
	config, err := os.ReadFile(filepath.Join(destination, "usr/lib/modprobe.d/katl-nvidia.conf"))
	if err != nil || string(config) != "blacklist nouveau\n" {
		t.Fatalf("driver module policy = %q, %v", config, err)
	}
}
