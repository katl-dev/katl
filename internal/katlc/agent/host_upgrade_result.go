package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/katlosimage"
)

// preparedUpgradeResult is the source-to-target preparation ABI. Its fields
// describe only the boot selection and the opaque candidate tree. Target-owned
// generation contents are not part of this interface.
type preparedUpgradeResult struct {
	Version               int    `json:"version"`
	OperationID           string `json:"operationID"`
	SourceGenerationID    string `json:"sourceGenerationID"`
	CandidateGenerationID string `json:"candidateGenerationID"`
	ImageSHA256           string `json:"imageSHA256"`
	RuntimeVersion        string `json:"runtimeVersion"`
	RuntimeArtifactSHA256 string `json:"runtimeArtifactSHA256"`
	RootSlot              string `json:"rootSlot"`
	RootPartitionUUID     string `json:"rootPartitionUUID"`
	UKIPath               string `json:"ukiPath"`
	LoaderEntryPath       string `json:"loaderEntryPath"`
	CandidateSHA256       string `json:"candidateSHA256"`
}

const preparedUpgradeResultVersion = 1

func preparedUpgradeResultPath(root, candidate string) string {
	return filepath.Join(runtimeRoot(root), "var/lib/katl/upgrade-handoffs", candidate, "prepare-result.json")
}

func writePreparedUpgradeResult(root string, handoff generation.UpgradeHandoff, spec generation.GenerationSpec) error {
	dir, err := generation.GenerationDir(root, handoff.CandidateGenerationID)
	if err != nil {
		return err
	}
	digest, err := generation.DigestDirectory(dir)
	if err != nil {
		return fmt.Errorf("digest prepared candidate: %w", err)
	}
	result := preparedUpgradeResult{
		Version: preparedUpgradeResultVersion, OperationID: handoff.OperationID,
		SourceGenerationID: handoff.SourceGenerationID, CandidateGenerationID: handoff.CandidateGenerationID,
		ImageSHA256: handoff.ImageSHA256, RuntimeVersion: spec.RuntimeVersion,
		RuntimeArtifactSHA256: spec.Root.RuntimeArtifactSHA256,
		RootSlot:              handoff.RootSlot, RootPartitionUUID: handoff.RootPartitionUUID,
		UKIPath: handoff.UKIPath, LoaderEntryPath: handoff.LoaderEntryPath,
		CandidateSHA256: digest,
	}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(preparedUpgradeResultPath(root, handoff.CandidateGenerationID), append(data, '\n'), 0o600)
}

func readPreparedUpgradeResult(root string, handoff generation.UpgradeHandoff, payload katlosimage.Payload) (preparedUpgradeResult, error) {
	data, err := os.ReadFile(preparedUpgradeResultPath(root, handoff.CandidateGenerationID))
	if err != nil {
		return preparedUpgradeResult{}, fmt.Errorf("read target preparation result: %w", err)
	}
	var result preparedUpgradeResult
	if err := json.Unmarshal(data, &result); err != nil {
		return preparedUpgradeResult{}, fmt.Errorf("decode target preparation result: %w", err)
	}
	if result.Version != preparedUpgradeResultVersion {
		return preparedUpgradeResult{}, fmt.Errorf("unsupported target preparation ABI version %d", result.Version)
	}
	if result.OperationID != handoff.OperationID || result.SourceGenerationID != handoff.SourceGenerationID || result.CandidateGenerationID != handoff.CandidateGenerationID ||
		result.ImageSHA256 != handoff.ImageSHA256 || result.RuntimeVersion != payload.Index.Version || result.RuntimeArtifactSHA256 != payload.Runtime.SHA256 ||
		result.RootSlot != handoff.RootSlot || !strings.EqualFold(result.RootPartitionUUID, handoff.RootPartitionUUID) ||
		result.UKIPath != handoff.UKIPath || result.LoaderEntryPath != handoff.LoaderEntryPath {
		return preparedUpgradeResult{}, fmt.Errorf("target preparation result changed the verified boot contract")
	}
	if len(result.CandidateSHA256) != 64 {
		return preparedUpgradeResult{}, fmt.Errorf("target preparation result has no candidate digest")
	}
	dir, err := generation.GenerationDir(root, handoff.CandidateGenerationID)
	if err != nil {
		return preparedUpgradeResult{}, err
	}
	for _, name := range []string{"spec.json", "status.json"} {
		info, err := os.Lstat(filepath.Join(dir, name))
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			return preparedUpgradeResult{}, fmt.Errorf("target preparation produced no regular %s", name)
		}
	}
	digest, err := generation.DigestDirectory(dir)
	if err != nil {
		return preparedUpgradeResult{}, err
	}
	if digest != result.CandidateSHA256 {
		return preparedUpgradeResult{}, fmt.Errorf("target preparation candidate digest mismatch")
	}
	return result, nil
}
