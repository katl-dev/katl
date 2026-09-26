package generation

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestUpgradeHandoffAcceptsInstalledRootSlots(t *testing.T) {
	for _, slot := range []string{"root-a", "root-b"} {
		t.Run(slot, func(t *testing.T) {
			root := t.TempDir()
			record := UpgradeHandoff{
				Version: UpgradeHandoffVersion, OperationID: "upgrade-1",
				SourceGenerationID: "source", CandidateGenerationID: "target",
				ImageSHA256: "digest", ImageSizeBytes: 100,
				RootSlot: slot, RootPartitionUUID: "partition",
				UKIPath:         UKIDirectory + "/katl-" + slot + "-1.efi",
				LoaderEntryPath: "loader/entries/katl-target.conf",
				CreatedAt:       time.Now().UTC(),
			}
			if err := WriteUpgradeHandoff(root, record); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(root, "var/lib/katl/generations/target")); !os.IsNotExist(err) {
				t.Fatalf("pending handoff created a generation: %v", err)
			}
			got, err := ReadUpgradeHandoff(root, "target")
			if err != nil || got.RootSlot != slot {
				t.Fatalf("ReadUpgradeHandoff() = %+v, %v", got, err)
			}
		})
	}
}
