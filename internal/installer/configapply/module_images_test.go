package configapply

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/extensionrelease"
	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/kernelmodule"
)

func TestApplyKernelImageLifecycle(t *testing.T) {
	if os.Getenv("KATL_TEST_IMAGE_MOUNTS") != "1" {
		t.Skip("set KATL_TEST_IMAGE_MOUNTS=1 with image-mount privileges")
	}
	root := t.TempDir()
	request := trustedBundleRequest(root, TrustedBundleRequest{})
	base := t.TempDir()
	for _, name := range []string{"modules.order", "modules.builtin", "modules.builtin.modinfo"} {
		writeModuleFixture(t, filepath.Join(base, "usr/lib/modules/6.12.1", name), "")
	}
	writeModuleFixture(t, filepath.Join(base, "usr/lib/os-release"), "ID=katlos\nVERSION_ID=0.1.0\nSYSEXT_LEVEL=katl-runtime-1\n")
	runtimePath, runtimeData := packModuleFixture(t, base)
	request.CurrentRecord.Root.RuntimeArtifactSHA256 = strings.TrimPrefix(digestPayload(runtimeData), "sha256:")
	device := filepath.Join(root, "dev/disk/by-partuuid", request.CurrentRecord.Root.PartitionUUID)
	if err := os.MkdirAll(filepath.Dir(device), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(runtimePath, device); err != nil {
		t.Fatal(err)
	}
	target := extensionrelease.Target{
		Version:          request.CurrentRecord.RuntimeVersion,
		Architecture:     request.CurrentRecord.Root.Architecture,
		Flavour:          "standard",
		RuntimeInterface: request.CurrentRecord.Root.RuntimeInterface,
		Kernel: kernelmodule.Target{
			Release:       "6.12.1",
			RuntimeSHA256: request.CurrentRecord.Root.RuntimeArtifactSHA256,
		},
	}
	request.CurrentRecord.ExtensionRelease = &extensionrelease.Manifest{Target: target}
	kubernetes := t.TempDir()
	writeModuleFixture(t, filepath.Join(kubernetes, "usr/lib/extension-release.d/extension-release.kubernetes"), "ID=katlos\nSYSEXT_LEVEL=katl-runtime-1\n")
	_, kubeImage := packModuleFixture(t, kubernetes)
	ref := &request.CurrentRecord.Sysexts[0]
	ref.SHA256 = strings.TrimPrefix(digestPayload(kubeImage), "sha256:")
	if err := os.WriteFile(filepath.Join(root, ref.Path), kubeImage, 0o644); err != nil {
		t.Fatal(err)
	}
	extension := t.TempDir()
	modulePath := "usr/lib/modules/6.12.1/extra/example.ko"
	writeModuleFixture(t, filepath.Join(extension, "usr/lib/extension-release.d/extension-release.driver"), "ID=katlos\nSYSEXT_LEVEL=katl-runtime-1\nARCHITECTURE=x86-64\n")
	if err := os.MkdirAll(filepath.Dir(filepath.Join(extension, modulePath)), 0o755); err != nil {
		t.Fatal(err)
	}
	compile := exec.Command("cc", "-x", "c", "-c", "-o", filepath.Join(extension, modulePath), "-")
	compile.Stdin = strings.NewReader(`
__attribute__((section(".modinfo"), used)) const char name[] = "name=example";
__attribute__((section(".modinfo"), used)) const char vermagic[] = "vermagic=6.12.1 SMP";
`)
	if output, err := compile.CombinedOutput(); err != nil {
		t.Fatalf("build test module: %v: %s", err, output)
	}
	moduleData, err := os.ReadFile(filepath.Join(extension, modulePath))
	if err != nil {
		t.Fatal(err)
	}
	_, image := packModuleFixture(t, extension)
	payload := manifest.SystemExtensionPayloadRef{
		Name:      "driver.raw",
		Role:      "systemd-sysext",
		MediaType: "application/vnd.katl.sysext.raw.v1",
		Digest:    digestPayload(image),
		SizeBytes: int64(len(image)),
	}
	desired := []manifest.SystemExtension{{
		Bundle:                     "registry.example/driver:v1",
		OCIManifestDigest:          "sha256:" + strings.Repeat("a", 64),
		BundleManifestDigest:       "sha256:" + strings.Repeat("b", 64),
		ArtifactVersion:            "1",
		PayloadVersion:             "1",
		Architecture:               target.Architecture,
		SupportedRuntimeInterfaces: []string{target.RuntimeInterface},
		Kernel: &kernelmodule.Contract{
			Target: target.Kernel,
			Modules: []kernelmodule.Module{{
				Name:     "example",
				Path:     modulePath,
				SHA256:   strings.TrimPrefix(digestPayload(moduleData), "sha256:"),
				Required: true,
			}},
		},
		Payloads: []manifest.SystemExtensionPayloadRef{payload},
	}}
	request.ClusterDefaults.SystemExtensions = &desired
	request.SystemExtensionPayloads = []SystemExtensionPayload{{
		Ref:  payload,
		Data: image,
	}}

	result, err := ApplyTrustedBundle(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	spec, _, err := generation.ReadGeneration(root, request.GenerationID)
	if err != nil {
		t.Fatal(err)
	}
	var indexesPath string
	for _, ref := range spec.Sysexts {
		if ref.Compatibility.ModuleIndexes != nil {
			indexesPath = filepath.Join(root, ref.Path)
		}
	}
	data, err := exec.Command("unsquashfs", "-cat", indexesPath, "usr/lib/modules/6.12.1/modules.dep").CombinedOutput()
	if err != nil || string(data) != "extra/example.ko:\n" {
		t.Fatalf("retained dependency index = %q, %v", data, err)
	}
	if _, err := generation.PlanActivation(result.Plan.GenerationRecord); err != nil {
		t.Fatal(err)
	}
	request.CurrentRecord = result.Plan.GenerationRecord
	request.CurrentManifest = result.Manifest
	request.GenerationID = "reapply"
	request.DesiredVersion = "3"
	if _, err := ApplyTrustedBundle(context.Background(), request); !errors.Is(err, ErrNoChanges) {
		t.Fatalf("unchanged module apply: %v", err)
	}
	desired = []manifest.SystemExtension{}
	request.SystemExtensionPayloads = nil
	request.GenerationID = "remove-driver"
	request.DesiredVersion = "4"
	removed, err := ApplyTrustedBundle(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range removed.Plan.GenerationRecord.Sysexts {
		if ref.Compatibility.Kernel != nil || ref.Compatibility.ModuleIndexes != nil {
			t.Fatalf("removed generation retained module assets: %+v", ref)
		}
	}
}

func writeModuleFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func packModuleFixture(t *testing.T, root string) (string, []byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "image.raw")
	output, err := exec.Command("mksquashfs", root, path, "-noappend", "-all-root", "-no-xattrs", "-processors", "1").CombinedOutput()
	if err != nil {
		t.Fatalf("package fixture: %v: %s", err, output)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return path, data
}
