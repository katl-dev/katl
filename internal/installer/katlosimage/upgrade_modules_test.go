package katlosimage

import (
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/kernelmodule"
)

func TestUpgradeDoesNotPreserveModuleIndexes(t *testing.T) {
	previous, _ := knownGoodGeneration(t, "gen0", strings.Repeat("a", 64), "v1.35.0")
	previous.Sysexts = append(previous.Sysexts, generation.ExtensionRef{
		Name:            kernelmodule.IndexExtensionName,
		Path:            "/var/lib/katl/generations/gen0/sysext/" + kernelmodule.IndexExtensionName + ".raw",
		ActivationPath:  "/run/extensions/" + kernelmodule.IndexExtensionName + ".raw",
		SHA256:          strings.Repeat("b", 64),
		ArtifactVersion: previous.RuntimeVersion,
		PayloadVersion:  "6.12.1",
		Architecture:    previous.Root.Architecture,
		Compatibility: generation.ExtensionCompatibility{
			RuntimeInterfaces: []string{previous.Root.RuntimeInterface},
			ModuleIndexes: &kernelmodule.IndexSelection{
				Target: kernelmodule.Target{
					Release:       "6.12.1",
					RuntimeSHA256: previous.Root.RuntimeArtifactSHA256,
				},
				ModulesSHA256: strings.Repeat("d", 64),
			},
		},
	})
	status, err := generation.NewGenerationStatus(previous, generation.CommitStateCommitted, generation.BootStateGood, generation.HealthStateHealthy, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	plan, err := upgradePayload(t, nil).HostUpgradePlan(validHostUpgradeRequest(previous, status))
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range plan.Spec.Sysexts {
		if ref.Compatibility.ModuleIndexes != nil {
			t.Fatal("upgrade carried dependency indexes from the previous kernel")
		}
	}
	for _, asset := range plan.PreservedAssets {
		if asset.Name == kernelmodule.IndexExtensionName {
			t.Fatal("upgrade staged obsolete module indexes")
		}
	}
}
