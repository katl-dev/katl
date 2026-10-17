package installer

import (
	"context"
	"fmt"
	"strings"

	"github.com/katl-dev/katl/internal/installer/kubeadmconfig"
)

func verifyRuntimeConfiguration(ctx context.Context, install *Context) error {
	selected := install.KubeadmConfigs[install.Manifest.Node.Kubernetes.Kubeadm.ConfigRef]
	if !selected.SharedKubeletBase {
		return nil
	}
	if install.KatlosImage == nil {
		return fmt.Errorf("selected runtime payload is not resolved")
	}
	commands, ok := install.Commands.(OutputCommandRunner)
	if !ok {
		return fmt.Errorf("runtime capability extraction is unavailable")
	}
	// The verified runtime root owns this declaration. Installer media and the
	// outer image index cannot prove the semantics of an overridden runtime.
	payload := install.KatlosImage
	data, err := commands.Output(ctx, "unsquashfs", "-cat", payload.ComponentPath(payload.Runtime), "usr/lib/katl/configuration-capabilities-v1")
	if err != nil {
		return fmt.Errorf("read selected runtime configuration capabilities: %w", err)
	}
	supported, err := supportsSharedKubeletBase(data)
	if err != nil {
		return err
	}
	if !supported {
		return fmt.Errorf("selected runtime does not declare %s", kubeadmconfig.SharedKubeletBaseFeature)
	}
	return nil
}

func supportsSharedKubeletBase(data []byte) (bool, error) {
	supported := false
	for _, line := range strings.Split(string(data), "\n") {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		for _, part := range strings.Split(name, "-") {
			if part == "" {
				return false, fmt.Errorf("malformed runtime configuration capability %q", name)
			}
			for _, char := range part {
				if char < 'a' || char > 'z' {
					if char < '0' || char > '9' {
						return false, fmt.Errorf("malformed runtime configuration capability %q", name)
					}
				}
			}
		}
		supported = supported || name == kubeadmconfig.SharedKubeletBaseFeature
	}
	return supported, nil
}
