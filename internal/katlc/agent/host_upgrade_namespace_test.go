package agent

import (
	"os"
	"path/filepath"
	"testing"

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
