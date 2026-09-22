package generation

import (
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/kernelmodule"
)

func TestKernelCompatibility(t *testing.T) {
	root := RootSelection{Architecture: "x86_64", RuntimeInterface: "katl-runtime-1", RuntimeArtifactSHA256: strings.Repeat("a", 64)}
	extension := ExtensionRef{
		Name: "drbd9", Architecture: "x86_64",
		Compatibility: ExtensionCompatibility{
			RuntimeInterfaces: []string{"katl-runtime-1"},
			Kernel: &kernelmodule.Contract{
				Target:  kernelmodule.Target{Release: "6.12", RuntimeSHA256: strings.Repeat("a", 64)},
				Modules: []kernelmodule.Module{{Name: "drbd", Path: "usr/lib/modules/6.12/extra/drbd.ko", SHA256: strings.Repeat("b", 64)}},
			},
		},
	}
	if err := ValidatePair(root, extension); err != nil {
		t.Fatal(err)
	}

	root.RuntimeArtifactSHA256 = strings.Repeat("c", 64)
	if err := ValidatePair(root, extension); err == nil {
		t.Fatal("runtime interface match hid incompatible kernel build")
	}

	extension.Compatibility.Kernel = nil
	if err := ValidatePair(root, extension); err != nil {
		t.Fatalf("userspace compatibility changed: %v", err)
	}
}
