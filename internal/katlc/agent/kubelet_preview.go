package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/katl-dev/katl/internal/installer/configapply"
	"github.com/katl-dev/katl/internal/installer/kubeadmplan"
	"github.com/katl-dev/katl/internal/installer/operation"
)

func validateSharedKubeletBase(root string, request *operation.KubeadmControlPlaneConfig) error {
	if !request.NodeLocalKubelet || !request.CoordinatorUpload {
		return nil
	}
	configuration, err := configapply.ReadGenerationConfiguration(root, request.DesiredGenerationID)
	if err != nil {
		return fmt.Errorf("read selected shared kubelet base: %w", err)
	}
	ref := configuration.Manifest.Node.Kubernetes.Kubeadm.ConfigRef
	plan, ok := configuration.KubeadmConfigs[ref]
	if !ok || ref != request.ConfigName || plan.Config.RenderPath != request.ConfigPath {
		return fmt.Errorf("selected generation does not own the requested shared kubelet base")
	}
	plan.NodeLocalKubelet = plan.NodeLocalKubelet || request.NodeLocalKubelet
	if err := plan.ValidateSharedKubeletBase(); err != nil {
		return err
	}
	digest, err := kubeadmplan.CanonicalKubeletConfigurationSHA256(plan.Config.Content)
	if err != nil || digest != request.DesiredConfigSHA256 {
		return fmt.Errorf("selected shared kubelet base does not match the requested configuration")
	}
	return nil
}

func (e *Executor) prepareKubeletPreview(operationID string) (string, error) {
	dir := filepath.Join("/var/lib/katl/operations", operationID, "tools/kubelet-preview")
	hostDir := rootedRuntimePath(e.Root, dir)
	// Native dry-run reads node instance settings from its output directory.
	// A fresh directory prevents an earlier preview from supplying stale inputs.
	if err := os.RemoveAll(hostDir); err != nil {
		return "", err
	}
	if err := os.MkdirAll(hostDir, 0o700); err != nil {
		return "", err
	}
	seeded := false
	for _, name := range []string{"instance-config.yaml", "kubeadm-flags.env"} {
		data, err := os.ReadFile(rootedRuntimePath(e.Root, filepath.Join("/var/lib/kubelet", name)))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("read native kubelet instance input %s: %w", name, err)
		}
		if err := os.WriteFile(filepath.Join(hostDir, name), data, 0o600); err != nil {
			return "", err
		}
		seeded = true
	}
	if !seeded {
		return "", fmt.Errorf("native kubelet instance configuration and legacy flags are both missing")
	}
	return dir, nil
}

func (e *Executor) previewKubeletConfig(ctx context.Context, record operation.OperationRecord, dir, patchesDir string) ([]byte, error) {
	argv := []string{"/usr/bin/env", "KUBEADM_UPGRADE_DRYRUN_DIR=" + dir, "/usr/bin/kubeadm", "upgrade", "node", "phase", "kubelet-config", "--dry-run"}
	if patchesDir != "" {
		argv = append(argv, "--patches", patchesDir)
	}
	if err := e.runControlPlaneConfigCommand(ctx, record, "preflight-kubelet-config-dry-run", argv, false); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(rootedRuntimePath(e.Root, filepath.Join(dir, "config.yaml")))
	if err != nil {
		return nil, fmt.Errorf("read native kubelet preview: %w", err)
	}
	return data, nil
}
