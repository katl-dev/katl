package generation

import (
	"slices"
	"testing"
)

func TestMergeKernelCommandLineOwnsGettyPolicy(t *testing.T) {
	base := []string{
		"root=PARTUUID=new",
		"systemd.getty_auto=no",
	}
	current := []string{
		"root=PARTUUID=old",
		"systemd.getty_auto=yes",
		"console=ttyS0,115200n8",
		"quiet",
	}
	want := []string{
		"root=PARTUUID=new",
		"systemd.getty_auto=no",
		"console=ttyS0,115200n8",
		"quiet",
	}

	if got := MergeKernelCommandLine(base, current); !slices.Equal(got, want) {
		t.Fatalf("MergeKernelCommandLine() = %#v, want %#v", got, want)
	}
}
