package disk

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

type OutputCommandRunner interface {
	CommandRunner
	Output(ctx context.Context, name string, args ...string) ([]byte, error)
}

type BootInstaller func(context.Context, BootRequest) (BootResult, error)

type BootRequest struct {
	Commands          OutputCommandRunner
	Plan              DiskLayoutPlan
	TargetMountPrefix string
}

type BootResult struct {
	ESP                   BootRef
	XBOOTLDR              *BootRef
	RootPartitionUUID     string
	PartitionUUIDsByLabel map[string]string
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

	espMount := targetPath(prefix, esp.MountPath)
	bootctlArgs := []string{"--esp-path=" + espMount}
	result := BootResult{
		ESP: BootRef{
			Label:     esp.GPTLabel,
			MountPath: espMount,
		},
		PartitionUUIDsByLabel: make(map[string]string),
	}
	if xbootldr, ok := findPartitionByName(request.Plan.Partitions, "xbootldr"); ok {
		ref := BootRef{
			Label:     xbootldr.GPTLabel,
			MountPath: targetPath(prefix, xbootldr.MountPath),
		}
		result.XBOOTLDR = &ref
		bootctlArgs = append(bootctlArgs, "--boot-path="+ref.MountPath)
	}
	bootctlArgs = append(bootctlArgs, "install")
	if err := request.Commands.Run(ctx, "bootctl", bootctlArgs...); err != nil {
		return BootResult{}, fmt.Errorf("install systemd-boot: %w", err)
	}

	if err := recordPartUUID(ctx, request.Commands, esp.GPTLabel, &result.ESP.PartitionUUID, result.PartitionUUIDsByLabel); err != nil {
		return BootResult{}, err
	}
	if result.XBOOTLDR != nil {
		if err := recordPartUUID(ctx, request.Commands, result.XBOOTLDR.Label, &result.XBOOTLDR.PartitionUUID, result.PartitionUUIDsByLabel); err != nil {
			return BootResult{}, err
		}
	}
	if err := recordPartUUID(ctx, request.Commands, request.Plan.Boot.RootPartitionLabel, &result.RootPartitionUUID, result.PartitionUUIDsByLabel); err != nil {
		return BootResult{}, err
	}

	return result, nil
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

func recordPartUUID(ctx context.Context, commands OutputCommandRunner, label string, target *string, labels map[string]string) error {
	uuid, err := readPartUUID(ctx, commands, label)
	if err != nil {
		return err
	}
	*target = uuid
	labels[label] = uuid
	return nil
}

func readPartUUID(ctx context.Context, commands OutputCommandRunner, label string) (string, error) {
	output, err := commands.Output(ctx, "blkid", "-t", "PARTLABEL="+label, "-s", "PARTUUID", "-o", "value")
	if err != nil {
		return "", fmt.Errorf("read %s PARTUUID: %w", label, err)
	}
	uuid := strings.TrimSpace(string(output))
	if uuid == "" {
		return "", fmt.Errorf("read %s PARTUUID: empty blkid output", label)
	}
	return uuid, nil
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
