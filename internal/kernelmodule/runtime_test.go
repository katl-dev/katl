package kernelmodule

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRuntimeComposition(t *testing.T) {
	base := os.Getenv("KATL_TEST_MODULE_RUNTIME")
	if base == "" {
		t.Skip("set KATL_TEST_MODULE_RUNTIME to a built, unmerged Katl runtime tree")
	}
	moduleRoot := filepath.Join(base, "usr/lib/modules")
	releases, err := os.ReadDir(moduleRoot)
	if err != nil || len(releases) != 1 {
		t.Fatalf("expected one target kernel: %v, %v", releases, err)
	}
	release := releases[0].Name()
	var source string
	err = filepath.WalkDir(filepath.Join(moduleRoot, release), func(path string, entry fs.DirEntry, err error) error {
		if err == nil && entry.Type().IsRegular() && isModule(path) && moduleName(path) == "dummy" {
			source = path
		}
		return err
	})
	if err != nil || source == "" {
		t.Fatalf("find real dummy module: %v", err)
	}
	contents, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	modulePath := filepath.Join("usr/lib/modules", release, "extra", filepath.Base(source))
	extension := t.TempDir()
	if err := copyModuleFile(source, filepath.Join(extension, modulePath)); err != nil {
		t.Fatal(err)
	}
	target := Target{
		Release:       release,
		RuntimeSHA256: strings.Repeat("a", 64),
	}
	layer, err := compose(context.Background(), composeRequest{
		Target:   target,
		BaseRoot: base,
		WorkDir:  t.TempDir(),
		Selections: []mountedBundle{{
			Name:  "test-replacement",
			Roots: []string{extension},
			Contract: &Contract{
				Target: target,
				Modules: []Module{{
					Name:     "dummy",
					Path:     modulePath,
					SHA256:   hex.EncodeToString(digest[:]),
					Replaces: "dummy",
				}},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := exec.Command("modprobe", "-C", "/dev/null", "-d", layer, "-S", release, "--show-depends", "dummy").CombinedOutput()
	if err != nil || !strings.Contains(string(data), "/extra/"+filepath.Base(source)) || strings.Contains(string(data), "/kernel/") {
		t.Fatalf("resolved real module provider = %q, %v", data, err)
	}
	t.Logf("target %s: modprobe selects the replacement from the generated binary index", release)
}
