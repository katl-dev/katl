// Command katl-system-disk-generator is a systemd generator that mounts /efi
// and /var from the disk holding the running root partition.
package main

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"

	"github.com/katl-dev/katl/internal/systemdisk"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "katl-system-disk-generator: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: katl-system-disk-generator NORMAL-DIR [EARLY-DIR LATE-DIR]")
	}
	boot := systemdisk.Boot{Sysfs: "/sys"}
	if _, err := os.Stat("/etc/initrd-release"); err == nil {
		boot.Initrd = true
	}
	cmdline, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return systemdisk.WriteFailure(args[0], fmt.Errorf("read kernel command line: %w", err))
	}
	boot.Cmdline = string(cmdline)

	var root unix.Stat_t
	if err := unix.Stat("/", &root); err != nil {
		return systemdisk.WriteFailure(args[0], fmt.Errorf("stat /: %w", err))
	}
	boot.Root = device(root)
	boot.Var = mountedDevice("/var", root)
	boot.EFI = mountedDevice("/efi", root)
	return systemdisk.Generate(args[0], boot)
}

// mountedDevice returns the device mounted at path, or nil when path is on
// the root filesystem.
func mountedDevice(path string, root unix.Stat_t) *systemdisk.Device {
	var stat unix.Stat_t
	if err := unix.Stat(path, &stat); err != nil || stat.Dev == root.Dev {
		return nil
	}
	mounted := device(stat)
	return &mounted
}

func device(stat unix.Stat_t) systemdisk.Device {
	return systemdisk.Device{Major: unix.Major(stat.Dev), Minor: unix.Minor(stat.Dev)}
}
