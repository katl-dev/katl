package generation

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestArmBootRecoveryOnlyForValidatedTrial(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	writeBootHealthGeneration(t, root, "known-good", "", CommitStateCommitted, BootStateGood, HealthStateHealthy, now.Add(-time.Hour))
	writeBootHealthGeneration(t, root, "candidate", "known-good", CommitStateCommitted, BootStateTrying, HealthStateUnknown, now)
	selection := BootSelectionRecord{
		APIVersion:                    APIVersion,
		Kind:                          BootSelectionKind,
		DefaultGenerationID:           "known-good",
		TargetBootGenerationID:        "candidate",
		TrialGenerationID:             "candidate",
		PreviousKnownGoodGenerationID: "known-good",
		DefaultBootEntry:              "loader/entries/katl-known-good.conf",
		TrialBootEntry:                "loader/entries/katl-candidate.conf",
		PreviousKnownGoodBootEntry:    "loader/entries/katl-known-good.conf",
		PendingHealthValidation:       true,
		PersistentDefaultPromotion:    DefaultPromotionPending,
		UpdatedAt:                     now,
	}
	writeBootHealthSelection(t, root, selection)
	writeBootHealthFallback(t, root, "known-good")

	armed, err := ArmBootRecovery(root, "candidate")
	if err != nil || !armed {
		t.Fatalf("ArmBootRecovery(candidate) = %t, %v", armed, err)
	}
	marker := filepath.Join(root, "run/katl/boot-health/pending")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("trial recovery authorization: %v", err)
	}

	armed, err = ArmBootRecovery(root, "known-good")
	if err != nil || armed {
		t.Fatalf("ArmBootRecovery(known-good) = %t, %v", armed, err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fallback retained trial recovery authorization: %v", err)
	}
}

func TestArmBootRecoveryRejectsUnhealthyFallback(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 9, 28, 13, 0, 0, 0, time.UTC)
	writeBootHealthGeneration(t, root, "fallback", "", CommitStateCommitted, BootStateFailed, HealthStateUnhealthy, now.Add(-time.Hour))
	writeBootHealthGeneration(t, root, "candidate", "fallback", CommitStateCommitted, BootStateTrying, HealthStateUnknown, now)
	writeBootHealthSelection(t, root, BootSelectionRecord{
		APIVersion:                    APIVersion,
		Kind:                          BootSelectionKind,
		DefaultGenerationID:           "fallback",
		TargetBootGenerationID:        "candidate",
		TrialGenerationID:             "candidate",
		PreviousKnownGoodGenerationID: "fallback",
		DefaultBootEntry:              "loader/entries/katl-fallback.conf",
		TrialBootEntry:                "loader/entries/katl-candidate.conf",
		PreviousKnownGoodBootEntry:    "loader/entries/katl-fallback.conf",
		PendingTransactionID:          "next-boot-apply",
		PendingHealthValidation:       true,
		PersistentDefaultPromotion:    DefaultPromotionPending,
		UpdatedAt:                     now,
	})

	armed, err := ArmBootRecovery(root, "candidate")
	if err == nil || armed || !strings.Contains(err.Error(), "not known-good") {
		t.Fatalf("ArmBootRecovery(candidate) = %t, %v, want refusal", armed, err)
	}
	if _, err := os.Stat(filepath.Join(root, "run/katl/boot-health/pending")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid fallback created recovery authorization: %v", err)
	}

	spec, status, err := ReadGeneration(root, "fallback")
	if err != nil {
		t.Fatal(err)
	}
	status.BootState = BootStateGood
	status.HealthState = HealthStateHealthy
	if err := WriteGenerationStatus(root, spec, status); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(rootedPathUnchecked(root, spec.Boot.UKIPath)); err != nil {
		t.Fatal(err)
	}
	armed, err = ArmBootRecovery(root, "candidate")
	if err == nil || armed || !strings.Contains(err.Error(), spec.Boot.UKIPath) {
		t.Fatalf("ArmBootRecovery(candidate without fallback UKI) = %t, %v, want refusal", armed, err)
	}

	uki := rootedPathUnchecked(root, spec.Boot.UKIPath)
	linkBootFixture(t, uki, uki)
	armed, err = ArmBootRecovery(root, "candidate")
	if armed || !errors.Is(err, unix.ELOOP) {
		t.Fatalf("ArmBootRecovery(candidate with unreadable fallback UKI) = %t, %v", armed, err)
	}
}

func TestArmBootRecoveryRequiresFullFallbackEvidence(t *testing.T) {
	for _, fault := range []struct {
		name, reason string
		inject       func(*testing.T, string, GenerationSpec)
	}{
		{"replaced root", "runtime SHA-256", func(t *testing.T, root string, spec GenerationSpec) {
			writeBootFixture(t, bootDevicePath(root, spec.Root.Slot), bootRootImage("replacement"))
		}},
		{"unreadable root", "bootability indeterminate", func(t *testing.T, root string, spec GenerationSpec) {
			path := bootDevicePath(root, spec.Root.Slot)
			removeBootFixture(t, path)
			linkBootFixture(t, path, path)
		}},
		{"missing extension", "/sysext/", func(t *testing.T, root string, spec GenerationSpec) {
			removeBootFixture(t, rootedPathUnchecked(root, spec.Sysexts[0].Path))
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

			if armed, err := ArmBootRecovery(root, "trial"); !armed || err != nil {
				t.Fatalf("initial arm = %t, %v", armed, err)
			}
			spec, _, err := ReadGeneration(root, selection.DefaultGenerationID)
			if err != nil {
				t.Fatal(err)
			}
			fault.inject(t, root, spec)
			if armed, err := ArmBootRecovery(root, "trial"); armed || err == nil || !strings.Contains(err.Error(), fault.reason) {
				t.Fatalf("arm after fault = %t, %v, want %q", armed, err, fault.reason)
			}
			if _, err := os.Stat(rootedPathUnchecked(root, bootRecoveryPath)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("refusal left reboot authorization: %v", err)
			}
		})
	}
}
