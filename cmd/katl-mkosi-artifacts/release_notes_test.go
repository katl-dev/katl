package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const changesFixture = "## Changes\n\nKeep these changes.\n\n## Verify\n\nVerify assets.\n\n## Support\n\nRead SUPPORT.md.\n"

func TestReleaseComponentsRendersTables(t *testing.T) {
	dir := releaseNotesFixture(t)

	if err := writeReleaseComponents(dir, []string{"standard", "lts"}); err != nil {
		t.Fatal(err)
	}
	notes := string(readFile(t, filepath.Join(dir, "RELEASE_NOTES.md")))

	wantKernels := "| Package | Standard | LTS |\n" +
		"| --- | --- | --- |\n" +
		"| Kernel | `6.19-2.x86_64` | `6.18-2.x86_64` |"
	wantPackages := "| Package | Version |\n" +
		"| --- | --- |\n" +
		"| systemd | `259-2.x86_64` |\n" +
		"| containerd | `2.2-1.x86_64` |\n" +
		"| crun | `1.26-1.x86_64` |"
	wantExtensions := "| Extension | Version |\n" +
		"| --- | --- |\n" +
		"| alpha | `1.2.3` |\n" +
		"| drbd9 | `9.3.4` |"
	for _, tc := range []struct{ name, header, want string }{
		{"kernels", "| Package | Standard | LTS |", wantKernels},
		{"packages", "| Package | Version |", wantPackages},
		{"extensions", "| Extension | Version |", wantExtensions},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := markdownTable(notes, tc.header); got != tc.want {
				t.Fatalf("table =\n%s\nwant:\n%s", got, tc.want)
			}
		})
	}
	if !strings.HasPrefix(notes, "## Changes\n") || !strings.Contains(notes, changesFixture[:strings.Index(changesFixture, "## Verify")]) {
		t.Fatalf("release notes did not preserve change notes:\n%s", notes)
	}
	if !(strings.Index(notes, "## Changes") < strings.Index(notes, "## Packages") && strings.Index(notes, "## Packages") < strings.Index(notes, "## Verify") && strings.Index(notes, "## Verify") < strings.Index(notes, "## Support")) {
		t.Fatalf("release note sections are out of order:\n%s", notes)
	}
}

func TestReleaseComponentsIsIdempotent(t *testing.T) {
	dir := releaseNotesFixture(t)

	if err := writeReleaseComponents(dir, []string{"lts"}); err != nil {
		t.Fatal(err)
	}
	if err := writeReleaseComponents(dir, []string{"standard", "lts"}); err != nil {
		t.Fatal(err)
	}
	first := readFile(t, filepath.Join(dir, "RELEASE_NOTES.md"))
	if err := writeReleaseComponents(dir, []string{"standard", "lts"}); err != nil {
		t.Fatal(err)
	}
	second := readFile(t, filepath.Join(dir, "RELEASE_NOTES.md"))

	if string(second) != string(first) {
		t.Fatalf("second render changed release notes:\n%s", second)
	}
	if strings.Count(string(second), componentsHeading) != 1 {
		t.Fatalf("generated section count = %d, want 1", strings.Count(string(second), componentsHeading))
	}
}

func TestReleaseComponentsRejectsInvalidPackageInventory(t *testing.T) {
	for _, tc := range []struct{ name, inventory, want string }{
		{"missing", "systemd\t259\n", "exactly one kernel-core"},
		{"ambiguous", "kernel-core\t6.19\nkernel-core\t6.20\nsystemd\t259\n", "found 2"},
		{"malformed", "kernel-core 6.19\n", "tab"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := releaseNotesFixture(t)
			writeFile(t, filepath.Join(dir, "katl-installer.packages.tsv"), tc.inventory)

			assertReleaseNotesUnchangedOnError(t, dir, tc.want)
		})
	}
}

func TestReleaseComponentsRejectsDifferentSharedVersions(t *testing.T) {
	dir := releaseNotesFixture(t)
	writeFile(t, filepath.Join(dir, "katl-runtime-lts.packages.tsv"), "kernel-longterm-core\t6.18-2.x86_64\nsystemd\t259-2.x86_64\ncontainerd\t2.3-1.x86_64\ncrun\t1.26-1.x86_64\n")

	assertReleaseNotesUnchangedOnError(t, dir, "containerd differs", "standard", "lts")
}

func TestReleaseComponentsRejectsDifferentImageKernels(t *testing.T) {
	dir := releaseNotesFixture(t)
	writeFile(t, filepath.Join(dir, "katl-installer.packages.tsv"), "kernel-core\t6.20-1.x86_64\nsystemd\t259-2.x86_64\n")

	assertReleaseNotesUnchangedOnError(t, dir, "installer and runtime kernel versions differ")
}

func TestReleaseComponentsRejectsDifferentExtensionVersions(t *testing.T) {
	dir := releaseNotesFixture(t)
	writeInventory(t, filepath.Join(dir, "katl-runtime-lts.extensions.json"), extensionInventory("lts", "6.18", []releaseExtensionVersion{
		extensionVersion("alpha", "1.2.3", "c"),
		extensionVersion("drbd9", "9.3.3", "d"),
	}))

	assertReleaseNotesUnchangedOnError(t, dir, "extension drbd9 differs", "standard", "lts")
}

func TestReleaseComponentsRejectsInvalidExtensionInventory(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*releaseExtensionInventory)
		want   string
	}{
		{"empty", func(inventory *releaseExtensionInventory) { inventory.Extensions = nil }, "inventory is empty"},
		{"wrong flavour", func(inventory *releaseExtensionInventory) { inventory.Flavour = "lts" }, "does not match"},
		{"unsafe version", func(inventory *releaseExtensionInventory) { inventory.Extensions[0].PayloadVersion = "1.2|3" }, "invalid extension payloadVersion"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := releaseNotesFixture(t)
			inventory := extensionInventory("standard", "6.19", []releaseExtensionVersion{
				extensionVersion("alpha", "1.2.3", "a"),
				extensionVersion("drbd9", "9.3.4", "b"),
			})
			tc.mutate(&inventory)
			writeInventory(t, filepath.Join(dir, "katl-runtime.extensions.json"), inventory)

			assertReleaseNotesUnchangedOnError(t, dir, tc.want)
		})
	}
}

func releaseNotesFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "RELEASE_NOTES.md"), changesFixture)
	writeFile(t, filepath.Join(dir, "katl-installer.packages.tsv"), "kernel-core\t6.19-2.x86_64\nsystemd\t259-2.x86_64\n")
	writeFile(t, filepath.Join(dir, "katl-runtime.packages.tsv"), "kernel-core\t0:6.19-2.x86_64\nsystemd\t0:259-2.x86_64\ncontainerd\t0:2.2-1.x86_64\ncrun\t0:1.26-1.x86_64\n")
	writeFile(t, filepath.Join(dir, "katl-installer-lts.packages.tsv"), "kernel-longterm-core\t0:6.18-2.x86_64\nsystemd\t0:259-3.x86_64\n")
	writeFile(t, filepath.Join(dir, "katl-runtime-lts.packages.tsv"), "kernel-longterm-core\t0:6.18-2.x86_64\nsystemd\t0:259-2.x86_64\ncontainerd\t0:2.2-1.x86_64\ncrun\t0:1.26-1.x86_64\n")
	writeInventory(t, filepath.Join(dir, "katl-runtime.extensions.json"), extensionInventory("standard", "6.19", []releaseExtensionVersion{
		extensionVersion("alpha", "1.2.3", "a"),
		extensionVersion("drbd9", "9.3.4", "b"),
	}))
	writeInventory(t, filepath.Join(dir, "katl-runtime-lts.extensions.json"), extensionInventory("lts", "6.18", []releaseExtensionVersion{
		extensionVersion("alpha", "1.2.3", "c"),
		extensionVersion("drbd9", "9.3.4", "d"),
	}))
	return dir
}

func extensionInventory(flavour, kernel string, extensions []releaseExtensionVersion) releaseExtensionInventory {
	return releaseExtensionInventory{
		SchemaVersion:    1,
		ArtifactKind:     releaseExtensionInventoryKind,
		Version:          "2026.9.0",
		Architecture:     "x86_64",
		Flavour:          flavour,
		RuntimeInterface: "katl-runtime-1",
		KernelRelease:    kernel,
		RuntimeSHA256:    strings.Repeat("d", 64),
		Extensions:       extensions,
	}
}

func extensionVersion(name, version, digestByte string) releaseExtensionVersion {
	repository := "registry.invalid/extensions/" + name
	return releaseExtensionVersion{
		Name:           name,
		PayloadVersion: version,
		Repository:     repository,
		Reference:      repository + "@sha256:" + strings.Repeat(digestByte, 64),
	}
}

func writeInventory(t *testing.T, path string, inventory releaseExtensionInventory) {
	t.Helper()
	data, err := json.Marshal(inventory)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, string(data))
}

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func markdownTable(notes, header string) string {
	start := strings.Index(notes, header)
	if start < 0 {
		return ""
	}
	table := notes[start:]
	if end := strings.Index(table, "\n\n"); end >= 0 {
		table = table[:end]
	}
	return table
}

func assertReleaseNotesUnchangedOnError(t *testing.T, dir, want string, flavours ...string) {
	t.Helper()
	if len(flavours) == 0 {
		flavours = []string{"standard"}
	}
	path := filepath.Join(dir, "RELEASE_NOTES.md")
	before := readFile(t, path)
	err := writeReleaseComponents(dir, flavours)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want %q", err, want)
	}
	after := readFile(t, path)
	if string(after) != string(before) {
		t.Fatal("failed validation changed notes")
	}
}
