package main

import (
	"archive/tar"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func kernelInputFixture(t *testing.T) (string, config) {
	t.Helper()
	root := t.TempDir()
	output := t.TempDir()
	for path, value := range map[string]string{
		"usr/lib/modules/6.18.1/example.ko":                    "runtime module",
		"usr/src/kernels/6.18.1/include/config/kernel.release": "6.18.1\n",
		"usr/src/kernels/6.18.1/include/generated/autoconf.h":  "#define CONFIG_MODVERSIONS 1\n",
		"usr/src/kernels/6.18.1/Module.symvers":                "symbol versions",
		"usr/src/kernels/6.18.1/.config":                       "CONFIG_MODVERSIONS=y\n",
		"etc/private-build-state":                              "must not be exported",
	} {
		path = filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(value), 0644); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := configFromEnv(map[string]string{
		"KATL_MKOSI_BUILD_DIR": output,
		"KATL_VERSION":         "2026.9.0-dev.17",
		"KATL_BUILD_COMMIT":    "different-build-id",
	}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, output, "katl-runtime-root.squashfs", "runtime")
	writeTestFile(t, output, "katl-runtime.efi", "boot")
	if err := runWriteRuntimeRoot([]string{"--artifact", cfg.RuntimeRoot}, io.Discard, io.Discard, cfg); err != nil {
		t.Fatal(err)
	}
	_, digest, err := fileInfo(cfg.RuntimeRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := runWriteRuntimeUKI([]string{
		"--artifact", cfg.RuntimeUKI,
		"--runtime-artifact", cfg.RuntimeRoot,
		"--runtime-sha256", digest,
		"--kernel-version", "6.18.1",
	}, io.Discard, io.Discard, cfg); err != nil {
		t.Fatal(err)
	}
	return root, cfg
}

func TestKernelInputExport(t *testing.T) {
	root, cfg := kernelInputFixture(t)
	output := filepath.Dir(cfg.RuntimeRoot)
	if err := runExportKernelInputs([]string{root, output}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(output, kernelInputsFile)
	first, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}

	file, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	reader := tar.NewReader(file)
	found := map[string]string{}
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(header.Name, "usr/src/kernels/6.18.1/") {
			t.Fatalf("unexpected archive member %s", header.Name)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		found[header.Name] = string(data)
	}
	if len(found) != 4 || found["usr/src/kernels/6.18.1/Module.symvers"] != "symbol versions" || found["usr/src/kernels/6.18.1/.config"] != "CONFIG_MODVERSIONS=y\n" {
		t.Fatalf("exported prepared inputs = %v", found)
	}
	if err := runExportKernelInputs([]string{root, output}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	repeated, err := os.ReadFile(archive)
	if err != nil || string(first) != string(repeated) {
		t.Fatalf("repeat export changed archive: %v", err)
	}

	if err := runBindKernelInputs(nil, cfg); err != nil {
		t.Fatal(err)
	}
	if err := runBindKernelInputs(nil, cfg); err != nil {
		t.Fatalf("repeat binding: %v", err)
	}
	inputs, err := readKernelInputs(archive)
	if err != nil {
		t.Fatal(err)
	}
	// Runtime SHA-256 is independently observed from the produced root artifact.
	_, runtimeDigest, err := fileInfo(cfg.RuntimeRoot)
	if err != nil {
		t.Fatal(err)
	}
	if inputs.Target.Release != "6.18.1" || inputs.Target.RuntimeSHA256 != runtimeDigest {
		t.Fatalf("input ownership = %#v", inputs.Target)
	}
}

func TestKernelInputsRejectIncompleteHeaders(t *testing.T) {
	for _, missing := range []string{"Module.symvers", ".config", "include/generated/autoconf.h", "include/config/kernel.release"} {
		t.Run(missing, func(t *testing.T) {
			root, cfg := kernelInputFixture(t)
			if err := os.Remove(filepath.Join(root, "usr/src/kernels/6.18.1", missing)); err != nil {
				t.Fatal(err)
			}
			if err := runExportKernelInputs([]string{root, filepath.Dir(cfg.RuntimeRoot)}, io.Discard, io.Discard); err == nil {
				t.Fatal("accepted incomplete kernel headers")
			}
		})
	}
}

func TestKernelInputBinding(t *testing.T) {
	for _, scenario := range []string{"corrupt archive", "different kernel", "different runtime"} {
		t.Run(scenario, func(t *testing.T) {
			root, cfg := kernelInputFixture(t)
			output := filepath.Dir(cfg.RuntimeRoot)
			if err := runExportKernelInputs([]string{root, output}, io.Discard, io.Discard); err != nil {
				t.Fatal(err)
			}
			if err := runBindKernelInputs(nil, cfg); err != nil {
				t.Fatal(err)
			}
			archive := filepath.Join(output, kernelInputsFile)
			inputs, err := readKernelInputs(archive)
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "corrupt archive":
				if err := os.WriteFile(archive, []byte("damaged"), 0644); err != nil {
					t.Fatal(err)
				}
			case "different kernel":
				inputs.Target.Release = "6.18.2"
				writeTestJSON(t, archive+".json", inputs)
			case "different runtime":
				inputs.Target.RuntimeSHA256 = strings.Repeat("b", 64)
				writeTestJSON(t, archive+".json", inputs)
			}
			before, err := os.ReadFile(archive + ".json")
			if err != nil {
				t.Fatal(err)
			}
			if err := runBindKernelInputs(nil, cfg); err == nil {
				t.Fatal("accepted invalid runtime inputs")
			}
			after, err := os.ReadFile(archive + ".json")
			if err != nil || string(after) != string(before) {
				t.Fatalf("failed binding changed metadata: %v", err)
			}
		})
	}
}

func TestExtensionRequiresRuntimeInputs(t *testing.T) {
	_, cfg := kernelInputFixture(t)
	t.Setenv("KATL_MKOSI_BUILD_DIR", filepath.Dir(cfg.RuntimeRoot))
	if err := os.MkdirAll(filepath.Join(cfg.RepoRoot, "extensions/example"), 0755); err != nil {
		t.Fatal(err)
	}
	writeTestJSON(t, filepath.Join(cfg.RepoRoot, "extensions/example/recipe.json"), kernelRecipe{
		Version:    "1.0",
		Repository: "registry.invalid/extensions/example",
		Sources: map[string]kernelSource{"source": {
			URL:    "https://example.invalid/source.tar.gz",
			SHA256: strings.Repeat("a", 64),
		}},
	})
	err := runBuildKernelExtension([]string{"example"}, io.Discard, io.Discard, cfg)
	if err == nil || !strings.Contains(err.Error(), "read kernel build inputs") {
		t.Fatalf("extension did not reject missing inputs before source or package acquisition: %v", err)
	}
}
