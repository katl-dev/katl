package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/katl-dev/katl/internal/installer/configapply"
)

func (e *Executor) prepareKubeadmPatches(operationID, generationID string) (string, error) {
	configuration, err := configapply.ReadGenerationConfiguration(e.Root, generationID)
	if err != nil {
		return "", fmt.Errorf("read selected kubeadm patches: %w", err)
	}
	ref := strings.TrimSpace(configuration.Manifest.Node.Kubernetes.Kubeadm.ConfigRef)
	plan := configuration.KubeadmConfigs[ref]
	if len(plan.Patches) == 0 {
		return "", nil
	}

	// Freeze the generation's complete patch set before kubeadm regenerates files;
	// an operation must not pick up stale patches from a previous preparation.
	dir := filepath.Join("/var/lib/katl/operations", operationID, "tools/kubeadm-patches")
	hostDir := rootedRuntimePath(e.Root, dir)
	if err := os.RemoveAll(hostDir); err != nil {
		return "", err
	}
	for _, patch := range plan.Patches {
		rel, err := plan.PatchRelativePath(patch)
		if err != nil {
			return "", err
		}
		path := filepath.Join(hostDir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return "", err
		}
		if err := os.WriteFile(path, patch.Content, 0o600); err != nil {
			return "", err
		}
	}
	return dir, nil
}
