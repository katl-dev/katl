package scenarios

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/vmtest"
)

func proveLocalControlPlaneHealth(t *testing.T, ctx context.Context, nodes []vmtest.RunningInstalledRuntimeNode, addresses map[string]string, kubeconfig, dir string) error {
	t.Helper()
	node := nodeByName(nodes, "cp-3")
	before, err := readAgentNodeStatus(ctx, node.Name, addresses[node.Name])
	if err != nil {
		return err
	}
	if before.GetKubernetes().GetState() != "ready" {
		return fmt.Errorf("cp-3 not ready before fault: %s", before.GetKubernetes())
	}
	// Freeze only the local API process; a peer remains available through the
	// managed API proxy while CRI still reports the frozen container as Running.
	container, err := runNodeCommandWithRetry(ctx, node, []string{"crictl", "ps", "--name", "kube-apiserver", "-q"}, 4096)
	if err != nil {
		return err
	}
	if container.ExitStatus != 0 || len(strings.Fields(string(container.Stdout))) != 1 {
		return fmt.Errorf("identify local API container: %s", commandErrorDetail(container))
	}
	id := strings.TrimSpace(string(container.Stdout))
	stopped, err := runNodeCommandWithRetry(ctx, node, []string{"ctr", "-n", "k8s.io", "tasks", "pause", id}, 4096)
	if err != nil {
		return err
	}
	if stopped.ExitStatus != 0 {
		return fmt.Errorf("freeze local API: %s", commandErrorDetail(stopped))
	}
	paused := true
	defer func() {
		if !paused {
			return
		}
		recovery, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		resumed, err := runNodeCommandWithRetry(recovery, node, []string{"ctr", "-n", "k8s.io", "tasks", "resume", id}, 4096)
		if err != nil || resumed.ExitStatus != 0 {
			t.Errorf("resume local API: %v", err)
		}
	}()
	if _, err := kubectlOutput(ctx, kubeconfig, "--server=https://"+addresses["cp-1"]+":6443", "get", "--raw=/readyz"); err != nil {
		return fmt.Errorf("healthy peer unavailable: %w", err)
	}
	observed, err := readAgentNodeStatus(ctx, node.Name, addresses[node.Name])
	if err != nil {
		return err
	}
	if observed.GetKubernetes().GetState() == "ready" || observed.GetKubernetes().GetControlPlaneComponentsReady() {
		return fmt.Errorf("healthy peer masked frozen local API: %s", observed.GetKubernetes())
	}
	data, err := json.MarshalIndent(observed.GetKubernetes(), "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "local-api-fault.json"), data, 0o600); err != nil {
		return err
	}
	resumed, err := runNodeCommandWithRetry(ctx, node, []string{"ctr", "-n", "k8s.io", "tasks", "resume", id}, 4096)
	if err != nil {
		return err
	}
	if resumed.ExitStatus != 0 {
		return fmt.Errorf("resume local API: %s", commandErrorDetail(resumed))
	}
	paused = false
	if _, err := kubectlOutput(ctx, kubeconfig, "--server=https://"+addresses[node.Name]+":6443", "get", "--raw=/readyz"); err != nil {
		return fmt.Errorf("local API did not recover: %w", err)
	}
	recovered, err := readAgentNodeStatus(ctx, node.Name, addresses[node.Name])
	if err != nil {
		return err
	}
	if recovered.GetKubernetes().GetState() != "ready" {
		return fmt.Errorf("local health did not recover: %s", recovered.GetKubernetes())
	}
	return nil
}
