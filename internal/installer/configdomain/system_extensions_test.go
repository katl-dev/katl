package configdomain

import (
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/installer/confext"
	"github.com/katl-dev/katl/internal/installer/manifest"
)

func TestSystemExtensionFilesRenderConfiguration(t *testing.T) {
	config := "router id from \"bird0\";\n"
	dropIn := "[Service]\nRestartSec=2s\n"
	extension := manifest.SystemExtension{
		Name: "bird", Bundle: "registry.example/bird:v1", State: manifest.SystemExtensionPresent,
		OCIManifestDigest:    "sha256:" + strings.Repeat("a", 64),
		BundleManifestDigest: "sha256:" + strings.Repeat("b", 64),
		ArtifactVersion:      "v1", PayloadVersion: "v1", Architecture: "x86_64",
		SupportedRuntimeInterfaces: []string{"katl-runtime-1"},
		Payloads: []manifest.SystemExtensionPayloadRef{{
			Name: "katl-bird.raw", Role: "systemd-sysext",
			MediaType: "application/vnd.katl.sysext.raw.v1",
			Digest:    "sha256:" + strings.Repeat("c", 64), SizeBytes: 1024,
		}},
		Configuration: manifest.SystemExtensionConfiguration{Files: []manifest.HostConfigurationFile{{
			Path: "/etc/bird.conf", Content: &config,
		}}},
		Units: []manifest.SystemExtensionUnit{{
			Name: "bird.service", Enable: true, RequiredForBootHealth: true,
			DropIns: []manifest.SystemExtensionUnitDropIn{{Name: "10-site.conf", Content: &dropIn}},
		}},
	}
	files, err := systemExtensionFiles([]manifest.SystemExtension{extension})
	if err != nil {
		t.Fatal(err)
	}
	byPath := make(map[string]confext.NativeEtcFile, len(files))
	for _, file := range files {
		byPath[file.Path] = file
	}
	if byPath["/etc/bird.conf"].Content != config {
		t.Fatalf("configuration = %#v", byPath["/etc/bird.conf"])
	}
	if byPath["/etc/systemd/system/bird.service.d/10-site.conf"].Content != dropIn {
		t.Fatalf("drop-in = %#v", byPath["/etc/systemd/system/bird.service.d/10-site.conf"])
	}
}
