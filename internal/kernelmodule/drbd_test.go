package kernelmodule

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDRBDImageComposition(t *testing.T) {
	directory := os.Getenv("KATL_TEST_DRBD_BUILD")
	if directory == "" {
		t.Skip("set KATL_TEST_DRBD_BUILD to the release-owned DRBD build directory with image-mount privileges")
	}
	data, err := os.ReadFile(filepath.Join(directory, "katl-drbd9.bundle.json"))
	if err != nil {
		t.Fatal(err)
	}
	var bundle struct {
		Kernel                     Contract `json:"kernel"`
		Architecture               string   `json:"architecture"`
		SupportedRuntimeInterfaces []string `json:"supportedRuntimeInterfaces"`
		Payloads                   []struct {
			FileName string `json:"fileName"`
			Digest   string `json:"digest"`
		} `json:"payloads"`
	}
	if err := json.Unmarshal(data, &bundle); err != nil {
		t.Fatal(err)
	}
	if len(bundle.Payloads) != 1 || len(bundle.SupportedRuntimeInterfaces) != 1 {
		t.Fatal("DRBD build must declare one image and one runtime interface")
	}
	payload := bundle.Payloads[0]
	prepared, err := PrepareImages(context.Background(), ImageRequest{
		Target: bundle.Kernel.Target,
		Runtime: Image{
			Path:   filepath.Join(directory, "katl-runtime-root.squashfs"),
			SHA256: bundle.Kernel.Target.RuntimeSHA256,
		},
		RuntimeInterface: bundle.SupportedRuntimeInterfaces[0],
		Architecture:     bundle.Architecture,
		WorkDir:          t.TempDir(),
		Bundles: []BundleImages{{
			Name:     "drbd9",
			Contract: &bundle.Kernel,
			Images: []Image{{
				Name:   payload.FileName,
				Path:   filepath.Join(directory, payload.FileName),
				SHA256: strings.TrimPrefix(payload.Digest, "sha256:"),
			}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()

	indexes := filepath.Join(t.TempDir(), "indexes")
	output, err := exec.Command("unsquashfs", "-no-progress", "-d", indexes, prepared.Path).CombinedOutput()
	if err != nil {
		t.Fatalf("extract generated indexes: %v: %s", err, output)
	}
	if err := os.Symlink("usr/lib", filepath.Join(indexes, "lib")); err != nil {
		t.Fatal(err)
	}
	// kmod's binary-index reader must choose the release-owned replacement,
	// including when resolving its transport's dependency on the core driver.
	output, err = exec.Command("modprobe", "-C", "/dev/null", "-d", indexes, "-S", bundle.Kernel.Target.Release, "--show-depends", "drbd_transport_tcp").CombinedOutput()
	if err != nil || !strings.Contains(string(output), "/extra/katl/drbd.ko") || !strings.Contains(string(output), "/extra/katl/drbd_transport_tcp.ko") || strings.Contains(string(output), "/kernel/drivers/block/drbd/") {
		t.Fatalf("selected DRBD provider: %v: %s", err, output)
	}
	if err := filepath.WalkDir(indexes, func(path string, entry os.DirEntry, err error) error {
		if err == nil && strings.HasSuffix(entry.Name(), ".ko") {
			t.Errorf("index image unexpectedly includes module %s", path)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
