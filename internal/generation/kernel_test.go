package generation

import (
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/kernelmodule"
)

func TestKernelCompatibility(t *testing.T) {
	root := RootSelection{
		Architecture:          "x86_64",
		RuntimeInterface:      "katl-runtime-1",
		RuntimeArtifactSHA256: strings.Repeat("a", 64),
	}
	extension := ExtensionRef{
		Name:         "drbd9",
		Architecture: "x86_64",
		Compatibility: ExtensionCompatibility{
			RuntimeInterfaces: []string{"katl-runtime-1"},
			Kernel: &kernelmodule.Contract{
				Target: kernelmodule.Target{
					Release:       "6.12",
					RuntimeSHA256: strings.Repeat("a", 64),
				},
				Modules: []kernelmodule.Module{{
					Name:   "drbd",
					Path:   "usr/lib/modules/6.12/extra/drbd.ko",
					SHA256: strings.Repeat("b", 64),
				}},
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

func TestModuleIndexCompatibility(t *testing.T) {
	root := RootSelection{
		Architecture:          "x86_64",
		RuntimeInterface:      "katl-runtime-1",
		RuntimeArtifactSHA256: strings.Repeat("a", 64),
	}
	indexes := ExtensionRef{
		Name:         kernelmodule.IndexExtensionName,
		Architecture: "x86_64",
		Compatibility: ExtensionCompatibility{
			RuntimeInterfaces: []string{"katl-runtime-1"},
			ModuleIndexes: &kernelmodule.IndexSelection{
				Target: kernelmodule.Target{
					Release:       "6.12.1",
					RuntimeSHA256: strings.Repeat("a", 64),
				},
				ModulesSHA256: strings.Repeat("d", 64),
			},
		},
	}
	if err := ValidatePair(root, indexes); err != nil {
		t.Fatal(err)
	}
	root.RuntimeArtifactSHA256 = strings.Repeat("b", 64)
	if err := ValidatePair(root, indexes); err == nil {
		t.Fatal("accepted indexes composed for a different runtime build")
	}
}

func TestExtensionActivationHasOneOwner(t *testing.T) {
	request := validFirstInstallRequest(t.TempDir())
	request.Sysexts = []ExtensionRef{{
		Name:            "example",
		Path:            "/var/lib/katl/generations/test/sysext/example.raw",
		ActivationPath:  "/run/extensions/example.raw",
		SHA256:          strings.Repeat("a", 64),
		ArtifactVersion: "1",
		PayloadVersion:  "1",
		Architecture:    request.Root.Architecture,
		Compatibility: ExtensionCompatibility{
			RuntimeInterfaces: []string{request.Root.RuntimeInterface},
		},
	}}
	other := request.Sysexts[0]
	other.Name = "another-extension"
	other.Path = "/var/lib/katl/generations/test/sysext/other.raw"
	request.Sysexts = append(request.Sysexts, other)

	if _, err := NewFirstInstallRecord(request); err == nil || !strings.Contains(err.Error(), "share activation path") {
		t.Fatalf("ambiguous activation path: %v", err)
	}
}
