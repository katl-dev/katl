package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
)

const kubernetesStatusProbeTimeout = 5 * time.Second

func nodeKubernetesStatus(ctx context.Context, root string, run ToolRunner) (*agentapi.KubernetesStatus, error) {
	kubeletConfig := rootedRuntimePath(root, "/etc/kubernetes/kubelet.conf")
	if _, err := os.Stat(kubeletConfig); errors.Is(err, os.ErrNotExist) {
		return &agentapi.KubernetesStatus{State: "not-configured"}, nil
	} else if err != nil {
		return nil, fmt.Errorf("inspect kubelet configuration: %w", err)
	}

	nodeName, err := kubernetesNodeName(root)
	if err != nil {
		return nil, err
	}
	report := &agentapi.KubernetesStatus{
		State:    "waiting-for-kubelet",
		Role:     "worker",
		NodeName: nodeName,
	}
	adminConfig := rootedRuntimePath(root, "/etc/kubernetes/admin.conf")
	if info, statErr := os.Stat(adminConfig); statErr == nil && !info.IsDir() {
		report.Role = "control-plane"
	} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect control-plane configuration: %w", statErr)
	}

	if run == nil {
		return nil, fmt.Errorf("Kubernetes status runner is not configured")
	}
	if _, ok := kubernetesStatusCommand(ctx, run, []string{"/usr/bin/systemctl", "is-active", "--quiet", "kubelet.service"}, false); !ok {
		report.FailureReason = "kubelet is not active"
		return report, nil
	}
	report.KubeletActive = true

	if report.Role == "control-plane" {
		for _, component := range []string{"etcd", "kube-apiserver", "kube-controller-manager", "kube-scheduler"} {
			if _, ok := kubernetesStatusCommand(ctx, run, []string{"/usr/bin/crictl", "ps", "--state", "Running", "--name", component, "-q"}, true); !ok {
				report.State = "waiting-for-control-plane"
				report.FailureReason = "local " + component + " component is not running"
				return report, nil
			}
		}
		report.ControlPlaneComponentsReady = true
	}

	readConfig := kubeletConfig
	if report.Role == "control-plane" {
		readConfig = adminConfig
	}
	readyState, ok := kubernetesStatusCommand(ctx, run, []string{
		"/usr/bin/kubectl", "--kubeconfig", readConfig,
		"get", "node", nodeName,
		"-o", `jsonpath={.status.conditions[?(@.type=="Ready")].status}`,
	}, true)
	if !ok || !strings.EqualFold(strings.TrimSpace(readyState), "true") {
		report.State = "waiting-for-node"
		report.FailureReason = "Kubernetes node " + nodeName + " is not Ready"
		return report, nil
	}
	report.NodeReady = true
	report.State = "ready"
	return report, nil
}

func kubernetesNodeName(root string) (string, error) {
	data, err := os.ReadFile(rootedRuntimePath(root, "/etc/hostname"))
	if err == nil {
		if name := strings.TrimSpace(string(data)); name != "" {
			return name, nil
		}
	}
	if strings.TrimSpace(root) != "" && filepath.Clean(root) != "/" {
		if err != nil {
			return "", fmt.Errorf("read Kubernetes node name: %w", err)
		}
		return "", fmt.Errorf("read Kubernetes node name: /etc/hostname is empty")
	}
	name, hostnameErr := os.Hostname()
	if hostnameErr != nil {
		return "", fmt.Errorf("read Kubernetes node name: %w", errors.Join(err, hostnameErr))
	}
	if name = strings.TrimSpace(name); name == "" {
		return "", fmt.Errorf("read Kubernetes node name: hostname is empty")
	}
	return name, nil
}

func kubernetesStatusCommand(ctx context.Context, run ToolRunner, argv []string, requireOutput bool) (string, bool) {
	probeCtx, cancel := context.WithTimeout(ctx, kubernetesStatusProbeTimeout)
	defer cancel()
	result := run(probeCtx, argv, func(int) {})
	if result.Err != nil || result.ExitStatus != 0 {
		return "", false
	}
	output := strings.TrimSpace(string(result.Stdout))
	return output, !requireOutput || output != ""
}
