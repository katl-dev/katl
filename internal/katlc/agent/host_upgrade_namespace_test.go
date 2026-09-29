package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/generation"
)

func TestPublishPreparedUpgradeIsCompleteAtVisibilityBoundary(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "var/lib/katl/artifacts/host-upgrade/prepare-test")
	privateRoot := filepath.Join(work, "state")
	writeResetGenerationZero(t, privateRoot)
	if err := os.MkdirAll(filepath.Join(root, "var/lib/katl/generations"), 0o700); err != nil {
		t.Fatal(err)
	}
	digest, err := generation.DigestDirectory(filepath.Join(privateRoot, "var/lib/katl/generations/0"))
	if err != nil {
		t.Fatal(err)
	}

	if err := publishPreparedUpgrade(root, preparedUpgrade{work: work, root: privateRoot, result: preparedUpgradeResult{CandidateSHA256: digest}}, "0"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := generation.ReadGeneration(root, "0"); err != nil {
		t.Fatalf("published generation is incomplete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(work, "publish", "0")); !os.IsNotExist(err) {
		t.Fatalf("staging directory remains visible: %v", err)
	}
}

func TestSnapshotUpgradeSourceCopiesOnlySelectedGenerationInputs(t *testing.T) {
	root := t.TempDir()
	writeResetGenerationZero(t, root)
	unrelated := filepath.Join(root, "var/lib/katl/generations/unrelated")
	if err := os.MkdirAll(unrelated, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unrelated, "large-evidence"), []byte("unrelated"), 0o600); err != nil {
		t.Fatal(err)
	}
	operationDir := filepath.Join(root, "var/lib/katl/operations/old-operation")
	if err := os.MkdirAll(operationDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(operationDir, "record.json"), []byte("evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "state")
	handoff := generation.UpgradeHandoff{
		Version: generation.UpgradeHandoffVersion, OperationID: "upgrade-1",
		SourceGenerationID: "0", CandidateGenerationID: "1",
		ImageSHA256: strings.Repeat("a", 64), ImageSizeBytes: 1024,
		RootSlot: "root-b", RootPartitionUUID: "part-b", UKIPath: "/EFI/Linux/katl-root-b-1.efi",
		LoaderEntryPath: "loader/entries/katl-1.conf", CreatedAt: time.Now().UTC(),
	}

	if err := snapshotUpgradeSource(root, target, handoff); err != nil {
		t.Fatal(err)
	}
	if _, _, err := generation.ReadGeneration(target, "0"); err != nil {
		t.Fatalf("selected generation is incomplete: %v", err)
	}
	for _, path := range []string{"var/lib/katl/generations/unrelated", "var/lib/katl/operations"} {
		if _, err := os.Stat(filepath.Join(target, path)); !os.IsNotExist(err) {
			t.Fatalf("snapshot retained unrelated input %s: %v", path, err)
		}
	}
}

func TestCleanupHostUpgradeStorageKeepsRecentUploadAndDurableEvidence(t *testing.T) {
	root := t.TempDir()
	base := hostUpgradeArtifactRoot(root)
	now := time.Now().UTC()
	paths := map[string]string{
		"recent":    filepath.Join(base, "uploads", strings.Repeat("a", 64)+".squashfs"),
		"old":       filepath.Join(base, "uploads", strings.Repeat("b", 64)+".squashfs"),
		"partial":   filepath.Join(base, "uploads", ".upload.partial"),
		"scratch":   filepath.Join(base, "prepare-crash", "state"),
		"operation": filepath.Join(root, "var/lib/katl/operations/upgrade-1/record.json"),
	}
	for _, path := range paths {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("data"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := now.Add(-2 * hostUpgradeArtifactRetention)
	if err := os.Chtimes(paths["old"], old, old); err != nil {
		t.Fatal(err)
	}

	if err := cleanupHostUpgradeStorage(context.Background(), root, "", nil, now); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"old", "partial", "scratch"} {
		if _, err := os.Stat(paths[name]); !os.IsNotExist(err) {
			t.Fatalf("temporary %s remains: %v", name, err)
		}
	}
	for _, name := range []string{"recent", "operation"} {
		if _, err := os.Stat(paths[name]); err != nil {
			t.Fatalf("retained %s was removed: %v", name, err)
		}
	}
}

func TestPublishPreparedUpgradeRejectsIncompleteCandidate(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "var/lib/katl/artifacts/host-upgrade/prepare-test")
	privateRoot := filepath.Join(work, "state")
	writeResetGenerationZero(t, privateRoot)
	if err := os.Remove(filepath.Join(privateRoot, "var/lib/katl/generations/0/status.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "var/lib/katl/generations"), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := publishPreparedUpgrade(root, preparedUpgrade{work: work, root: privateRoot}, "0"); err == nil {
		t.Fatal("incomplete candidate was published")
	}
	if _, err := os.Stat(filepath.Join(root, "var/lib/katl/generations/0")); !os.IsNotExist(err) {
		t.Fatalf("partial candidate appeared in live generation directory: %v", err)
	}
}

func TestCopyUpgradeTreePreservesConfextDigest(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	target := filepath.Join(root, "target")
	if err := os.MkdirAll(filepath.Join(source, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "etc", "hostname"), []byte("cp-1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	want, err := generation.DigestDirectory(source)
	if err != nil {
		t.Fatal(err)
	}

	if err := copyUpgradeTree(source, target); err != nil {
		t.Fatal(err)
	}
	got, err := generation.DigestDirectory(target)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("copied confext digest = %s, want %s", got, want)
	}
	for _, dir := range []string{target, filepath.Join(target, "etc")} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o755 {
			t.Fatalf("copied confext directory %s mode = %04o, want 0755", dir, info.Mode().Perm())
		}
	}
}
