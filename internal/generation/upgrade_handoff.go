package generation

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const UpgradeHandoffVersion = 1

type UpgradeHandoff struct {
	Version               int       `json:"version"`
	OperationID           string    `json:"operationID"`
	SourceGenerationID    string    `json:"sourceGenerationID"`
	CandidateGenerationID string    `json:"candidateGenerationID"`
	ImageSHA256           string    `json:"imageSHA256"`
	ImageSizeBytes        uint64    `json:"imageSizeBytes"`
	RootSlot              string    `json:"rootSlot"`
	RootPartitionUUID     string    `json:"rootPartitionUUID"`
	UKIPath               string    `json:"ukiPath"`
	LoaderEntryPath       string    `json:"loaderEntryPath"`
	CreatedAt             time.Time `json:"createdAt"`
}

func upgradeHandoffDirectory(root, candidate string) (string, error) {
	candidate, err := cleanSegment("generation id", candidate)
	if err != nil {
		return "", err
	}
	return rootedPath(root, filepath.Join("/var/lib/katl/upgrade-handoffs", candidate))
}

func upgradeHandoffPath(root, candidate string) (string, error) {
	dir, err := upgradeHandoffDirectory(root, candidate)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "handoff.json"), nil
}

func WriteUpgradeHandoff(root string, record UpgradeHandoff) error {
	if err := validateUpgradeHandoff(record); err != nil {
		return err
	}
	path, err := upgradeHandoffPath(root, record.CandidateGenerationID)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'), 0o600)
}

func ReadUpgradeHandoff(root, candidate string) (UpgradeHandoff, error) {
	path, err := upgradeHandoffPath(root, candidate)
	if err != nil {
		return UpgradeHandoff{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return UpgradeHandoff{}, err
	}
	var record UpgradeHandoff
	if err := json.Unmarshal(data, &record); err != nil {
		return UpgradeHandoff{}, err
	}
	if err := validateUpgradeHandoff(record); err != nil {
		return UpgradeHandoff{}, err
	}
	if record.CandidateGenerationID != candidate {
		return UpgradeHandoff{}, fmt.Errorf("handoff candidate does not match selected generation")
	}
	return record, nil
}

func validateUpgradeHandoff(record UpgradeHandoff) error {
	if record.Version != UpgradeHandoffVersion || record.OperationID == "" || record.SourceGenerationID == "" || record.CandidateGenerationID == "" || record.ImageSHA256 == "" || record.ImageSizeBytes == 0 || record.RootPartitionUUID == "" || record.UKIPath == "" || record.LoaderEntryPath == "" || record.CreatedAt.IsZero() {
		return fmt.Errorf("incomplete or unsupported upgrade handoff")
	}
	if _, err := MetadataPath("/", record.SourceGenerationID); err != nil {
		return err
	}
	if _, err := MetadataPath("/", record.CandidateGenerationID); err != nil {
		return err
	}
	if record.RootSlot != "root-a" && record.RootSlot != "root-b" {
		return fmt.Errorf("invalid handoff root slot")
	}
	if !strings.HasPrefix(record.UKIPath, UKIDirectory+"/") || !strings.HasPrefix(record.LoaderEntryPath, "loader/entries/") {
		return fmt.Errorf("invalid handoff boot path")
	}
	return nil
}
