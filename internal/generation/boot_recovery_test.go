package generation

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTrialFallbackVerification(t *testing.T) {
	for _, fault := range []struct {
		name, stagingError string
		inject             func(*testing.T, string, GenerationSpec)
	}{
		{"replaced root", "runtime SHA-256", func(t *testing.T, root string, spec GenerationSpec) {
			writeBootFixture(t, bootDevicePath(root, spec.Root.Slot), bootRootImage("replacement"))
		}},
		{"private devices", "open root PARTUUID", func(t *testing.T, root string, spec GenerationSpec) {
			// The sandbox hides the device node while leaving sysfs visible.
			removeBootFixture(t, bootDevicePath(root, spec.Root.Slot))
		}},
		{"missing extension", "/sysext/", func(t *testing.T, root string, spec GenerationSpec) {
			removeBootFixture(t, rootedPathUnchecked(root, spec.Sysexts[0].Path))
		}},
		{"missing PARTUUID", "absent from the system disk", func(t *testing.T, root string, spec GenerationSpec) {
			path := filepath.Join(root, "sys/devices/vda", filepath.Base(bootDevicePath(root, spec.Root.Slot)), "uevent")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			writeBootFixture(t, path, []byte(strings.ReplaceAll(string(data), spec.Root.PartitionUUID, "99999999-2222-3333-4444-555555555555")))
		}},
	} {
		t.Run(fault.name, func(t *testing.T) {
			root, now := managementFixture(t)
			managedFixture(t, root, "trial", "root-b", "2", now)
			selection, err := ReadBootSelection(root)
			if err != nil {
				t.Fatal(err)
			}
			selection.PreviousKnownGoodGenerationID = selection.DefaultGenerationID
			selection.PreviousKnownGoodBootEntry = selection.DefaultBootEntry
			selection.TargetBootGenerationID, selection.TrialGenerationID = "trial", "trial"
			selection.TargetBootEntry, selection.TrialBootEntry = "loader/entries/katl-trial.conf", "loader/entries/katl-trial.conf"
			selection.PendingHealthValidation = true
			selection.PendingTransactionID = "op"
			selection.PersistentDefaultPromotion = DefaultPromotionPending
			writeBootHealthSelection(t, root, selection)

			if err := CheckTrialFallback(root, selection); err != nil {
				t.Fatalf("initial fallback check: %v", err)
			}
			spec, _, err := ReadGeneration(root, selection.DefaultGenerationID)
			if err != nil {
				t.Fatal(err)
			}
			fault.inject(t, root, spec)
			if err := CheckTrialFallback(root, selection); err == nil || !strings.Contains(err.Error(), fault.stagingError) {
				t.Fatalf("staging check after fault = %v, want %q", err, fault.stagingError)
			}
		})
	}
}
