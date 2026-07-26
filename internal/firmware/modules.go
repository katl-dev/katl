package firmware

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

var installerFirmwareModuleFamilies = []string{
	"drivers/ata/",
	"drivers/block/",
	"drivers/hv/",
	"drivers/md/",
	"drivers/message/",
	"drivers/net/ethernet/",
	"drivers/net/hyperv/",
	"drivers/net/mdio/",
	"drivers/net/phy/",
	"drivers/net/virtio_net",
	"drivers/net/vmxnet3/",
	"drivers/nvme/",
	"drivers/scsi/",
	"drivers/usb/host/",
	"drivers/usb/storage/",
	"drivers/vhost/",
	"drivers/virtio/",
}

func VerifyInstallerModuleFirmware(root, modinfo string) (int, int, error) {
	if strings.TrimSpace(modinfo) == "" {
		modinfo = "modinfo"
	}
	moduleRoot := filepath.Join(root, "usr", "lib", "modules")
	firmwareRoot := filepath.Join(root, "usr", "lib", "firmware")
	var modules []string
	if err := filepath.WalkDir(moduleRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !isKernelModule(entry.Name()) {
			return nil
		}
		relative, err := filepath.Rel(moduleRoot, path)
		if err != nil {
			return err
		}
		if !installerFirmwareModule(relative) {
			return nil
		}
		modules = append(modules, path)
		return nil
	}); err != nil {
		return 0, 0, fmt.Errorf("walk installer kernel modules: %w", err)
	}
	sort.Strings(modules)
	requirements := make(map[string][]string)
	for _, module := range modules {
		output, err := exec.Command(modinfo, "-F", "firmware", module).CombinedOutput()
		if err != nil {
			return len(modules), len(requirements), fmt.Errorf("inspect firmware requirements for %s: %w: %s", filepath.Base(module), err, strings.TrimSpace(string(output)))
		}
		for _, line := range strings.Split(string(output), "\n") {
			requirement := strings.TrimSpace(line)
			if requirement == "" {
				continue
			}
			requirements[requirement] = append(requirements[requirement], module)
		}
	}
	names := make([]string, 0, len(requirements))
	for name := range requirements {
		names = append(names, name)
	}
	sort.Strings(names)
	var missing []string
	for _, name := range names {
		if err := validateFirmwarePath(name); err != nil {
			return len(modules), len(requirements), err
		}
		present, err := firmwarePresent(firmwareRoot, name)
		if err != nil {
			return len(modules), len(requirements), err
		}
		if !present {
			missing = append(missing, fmt.Sprintf(
				"%s requires %s",
				filepath.Base(requirements[name][0]),
				name,
			))
		}
	}
	if len(missing) > 0 {
		return len(modules), len(requirements), fmt.Errorf(
			"claimed installer drivers are missing firmware from the same initrd:\n- %s",
			strings.Join(missing, "\n- "),
		)
	}
	return len(modules), len(requirements), nil
}

func installerFirmwareModule(path string) bool {
	path = filepath.ToSlash(path)
	kernelAt := strings.Index(path, "/kernel/")
	if kernelAt < 0 {
		return false
	}
	path = path[kernelAt+len("/kernel/"):]
	for _, family := range installerFirmwareModuleFamilies {
		if strings.HasPrefix(path, family) {
			return true
		}
	}
	return false
}

func isKernelModule(name string) bool {
	for _, suffix := range []string{".ko", ".ko.gz", ".ko.xz", ".ko.zst"} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

func validateFirmwarePath(path string) error {
	if filepath.IsAbs(path) || filepath.Clean(path) != path || strings.HasPrefix(path, ".."+string(filepath.Separator)) || path == ".." {
		return fmt.Errorf("kernel module reported unsafe firmware path %q", path)
	}
	return nil
}

func firmwarePresent(root, requirement string) (bool, error) {
	for _, suffix := range []string{"", ".xz", ".zst", ".gz"} {
		candidate := filepath.Join(root, requirement+suffix)
		if strings.ContainsAny(requirement, "*?[") {
			matches, err := filepath.Glob(candidate)
			if err != nil {
				return false, fmt.Errorf("match firmware requirement %q: %w", requirement, err)
			}
			if len(matches) > 0 {
				return true, nil
			}
			continue
		}
		if _, err := os.Stat(candidate); err == nil {
			return true, nil
		} else if !os.IsNotExist(err) {
			return false, fmt.Errorf("stat firmware %s: %w", candidate, err)
		}
	}
	return false, nil
}
