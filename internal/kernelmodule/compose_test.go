package kernelmodule

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompositionUsesSelectedProvider(t *testing.T) {
	base, selection := compositionFixture(t)
	basePath := filepath.Join(base, "usr/lib/modules/6.12.1/kernel/example.ko")
	buildModule(t, basePath, "example", "6.12.1")
	selection.Contract.Modules[0].Replaces = "example"

	layer, err := compose(context.Background(), composeRequest{
		Target:     selection.Contract.Target,
		BaseRoot:   base,
		Selections: []mountedBundle{selection},
		WorkDir:    t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(layer, "usr/lib/modules/6.12.1/modules.dep"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "extra/example.ko:\n" {
		t.Fatalf("module dependency index = %q", data)
	}
	if _, err := exec.LookPath("modprobe"); err != nil {
		t.Fatal(err)
	}
	// Consult the runtime consumer's binary index reader as well as depmod's
	// text output. --show-depends never inserts the fixture into the kernel.
	resolved, err := exec.Command("modprobe", "-C", "/dev/null", "-d", layer, "-S", "6.12.1", "--show-depends", "example").CombinedOutput()
	if err != nil || !strings.Contains(string(resolved), "/extra/example.ko") || strings.Contains(string(resolved), "/kernel/example.ko") {
		t.Fatalf("modprobe selected provider = %q, %v", resolved, err)
	}
	if _, err := os.Stat(basePath); err != nil {
		t.Fatalf("composition changed the base runtime: %v", err)
	}
	if _, err := os.Stat(filepath.Join(layer, selection.Contract.Modules[0].Path)); !os.IsNotExist(err) {
		t.Fatalf("index layer includes module payload: %v", err)
	}

	// Removing the extension must not inherit the previous generated index.
	removed, err := compose(context.Background(), composeRequest{
		Target:   selection.Contract.Target,
		BaseRoot: base,
		WorkDir:  t.TempDir(),
	})
	if err != nil || removed != "" {
		t.Fatalf("removal retained a module layer: %q, %v", removed, err)
	}
}

func TestCompositionRejectsInvalidSelections(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*testing.T, string, *mountedBundle)
		want   string
	}{
		{
			name: "required module without identity",
			change: func(_ *testing.T, _ string, s *mountedBundle) {
				s.Contract.Modules[0].Required = true
			},
			want: "source identity",
		},
		{
			name: "undeclared module",
			change: func(t *testing.T, _ string, s *mountedBundle) {
				writeModuleFile(t, s.Roots[0], "usr/lib/modules/6.12.1/extra/hidden.ko", "not declared")
			},
			want: "undeclared",
		},
		{
			name: "global index",
			change: func(t *testing.T, _ string, s *mountedBundle) {
				writeModuleFile(t, s.Roots[0], "usr/lib/modules/6.12.1/modules.dep", "extension-owned index")
			},
			want: "global indexes belong to Katl",
		},
		{
			name: "userspace contract",
			change: func(_ *testing.T, _ string, s *mountedBundle) {
				s.Contract = nil
			},
			want: "undeclared",
		},
		{
			name: "missing module",
			change: func(t *testing.T, _ string, s *mountedBundle) {
				s.Roots = []string{t.TempDir()}
			},
			want: "missing declared module",
		},
		{
			name: "corrupt payload",
			change: func(t *testing.T, _ string, s *mountedBundle) {
				writeModuleFile(t, s.Roots[0], s.Contract.Modules[0].Path, "corrupted")
			},
			want: "SHA-256 mismatch",
		},
		{
			name: "wrong vermagic",
			change: func(t *testing.T, _ string, s *mountedBundle) {
				path := filepath.Join(s.Roots[0], s.Contract.Modules[0].Path)
				s.Contract.Modules[0].SHA256 = buildModule(t, path, "example", "6.12.2")
			},
			want: "vermagic",
		},
		{
			name: "wrong embedded name",
			change: func(t *testing.T, _ string, s *mountedBundle) {
				path := filepath.Join(s.Roots[0], s.Contract.Modules[0].Path)
				s.Contract.Modules[0].SHA256 = buildModule(t, path, "other", "6.12.1")
			},
			want: "name does not match",
		},
		{
			name: "missing dependency",
			change: func(t *testing.T, _ string, s *mountedBundle) {
				path := filepath.Join(s.Roots[0], s.Contract.Modules[0].Path)
				s.Contract.Modules[0].SHA256 = buildModule(t, path, "example", "6.12.1", "missing_module")
			},
			want: "requires unavailable module",
		},
		{
			name: "implicit base replacement",
			change: func(t *testing.T, base string, _ *mountedBundle) {
				writeModuleFile(t, base, "usr/lib/modules/6.12.1/kernel/example.ko", "base provider")
			},
			want: "replacement declaration",
		},
		{
			name: "missing replacement target",
			change: func(_ *testing.T, _ string, s *mountedBundle) {
				s.Contract.Modules[0].Replaces = "example"
			},
			want: "replacement declaration",
		},
		{
			name: "built-in provider",
			change: func(t *testing.T, base string, s *mountedBundle) {
				writeModuleFile(t, base, "usr/lib/modules/6.12.1/modules.builtin", "kernel/example.ko\n")
				s.Contract.Modules[0].Replaces = "example"
			},
			want: "built-in kernel provider",
		},
		{
			name: "symlink module",
			change: func(t *testing.T, _ string, s *mountedBundle) {
				other := t.TempDir()
				path := filepath.Join(other, s.Contract.Modules[0].Path)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(s.Roots[0], s.Contract.Modules[0].Path), path); err != nil {
					t.Fatal(err)
				}
				s.Roots = []string{other}
			},
			want: "regular file",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			base, selection := compositionFixture(t)
			target := selection.Contract.Target
			test.change(t, base, &selection)

			_, err := compose(context.Background(), composeRequest{
				Target:     target,
				BaseRoot:   base,
				Selections: []mountedBundle{selection},
				WorkDir:    t.TempDir(),
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestCompositionRejectsDuplicateProviders(t *testing.T) {
	base, first := compositionFixture(t)
	second := first
	second.Name = "another-extension"
	_, err := compose(context.Background(), composeRequest{
		Target:     first.Contract.Target,
		BaseRoot:   base,
		Selections: []mountedBundle{first, second},
		WorkDir:    t.TempDir(),
	})
	if err == nil || !strings.Contains(err.Error(), "provided by both") {
		t.Fatalf("duplicate provider error = %v", err)
	}
}

func compositionFixture(t *testing.T) (string, mountedBundle) {
	t.Helper()
	for _, tool := range []string{"cc", "modinfo", "depmod", "modprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("module composition requires %s: %v", tool, err)
		}
	}
	base, extension := t.TempDir(), t.TempDir()
	for _, file := range []string{"modules.builtin", "modules.builtin.modinfo", "modules.order"} {
		writeModuleFile(t, base, "usr/lib/modules/6.12.1/"+file, "")
	}
	path := "usr/lib/modules/6.12.1/extra/example.ko"
	digest := buildModule(t, filepath.Join(extension, path), "example", "6.12.1")
	return base, mountedBundle{
		Name:  "example-driver",
		Roots: []string{extension},
		Contract: &Contract{
			Target: Target{
				Release:       "6.12.1",
				RuntimeSHA256: strings.Repeat("a", 64),
			},
			Modules: []Module{{
				Name:   "example",
				Path:   path,
				SHA256: digest,
			}},
		},
	}
}

// Small ELF fixtures exercise the real kmod readers without requiring matching
// host kernel headers or loading test code into the kernel.
func buildModule(t *testing.T, path, name, release string, dependencies ...string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("cc", "-x", "c", "-c", "-o", path, "-")
	command.Stdin = strings.NewReader(fmt.Sprintf(`
__attribute__((section(".modinfo"), used)) const char modname[] = "name=%s";
__attribute__((section(".modinfo"), used)) const char vermagic[] = "vermagic=%s SMP";
__attribute__((section(".modinfo"), used)) const char depends[] = "depends=%s";
`, name, release, strings.Join(dependencies, ",")))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("compile module fixture: %v: %s", err, output)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func writeModuleFile(t *testing.T, root, path, content string) {
	t.Helper()
	path = filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
