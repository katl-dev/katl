package kernelcmdline

import (
	"slices"
	"testing"
)

func TestConfiguredArgumentsCanBeReplacedWithoutLosingHostArguments(t *testing.T) {
	current := []string{
		"root=PARTUUID=11111111-2222-3333-4444-555555555555",
		"rootfstype=squashfs",
		"ro",
		"console=ttyS0,115200n8",
		"intel_iommu=on",
	}
	next := ReplaceConfigured(current, []string{"intel_iommu=on"}, []string{"amd_iommu=on", "iommu=pt"})
	next = MergeCurrent(next, current, []string{"intel_iommu=on"})

	for _, option := range []string{"console=ttyS0,115200n8", "amd_iommu=on", "iommu=pt"} {
		if !slices.Contains(next, option) {
			t.Fatalf("next command line %q does not contain %q", next, option)
		}
	}
	if slices.Contains(next, "intel_iommu=on") {
		t.Fatalf("next command line still contains replaced argument: %q", next)
	}
}

func TestValidateConfiguredRejectsKatlOwnedArguments(t *testing.T) {
	for _, option := range []string{
		"root=PARTUUID=11111111-2222-3333-4444-555555555555",
		"katl.generation=other",
		"systemd.unit=rescue.target",
		"systemd.volatile=yes",
		"console=tty0",
		"console=tty1",
		"console=tty3,9600",
	} {
		if err := ValidateConfigured([]string{option}); err == nil {
			t.Fatalf("ValidateConfigured(%q) succeeded", option)
		}
	}
}

func TestMergeCurrentReplacesGraphicalConsole(t *testing.T) {
	got := MergeCurrent(
		[]string{"console=ttyS0,115200n8", "console=tty3"},
		[]string{"console=tty0", "console=ttyS0,115200n8", "intel_iommu=on"}, nil,
	)
	want := []string{"console=ttyS0,115200n8", "console=tty3", "intel_iommu=on"}
	if !slices.Equal(got, want) {
		t.Fatalf("merged arguments = %q, want %q", got, want)
	}
	if err := ValidateConfigured([]string{"console=ttyS1,9600"}); err != nil {
		t.Fatalf("custom serial console: %v", err)
	}
}

func TestValidateRequiredCompatibilityRejectsDuplicateImageRequirement(t *testing.T) {
	err := ValidateRequiredCompatibility([]string{"console=tty0"}, []string{"rootfstype=squashfs", "console=tty0"})
	if err == nil {
		t.Fatal("ValidateRequiredCompatibility() succeeded")
	}
}
