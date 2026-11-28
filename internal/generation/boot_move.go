package generation

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/katl-dev/katl/internal/persistedrecord"
)

// ErrBootMoveBlocked signals that a legacy operation must wait for a runtime
// that can read and complete the durable move.
var ErrBootMoveBlocked = errors.New("boot move in progress")

const (
	bootStateDir          = "/var/lib/katl/boot"
	bootMovePath          = bootStateDir + "/move-intent.json"
	bootMoveRecordType    = "katl.boot.move-intent"
	bootMoveRecordVersion = 1
)

// bootMoveIntent is the v1 payload. Pointers distinguish omitted fields from
// explicit zero values, such as the legacy revision or clearing an EFI variable.
type bootMoveIntent struct {
	OperationID               string              `json:"operationID"`
	BootID                    string              `json:"bootID"`
	ExpectedSelectionRevision *uint64             `json:"expectedSelectionRevision"`
	SourceGenerationID        string              `json:"sourceGenerationID"`
	DestinationGenerationID   *string             `json:"destinationGenerationID"`
	PriorDefault              *bootMoveTarget     `json:"priorDefault"`
	PriorOneShot              *bootMoveTarget     `json:"priorOneShot"`
	DesiredDefault            *bootMoveTarget     `json:"desiredDefault"`
	DesiredOneShot            *bootMoveTarget     `json:"desiredOneShot"`
	DesiredSelection          BootSelectionRecord `json:"desiredSelection"`
	StatusChanges             []bootStatusChange  `json:"statusChanges"`
	Reboot                    bootMoveReboot      `json:"reboot"`
}

type bootMoveTarget struct {
	GenerationID *string `json:"generationID"`
	Entry        *string `json:"entry"`
}

type bootStatusChange struct {
	GenerationID      string    `json:"generationID"`
	At                time.Time `json:"at"`
	Reason            string    `json:"reason"`
	CommitState       string    `json:"commitState"`
	BootState         string    `json:"bootState"`
	HealthState       string    `json:"healthState"`
	UnavailableReason *string   `json:"unavailableReason"`
}

type bootMoveReboot struct {
	TrialID  *string `json:"trialID"`
	Eligible *bool   `json:"eligible"`
	Spent    *bool   `json:"spent"`
}

func withBootStateLock(root string, action func() error) error {
	return withStateLock(rootedPathUnchecked(root, bootStateDir), action)
}

func readBootMoveIntent(root string) (*bootMoveIntent, error) {
	path := rootedPathUnchecked(root, bootMovePath)
	intent, err := readBootMoveFile(path)
	if err == nil && intent != nil {
		err = intent.validateBindings(root)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: read boot move intent at %s: %v. From the node console, restore access to intact boot state or boot a runtime that supports this intent before retrying", ErrBootMoveBlocked, path, err)
	}
	return intent, nil
}

func readBootMoveFile(path string) (*bootMoveIntent, error) {
	// An unexpected file type must not look like an absent record or block boot.
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("intent is not a regular file")
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, err
	}
	envelope, err := persistedrecord.DecodeEnvelope(data)
	if err != nil {
		return nil, err
	}
	if envelope.RecordType != bootMoveRecordType || envelope.RecordVersion != bootMoveRecordVersion {
		return nil, fmt.Errorf("unsupported boot move intent %s/v%d", envelope.RecordType, envelope.RecordVersion)
	}
	intent, err := persistedrecord.DecodePayload[bootMoveIntent](envelope)
	if err != nil {
		return nil, err
	}
	if err := intent.validate(); err != nil {
		return nil, err
	}
	return &intent, nil
}

func (intent bootMoveIntent) validate() error {
	if intent.DestinationGenerationID == nil {
		return fmt.Errorf("destinationGenerationID is required")
	}
	for _, field := range []struct{ name, value string }{
		{"operationID", intent.OperationID},
		{"bootID", intent.BootID},
		{"sourceGenerationID", intent.SourceGenerationID},
	} {
		if err := validateMoveID(field.name, field.value); err != nil {
			return err
		}
	}
	if *intent.DestinationGenerationID != "" {
		if err := validateMoveID("destinationGenerationID", *intent.DestinationGenerationID); err != nil {
			return err
		}
	}
	for _, field := range []struct {
		name   string
		target *bootMoveTarget
	}{
		{"priorDefault", intent.PriorDefault},
		{"priorOneShot", intent.PriorOneShot},
		{"desiredDefault", intent.DesiredDefault},
		{"desiredOneShot", intent.DesiredOneShot},
	} {
		if field.target == nil || field.target.GenerationID == nil || field.target.Entry == nil {
			return fmt.Errorf("%s requires generationID and entry", field.name)
		}
		if id := *field.target.GenerationID; id != "" {
			if err := validateMoveID(field.name+".generationID", id); err != nil {
				return err
			}
		}
		if entry := *field.target.Entry; entry != "" {
			if err := validateMoveID(field.name+".entry", entry); err != nil {
				return err
			}
			if *field.target.GenerationID == "" {
				return fmt.Errorf("%s.entry requires a generationID", field.name)
			}
		}
	}
	if *intent.DesiredOneShot.Entry == "" && *intent.DesiredOneShot.GenerationID != "" {
		return fmt.Errorf("a cleared desired one-shot cannot name a generation")
	}
	revision := intent.DesiredSelection.Revision
	if intent.ExpectedSelectionRevision == nil || revision == 0 || revision-1 != *intent.ExpectedSelectionRevision {
		return fmt.Errorf("desiredSelection.revision must follow expectedSelectionRevision")
	}
	if err := ValidateBootSelection(intent.DesiredSelection); err != nil {
		return fmt.Errorf("invalid desiredSelection: %w", err)
	}
	if err := intent.validateDesiredTargets(); err != nil {
		return err
	}
	if intent.StatusChanges == nil {
		return fmt.Errorf("statusChanges is required")
	}
	seen := make(map[string]bool, len(intent.StatusChanges))
	for _, change := range intent.StatusChanges {
		if err := validateMoveID("statusChanges.generationID", change.GenerationID); err != nil {
			return err
		}
		if seen[change.GenerationID] {
			return fmt.Errorf("duplicate status change for generation %s", change.GenerationID)
		}
		seen[change.GenerationID] = true
		if change.At.IsZero() || strings.TrimSpace(change.Reason) == "" || change.UnavailableReason == nil ||
			!validCommitState(change.CommitState) || !validBootState(change.BootState) || !validHealthState(change.HealthState) {
			return fmt.Errorf("invalid status change for generation %s", change.GenerationID)
		}
	}
	reboot := intent.Reboot
	if reboot.TrialID == nil || reboot.Eligible == nil || reboot.Spent == nil {
		return fmt.Errorf("reboot requires trialID, eligible and spent")
	}
	if *reboot.Eligible {
		if err := validateMoveID("reboot.trialID", *reboot.TrialID); err != nil {
			return err
		}
		if *reboot.Spent != (intent.DesiredSelection.SpentRecoveryTrialID == *reboot.TrialID) {
			return fmt.Errorf("desiredSelection.spentRecoveryTrialID must agree with reboot.trialID and spent")
		}
	} else if *reboot.Spent || *reboot.TrialID != "" {
		return fmt.Errorf("reboot trialID and spent require eligibility")
	}
	return nil
}

func (intent bootMoveIntent) validateDesiredTargets() error {
	selection := intent.DesiredSelection
	defaultID, defaultEntry := *intent.DesiredDefault.GenerationID, *intent.DesiredDefault.Entry
	if defaultID != selection.DefaultGenerationID ||
		(defaultEntry != "" && defaultEntry != filepath.Base(selection.DefaultBootEntry)) {
		return fmt.Errorf("desiredDefault disagrees with desiredSelection default")
	}
	oneShotID, oneShotEntry := *intent.DesiredOneShot.GenerationID, *intent.DesiredOneShot.Entry
	for _, target := range []struct{ id, entry string }{
		{selection.TargetBootGenerationID, selection.TargetBootEntry},
		{selection.TrialGenerationID, selection.TrialBootEntry},
	} {
		if target.id == "" && target.entry == "" {
			continue
		}
		if oneShotID == "" || target.id != oneShotID || filepath.Base(target.entry) != oneShotEntry {
			return fmt.Errorf("desiredOneShot disagrees with desiredSelection target or trial")
		}
	}
	if selection.TargetBootGenerationID == "" && selection.TrialGenerationID == "" &&
		oneShotID != "" && oneShotID != defaultID {
		return fmt.Errorf("desiredOneShot without a selection target or trial must name the default generation")
	}
	return nil
}

func (intent bootMoveIntent) validateBindings(root string) error {
	for _, target := range []*bootMoveTarget{intent.PriorDefault, intent.PriorOneShot, intent.DesiredDefault, intent.DesiredOneShot} {
		id, entry := *target.GenerationID, *target.Entry
		if id == "" {
			continue
		}
		spec, _, err := ReadGeneration(root, id)
		if err != nil {
			return fmt.Errorf("read recorded boot target %s: %w", id, err)
		}
		if spec.GenerationID != id || spec.Boot.LoaderEntryPath == "" ||
			(entry != "" && entry != filepath.Base(spec.Boot.LoaderEntryPath)) ||
			(target == intent.DesiredDefault && intent.DesiredSelection.DefaultBootEntry != spec.Boot.LoaderEntryPath) {
			return fmt.Errorf("recorded boot target %s does not match its generation loader entry", id)
		}
		if target == intent.DesiredOneShot {
			selection := intent.DesiredSelection
			if (selection.TargetBootGenerationID != "" && selection.TargetBootEntry != spec.Boot.LoaderEntryPath) ||
				(selection.TrialGenerationID != "" && selection.TrialBootEntry != spec.Boot.LoaderEntryPath) {
				return fmt.Errorf("desiredSelection target or trial does not match generation %s loader entry", id)
			}
		}
	}
	return nil
}

func (intent bootMoveIntent) nextGenerationID() string {
	return cmp.Or(*intent.DesiredOneShot.GenerationID, *intent.DesiredDefault.GenerationID)
}

func validateMoveID(name, value string) error {
	if value != strings.TrimSpace(value) || strings.ContainsRune(value, 0) {
		return fmt.Errorf("%s must be a clean path segment", name)
	}
	_, err := cleanSegment(name, value)
	return err
}

func (intent bootMoveIntent) validateBootedGeneration(id string) error {
	accepted := []string{*intent.PriorDefault.GenerationID, *intent.PriorOneShot.GenerationID,
		*intent.DesiredDefault.GenerationID, *intent.DesiredOneShot.GenerationID}
	if slices.Contains(accepted, id) {
		return nil
	}
	return fmt.Errorf("boot move in progress (operation %s): generation %s is outside the recorded boot targets. From the node console, boot a recorded target with a runtime that can complete the move", intent.OperationID, id)
}
