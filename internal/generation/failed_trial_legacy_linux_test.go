package generation

import (
	"path/filepath"
	"testing"
)

func TestFailedTrialSelectionLegacy(t *testing.T) {
	for _, tc := range []struct {
		name, lostArtifact, defaultID, defaultEntry, problem string
		recoveryRequired                                     bool
	}{
		{
			name:         "fallback passes file checks",
			lostArtifact: "var/lib/katl/generations/gen3/sysext/kubernetes.raw",
			defaultID:    "gen3", defaultEntry: "loader/entries/katl-gen3.conf",
		},
		{
			name:         "fallback unavailable with healthy default",
			lostArtifact: "efi/EFI/Linux/katl-gen3.efi",
			defaultID:    "gen1", defaultEntry: "loader/entries/katl-gen1.conf",
			recoveryRequired: true,
			problem: "generation gen3 cannot boot: boot artifact unavailable: /efi/EFI/Linux/katl-gen3.efi. " +
				"The boot default was left unchanged. Preserve boot diagnostics and recover from the console",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newMoveWorld(t)
			selection := armRequest(w).Selection
			selection.PreviousKnownGoodGenerationID = "gen3"
			selection.PreviousKnownGoodBootEntry = "loader/entries/katl-gen3.conf"
			removeBootFixture(t, filepath.Join(w.root, tc.lostArtifact))

			got, problem, err := failedTrialSelection(w.root, selection, "gen2")
			if err != nil {
				t.Fatal(err)
			}
			if got.DefaultGenerationID != tc.defaultID || got.DefaultBootEntry != tc.defaultEntry ||
				got.RecoveryRequired != tc.recoveryRequired || problem != tc.problem {
				t.Fatalf("default %q entry %q, recoveryRequired %t, problem %q; want %q, %q, %t, %q",
					got.DefaultGenerationID, got.DefaultBootEntry, got.RecoveryRequired, problem,
					tc.defaultID, tc.defaultEntry, tc.recoveryRequired, tc.problem)
			}
		})
	}
}
