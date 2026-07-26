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
	} {
		if err := ValidateConfigured([]string{option}); err == nil {
			t.Fatalf("ValidateConfigured(%q) succeeded", option)
		}
	}
}

func TestValidateRequiredCompatibilityRejectsDuplicateImageRequirement(t *testing.T) {
	err := ValidateRequiredCompatibility([]string{"console=tty0"}, []string{"rootfstype=squashfs", "console=tty0"})
	if err == nil {
		t.Fatal("ValidateRequiredCompatibility() succeeded")
	}
}
