package generation

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	BootSelectionKind = "BootSelection"

	DefaultPromotionPending = "pending"
	DefaultPromotionDone    = "promoted"
)

type BootSelectionRecord struct {
	APIVersion                    string    `json:"apiVersion"`
	Kind                          string    `json:"kind"`
	DefaultGenerationID           string    `json:"defaultGenerationID"`
	TargetBootGenerationID        string    `json:"targetBootGenerationID,omitempty"`
	TrialGenerationID             string    `json:"trialGenerationID,omitempty"`
	PreviousKnownGoodGenerationID string    `json:"previousKnownGoodGenerationID,omitempty"`
	BootedGenerationID            string    `json:"bootedGenerationID,omitempty"`
	ActiveGenerationID            string    `json:"activeGenerationID,omitempty"`
	Generation0FallbackID         string    `json:"generation0FallbackID,omitempty"`
	FailedBootGenerationID        string    `json:"failedBootGenerationID,omitempty"`
	DefaultBootEntry              string    `json:"defaultBootEntry,omitempty"`
	TargetBootEntry               string    `json:"targetBootEntry,omitempty"`
	TrialBootEntry                string    `json:"trialBootEntry,omitempty"`
	PreviousKnownGoodBootEntry    string    `json:"previousKnownGoodBootEntry,omitempty"`
	BootedBootEntry               string    `json:"bootedBootEntry,omitempty"`
	BootCountedTrialPath          string    `json:"bootCountedTrialPath,omitempty"`
	PendingTransactionID          string    `json:"pendingTransactionID,omitempty"`
	PendingHealthValidation       bool      `json:"pendingHealthValidation"`
	PersistentDefaultPromotion    string    `json:"persistentDefaultPromotion,omitempty"`
	RecoveryRequired              bool      `json:"recoveryRequired"`
	UpdatedAt                     time.Time `json:"updatedAt"`
}

func BootSelectionPath(root string) (string, error) {
	return rootedPath(root, "/var/lib/katl/boot/selection.json")
}

func ReadBootSelection(root string) (BootSelectionRecord, error) {
	path, err := BootSelectionPath(root)
	if err != nil {
		return BootSelectionRecord{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return BootSelectionRecord{}, fmt.Errorf("read boot selection: %w", err)
	}
	selection, err := decodeBootSelection(data)
	if err != nil {
		return BootSelectionRecord{}, err
	}
	if err := ValidateBootSelection(selection); err != nil {
		return BootSelectionRecord{}, err
	}
	return selection, nil
}

func WriteBootSelection(root string, selection BootSelectionRecord) error {
	if err := ValidateBootSelection(selection); err != nil {
		return err
	}
	path, err := BootSelectionPath(root)
	if err != nil {
		return err
	}
	data, err := marshalRecordEnvelope(BootSelectionRecordType, selection)
	if err != nil {
		return fmt.Errorf("marshal boot selection: %w", err)
	}
	if err := writeFileAtomic(path, data, 0o644); err != nil {
		return fmt.Errorf("write boot selection: %w", err)
	}
	return nil
}

func decodeBootSelection(data []byte) (BootSelectionRecord, error) {
	if selection, ok, err := decodeRecordEnvelope[BootSelectionRecord](data, BootSelectionRecordType); ok {
		if err != nil {
			return BootSelectionRecord{}, fmt.Errorf("decode boot selection envelope: %w", err)
		}
		return selection, nil
	}
	var selection BootSelectionRecord
	if err := json.Unmarshal(data, &selection); err != nil {
		return BootSelectionRecord{}, fmt.Errorf("decode boot selection: %w", err)
	}
	return selection, nil
}

func ValidateBootSelection(selection BootSelectionRecord) error {
	if selection.APIVersion != APIVersion {
		return fmt.Errorf("boot selection apiVersion must be %q", APIVersion)
	}
	if selection.Kind != BootSelectionKind {
		return fmt.Errorf("boot selection kind must be %q", BootSelectionKind)
	}
	if strings.TrimSpace(selection.DefaultGenerationID) == "" && !selection.RecoveryRequired {
		return fmt.Errorf("boot selection defaultGenerationID is required")
	}
	for name, value := range map[string]string{
		"defaultGenerationID":           selection.DefaultGenerationID,
		"targetBootGenerationID":        selection.TargetBootGenerationID,
		"trialGenerationID":             selection.TrialGenerationID,
		"previousKnownGoodGenerationID": selection.PreviousKnownGoodGenerationID,
		"bootedGenerationID":            selection.BootedGenerationID,
		"activeGenerationID":            selection.ActiveGenerationID,
		"generation0FallbackID":         selection.Generation0FallbackID,
		"failedBootGenerationID":        selection.FailedBootGenerationID,
	} {
		if strings.TrimSpace(value) == "" {
			continue
		}
		if _, err := cleanSegment(name, value); err != nil {
			return err
		}
	}
	if selection.PendingHealthValidation && strings.TrimSpace(selection.BootedGenerationID) == "" && strings.TrimSpace(selection.TargetBootGenerationID) == "" && strings.TrimSpace(selection.TrialGenerationID) == "" {
		return fmt.Errorf("pending health validation requires a booted, target, or trial generation")
	}
	if selection.PendingHealthValidation && selection.PersistentDefaultPromotion != DefaultPromotionPending {
		return fmt.Errorf("pending health validation requires pending persistent default promotion")
	}
	switch selection.PersistentDefaultPromotion {
	case "", DefaultPromotionPending, DefaultPromotionDone:
	default:
		return fmt.Errorf("unsupported persistent default promotion state %q", selection.PersistentDefaultPromotion)
	}
	if strings.TrimSpace(selection.FailedBootGenerationID) != "" && !selection.RecoveryRequired && strings.TrimSpace(selection.PreviousKnownGoodGenerationID) == "" {
		return fmt.Errorf("failed boot selection requires previous known-good generation or recoveryRequired")
	}
	if selection.UpdatedAt.IsZero() {
		return fmt.Errorf("boot selection updatedAt is required")
	}
	for name, value := range map[string]string{
		"defaultBootEntry":           selection.DefaultBootEntry,
		"targetBootEntry":            selection.TargetBootEntry,
		"trialBootEntry":             selection.TrialBootEntry,
		"previousKnownGoodBootEntry": selection.PreviousKnownGoodBootEntry,
		"bootedBootEntry":            selection.BootedBootEntry,
	} {
		if err := validateBootEntryPath(name, value); err != nil {
			return err
		}
	}
	return nil
}

func validateBootEntryPath(name string, value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if filepath.IsAbs(value) || strings.Contains(value, "..") {
		return fmt.Errorf("%s must be a $BOOT-relative path", name)
	}
	cleaned := filepath.ToSlash(filepath.Clean(value))
	if cleaned == "." || strings.HasPrefix(cleaned, "../") || cleaned != value {
		return fmt.Errorf("%s must be a clean $BOOT-relative path", name)
	}
	return nil
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	cleanup = false
	dirHandle, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer dirHandle.Close()
	if err := dirHandle.Sync(); err != nil {
		return err
	}
	return nil
}
