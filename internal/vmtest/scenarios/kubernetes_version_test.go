package scenarios

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/katl-dev/katl/internal/vmtest"
)

func TestScenarioKubernetesVersion(t *testing.T) {
	repo := t.TempDir()
	mkosi := filepath.Join(repo, "_build/mkosi")
	t.Setenv(vmtest.WorldManifestEnv, "")
	if err := os.MkdirAll(mkosi, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, version := range map[string]string{
		"katl-kubernetes.raw.json":                "v1.36.2",
		"katl-kubernetes-upgrade-source.raw.json": "v1.36.0",
	} {
		if err := os.WriteFile(filepath.Join(mkosi, name), []byte(`{"payloadVersion":"`+version+`"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, key := range []string{"KATL_KUBERNETES_VERSION", "KATL_KUBERNETES_PAYLOAD_VERSION", "KATL_KUBERNETES_SYSEXT_METADATA", "KATL_VMTEST_KUBERNETES_BUNDLE"} {
		t.Setenv(key, "")
	}

	if got := operationBackedKubernetesVersion(t, repo, false); got != "v1.36.2" {
		t.Fatalf("default scenario version = %q, want the default payload v1.36.2", got)
	}
	if got := operationBackedKubernetesVersion(t, repo, true); got != "v1.36.0" {
		t.Fatalf("upgrade scenario version = %q, want the upgrade source v1.36.0", got)
	}

	// Fixture intent follows the scenario, so a bootstrap request for the
	// scenario's version matches what the installed nodes recorded.
	if got := fixtureKubernetesVersion("v1.36.0"); got != "v1.36.0" {
		t.Fatalf("fixture version = %q, want the scenario version v1.36.0", got)
	}
	t.Setenv("KATL_KUBERNETES_VERSION", "v1.36.5")
	if got := fixtureKubernetesVersion("v1.36.0"); got != "v1.36.5" {
		t.Fatalf("fixture version with override = %q, want v1.36.5", got)
	}
}
