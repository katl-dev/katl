package katlosimage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpgradePreflightVerifiesRetainedAssets(t *testing.T) {
	for _, kind := range []string{"sysext", "confext"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			previous, _ := knownGoodGeneration(t, "gen0", sha256Bytes([]byte("kubernetes sysext")), "v1.35.0")
			previous, status := writePreservedGenerationAssets(t, root, previous)
			plan, err := upgradePayload(t, nil).HostUpgradePlan(validHostUpgradeRequest(previous, status))
			if err != nil {
				t.Fatal(err)
			}
			if err := ValidateUpgradeAssets(root, plan); err != nil {
				t.Fatal(err)
			}

			found := false
			for _, asset := range plan.PreservedAssets {
				if asset.Kind != kind {
					continue
				}
				found = true
				path := filepath.Join(root, asset.SourcePath)
				if asset.Directory {
					path = filepath.Join(path, "unexpected-file")
				}
				if err := os.WriteFile(path, []byte("changed after generation commit"), 0o600); err != nil {
					t.Fatal(err)
				}
				break
			}
			if !found {
				t.Fatalf("fixture has no %s asset", kind)
			}

			if err := ValidateUpgradeAssets(root, plan); err == nil || !strings.Contains(err.Error(), "SHA-256 mismatch") {
				t.Fatalf("corrupt retained %s: %v", kind, err)
			}
			if _, err := os.Stat(filepath.Join(root, "var/lib/katl/generations", plan.Spec.GenerationID)); !os.IsNotExist(err) {
				t.Fatalf("preflight wrote candidate state: %v", err)
			}
		})
	}
}
