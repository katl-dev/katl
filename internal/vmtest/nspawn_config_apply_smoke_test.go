package vmtest

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/zariel/katl/internal/nspawntest"
)

func TestConfigApplyNspawnSmoke(t *testing.T) {
	if worldRun, ok := nspawnWorldRunFor(t, "config apply smoke"); ok {
		runtimeFixture := runtimeUserspaceFixture(t)
		fixture := configApplyNspawnFixture(t)
		runtimeWorkspace, err := worldRun.Scenario.BindWorkspaceFromRoot("runtime fixture", "/mnt/katl-runtime-fixture", runtimeFixture.Root)
		if err != nil {
			failWorldSetup(t, worldRun.Scenario, err)
		}
		configWorkspace, err := worldRun.Scenario.BindWorkspaceFromRoot("config apply fixture", "/mnt/katl-config-apply-fixture", fixture.Root)
		if err != nil {
			failWorldSetup(t, worldRun.Scenario, err)
		}
		worldRun.Runner.Run(t, nspawntest.Scenario{
			Name: "config apply smoke",
			Binds: []nspawntest.Bind{
				{
					Source: runtimeWorkspace.Source,
					Target: runtimeWorkspace.Target,
				},
				{
					Source: configWorkspace.Source,
					Target: configWorkspace.Target,
				},
			},
			Commands: configApplyNspawnCommands(runtimeFixture.GenerationID),
		})
		return
	}
	options := nspawntest.DefaultOptions()
	if !options.Enabled {
		t.Skip("run nspawn config-apply smoke through scripts/vmtest-run")
	}
	_ = RequireWorld(t)
}

func configApplyNspawnCommands(generationID string) []nspawntest.Command {
	return []nspawntest.Command{{
		Name:    "config apply rejected and accepted requests",
		Argv:    []string{"sh", "-ceu", configApplyNspawnScript(generationID)},
		Timeout: 2 * time.Minute,
	}}
}

type configApplyFixture struct {
	Root string
}

func configApplyNspawnFixture(t *testing.T) configApplyFixture {
	t.Helper()
	root := t.TempDir()
	if runtime.GOOS != "linux" {
		t.Skipf("config apply nspawn helper build requires linux target, got %s", runtime.GOOS)
	}
	helper := filepath.Join(root, "configapply-smoke")
	cmd := exec.Command("go", "build", "-o", helper, "./internal/vmtest/testcmd/configapply-smoke")
	cmd.Dir = repoRoot(t)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build config apply smoke helper: %v\n%s", err, output)
	}
	writeText(t, filepath.Join(root, "current-manifest.json"), configApplyCurrentManifest)
	writeText(t, filepath.Join(root, "rejected-request.yaml"), configApplyRejectedRequest)
	writeText(t, filepath.Join(root, "accepted-request.yaml"), configApplyAcceptedRequest)
	return configApplyFixture{Root: root}
}

func configApplyNspawnScript(currentGeneration string) string {
	return strings.ReplaceAll(`set -eu
fixture=/mnt/katl-config-apply-fixture
work="$(mktemp -d)"
cp -a /mnt/katl-runtime-fixture/. "$work/"
helper="$fixture/configapply-smoke"
manifest="$fixture/current-manifest.json"
rejected_generation=2026.06.06-101
accepted_generation=2026.06.06-102

if "$helper" \
	--root="$work" \
	--current-generation="CURRENT_GENERATION" \
	--next-generation="$rejected_generation" \
	--node=cp-1 \
	--manifest="$manifest" \
	--request="$fixture/rejected-request.yaml" \
	>"$work/rejected.out" 2>"$work/rejected.err"; then
	echo "unsupported request unexpectedly succeeded" >&2
	exit 1
fi
grep -F "request rejected" "$work/rejected.err"
rejected_audit="$work/var/lib/katl/config-requests/operator/3.json"
test -f "$rejected_audit"
grep -F '"decision": "rejected"' "$rejected_audit"
grep -F '"requestedApplyMode": "live"' "$rejected_audit"
grep -F '"domain": "networkd"' "$rejected_audit"
grep -F '"decision": "staged-required"' "$rejected_audit"
grep -F "live preflight is required" "$rejected_audit"
grep -F '"failureReason": "config apply live request rejected for 1 domain(s)"' "$rejected_audit"
test ! -e "$work/var/lib/katl/generations/$rejected_generation"
test ! -e "$work/run/extensions/kubernetes"
test ! -e "$work/run/confexts/katl-node"

"$helper" \
	--root="$work" \
	--current-generation="CURRENT_GENERATION" \
	--next-generation="$accepted_generation" \
	--node=cp-1 \
	--manifest="$manifest" \
	--request="$fixture/accepted-request.yaml" \
	>"$work/accepted.out" 2>"$work/accepted.err"

accepted_audit="$work/var/lib/katl/config-requests/operator/4.json"
accepted_metadata="$work/var/lib/katl/generations/$accepted_generation/metadata.json"
accepted_status="$work/var/lib/katl/generations/$accepted_generation/config-apply-status.json"
accepted_network="$work/var/lib/katl/generations/$accepted_generation/confext/etc/systemd/network/20-nspawn-accepted.network"
test -f "$accepted_audit"
test -f "$accepted_metadata"
test -f "$accepted_status"
test -f "$accepted_network"
grep -F '"decision": "accepted"' "$accepted_audit"
grep -F '"candidateGenerationID": "'$accepted_generation'"' "$accepted_audit"
grep -F '"previousGenerationID": "CURRENT_GENERATION"' "$accepted_metadata"
grep -F '"payloadVersion": "v1.36.0"' "$accepted_metadata"
grep -F '"acceptedApplyMode": "next-boot"' "$accepted_status"
grep -F '"phase": "next-boot"' "$accepted_status"
grep -F '"domain": "networkd"' "$accepted_status"
grep -F "Address=192.0.2.10/24" "$accepted_network"
grep -F "CONFEXT_LEVEL=1" "$work/var/lib/katl/generations/$accepted_generation/confext/etc/extension-release.d/extension-release.katl-node"
test ! -e "$work/run/extensions/kubernetes"
test ! -e "$work/run/confexts/katl-node"
`, "CURRENT_GENERATION", currentGeneration)
}

func writeText(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

const configApplyCurrentManifest = `{
  "apiVersion": "install.katl.dev/v1alpha1",
  "kind": "InstallManifest",
  "node": {
    "identity": {
      "hostname": "cp-1",
      "ssh": {
        "authorizedKeys": [
          "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITestBaseKey katl"
        ]
      }
    },
    "systemRole": "control-plane"
  },
  "install": {
    "allowDestructiveInstall": true,
    "targetDisk": {
      "byID": "disk/by-id/test"
    }
  },
  "katlosImage": {
    "localRef": "images/katlos.raw",
    "sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "sizeBytes": 1024,
    "version": "0.1.0",
    "architecture": "x86_64",
    "role": "install"
  }
}
`

const configApplyRejectedRequest = `apiVersion: katl.dev/v1alpha1
kind: NodeConfigurationChange
metadata:
  sourceID: operator
  desiredVersion: "3"
apply:
  mode: live
spec:
  clusterDefaults:
    networkd:
      files:
        - name: 20-rejected.network
          content: |
            [Match]
            Name=*
            [Network]
            DHCP=yes
`

const configApplyAcceptedRequest = `apiVersion: katl.dev/v1alpha1
kind: NodeConfigurationChange
metadata:
  sourceID: operator
  desiredVersion: "4"
apply:
  mode: next-boot
spec:
  clusterDefaults:
    networkd:
      files:
        - name: 20-nspawn-accepted.network
          content: |
            [Match]
            Name=*
            [Network]
            Address=192.0.2.10/24
`
