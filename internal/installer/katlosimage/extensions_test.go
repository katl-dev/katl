package katlosimage

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/extensionrelease"
	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/installer/systemextensionbundle"
	"github.com/katl-dev/katl/internal/kernelmodule"
)

func TestBuiltImageExtensionClosure(t *testing.T) {
	image := os.Getenv("KATL_TEST_KATLOS_IMAGE")
	if image == "" {
		t.Skip("set KATL_TEST_KATLOS_IMAGE to a built image advertising release extensions")
	}
	data, err := os.ReadFile(image + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var metadata ArtifactMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "image")
	output, err := exec.Command("unsquashfs", "-no-progress", "-d", root, image).CombinedOutput()
	if err != nil {
		t.Fatalf("extract built image: %v: %s", err, output)
	}
	payload, err := ResolveDirectory(context.Background(), root, manifest.KatlosImage{
		Role:             metadata.ImageRole,
		Version:          metadata.Version,
		Architecture:     metadata.Architecture,
		RuntimeInterface: metadata.RuntimeInterface,
	})
	if err != nil {
		t.Fatal(err)
	}
	if payload.Index.ExtensionRelease == nil || payload.Index.ExtensionRelease.Extensions["ghcr.io/katl-dev/katl/extensions/drbd9"] == "" {
		t.Fatal("built image does not advertise DRBD9")
	}
	resolved, err := payload.ResolveReleaseExtension(context.Background(), systemextensionbundle.ResolveRequest{
		Reference:        payload.Index.ExtensionRelease.Extensions["ghcr.io/katl-dev/katl/extensions/drbd9"],
		Architecture:     metadata.Architecture,
		RuntimeInterface: metadata.RuntimeInterface,
		RuntimeSHA256:    payload.Runtime.SHA256,
	})
	if err != nil || resolved.Bundle.Kernel == nil || len(resolved.Payloads) == 0 {
		t.Fatalf("built image lacks complete kernel-bound driver: %v", err)
	}
}

func TestImageExtensionClosure(t *testing.T) {
	root, index := writeImagePayload(t, func(*Index) {})
	var runtimeSHA string
	for _, component := range index.Components {
		if component.Role == ComponentRuntimeRoot {
			runtimeSHA = component.SHA256
		}
	}
	target := extensionrelease.Target{
		Version:          index.Version,
		Architecture:     index.Architecture,
		Flavour:          "standard",
		RuntimeInterface: index.RuntimeInterface,
		Kernel: kernelmodule.Target{
			Release:       "6.12.1",
			RuntimeSHA256: runtimeSHA,
		},
	}
	image := filepath.Join(t.TempDir(), "drbd.raw")
	if err := os.WriteFile(image, []byte("selected driver image"), 0o644); err != nil {
		t.Fatal(err)
	}
	packed, built, err := systemextensionbundle.Export(context.Background(), filepath.Join(root, ExtensionLayoutPath), systemextensionbundle.BuildRequest{
		Name:                       "drbd9",
		ArtifactVersion:            index.Version,
		PayloadVersion:             "9.3.4",
		Architecture:               index.Architecture,
		SupportedRuntimeInterfaces: []string{index.RuntimeInterface},
		CreatedAt:                  time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC),
		Kernel: &kernelmodule.Contract{
			Target: target.Kernel,
			Modules: []kernelmodule.Module{{
				Name:   "drbd",
				Path:   "usr/lib/modules/6.12.1/extra/drbd.ko",
				SHA256: strings.Repeat("c", 64),
			}},
		},
		Payloads: []systemextensionbundle.Input{{
			Path: image,
			Role: systemextensionbundle.SysextRole,
		}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ref := "registry.example/drbd9@" + packed.ManifestDigest
	index.ExtensionRelease = &extensionrelease.Manifest{
		Target:     target,
		Extensions: map[string]string{"registry.example/drbd9": ref},
	}
	data, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "katlos/image.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	payload, err := ResolveDirectory(context.Background(), root, expectedImage())
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := payload.ResolveReleaseExtension(context.Background(), systemextensionbundle.ResolveRequest{
		Reference:        ref,
		Architecture:     target.Architecture,
		RuntimeInterface: target.RuntimeInterface,
		RuntimeSHA256:    target.Kernel.RuntimeSHA256,
	})
	if err != nil || resolved.Reference != ref || len(resolved.Payloads) != 1 || string(resolved.Payloads[0].Data) != "selected driver image" {
		t.Fatalf("offline release artifact = %+v, %v", resolved, err)
	}

	index.ExtensionRelease.Target.Kernel.Release = "6.12.2"
	data, err = json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "katlos/image.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveDirectory(context.Background(), root, expectedImage()); err == nil || !strings.Contains(err.Error(), "kernel release") {
		t.Fatalf("wrong-kernel embedded extension = %v", err)
	}
	index.ExtensionRelease.Target.Kernel.Release = "6.12.1"
	data, err = json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "katlos/image.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	blob := filepath.Join(root, ExtensionLayoutPath, "blobs", "sha256", strings.TrimPrefix(built.Bundle.Payloads[0].Digest, "sha256:"))
	if err := os.Chmod(blob, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blob, []byte(strings.Repeat("x", len("selected driver image"))), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveDirectory(context.Background(), root, expectedImage()); err == nil || !strings.Contains(err.Error(), "verify OCI layer") {
		t.Fatalf("corrupt embedded extension = %v", err)
	}
	if err := os.RemoveAll(filepath.Join(root, ExtensionLayoutPath)); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveDirectory(context.Background(), root, expectedImage()); err == nil || !strings.Contains(err.Error(), "local OCI layout") {
		t.Fatalf("image with missing advertised closure = %v", err)
	}
}
