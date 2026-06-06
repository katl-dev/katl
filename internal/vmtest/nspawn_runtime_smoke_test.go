package vmtest

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zariel/katl/internal/installer/confext"
	"github.com/zariel/katl/internal/installer/generation"
	"github.com/zariel/katl/internal/nspawntest"
)

func TestRuntimeUserspaceNspawnSmoke(t *testing.T) {
	fixture := runtimeUserspaceFixture(t)
	if worldRun, ok := nspawnWorldRunFor(t, "runtime userspace smoke"); ok {
		workspace, err := worldRun.Scenario.BindWorkspaceFromRoot("runtime fixture", "/mnt/katl-runtime-fixture", fixture.Root)
		if err != nil {
			failWorldSetup(t, worldRun.Scenario, err)
		}
		worldRun.Runner.Run(t, nspawntest.Scenario{
			Name: "runtime userspace smoke",
			Binds: []nspawntest.Bind{{
				Source: workspace.Source,
				Target: workspace.Target,
			}},
			Commands: runtimeUserspaceCommands(fixture.GenerationID),
		})
		return
	}
	options := nspawntest.DefaultOptions()
	if !options.Enabled {
		t.Skip("run nspawn runtime smoke through scripts/vmtest-run")
	}
	_ = RequireWorld(t)
}

func runtimeUserspaceCommands(generationID string) []nspawntest.Command {
	return []nspawntest.Command{
		{
			Name: "runtime helper execution",
			Argv: []string{"sh", "-ceu", runtimeHelperScript(generationID)},
		},
		{
			Name: "optional Kubernetes tools",
			Argv: []string{"sh", "-ceu", optionalKubernetesToolsScript},
		},
		{
			Name: "generation metadata inspection",
			Argv: []string{"sh", "-ceu", metadataInspectionScript(generationID)},
		},
	}
}

type runtimeFixture struct {
	Root         string
	GenerationID string
}

func runtimeUserspaceFixture(t *testing.T) runtimeFixture {
	t.Helper()
	root := t.TempDir()
	generationID := "2026.06.06-001"
	generationsRoot := filepath.Join(root, "var/lib/katl/generations")
	tree, err := confext.RenderGenerationTree(confext.GenerationTreeRequest{
		GenerationsRoot: generationsRoot,
		GenerationID:    generationID,
		Files: []confext.NativeEtcFile{{
			Path:    "/etc/systemd/network/10-smoke.network",
			Content: "[Match]\nName=*\n[Network]\nDHCP=yes\n",
		}},
		Extension: confext.ExtensionRelease{
			Name:         "katl-node",
			ID:           "katl",
			VersionID:    "0.1.0",
			ConfextLevel: 1,
		},
		Chown: func(string, int, int) error { return nil },
	})
	if err != nil {
		t.Fatalf("RenderGenerationTree() error = %v", err)
	}
	confextSHA, err := generation.DigestDirectory(tree.ConfextDir)
	if err != nil {
		t.Fatalf("DigestDirectory() error = %v", err)
	}
	sysextPath := filepath.Join(generationsRoot, generationID, "sysext", "kubernetes.raw")
	sysextContent := []byte("katl test Kubernetes sysext placeholder\n")
	if err := os.MkdirAll(filepath.Dir(sysextPath), 0o755); err != nil {
		t.Fatalf("create sysext dir: %v", err)
	}
	if err := os.WriteFile(sysextPath, sysextContent, 0o644); err != nil {
		t.Fatalf("write sysext fixture: %v", err)
	}
	sysextSHA := sha256.Sum256(sysextContent)
	record, err := generation.NewFirstInstallRecord(generation.FirstInstallRequest{
		GenerationID:          generationID,
		RuntimeVersion:        "0.1.0",
		RuntimeInterface:      "katl-runtime-1",
		RuntimeArchitecture:   "x86_64",
		RootSlot:              "root-a",
		RootPartitionUUID:     "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		RuntimeArtifactSHA256: strings.Repeat("a", 64),
		UKIPath:               "/EFI/Linux/katl.efi",
		Sysexts: []generation.ExtensionRef{{
			Name:            "kubernetes",
			Path:            "/var/lib/katl/generations/" + generationID + "/sysext/kubernetes.raw",
			ActivationPath:  "/run/extensions/kubernetes",
			SHA256:          hex.EncodeToString(sysextSHA[:]),
			ArtifactVersion: "0.1.0",
			PayloadVersion:  "v1.36.0",
			Architecture:    "x86_64",
			Compatibility: generation.ExtensionCompatibility{
				RuntimeInterfaces: []string{"katl-runtime-1"},
			},
		}},
		GeneratedConfext: generation.GeneratedConfext{
			Name:           "katl-node",
			Path:           "/var/lib/katl/generations/" + generationID + "/confext",
			ActivationPath: "/run/confexts/katl-node",
			SHA256:         confextSHA,
			Compatibility: generation.ConfextCompatibility{
				ID:           "katl",
				VersionID:    "0.1.0",
				ConfextLevel: 1,
			},
		},
		KernelCommandLine: []string{"katl.generation=" + generationID},
		CreatedAt:         time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("NewFirstInstallRecord() error = %v", err)
	}
	metadataPath, err := generation.MetadataPath(root, generationID)
	if err != nil {
		t.Fatalf("MetadataPath() error = %v", err)
	}
	if err := generation.WriteRecord(metadataPath, record); err != nil {
		t.Fatalf("WriteRecord() error = %v", err)
	}
	return runtimeFixture{Root: root, GenerationID: generationID}
}

func runtimeHelperScript(generationID string) string {
	return strings.ReplaceAll(`set -eu
test -x /usr/lib/katl/runtime/katl-generation-activate
test -x /usr/lib/katl/runtime/katl-runtime-status
work="$(mktemp -d)"
cp -a /mnt/katl-runtime-fixture/. "$work/"
/usr/lib/katl/runtime/katl-generation-activate --root="$work" --generation="GENERATION_ID"
test -L "$work/run/extensions/kubernetes"
test -L "$work/run/confexts/katl-node"
mkdir -p "$work/var/lib/katl/install"
cat > "$work/var/lib/katl/install/status.json" <<'JSON'
{
  "apiVersion": "katl.dev/v1alpha1",
  "kind": "InstallToBootstrapStatus",
  "state": "kubeadm-ready",
  "inputMode": "test",
  "inputSource": "nspawn-runtime-smoke",
  "requestDigest": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "katlosImage": {
    "sha256": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
    "version": "0.1.0",
    "architecture": "x86_64",
    "runtimeInterface": "katl-runtime-1",
    "role": "install"
  },
  "targetDiskStableID": "disk/by-id/test",
  "selectedRootSlot": "root-a",
  "installedGeneration": "GENERATION_ID",
  "updatedAt": "2026-06-06T12:00:00Z"
}
JSON
/usr/lib/katl/runtime/katl-runtime-status --root="$work" | tee "$work/status.out"
grep -F waiting-for-cluster-bootstrap "$work/status.out"
grep -F waiting-for-cluster-bootstrap "$work/var/lib/katl/install/status.json"
`, "GENERATION_ID", generationID)
}

const optionalKubernetesToolsScript = `set -eu
for tool in kubeadm kubelet kubectl containerd crictl; do
	if command -v "$tool" >/dev/null 2>&1; then
		"$tool" --version >/dev/null 2>&1 ||
			"$tool" version --client >/dev/null 2>&1 ||
			"$tool" --help >/dev/null 2>&1
	fi
done
`

func metadataInspectionScript(generationID string) string {
	return strings.ReplaceAll(`set -eu
root=/mnt/katl-runtime-fixture
metadata="$root/var/lib/katl/generations/GENERATION_ID/metadata.json"
test -f "$metadata"
grep -F '"generationID": "GENERATION_ID"' "$metadata"
grep -F '"payloadVersion": "v1.36.0"' "$metadata"
grep -F '"runtimeInterfaces": [' "$metadata"
test -f "$root/var/lib/katl/generations/GENERATION_ID/sysext/kubernetes.raw"
test -d "$root/var/lib/katl/generations/GENERATION_ID/confext"
test -f "$root/var/lib/katl/generations/GENERATION_ID/confext/etc/extension-release.d/extension-release.katl-node"
grep -F 'CONFEXT_LEVEL=1' "$root/var/lib/katl/generations/GENERATION_ID/confext/etc/extension-release.d/extension-release.katl-node"
test -f "$root/var/lib/katl/generations/GENERATION_ID/confext/etc/systemd/network/10-smoke.network"
`, "GENERATION_ID", generationID)
}
