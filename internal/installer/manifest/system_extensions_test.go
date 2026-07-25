package manifest

import (
	"strings"
	"testing"
)

func TestValidateSystemExtensionsTypedList(t *testing.T) {
	content := "router id from \"bird0\";\n"
	extensions := []SystemExtension{resolvedSystemExtensionForTest("bird", "registry.example/bird:v1", &content)}
	if err := ValidateSystemExtensions(extensions, false); err != nil {
		t.Fatalf("ValidateSystemExtensions() error = %v", err)
	}
	extensions = append(extensions, extensions[0])
	if err := ValidateSystemExtensions(extensions, false); err == nil || !strings.Contains(err.Error(), "duplicates another system extension") {
		t.Fatalf("duplicate validation error = %v", err)
	}
}

func TestValidateSystemExtensionsRejectsLooseAndMalformedInputs(t *testing.T) {
	for _, extension := range []SystemExtension{
		{Name: "bird", Bundle: "./bird.raw"},
		{Name: "bird", State: SystemExtensionAbsent, Bundle: "registry.example/bird:v1"},
	} {
		if err := ValidateSystemExtensions([]SystemExtension{extension}, true); err == nil {
			t.Fatalf("ValidateSystemExtensions(%#v) accepted invalid authoring input", extension)
		}
	}
}

func resolvedSystemExtensionForTest(name, bundle string, content *string) SystemExtension {
	return SystemExtension{
		Name: name, State: SystemExtensionPresent, Bundle: bundle,
		OCIManifestDigest:    "sha256:" + strings.Repeat("a", 64),
		BundleManifestDigest: "sha256:" + strings.Repeat("b", 64),
		ArtifactVersion:      "v1", PayloadVersion: "v1", Architecture: "x86_64",
		SupportedRuntimeInterfaces: []string{"katl-runtime-1"},
		Payloads: []SystemExtensionPayloadRef{{
			Name: "katl-" + name + ".raw", Role: "systemd-sysext",
			MediaType: "application/vnd.katl.sysext.raw.v1",
			Digest:    "sha256:" + strings.Repeat("c", 64), SizeBytes: 1024,
		}},
		Configuration: SystemExtensionConfiguration{Files: []HostConfigurationFile{{
			Path: "/etc/" + name + ".conf", Content: content,
		}}},
	}
}
