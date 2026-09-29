package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/katlosimage"
	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/installer/operation"
)

// PreflightHostUpgrade runs target-owned generation planning against a private
// source snapshot. The caller mounts the verified image before sandboxing this
// process, so target code needs neither mount privileges nor device access.
func PreflightHostUpgrade(ctx context.Context, root, imageRoot, candidate, source, operationID, configPath string) (generation.GenerationSpec, error) {
	payload, err := katlosimage.ResolveDirectory(ctx, imageRoot, manifest.KatlosImage{Role: katlosimage.RoleUpgrade})
	if err != nil {
		return generation.GenerationSpec{}, err
	}
	store, err := operation.NewStore(filepath.Join(runtimeRoot(root), "var/lib/katl/operations"))
	if err != nil {
		return generation.GenerationSpec{}, err
	}
	handoff, err := generation.ReadUpgradeHandoff(root, candidate)
	if err != nil {
		return generation.GenerationSpec{}, err
	}
	if handoff.OperationID != operationID || handoff.SourceGenerationID != source {
		return generation.GenerationSpec{}, fmt.Errorf("preflight inputs do not match the staged handoff")
	}
	config, err := readHostUpgradeConfiguration(configPath, handoff.ConfigurationSHA256)
	if err != nil {
		return generation.GenerationSpec{}, err
	}
	executor := NewExecutor(root, store, "")
	executor.Now = func() time.Time { return handoff.CreatedAt }
	defer executor.Shutdown(context.Background())
	record := operation.OperationRecord{
		OperationID: operationID, ExpectedCurrentGenerationID: source,
		HostUpgradeRequest: &operation.HostUpgrade{CandidateGenerationID: candidate, ConfigYAML: config},
	}
	prepared, err := executor.planHostUpgradeWithHandoff(ctx, record, payload, true)
	if err != nil {
		return generation.GenerationSpec{}, err
	}
	defer prepared.close()
	if err := commitPreparedHostUpgrade(root, handoff, payload, prepared.plan, prepared.extensions); err != nil {
		return generation.GenerationSpec{}, err
	}
	if err := writePreparedUpgradeResult(root, handoff, prepared.plan.Spec, prepared.domains); err != nil {
		return generation.GenerationSpec{}, err
	}
	return prepared.plan.Spec, nil
}

func readHostUpgradeConfiguration(path, wantDigest string) (string, error) {
	if path == "" {
		if wantDigest != "" {
			return "", fmt.Errorf("target preparation configuration is missing")
		}
		return "", nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	document := string(data)
	if hostUpgradeConfigurationDigest(document) != wantDigest {
		return "", fmt.Errorf("target preparation configuration does not match the staged handoff")
	}
	return document, nil
}

func hostUpgradeConfigurationDigest(document string) string {
	digest := sha256.Sum256([]byte(document))
	return hex.EncodeToString(digest[:])
}
