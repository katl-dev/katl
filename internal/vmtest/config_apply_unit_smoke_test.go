package vmtest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zariel/katl/internal/installer/configapply"
	"github.com/zariel/katl/internal/installer/generation"
	"github.com/zariel/katl/internal/installer/manifest"
)

func TestConfigApplySmokeRejectsLiveAndStagesNextBoot(t *testing.T) {
	fixture := runtimeUserspaceFixture(t)
	work := t.TempDir()
	if err := copyOptionalDir(fixture.Root, work); err != nil {
		t.Fatalf("copy runtime fixture: %v", err)
	}
	currentManifest, err := manifest.Decode(strings.NewReader(configApplyCurrentManifest))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	metadataPath, err := generation.MetadataPath(work, fixture.GenerationID)
	if err != nil {
		t.Fatalf("MetadataPath() error = %v", err)
	}
	currentRecord, err := generation.ReadRecord(metadataPath)
	if err != nil {
		t.Fatalf("ReadRecord() error = %v", err)
	}
	base := configapply.TrustedBundleRequest{
		Root:            work,
		NodeName:        "cp-1",
		CurrentManifest: currentManifest,
		CurrentRecord:   currentRecord,
		Chown:           func(string, int, int) error { return nil },
		Now:             func() time.Time { return time.Date(2026, 6, 6, 13, 0, 0, 0, time.UTC) },
	}

	rejectedGeneration := "2026.06.06-101"
	rejected, err := configapply.ApplyNodeConfigurationChange(t.Context(), strings.NewReader(configApplyRejectedRequest), withGeneration(base, rejectedGeneration))
	if err == nil || !strings.Contains(err.Error(), "request rejected") {
		t.Fatalf("rejected ApplyNodeConfigurationChange() error = %v, result = %#v", err, rejected)
	}
	if rejected.Audit.Decision != configapply.DecisionRejected || rejected.Audit.RequestedApplyMode != generation.ApplyModeLive {
		t.Fatalf("rejected audit = %#v", rejected.Audit)
	}
	if len(rejected.Audit.Diagnostics) != 1 ||
		rejected.Audit.Diagnostics[0].Domain != configapply.DomainNetworkd ||
		rejected.Audit.Diagnostics[0].Decision != configapply.DecisionStagedRequired ||
		rejected.Audit.Diagnostics[0].Classification != configapply.ClassificationStagedOnly ||
		!strings.Contains(rejected.Audit.Diagnostics[0].Message, "staged-only") {
		t.Fatalf("rejected diagnostics = %#v", rejected.Audit.Diagnostics)
	}
	if !strings.Contains(rejected.Audit.FailureReason, "config apply live request rejected for 1 domain") {
		t.Fatalf("rejected failure reason = %q", rejected.Audit.FailureReason)
	}
	assertPathMissing(t, filepath.Join(work, "var/lib/katl/generations", rejectedGeneration))
	assertPathMissing(t, filepath.Join(work, "run/extensions/kubernetes"))
	assertPathMissing(t, filepath.Join(work, "run/confexts/katl-node"))

	acceptedGeneration := "2026.06.06-102"
	accepted, err := configapply.ApplyNodeConfigurationChange(context.Background(), strings.NewReader(configApplyAcceptedRequest), withGeneration(base, acceptedGeneration))
	if err != nil {
		t.Fatalf("accepted ApplyNodeConfigurationChange() error = %v", err)
	}
	if accepted.Audit.Decision != configapply.DecisionAccepted || accepted.Audit.CandidateGeneration != acceptedGeneration {
		t.Fatalf("accepted audit = %#v", accepted.Audit)
	}
	if accepted.Status.AcceptedApplyMode != generation.ApplyModeNextBoot || accepted.Status.Phase != generation.ConfigApplyPhaseNextBoot {
		t.Fatalf("accepted status = %#v", accepted.Status)
	}
	assertFileContains(t, accepted.AuditPath, `"decision": "accepted"`, `"candidateGenerationID": "`+acceptedGeneration+`"`)
	assertFileContains(t, accepted.MetadataPath, `"previousGenerationID": "`+fixture.GenerationID+`"`, `"payloadVersion": "v1.36.0"`)
	assertFileContains(t, accepted.StatusPath, `"acceptedApplyMode": "next-boot"`, `"phase": "next-boot"`, `"domain": "networkd"`)
	assertFileContains(t, filepath.Join(work, "var/lib/katl/generations", acceptedGeneration, "confext/etc/systemd/network/20-unprivileged-accepted.network"), "Address=192.0.2.10/24")
	assertFileContains(t, filepath.Join(work, "var/lib/katl/generations", acceptedGeneration, "confext/etc/extension-release.d/extension-release.katl-node"), "CONFEXT_LEVEL=1")
	assertPathMissing(t, filepath.Join(work, "run/extensions/kubernetes"))
	assertPathMissing(t, filepath.Join(work, "run/confexts/katl-node"))
}

func withGeneration(request configapply.TrustedBundleRequest, generationID string) configapply.TrustedBundleRequest {
	request.GenerationID = generationID
	return request
}

func assertFileContains(t *testing.T, path string, wants ...string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	text := string(data)
	for _, want := range wants {
		if !strings.Contains(text, want) {
			t.Fatalf("%s missing %q:\n%s", path, want, text)
		}
	}
}

func assertPathMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("%s exists, want missing", path)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat %s: %v", path, err)
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
          "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDAxMjM0NTY3ODlhYmNkZWYwMTIzNDU2Nzg5YWJjZGVm katl"
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
        - name: 20-unprivileged-accepted.network
          content: |
            [Match]
            Name=*
            [Network]
            Address=192.0.2.10/24
`
