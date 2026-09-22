package manifest

import (
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/kernelmodule"
)

func TestModuleIndexNameIsReserved(t *testing.T) {
	extension := resolvedSystemExtensionForTest("driver", "registry.example/driver:v1", nil)
	extension.Configuration = SystemExtensionConfiguration{}
	extension.Payloads[0].Name = kernelmodule.IndexExtensionName + ".raw"
	if err := ValidateSystemExtensions([]SystemExtension{extension}, false); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("extension can shadow generated module indexes: %v", err)
	}
}

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
		{Bundle: "./bird.raw"},
		{State: SystemExtensionAbsent},
		{
			State:   SystemExtensionAbsent,
			Release: "registry.example/bird",
			Units:   []SystemExtensionUnit{{Name: "bird.service"}},
		},
	} {
		if err := ValidateSystemExtensions([]SystemExtension{extension}, true); err == nil {
			t.Fatalf("ValidateSystemExtensions(%#v) accepted invalid authoring input", extension)
		}
	}
}

func TestRemovalSelectors(t *testing.T) {
	for _, extension := range []SystemExtension{
		{
			State:   SystemExtensionAbsent,
			Release: "registry.example/bird",
		},
		{
			State:  SystemExtensionAbsent,
			Bundle: "registry.example/bird@sha256:" + strings.Repeat("a", 64),
		},
	} {
		if err := ValidateSystemExtensions([]SystemExtension{extension}, true); err != nil {
			t.Fatalf("removal must accept a selector without resolved metadata: %v", err)
		}
		if extension.Repository() != "registry.example/bird" {
			t.Fatalf("removal repository = %q", extension.Repository())
		}
	}
}

func TestPayloadIdentity(t *testing.T) {
	for _, extension := range []SystemExtension{
		{Release: "registry.example/team/driver"},
		{Bundle: "registry.example/team/driver:v2"},
		{Bundle: "registry.example/team/driver@sha256:" + strings.Repeat("a", 64)},
	} {
		if got := extension.PayloadID("driver.raw"); got != "registry.example/team/driver#driver.raw" {
			t.Errorf("repository-owned payload = %q", got)
		}
	}

	other := SystemExtension{Release: "other.example/team/driver"}
	if got := other.PayloadID("driver.raw"); got != "other.example/team/driver#driver.raw" {
		t.Errorf("other repository payload = %q", got)
	}
}

func resolvedSystemExtensionForTest(name, bundle string, content *string) SystemExtension {
	return SystemExtension{
		State:                      SystemExtensionPresent,
		Bundle:                     bundle,
		OCIManifestDigest:          "sha256:" + strings.Repeat("a", 64),
		BundleManifestDigest:       "sha256:" + strings.Repeat("b", 64),
		ArtifactVersion:            "v1",
		PayloadVersion:             "v1",
		Architecture:               "x86_64",
		SupportedRuntimeInterfaces: []string{"katl-runtime-1"},
		Payloads: []SystemExtensionPayloadRef{{
			Name:      "katl-" + name + ".raw",
			Role:      "systemd-sysext",
			MediaType: "application/vnd.katl.sysext.raw.v1",
			Digest:    "sha256:" + strings.Repeat("c", 64),
			SizeBytes: 1024,
		}},
		Configuration: SystemExtensionConfiguration{Files: []HostConfigurationFile{{
			Path:    "/etc/" + name + ".conf",
			Content: content,
		}}},
	}
}
