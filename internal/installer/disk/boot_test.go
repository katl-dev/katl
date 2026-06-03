package disk

import (
	"context"
	"strings"
	"testing"
)

func TestInstallBoot(t *testing.T) {
	commands := &NoopCommandRunner{}
	plan := executorPlan()
	plan.Partitions = append(plan.Partitions[:1], append([]PartitionPlan{
		{Name: "xbootldr", GPTLabel: GPTLabelXBOOTLDR, Filesystem: "vfat", MountPath: "/boot"},
	}, plan.Partitions[1:]...)...)

	result, err := InstallBoot(context.Background(), BootRequest{
		Commands:          commands,
		Plan:              plan,
		TargetMountPrefix: "/target",
	})
	if err != nil {
		t.Fatalf("InstallBoot() error = %v", err)
	}
	if result.ESP.PartitionUUID != "uuid-katl_esp" || result.XBOOTLDR == nil || result.XBOOTLDR.PartitionUUID != "uuid-katl_xbootldr" {
		t.Fatalf("boot partitions = %#v", result)
	}
	if result.RootPartitionUUID != "uuid-katl_root_a" {
		t.Fatalf("root partition UUID = %q", result.RootPartitionUUID)
	}
	if len(commands.Calls) == 0 || commands.Calls[0].Name != "bootctl" {
		t.Fatalf("first call = %#v", commands.Calls)
	}
	gotArgs := strings.Join(commands.Calls[0].Args, " ")
	if !strings.Contains(gotArgs, "--esp-path=/target/efi") || !strings.Contains(gotArgs, "--boot-path=/target/boot") {
		t.Fatalf("bootctl args = %q", gotArgs)
	}
}

func TestInstallBootRejectsXBOOTLDR(t *testing.T) {
	plan := executorPlan()
	plan.Partitions = append(plan.Partitions[:1], append([]PartitionPlan{
		{Name: "xbootldr", GPTLabel: GPTLabelXBOOTLDR, Filesystem: "ext4", MountPath: "/boot"},
	}, plan.Partitions[1:]...)...)

	_, err := InstallBoot(context.Background(), BootRequest{
		Commands: &NoopCommandRunner{},
		Plan:     plan,
	})
	if err == nil || !strings.Contains(err.Error(), "XBOOTLDR filesystem") {
		t.Fatalf("InstallBoot() error = %v, want XBOOTLDR filesystem failure", err)
	}
}

func TestInstallBootNeedsUUID(t *testing.T) {
	_, err := InstallBoot(context.Background(), BootRequest{
		Commands: emptyUUIDRunner{},
		Plan:     executorPlan(),
	})
	if err == nil || !strings.Contains(err.Error(), "empty blkid output") {
		t.Fatalf("InstallBoot() error = %v, want empty UUID failure", err)
	}
}

type emptyUUIDRunner struct{}

func (emptyUUIDRunner) Run(context.Context, string, ...string) error {
	return nil
}

func (emptyUUIDRunner) Output(context.Context, string, ...string) ([]byte, error) {
	return nil, nil
}
