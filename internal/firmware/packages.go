package firmware

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

type PackageCapability struct {
	Name       string
	Capability string
}

var packageCapabilities = map[string][]PackageCapability{
	"installer": {
		{Name: "linux-firmware", Capability: "general redistributable device firmware"},
		{Name: "microcode_ctl", Capability: "Intel CPU microcode"},
		{Name: "amd-ucode-firmware", Capability: "AMD CPU microcode"},
	},
	"runtime": {
		{Name: "linux-firmware", Capability: "general redistributable device firmware"},
		{Name: "microcode_ctl", Capability: "Intel CPU microcode"},
		{Name: "amd-ucode-firmware", Capability: "AMD CPU microcode"},
		{Name: "intel-gpu-firmware", Capability: "Intel i915 and xe GPU firmware"},
		{Name: "amd-gpu-firmware", Capability: "AMD amdgpu firmware"},
	},
}

func VerifyPackageInventory(kind, path string) (map[string]string, error) {
	required, ok := packageCapabilities[kind]
	if !ok {
		return nil, fmt.Errorf("unsupported firmware package inventory kind %q", kind)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s package inventory %s: %w", kind, path, err)
	}
	defer file.Close()
	packages := make(map[string]string)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "\t", 2)
		if len(fields) != 2 || strings.TrimSpace(fields[0]) == "" || strings.TrimSpace(fields[1]) == "" {
			return nil, fmt.Errorf("%s package inventory has malformed line %q", kind, line)
		}
		packages[fields[0]] = fields[1]
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read %s package inventory: %w", kind, err)
	}
	for _, capability := range required {
		if strings.TrimSpace(packages[capability.Name]) == "" {
			return nil, fmt.Errorf("%s %s capability is missing: expected package %s in %s", kind, capability.Capability, capability.Name, path)
		}
	}
	return packages, nil
}
