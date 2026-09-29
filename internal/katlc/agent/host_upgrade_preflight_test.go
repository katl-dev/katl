package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadHostUpgradeConfigurationBindsHandoffInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "configuration.yaml")
	document := "apiVersion: katl.dev/v1alpha1\n"
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := readHostUpgradeConfiguration(path, hostUpgradeConfigurationDigest(document))
	if err != nil || got != document {
		t.Fatalf("readHostUpgradeConfiguration() = %q, %v", got, err)
	}
	if _, err := readHostUpgradeConfiguration(path, strings.Repeat("a", 64)); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatched configuration error = %v", err)
	}
	if _, err := readHostUpgradeConfiguration("", hostUpgradeConfigurationDigest(document)); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("missing configuration error = %v", err)
	}
}
