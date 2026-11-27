package generation

import (
	"path/filepath"
	"testing"
)

func TestFailedTrialSelectionLegacy(t *testing.T) {
	for _, tc := range []struct {
		name, lostArtifact        string
		selection                 BootSelectionRecord
		defaultID, defaultEntry   string
		previousID, previousEntry string
		recoveryRequired          bool
		problem                   string
	}{
		{
			name:         "fallback passes file checks",
			lostArtifact: "var/lib/katl/generations/gen3/sysext/kubernetes.raw",
			selection: BootSelectionRecord{
				DefaultGenerationID: "gen1", DefaultBootEntry: "loader/entries/katl-gen1.conf",
				PreviousKnownGoodGenerationID: "gen3", PreviousKnownGoodBootEntry: "loader/entries/katl-gen3.conf",
			},
			defaultID: "gen3", defaultEntry: "loader/entries/katl-gen3.conf",
			previousID: "gen3", previousEntry: "loader/entries/katl-gen3.conf",
		},
		{
			name:         "fallback unavailable with healthy default",
			lostArtifact: "efi/EFI/Linux/katl-gen3.efi",
			selection: BootSelectionRecord{
				DefaultGenerationID: "gen1", DefaultBootEntry: "loader/entries/katl-gen1.conf",
				PreviousKnownGoodGenerationID: "gen3", PreviousKnownGoodBootEntry: "loader/entries/katl-gen3.conf",
			},
			defaultID: "gen1", defaultEntry: "loader/entries/katl-gen1.conf",
			recoveryRequired: true,
			problem: "generation gen3 cannot boot: boot artifact unavailable: /efi/EFI/Linux/katl-gen3.efi. " +
				"The boot default was left unchanged. Preserve boot diagnostics and recover from the console",
		},
		{
			name: "fallback preserves omitted previous entry",
			selection: BootSelectionRecord{
				DefaultGenerationID: "gen1", DefaultBootEntry: "loader/entries/katl-gen1.conf",
				PreviousKnownGoodGenerationID: "gen1",
			},
			defaultID: "gen1", defaultEntry: "loader/entries/katl-gen1.conf",
			previousID: "gen1",
		},
		{
			name: "no fallback preserves pointers",
			selection: BootSelectionRecord{
				DefaultGenerationID: "gen2", DefaultBootEntry: "loader/entries/katl-gen2.conf",
				PreviousKnownGoodBootEntry: "loader/entries/katl-gen1.conf",
			},
			defaultID: "gen2", defaultEntry: "loader/entries/katl-gen2.conf",
			previousEntry: "loader/entries/katl-gen1.conf", recoveryRequired: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newMoveWorld(t)
			if tc.lostArtifact != "" {
				removeBootFixture(t, filepath.Join(w.root, tc.lostArtifact))
			}

			got, problem, err := failedTrialSelection(w.root, tc.selection, "gen2")
			if err != nil {
				t.Fatal(err)
			}
			if got.DefaultGenerationID != tc.defaultID || got.DefaultBootEntry != tc.defaultEntry ||
				got.RecoveryRequired != tc.recoveryRequired || problem != tc.problem {
				t.Fatalf("default %q entry %q, recoveryRequired %t, problem %q; want %q, %q, %t, %q",
					got.DefaultGenerationID, got.DefaultBootEntry, got.RecoveryRequired, problem,
					tc.defaultID, tc.defaultEntry, tc.recoveryRequired, tc.problem)
			}
			if got.PreviousKnownGoodGenerationID != tc.previousID || got.PreviousKnownGoodBootEntry != tc.previousEntry {
				t.Fatalf("previous known-good %q entry %q, want %q and %q",
					got.PreviousKnownGoodGenerationID, got.PreviousKnownGoodBootEntry, tc.previousID, tc.previousEntry)
			}
		})
	}
}
