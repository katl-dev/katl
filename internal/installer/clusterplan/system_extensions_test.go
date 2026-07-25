package clusterplan

import (
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/installer/manifest"
)

func TestMergeSystemExtensionsReplacesAndRemovesByName(t *testing.T) {
	base := []manifest.SystemExtension{resolvedExtension("bird", "registry.example/bird:v1"), resolvedExtension("agent", "registry.example/agent:v1")}
	replacement := resolvedExtension("bird", "registry.example/bird:v2")
	got, err := mergeSystemExtensions(base, []manifest.SystemExtension{
		replacement,
		{Name: "agent", State: manifest.SystemExtensionAbsent},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "bird" || got[0].Bundle != "registry.example/bird:v2" {
		t.Fatalf("merged system extensions = %#v", got)
	}
}

func resolvedExtension(name, bundle string) manifest.SystemExtension {
	return manifest.SystemExtension{
		Name: name, Bundle: bundle, State: manifest.SystemExtensionPresent,
		OCIManifestDigest:    "sha256:" + strings.Repeat("a", 64),
		BundleManifestDigest: "sha256:" + strings.Repeat("b", 64),
		ArtifactVersion:      "v1", PayloadVersion: "v1", Architecture: "x86_64",
		SupportedRuntimeInterfaces: []string{"katl-runtime-1"},
		Payloads: []manifest.SystemExtensionPayloadRef{{
			Name: name + ".raw", Role: "systemd-sysext",
			MediaType: "application/vnd.katl.sysext.raw.v1",
			Digest:    "sha256:" + strings.Repeat(name[:1], 64), SizeBytes: 12,
		}},
	}
}
