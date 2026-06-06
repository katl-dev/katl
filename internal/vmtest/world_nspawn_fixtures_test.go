package vmtest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorldFixturesPublishNspawnUserspaceRoot(t *testing.T) {
	world := testWorld(t)
	scenario := world.NewScenario(t, "nspawn root")
	source := writeNspawnRoot(t, filepath.Join(t.TempDir(), "source-root"), "a")

	fixture, err := scenario.NspawnUserspaceRoot(source)
	if err != nil {
		t.Fatalf("NspawnUserspaceRoot() error = %v", err)
	}
	if fixture.Root != filepath.Join(scenario.Dir, "nspawn", "root") {
		t.Fatalf("root = %q", fixture.Root)
	}
	if _, err := os.Stat(filepath.Join(fixture.Root, "usr/lib/katl/runtime/katl-runtime-status")); err != nil {
		t.Fatalf("published helper missing: %v", err)
	}

	var manifest nspawnUserspaceFixtureRecord
	readJSONForTest(t, fixture.ManifestPath, &manifest)
	if manifest.APIVersion != WorldAPIVersion || manifest.Kind != "NspawnUserspaceFixture" {
		t.Fatalf("manifest envelope = %#v", manifest)
	}
	if manifest.Root != fixture.Root || manifest.Source.Kind != "directory" || manifest.Source.TreeSHA256 == "" {
		t.Fatalf("manifest source = %#v", manifest)
	}
	if len(manifest.Checks) != 7 {
		t.Fatalf("checks = %#v", manifest.Checks)
	}

	scenarioManifest := readScenarioManifest(t, scenario.ManifestPath)
	if !hasFixtureKind(scenarioManifest.Fixtures, FixtureNspawnUserspaceRoot) {
		t.Fatalf("scenario fixtures = %#v", scenarioManifest.Fixtures)
	}
}

func TestWorldFixturesRejectStaleNspawnUserspaceRoot(t *testing.T) {
	world := testWorld(t)
	scenario := world.NewScenario(t, "stale nspawn")
	first := writeNspawnRoot(t, filepath.Join(t.TempDir(), "source-a"), "a")
	second := writeNspawnRoot(t, filepath.Join(t.TempDir(), "source-b"), "b")

	if _, err := scenario.NspawnUserspaceRoot(first); err != nil {
		t.Fatalf("NspawnUserspaceRoot(first) error = %v", err)
	}
	if _, err := scenario.NspawnUserspaceRoot(second); err == nil || !strings.Contains(err.Error(), "cached nspawn userspace root") {
		t.Fatalf("NspawnUserspaceRoot(second) error = %v, want stale cache rejection", err)
	}
}

func TestWorldFixturesStageNspawnImageAndBindWorkspace(t *testing.T) {
	world := testWorld(t)
	scenario := world.NewScenario(t, "nspawn image")
	image := writeFixtureFile(t, filepath.Join(t.TempDir(), "root.squashfs"), "image-a")

	fixture, err := scenario.NspawnUserspaceImage(image)
	if err != nil {
		t.Fatalf("NspawnUserspaceImage() error = %v", err)
	}
	if fixture.Image != filepath.Join(scenario.Dir, "nspawn", "root.squashfs") {
		t.Fatalf("image = %q", fixture.Image)
	}
	var manifest nspawnUserspaceFixtureRecord
	readJSONForTest(t, fixture.ManifestPath, &manifest)
	if manifest.Image != fixture.Image || manifest.Source.Kind != "image" || manifest.Source.SHA256 == "" {
		t.Fatalf("image manifest = %#v", manifest)
	}

	workspace, err := scenario.BindWorkspace("runtime fixture", "/mnt/katl-runtime-fixture")
	if err != nil {
		t.Fatalf("BindWorkspace() error = %v", err)
	}
	if workspace.Source != filepath.Join(scenario.Dir, "binds", "runtime-fixture") || workspace.Target != "/mnt/katl-runtime-fixture" {
		t.Fatalf("workspace = %#v", workspace)
	}
	if _, err := scenario.BindWorkspace("bad", "relative/path"); err == nil || !strings.Contains(err.Error(), "must be absolute") {
		t.Fatalf("BindWorkspace(relative) error = %v, want target rejection", err)
	}
	scenarioManifest := readScenarioManifest(t, scenario.ManifestPath)
	if !hasFixtureKind(scenarioManifest.Fixtures, FixtureNspawnUserspaceImage) || !hasFixtureKind(scenarioManifest.Fixtures, FixtureBindWorkspace) {
		t.Fatalf("scenario fixtures = %#v", scenarioManifest.Fixtures)
	}
}

func writeNspawnRoot(t *testing.T, root string, suffix string) string {
	t.Helper()
	for _, path := range []string{
		"usr/bin/sh",
		"usr/bin/cp",
		"usr/bin/grep",
		"usr/bin/mktemp",
		"usr/bin/systemd-analyze",
		"usr/lib/katl/runtime/katl-generation-activate",
		"usr/lib/katl/runtime/katl-runtime-status",
	} {
		writeNspawnExecutable(t, filepath.Join(root, path), suffix)
	}
	if err := os.MkdirAll(filepath.Join(root, "usr/local/bin"), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.Symlink("../../usr/bin/sh", filepath.Join(root, "usr/local/bin/sh-link")); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}
	return root
}

func writeNspawnExecutable(t *testing.T, path string, suffix string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	content := "#!/bin/sh\nprintf '%s\\n' " + suffix + "\n"
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
}
