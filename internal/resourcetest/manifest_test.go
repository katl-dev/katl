package resourcetest

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const testSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestManifestRoundTrip(t *testing.T) {
	manifest := validManifest()
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	decoded, err := DecodeManifest(strings.NewReader(string(data)))
	if err != nil {
		t.Fatalf("DecodeManifest() error = %v", err)
	}
	if decoded.RunID != manifest.RunID || decoded.Scenarios[0].Status != StatusPassed {
		t.Fatalf("decoded manifest = %#v", decoded)
	}
}

func TestValidateManifestRejectsInvalidDigest(t *testing.T) {
	manifest := validManifest()
	manifest.Artifacts[0].Digest = strings.ToUpper(testSHA)
	err := ValidateManifest(manifest)
	if err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("ValidateManifest() error = %v, want sha256 rejection", err)
	}
}

func TestValidateManifestRejectsInvalidPackageLockDigest(t *testing.T) {
	manifest := validManifest()
	manifest.PackageSets[0].LockDigest = strings.ToUpper(testSHA)
	err := ValidateManifest(manifest)
	if err == nil || !strings.Contains(err.Error(), "lockSHA256") {
		t.Fatalf("ValidateManifest() error = %v, want lockSHA256 rejection", err)
	}
}

func TestValidateManifestRejectsUnknownFixtureRef(t *testing.T) {
	manifest := validManifest()
	manifest.Scenarios[0].FixtureRefs = []string{"missing"}
	err := ValidateManifest(manifest)
	if err == nil || !strings.Contains(err.Error(), `fixtureRef "missing"`) {
		t.Fatalf("ValidateManifest() error = %v, want fixture ref rejection", err)
	}
}

func TestValidateManifestRejectsUnknownCapabilityRef(t *testing.T) {
	manifest := validManifest()
	manifest.Scenarios[0].RequiredCapabilities = []string{"missing"}
	err := ValidateManifest(manifest)
	if err == nil || !strings.Contains(err.Error(), `required capability "missing"`) {
		t.Fatalf("ValidateManifest() error = %v, want capability ref rejection", err)
	}
}

func TestValidateManifestRejectsUnknownPackageSetRef(t *testing.T) {
	manifest := validManifest()
	manifest.MkosiProfiles[0].PackageSetRef = "missing"
	err := ValidateManifest(manifest)
	if err == nil || !strings.Contains(err.Error(), `packageSetRef "missing"`) {
		t.Fatalf("ValidateManifest() error = %v, want package set ref rejection", err)
	}
}

func TestValidateManifestRejectsUnsupportedStatus(t *testing.T) {
	manifest := validManifest()
	manifest.Scenarios[0].Status = Status("skipped")
	err := ValidateManifest(manifest)
	if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("ValidateManifest() error = %v, want status rejection", err)
	}
}

func TestClassifyScenario(t *testing.T) {
	tests := []struct {
		name        string
		observation ScenarioObservation
		want        Status
	}{
		{name: "disabled", observation: ScenarioObservation{}, want: StatusDisabled},
		{name: "setup failed", observation: ScenarioObservation{Enabled: true}, want: StatusSetupFailed},
		{name: "host skipped", observation: ScenarioObservation{Enabled: true, SetupComplete: true, HostCapabilitySkip: true}, want: StatusHostSkipped},
		{name: "passed", observation: ScenarioObservation{Enabled: true, SetupComplete: true, HarnessStatus: HarnessPassed}, want: StatusPassed},
		{name: "failed", observation: ScenarioObservation{Enabled: true, SetupComplete: true, HarnessStatus: HarnessFailed}, want: StatusFailed},
		{name: "generic skip", observation: ScenarioObservation{Enabled: true, SetupComplete: true, HarnessStatus: HarnessSkipped}, want: StatusSetupFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyScenario(tt.observation); got != tt.want {
				t.Fatalf("ClassifyScenario() = %q, want %q", got, tt.want)
			}
		})
	}
}

func validManifest() Manifest {
	return Manifest{
		APIVersion: APIVersion,
		Kind:       Kind,
		RunID:      "20260606T120000Z",
		Created:    time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC),
		Git:        GitState{Revision: "ca065f2", Dirty: true},
		Tools: []Tool{{
			Name:    "go",
			Version: "go1.26",
		}},
		PackageSets: []PackageSet{{
			Name:   "runtime",
			Source: "mkosi.profiles/runtime",
			Digest: testSHA,
			Packages: []Package{{
				Name:     "systemd",
				NEVRA:    "systemd-0:258-1.x86_64",
				Checksum: testSHA,
			}},
		}},
		MkosiProfiles: []MkosiProfile{{
			Name:          "runtime",
			Path:          "mkosi.profiles/runtime",
			ConfigDigest:  testSHA,
			PackageSetRef: "runtime",
		}},
		HostCapabilities: []HostCapability{{
			Name:    "libvirt",
			Present: true,
		}},
		Artifacts: []Artifact{{
			Name:      "runtime-root",
			Kind:      "squashfs",
			Path:      "_build/mkosi/katl-runtime-root.squashfs",
			Digest:    testSHA,
			SizeBytes: 1024,
		}},
		Fixtures: []Fixture{{
			Name:         "installed-runtime",
			Kind:         "installed-runtime",
			Path:         "_build/vmtest/fixtures/installed-runtime",
			Manifest:     "_build/vmtest/fixtures/installed-runtime.json",
			ArtifactRefs: []string{"runtime-root"},
		}},
		Scenarios: []Scenario{{
			Name:                 "installed runtime agent smoke",
			Suite:                "vmtest",
			Status:               StatusPassed,
			ResultPath:           "_build/vmtest/installed-runtime-agent/result.json",
			RunDir:               "_build/vmtest/installed-runtime-agent",
			FixtureRefs:          []string{"installed-runtime"},
			RequiredCapabilities: []string{"libvirt"},
		}},
	}
}
