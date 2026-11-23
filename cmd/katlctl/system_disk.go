package main

import (
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/katl-dev/katl/internal/installer/discovery"
	"github.com/katl-dev/katl/internal/installer/handoff"
	"github.com/katl-dev/katl/internal/installer/manifest"
)

// systemDiskArgument maps a disk path given on the command line to the
// selector it names.
func systemDiskArgument(value string) (manifest.DiskSelector, error) {
	var selector manifest.DiskSelector
	switch {
	case strings.HasPrefix(value, "/dev/disk/by-id/"):
		selector.ByID = value
	case strings.HasPrefix(value, "/dev/disk/by-path/"):
		selector.ByPath = value
	default:
		selector.Name = value
	}
	if err := selector.Target().Validate(); err != nil {
		return manifest.DiskSelector{}, fmt.Errorf("disk %q must be a /dev/disk/by-id path (recommended), a /dev/disk/by-path path, or a kernel device path such as /dev/sda", value)
	}
	return selector, nil
}

func systemDiskArgumentString(selector manifest.DiskSelector) string {
	for _, value := range []string{selector.ByID, selector.ByPath, selector.Name} {
		if value != "" {
			return value
		}
	}
	return selector.Target().String()
}

// installerSystemDisk chooses the strongest identity the installer reports for
// a disk. The GPT disk GUID is never chosen because installation replaces it,
// which would leave the configuration stale for a reinstall.
func installerSystemDisk(disk handoff.HandoffDisk) manifest.DiskSelector {
	switch {
	case len(disk.ByID) > 0:
		return manifest.DiskSelector{ByID: slices.Min(disk.ByID)}
	case disk.WWN != "":
		return manifest.DiskSelector{WWN: disk.WWN}
	case disk.Serial != "":
		return manifest.DiskSelector{Serial: disk.Serial}
	case len(disk.ByPath) > 0:
		return manifest.DiskSelector{ByPath: slices.Min(disk.ByPath)}
	default:
		return manifest.DiskSelector{Name: disk.Path}
	}
}

func handoffDiskNames(disk handoff.HandoffDisk, value string) bool {
	return disk.Path == value || slices.Contains(disk.ByID, value) || slices.Contains(disk.ByPath, value)
}

func describeHandoffDisk(disk handoff.HandoffDisk) string {
	fields := []string{"size=" + discovery.FormatSize(disk.SizeBytes) + " (" + strconv.FormatUint(disk.SizeBytes, 10) + " bytes)"}
	if disk.Type != "" {
		fields = append(fields, "type="+disk.Type)
	}
	for _, alias := range disk.ByID {
		fields = append(fields, "byID="+alias)
	}
	if disk.WWN != "" {
		fields = append(fields, "wwn="+disk.WWN)
	}
	if disk.Serial != "" {
		fields = append(fields, "serial="+disk.Serial)
	}
	for _, alias := range disk.ByPath {
		fields = append(fields, "byPath="+alias)
	}
	if disk.GPTGUID != "" {
		fields = append(fields, "gptGUID="+disk.GPTGUID)
	}
	if disk.Model != "" {
		fields = append(fields, "model="+disk.Model)
	}
	return disk.Path + " (" + strings.Join(fields, ", ") + ")"
}

// weakSystemDiskWarning explains the guarantee given up when a node's system
// disk is selected without a hardware identity. It is empty otherwise.
func weakSystemDiskWarning(node string, selector manifest.DiskSelector) string {
	target := selector.Target()
	if target.Stable() {
		return ""
	}
	var caveats []string
	if target.ByPath != "" {
		caveats = append(caveats, "byPath follows the attachment slot, so a different disk in that slot matches and a topology change breaks it")
	}
	if target.GPTGUID != "" {
		caveats = append(caveats, "gptGUID is replaced by installation and shared by cloned disks")
	}
	if target.Name != "" {
		caveats = append(caveats, "kernel names such as "+target.Name+" can change with enumeration order")
	}
	if target.Model != "" || target.Size != "" || target.Type != "" {
		caveats = append(caveats, "model, size, and type can match a disk added later")
	}
	return fmt.Sprintf("warning: node %s selects its system disk by %s without byID, wwn, or serial; %s. Prefer a /dev/disk/by-id path when the installer reports one.", node, target, strings.Join(caveats, "; "))
}

// writeInstallerDisks lists the disks a waiting installer reports with the
// selector config generation would record for each.
func writeInstallerDisks(w io.Writer, disks []handoff.HandoffDisk) error {
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	table := newTable(w)
	table.row("DISK", "SIZE", "TYPE", "MODEL", "SELECTABLE", "SELECTOR")
	for _, disk := range disks {
		selectable := "yes"
		switch {
		case disk.ReadOnly:
			selectable = "no (read-only)"
		case disk.Mounted:
			selectable = "no (mounted)"
		case !disk.Selectable:
			selectable = "no"
		}
		model := disk.Model
		if model == "" {
			model = "-"
		}
		table.row(disk.Path, discovery.FormatSize(disk.SizeBytes), disk.Type, model, selectable, installerSystemDisk(disk).Target().String())
	}
	return table.flush()
}
