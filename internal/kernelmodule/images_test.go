package kernelmodule

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestImageComposition(t *testing.T) {
	if os.Getenv("KATL_TEST_IMAGE_MOUNTS") != "1" {
		t.Skip("set KATL_TEST_IMAGE_MOUNTS=1 with image-mount privileges")
	}
	base, selection := compositionFixture(t)
	writeModuleFile(t, base, "usr/lib/os-release", "ID=katlos\nSYSEXT_LEVEL=katl-runtime-1\n")
	writeModuleFile(t, selection.Roots[0], "usr/lib/extension-release.d/extension-release.example", "ID=katlos\nSYSEXT_LEVEL=katl-runtime-1\n")
	runtime := squashImage(t, base)
	extension := squashImage(t, selection.Roots[0])
	extension.Name = "example.raw"
	selection.Contract.Target.RuntimeSHA256 = runtime.SHA256

	prepared, err := PrepareImages(context.Background(), ImageRequest{
		Target:           selection.Contract.Target,
		Runtime:          runtime,
		RuntimeInterface: "katl-runtime-1",
		Architecture:     "x86_64",
		WorkDir:          t.TempDir(),
		Bundles: []BundleImages{{
			Name:     selection.Name,
			Images:   []Image{extension},
			Contract: selection.Contract,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	data, err := exec.Command("unsquashfs", "-cat", prepared.Path, "usr/lib/modules/6.12.1/modules.dep").CombinedOutput()
	if err != nil || string(data) != "extra/example.ko:\n" {
		t.Fatalf("packaged module index = %q, %v", data, err)
	}
	data, err = exec.Command("unsquashfs", "-cat", prepared.Path, "usr/lib/extension-release.d/extension-release."+IndexExtensionName).CombinedOutput()
	if err != nil || string(data) != "ID=katlos\nSYSEXT_LEVEL=katl-runtime-1\nARCHITECTURE=x86-64\n" {
		t.Fatalf("packaged extension compatibility = %q, %v", data, err)
	}
	data, err = os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), prepared.directory) {
		t.Fatal("preparation left its source images mounted")
	}
	if err := verifyImage(Image{
		Path:   prepared.Path,
		SHA256: prepared.SHA256,
	}, false); err != nil {
		t.Fatal(err)
	}
}

func squashImage(t *testing.T, tree string) Image {
	t.Helper()
	path := filepath.Join(t.TempDir(), "image.raw")
	output, err := exec.Command("mksquashfs", tree, path, "-noappend", "-all-root", "-no-xattrs", "-processors", "1").CombinedOutput()
	if err != nil {
		t.Fatalf("build image fixture: %v: %s", err, output)
	}
	digest, err := fileDigest(path)
	if err != nil {
		t.Fatal(err)
	}
	return Image{
		Path:   path,
		SHA256: digest,
	}
}
