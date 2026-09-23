package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublishFlavour(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"katlos-upgrade-2026.9.0-x86_64.squashfs":        "image",
		"katlos-upgrade-2026.9.0-x86_64.squashfs.json":   `{"flavour":"lts","path":"katlos-upgrade-2026.9.0-x86_64.squashfs","checksumPath":"katlos-upgrade-2026.9.0-x86_64.squashfs.sha256","version":"2026.9.0"}`,
		"katlos-upgrade-2026.9.0-x86_64.squashfs.sha256": "abc  katlos-upgrade-2026.9.0-x86_64.squashfs\n",
		"katl-installer.vmlinuz":                         "kernel",
		"katlctl-2026.9.0-linux-amd64":                   "cli",
		"katl-runtime.packages.tsv":                      "kernel-longterm-core\t6.18\n",
		"katl-runtime.extensions.json":                   `{"flavour":"lts","version":"2026.9.0"}`,
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := publishFlavour(dir, "lts"); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"katlos-lts-upgrade-2026.9.0-x86_64.squashfs":        "image",
		"katlos-lts-upgrade-2026.9.0-x86_64.squashfs.sha256": "abc  katlos-lts-upgrade-2026.9.0-x86_64.squashfs\n",
		"katl-installer-lts.vmlinuz":                         "kernel",
		"katlctl-2026.9.0-linux-amd64":                       "cli",
		"katl-runtime-lts.packages.tsv":                      "kernel-longterm-core\t6.18\n",
		"katl-runtime-lts.extensions.json":                   "{\n  \"flavour\": \"lts\",\n  \"version\": \"2026.9.0\"\n}\n",
	} {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(got) != want {
			t.Fatalf("%s = %q, %v", name, got, err)
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, "katlos-lts-upgrade-2026.9.0-x86_64.squashfs.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"path": "katlos-lts-upgrade-2026.9.0-x86_64.squashfs"`) || !strings.Contains(string(data), `"version": "2026.9.0"`) {
		t.Fatalf("metadata = %s", data)
	}
}

func TestStandardMetadataRetainsUpgradeCompatibility(t *testing.T) {
	cfg, err := configFromEnv(map[string]string{"KATL_FLAVOUR": "standard"}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Existing agents reject unknown JSON fields; the standard image must omit
	// the new field so it can introduce flavour-aware agents on existing nodes.
	data, err := json.Marshal(katlosIndex{Flavour: cfg.Flavour})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"flavour"`) {
		t.Fatalf("standard index adds a field old agents reject: %s", data)
	}
}

func TestInstallerPair(t *testing.T) {
	dir := t.TempDir()
	installer, image := filepath.Join(dir, "installer.json"), filepath.Join(dir, "image.json")
	if err := os.WriteFile(installer, []byte(`{"version":"2026.9.0","architecture":"x86_64","flavour":"lts"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, data string
		valid      bool
	}{
		{"matching", `{"version":"2026.9.0","architecture":"x86_64","flavour":"lts"}`, true},
		{"wrong flavour", `{"version":"2026.9.0","architecture":"x86_64"}`, false},
		{"wrong version", `{"version":"2026.9.1","architecture":"x86_64","flavour":"lts"}`, false},
		{"unknown flavour", `{"version":"2026.9.0","architecture":"x86_64","flavour":"nightly"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(image, []byte(tc.data), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := verifyInstallerPair(installer, image); (err == nil) != tc.valid {
				t.Fatalf("error = %v", err)
			}
		})
	}
}
