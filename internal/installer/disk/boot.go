package disk

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

type BootInstaller func(context.Context, BootRequest) (BootResult, error)

type BootRequest struct {
	Commands          CommandRunner
	Plan              DiskLayoutPlan
	TargetMountPrefix string
}

type BootResult struct {
	ESP               BootRef
	XBOOTLDR          *BootRef
	RootPartitionUUID string
}

type BootRef struct {
	Label         string
	MountPath     string
	PartitionUUID string
}

func InstallBoot(ctx context.Context, request BootRequest) (BootResult, error) {
	if err := checkBootPlan(request.Plan); err != nil {
		return BootResult{}, err
	}
	if request.Commands == nil {
		return BootResult{}, fmt.Errorf("boot command runner is required")
	}
	prefix := request.TargetMountPrefix
	if prefix == "" {
		prefix = "/mnt/target"
	}

	esp, _ := findPartitionByName(request.Plan.Partitions, "esp")
	espRef, err := bootRef(request.Plan, esp, prefix)
	if err != nil {
		return BootResult{}, err
	}
	result := BootResult{ESP: espRef}
	bootctlArgs := []string{"--esp-path=" + espRef.MountPath}
	if xbootldr, ok := findPartitionByName(request.Plan.Partitions, "xbootldr"); ok {
		ref, err := bootRef(request.Plan, xbootldr, prefix)
		if err != nil {
			return BootResult{}, err
		}
		result.XBOOTLDR = &ref
		bootctlArgs = append(bootctlArgs, "--boot-path="+ref.MountPath)
	}
	result.RootPartitionUUID = request.Plan.PartitionUUIDs[request.Plan.Boot.RootPartitionLabel]
	if result.RootPartitionUUID == "" {
		return BootResult{}, fmt.Errorf("root partition %s is not bound to target disk %s", request.Plan.Boot.RootPartitionLabel, request.Plan.TargetDiskPath)
	}

	bootctlArgs = append(bootctlArgs, "install")
	if err := request.Commands.Run(ctx, "bootctl", bootctlArgs...); err != nil {
		return BootResult{}, fmt.Errorf("install systemd-boot: %w", err)
	}
	return result, nil
}

func bootRef(plan DiskLayoutPlan, partition PartitionPlan, prefix string) (BootRef, error) {
	uuid := plan.PartitionUUIDs[partition.GPTLabel]
	if uuid == "" {
		return BootRef{}, fmt.Errorf("partition %s is not bound to target disk %s", partition.GPTLabel, plan.TargetDiskPath)
	}
	return BootRef{Label: partition.GPTLabel, MountPath: targetPath(prefix, partition.MountPath), PartitionUUID: uuid}, nil
}

func checkBootPlan(plan DiskLayoutPlan) error {
	esp, ok := findPartitionByName(plan.Partitions, "esp")
	if !ok {
		return fmt.Errorf("ESP partition is required")
	}
	if esp.Filesystem != "vfat" {
		return fmt.Errorf("ESP filesystem %q is unsupported", esp.Filesystem)
	}
	if xbootldr, ok := findPartitionByName(plan.Partitions, "xbootldr"); ok && xbootldr.Filesystem != "vfat" {
		return fmt.Errorf("XBOOTLDR filesystem %q is unsupported", xbootldr.Filesystem)
	}
	return nil
}

func targetPath(prefix, mountPath string) string {
	return filepath.Join(prefix, strings.TrimPrefix(mountPath, "/"))
}

func findPartitionByName(partitions []PartitionPlan, name string) (PartitionPlan, bool) {
	for _, partition := range partitions {
		if partition.Name == name {
			return partition, true
		}
	}
	return PartitionPlan{}, false
}
