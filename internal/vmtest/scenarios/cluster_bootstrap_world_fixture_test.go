package scenarios

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zariel/katl/internal/vmtest"
)

func publishedRuntimeBuildRoots(world vmtest.World, repo string) []string {
	return []string{
		filepath.Join(world.RunDir, "build"),
	}
}

func ensurePublishedRuntimeFixturesForWorld(world vmtest.World, repo string, specs []vmtest.NodeSpec, kvm vmtest.KVMPolicy) error {
	timeout := time.Duration(firstInt(len(specs), 1)) * 30 * time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return vmtest.EnsurePublishedFirstInstallRuntimeFixtures(ctx, world, repo, specs, vmtest.FirstInstallRuntimeFixtureOptions{
		Input: vmtest.DefaultFirstInstallWorldInputFromEnv(vmtest.FirstInstallWorldPreseed, katlctlEnvBool("KATL_FIRST_INSTALL_USE_INSTALLED_ESP")),
		KVM:   kvm,
	})
}

func failWorldFixtureSetup(t *testing.T, world vmtest.World, scenarioName string, err error) {
	t.Helper()
	scenario, scenarioErr := world.PlanScenario(scenarioName)
	if scenarioErr != nil {
		t.Fatalf("plan world setup failure scenario: %v; original error: %v", scenarioErr, err)
	}
	failTwoNodeWorldSetup(t, scenario, err)
}

func katlctlEnvBool(name string) bool {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return false
	}
	parsed, err := strconv.ParseBool(value)
	return err == nil && parsed
}

func firstInt(values ...int) int {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
}

func writeKatlctlPublishedInstalledRuntimeFixture(t *testing.T, root, name, nodeName string, role vmtest.NodeRole) string {
	t.Helper()
	dir := filepath.Join(root, "build", "published", name)
	disk := writeKatlctlFixtureFile(t, filepath.Join(dir, "installed-runtime.raw"), "disk-"+name)
	esp := writeKatlctlFixtureESP(t, filepath.Join(dir, "esp"))
	metadata := writeKatlctlFixtureNodeMetadata(t, filepath.Join(dir, "node.json"), nodeName, role)
	fixtureManifest := writeKatlctlInstalledRuntimeFixtureManifest(t, dir, nodeName, role, disk, esp, metadata)

	publishedManifest := filepath.Join(dir, "published-first-install-runtime-fixture.json")
	content := map[string]string{
		"apiVersion":      vmtest.WorldAPIVersion,
		"kind":            "PublishedFirstInstallRuntimeFixture",
		"nodeName":        nodeName,
		"systemRole":      string(role),
		"fixtureManifest": "installed-runtime-fixture.json",
		"diskFormat":      string(vmtest.DiskRaw),
	}
	writeKatlctlJSONFile(t, publishedManifest, content)
	return fixtureManifest
}

func writeKatlctlInstalledRuntimeFixtureManifest(t *testing.T, dir, nodeName string, role vmtest.NodeRole, disk, esp, metadata string) string {
	t.Helper()
	manifest := filepath.Join(dir, "installed-runtime-fixture.json")
	record := map[string]any{
		"apiVersion": vmtest.WorldAPIVersion,
		"kind":       "InstalledRuntimeVMTestFixture",
		"nodeName":   nodeName,
		"systemRole": string(role),
		"disk": map[string]string{
			"path":   "installed-runtime.raw",
			"format": string(vmtest.DiskRaw),
			"sha256": mustKatlctlFileSHA256(t, disk),
		},
		"espArtifacts": map[string]string{
			"path":       "esp",
			"treeSHA256": mustKatlctlTreeSHA256(t, esp),
		},
		"nodeMetadata": map[string]string{
			"path":   "node.json",
			"sha256": mustKatlctlFileSHA256(t, metadata),
		},
	}
	writeKatlctlJSONFile(t, manifest, record)
	return manifest
}

func writeKatlctlFixtureFile(t *testing.T, path string, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
	return path
}

func writeKatlctlFixtureESP(t *testing.T, path string) string {
	t.Helper()
	writeKatlctlFixtureFile(t, filepath.Join(path, "loader", "entries", "katl.conf"), "title Katl\noptions root=PARTUUID=11111111-2222-3333-4444-555555555555 rootfstype=squashfs katl.generation=2026.06.06 systemd.machine_id=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa ro\n")
	return path
}

func writeKatlctlFixtureNodeMetadata(t *testing.T, path, nodeName string, role vmtest.NodeRole) string {
	t.Helper()
	content := `{"apiVersion":"katl.dev/v1alpha1","kind":"NodeMetadata","identity":{"hostname":"` + nodeName + `"},"systemRole":"` + string(role) + `"}`
	return writeKatlctlFixtureFile(t, path, content)
}

func writeKatlctlJSONFile(t *testing.T, path string, value any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", filepath.Dir(path), err)
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent() error = %v", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, err)
	}
	if string(data) != want {
		t.Fatalf("%s content = %q, want %q", path, data, want)
	}
}

func mustKatlctlFileSHA256(t *testing.T, path string) string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("Open(%s) error = %v", path, err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		t.Fatalf("hash %s: %v", path, err)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func mustKatlctlTreeSHA256(t *testing.T, root string) string {
	t.Helper()
	hash := sha256.New()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		mode := fmt.Sprintf("%o", info.Mode().Perm())
		if entry.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("unsupported non-regular entry: %s", rel)
		}
		if entry.IsDir() {
			_, _ = fmt.Fprintf(hash, "dir %s %s\n", mode, rel)
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("unsupported non-regular entry: %s", rel)
		}
		fileSHA := mustKatlctlFileSHA256(t, path)
		_, _ = fmt.Fprintf(hash, "file %s %s %s\n", mode, fileSHA, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("hash ESP tree %s: %v", root, err)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func hasPathPrefix(path, prefix string) bool {
	rel, err := filepath.Rel(prefix, path)
	return err == nil && rel != "." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != ".."
}
