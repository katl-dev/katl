package systemextensionbundle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuildUsesCommonDescriptorsAndCanonicalIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "katl-bird.raw")
	if err := os.WriteFile(path, []byte("bird sysext"), 0o644); err != nil {
		t.Fatal(err)
	}
	built, err := Build(BuildRequest{
		Name: "bird", ArtifactVersion: "v3.1.2-katl.1", PayloadVersion: "v3.1.2",
		Architecture: "x86_64", SupportedRuntimeInterfaces: []string{"katl-runtime-1", "katl-runtime-1"},
		CreatedAt: time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC),
		Payloads:  []Input{{Path: path, Role: SysextRole}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if built.Bundle.APIVersion != APIVersion || built.Bundle.ArtifactKind != ArtifactKind ||
		len(built.Bundle.Payloads) != 1 || built.Bundle.Payloads[0].MediaType != SysextMediaType ||
		built.Bundle.Payloads[0].FileName != "katl-bird.raw" ||
		len(built.Bundle.SupportedRuntimeInterfaces) != 1 ||
		built.ManifestHash != digestBytes(built.Manifest) {
		t.Fatalf("built bundle = %#v", built)
	}
}

func TestBuildRejectsBundleWithoutSysext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bird.confext.raw")
	if err := os.WriteFile(path, []byte("confext"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Build(BuildRequest{
		Name: "bird", ArtifactVersion: "v1", PayloadVersion: "v1", Architecture: "x86_64",
		SupportedRuntimeInterfaces: []string{"katl-runtime-1"},
		CreatedAt:                  time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC),
		Payloads:                   []Input{{Path: path, Role: ConfextRole}},
	})
	if err == nil || !strings.Contains(err.Error(), "at least one systemd-sysext") {
		t.Fatalf("Build() error = %v", err)
	}
}
