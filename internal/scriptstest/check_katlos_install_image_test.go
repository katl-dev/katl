package scriptstest

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestCheckKatlOSInstallImageKernelCommandLine(t *testing.T) {
	type testCase struct {
		name    string
		command []string
		wantErr bool
	}
	required := []string{
		"rootfstype=squashfs",
		"ro",
		"console=ttyS0,115200n8",
		"console=tty0",
	}
	cases := []testCase{
		{
			name: "required semantics in any order with additions",
			command: []string{
				"console=tty0",
				"quiet",
				"ro",
				"console=ttyS0,115200n8",
				"rootfstype=squashfs",
			},
		},
	}
	for _, missing := range required {
		cases = append(cases, testCase{
			name:    "missing " + missing,
			command: slices.DeleteFunc(slices.Clone(required), func(arg string) bool { return arg == missing }),
			wantErr: true,
		})
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			artifact, fakeBin, squashfsRoot := writeKatlOSImageCheckFixture(t, tc.command)
			cmd := exec.Command(filepath.Join(repoRoot(t), "scripts", "check-katlos-install-image"), artifact)
			cmd.Dir = repoRoot(t)
			cmd.Env = append(os.Environ(),
				"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
				"KATL_FAKE_SQUASHFS_ROOT="+squashfsRoot,
			)
			output, err := cmd.CombinedOutput()
			if tc.wantErr {
				if err == nil || !strings.Contains(string(output), "runtime UKI component metadata is incomplete") {
					t.Fatalf("check-katlos-install-image error = %v, output = %q", err, output)
				}
				return
			}
			if err != nil {
				t.Fatalf("check-katlos-install-image failed: %v\n%s", err, output)
			}
		})
	}
}

func writeKatlOSImageCheckFixture(t *testing.T, commandLine []string) (string, string, string) {
	t.Helper()
	dir := t.TempDir()
	artifact := writeArtifact(t, dir, "katlos-install.squashfs", "katlos image")
	writeChecksum(t, artifact)
	writeJSONFile(t, artifact+".json", map[string]any{
		"apiVersion":        "katl.dev/v1alpha1",
		"kind":              "KatlOSImageArtifact",
		"imageRole":         "install",
		"format":            "squashfs",
		"path":              filepath.Base(artifact),
		"sizeBytes":         int64(len("katlos image")),
		"sha256":            fileSHA256(t, artifact),
		"embeddedIndexPath": "katlos/image.json",
	})

	squashfsRoot := filepath.Join(dir, "squashfs")
	for _, path := range []string{
		"katlos",
		"components/runtime",
		"components/boot",
		"components/metadata",
	} {
		if err := os.MkdirAll(filepath.Join(squashfsRoot, path), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s) error = %v", path, err)
		}
	}
	runtimeRoot := writeArtifact(t, filepath.Join(squashfsRoot, "components", "runtime"), "root.squashfs", "runtime root")
	runtimeUKI := writeArtifact(t, filepath.Join(squashfsRoot, "components", "boot"), "katl.efi", "runtime uki")
	writeJSONFile(t, filepath.Join(squashfsRoot, "components", "metadata", "runtime-root.json"), map[string]any{"kind": "runtime-root"})
	writeJSONFile(t, filepath.Join(squashfsRoot, "components", "metadata", "runtime-uki.json"), map[string]any{"kind": "runtime-uki"})
	writeArtifact(t, filepath.Join(squashfsRoot, "components", "metadata"), "runtime-root.sha256", fileSHA256(t, runtimeRoot)+"  ../runtime/root.squashfs")
	writeArtifact(t, filepath.Join(squashfsRoot, "components", "metadata"), "runtime-uki.sha256", fileSHA256(t, runtimeUKI)+"  ../boot/katl.efi")
	writeJSONFile(t, filepath.Join(squashfsRoot, "katlos", "image.json"), map[string]any{
		"apiVersion":       "katl.dev/v1alpha1",
		"kind":             "KatlOSImage",
		"imageRole":        "install",
		"format":           "squashfs",
		"runtimeInterface": "katl-runtime-1",
		"components": []map[string]any{
			{
				"role":      "runtime-root",
				"path":      "components/runtime/root.squashfs",
				"sizeBytes": int64(len("runtime root")),
				"sha256":    fileSHA256(t, runtimeRoot),
				"compatibility": map[string]any{
					"runtimeInterface": "katl-runtime-1",
				},
				"installTarget": map[string]any{
					"kind":       "root-slot",
					"filesystem": "squashfs",
				},
			},
			{
				"role":      "runtime-uki",
				"path":      "components/boot/katl.efi",
				"sizeBytes": int64(len("runtime uki")),
				"sha256":    fileSHA256(t, runtimeUKI),
				"compatibility": map[string]any{
					"runtimeInterface":  "katl-runtime-1",
					"kernelCommandLine": commandLine,
				},
				"installTarget": map[string]any{
					"kind": "esp-or-xbootldr",
				},
			},
		},
	})

	fakeBin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(fakeBin, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", fakeBin, err)
	}
	writeFakeExecutable(t, fakeBin, "unsquashfs", `
if [[ "$1" != "-cat" ]]; then
  printf 'unexpected unsquashfs arguments: %s\n' "$*" >&2
  exit 2
fi
command cat "$KATL_FAKE_SQUASHFS_ROOT/$3"
`)
	return artifact, fakeBin, squashfsRoot
}
