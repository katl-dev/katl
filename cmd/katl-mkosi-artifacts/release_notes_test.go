package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReleaseComponents(t *testing.T) {
	dir := t.TempDir()
	put := func(name, data string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	put("RELEASE_NOTES.md", "## Changes\n\nKeep these changes.\n")
	put("katl-installer.packages.tsv", "kernel-core\t6.19-2.x86_64\nsystemd\t259-2.x86_64\n")
	put("katl-runtime.packages.tsv", "kernel-core\t0:6.19-2.x86_64\nsystemd\t0:259-2.x86_64\ncontainerd\t0:2.2-1.x86_64\ncrun\t0:1.26-1.x86_64\n")
	put("katl-installer-lts.packages.tsv", "kernel-longterm-core\t0:6.18-1.x86_64\nsystemd\t0:259-3.x86_64\n")
	put("katl-runtime-lts.packages.tsv", "kernel-longterm-core\t0:6.18-2.x86_64\nsystemd\t0:259-4.x86_64\ncontainerd\t0:2.2-2.x86_64\ncrun\t2:1.26-2.x86_64\n")
	put("katl-runtime.extensions.json", `{"schemaVersion":1,"artifactKind":"katl.release-extension-inventory.v1","version":"2026.9.0","architecture":"x86_64","flavour":"standard","runtimeInterface":"katl-runtime-1","kernelRelease":"6.19","runtimeSHA256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","extensions":[{"name":"drbd9","payloadVersion":"9.3.4","repository":"registry.invalid/extensions/drbd9","reference":"registry.invalid/extensions/drbd9@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]}`)
	put("katl-runtime-lts.extensions.json", `{"schemaVersion":1,"artifactKind":"katl.release-extension-inventory.v1","version":"2026.9.0","architecture":"x86_64","flavour":"lts","runtimeInterface":"katl-runtime-1","kernelRelease":"6.18","runtimeSHA256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","extensions":[{"name":"drbd9","payloadVersion":"9.3.3","repository":"registry.invalid/extensions/drbd9","reference":"registry.invalid/extensions/drbd9@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}]}`)
	if err := writeReleaseComponents(dir, []string{"lts"}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := writeReleaseComponents(dir, []string{"standard", "lts"}); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(dir, "RELEASE_NOTES.md"))
		if err != nil {
			t.Fatal(err)
		}
		want := "## Included components\n\nExact installed RPM versions from the shipped package inventories (version-release.architecture, nonzero RPM epochs retained). Release-owned extension versions come from the verified OCI bundles selected by each image. Kubernetes extensions are distributed separately.\n\n| Flavour | Image | Kernel | systemd | containerd | crun |\n| --- | --- | --- | --- | --- | --- |\n| standard | installer | `6.19-2.x86_64` | `259-2.x86_64` | — | — |\n| standard | runtime | `6.19-2.x86_64` | `259-2.x86_64` | `2.2-1.x86_64` | `1.26-1.x86_64` |\n| lts | installer | `6.18-1.x86_64` | `259-3.x86_64` | — | — |\n| lts | runtime | `6.18-2.x86_64` | `259-4.x86_64` | `2.2-2.x86_64` | `2:1.26-2.x86_64` |\n\n| Flavour | Release extension | Version |\n| --- | --- | --- |\n| standard | drbd9 | `9.3.4` |\n| lts | drbd9 | `9.3.3` |\n\n## Changes\n\nKeep these changes.\n"
		if string(data) != want {
			t.Fatalf("notes = %s, want %s", data, want)
		}
	}
	for _, tc := range []struct{ name, inventory, want string }{
		{"missing", "systemd\t259\n", "exactly one kernel-core"},
		{"ambiguous", "kernel-core\t6.19\nkernel-core\t6.20\nsystemd\t259\n", "found 2"},
		{"malformed", "kernel-core 6.19\n", "tab"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, _ := os.ReadFile(filepath.Join(dir, "RELEASE_NOTES.md"))
			put("katl-installer.packages.tsv", tc.inventory)
			err := writeReleaseComponents(dir, []string{"standard"})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %s", err, tc.want)
			}
			after, _ := os.ReadFile(filepath.Join(dir, "RELEASE_NOTES.md"))
			if string(before) != string(after) {
				t.Fatal("failed validation changed notes")
			}
		})
	}
}

func TestReleaseComponentsRejectsInvalidExtensionInventory(t *testing.T) {
	dir := t.TempDir()
	put := func(name, data string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	put("RELEASE_NOTES.md", "## Changes\n\nKeep these changes.\n")
	put("katl-installer.packages.tsv", "kernel-core\t6.19-2.x86_64\nsystemd\t259-2.x86_64\n")
	put("katl-runtime.packages.tsv", "kernel-core\t6.19-2.x86_64\nsystemd\t259-2.x86_64\ncontainerd\t2.2-1.x86_64\ncrun\t1.26-1.x86_64\n")

	for _, tc := range []struct {
		name, inventory, want string
	}{
		{"empty", `{"schemaVersion":1,"artifactKind":"katl.release-extension-inventory.v1","version":"2026.9.0","architecture":"x86_64","flavour":"standard","runtimeInterface":"katl-runtime-1","kernelRelease":"6.19","runtimeSHA256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","extensions":[]}`, "inventory is empty"},
		{"wrong flavour", `{"schemaVersion":1,"artifactKind":"katl.release-extension-inventory.v1","version":"2026.9.0","architecture":"x86_64","flavour":"lts","runtimeInterface":"katl-runtime-1","kernelRelease":"6.19","runtimeSHA256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","extensions":[{"name":"drbd9","payloadVersion":"9.3.4","repository":"registry.invalid/drbd9","reference":"registry.invalid/drbd9@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]}`, "does not match"},
		{"unsafe version", `{"schemaVersion":1,"artifactKind":"katl.release-extension-inventory.v1","version":"2026.9.0","architecture":"x86_64","flavour":"standard","runtimeInterface":"katl-runtime-1","kernelRelease":"6.19","runtimeSHA256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","extensions":[{"name":"drbd9","payloadVersion":"9.3|4","repository":"registry.invalid/drbd9","reference":"registry.invalid/drbd9@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]}`, "invalid extension payloadVersion"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			put("katl-runtime.extensions.json", tc.inventory)
			before, _ := os.ReadFile(filepath.Join(dir, "RELEASE_NOTES.md"))
			err := writeReleaseComponents(dir, []string{"standard"})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %s", err, tc.want)
			}
			after, _ := os.ReadFile(filepath.Join(dir, "RELEASE_NOTES.md"))
			if string(before) != string(after) {
				t.Fatal("failed validation changed notes")
			}
		})
	}
}
