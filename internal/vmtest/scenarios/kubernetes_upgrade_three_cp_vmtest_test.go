package scenarios

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/installer/artifact"
	"github.com/katl-dev/katl/internal/installer/operation"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/vmtest"
)

func runThreeControlPlaneUpgradeProof(t *testing.T, ctx context.Context, result vmtest.Result, nodes []vmtest.RunningInstalledRuntimeNode, addresses map[string]string, endpoint, kubeconfig string) error {
	t.Helper()
	dir := filepath.Join(result.RunDir, "kubernetes-upgrade")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	localArtifact := filepath.Join(katlRepoRoot(t), "_build/mkosi/katl-kubernetes-upgrade.raw")
	metadata, err := artifact.ReadLocal(localArtifact + ".json")
	if err != nil {
		return err
	}
	configPath, contextPath, err := writeThreeControlPlaneUpgradeInput(ctx, dir, nodes, addresses, endpoint, metadata.PayloadVersion)
	if err != nil {
		return err
	}
	bootIDs, err := captureNodeBootIDs(ctx, nodes...)
	if err != nil {
		return err
	}
	if _, err := kubectlOutput(ctx, kubeconfig, "create", "configmap", "katl-rollout-evidence", "--from-literal=value=acknowledged"); err != nil {
		return err
	}
	katlctl := buildKatlctlCommand(t, ctx, katlRepoRoot(t))
	args := []string{"kubernetes", "upgrade", "--config", configPath, "--context-file", contextPath, "--artifact", localArtifact, "--output", "json"}
	stdout, stderr, err := runProofKatlctl(ctx, katlctl, dir, "upgrade", args...)
	if err != nil {
		return fmt.Errorf("public three-voter upgrade: %w: %s", err, stderr)
	}
	var report struct {
		RecoveryRequired bool `json:"recoveryRequired"`
		Nodes            []struct {
			Name   string                        `json:"name"`
			Result string                        `json:"result"`
			Etcd   *agentapi.EtcdUpgradeEvidence `json:"etcd"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(stdout, &report); err != nil {
		return err
	}
	if report.RecoveryRequired || len(report.Nodes) != 3 {
		return fmt.Errorf("incomplete three-voter rollout: %s", stdout)
	}
	for i, node := range nodes {
		upgraded := report.Nodes[i]
		if upgraded.Name != node.Name || upgraded.Result != operation.ResultSucceeded || upgraded.Etcd.GetSnapshot() == nil || upgraded.Etcd.GetAfter().GetLocalMemberId() == "" {
			return fmt.Errorf("missing %s snapshot or rejoin evidence: %s", node.Name, stdout)
		}
		// Query each API server and etcd member independently of operation success.
		server := "--server=https://" + net.JoinHostPort(addresses[node.Name], "6443")
		version, err := kubectlOutput(ctx, kubeconfig, server, "get", "--raw=/version")
		if err != nil {
			return err
		}
		var observed struct {
			GitVersion string `json:"gitVersion"`
		}
		if err := json.Unmarshal(version, &observed); err != nil || observed.GitVersion != metadata.PayloadVersion {
			return fmt.Errorf("%s API version = %s: %v", node.Name, version, err)
		}
		value, err := kubectlOutput(ctx, kubeconfig, server, "get", "configmap", "katl-rollout-evidence", "-o", "jsonpath={.data.value}")
		if err != nil || string(value) != "acknowledged" {
			return fmt.Errorf("%s lost acknowledged API state: %s, %v", node.Name, value, err)
		}
		etcd, err := readLiveEtcdStatus(ctx, node.Name, addresses[node.Name])
		if err != nil || etcd.GetHealthyMembers() != 3 || etcd.GetObservationError() != "" || len(etcd.GetObservation().GetMembers()) != 3 {
			return fmt.Errorf("%s direct etcd state: %v, %v", node.Name, etcd, err)
		}
	}
	if err := assertNodeBootIDsUnchanged(ctx, bootIDs, nodes...); err != nil {
		return err
	}
	stdout, stderr, err = runProofKatlctl(ctx, katlctl, dir, "repeat", args...)
	if err != nil {
		return fmt.Errorf("repeat completed rollout: %w: %s", err, stderr)
	}
	var repeated struct {
		Nodes []json.RawMessage `json:"nodes"`
	}
	if err := json.Unmarshal(stdout, &repeated); err != nil || len(repeated.Nodes) != 0 {
		return fmt.Errorf("completed rollout was not a no-op: %s: %v", stdout, err)
	}
	return nil
}

func writeThreeControlPlaneUpgradeInput(ctx context.Context, dir string, nodes []vmtest.RunningInstalledRuntimeNode, addresses map[string]string, endpoint, targetVersion string) (string, string, error) {
	contextPath := filepath.Join(dir, "katlctl.yaml")
	enrollments, err := readThreeControlPlaneEnrollments(ctx, addresses)
	if err != nil {
		return "", "", err
	}
	if err := writeThreeControlPlaneWorkstationContext(contextPath, "three-control-plane", addresses, enrollments); err != nil {
		return "", "", err
	}
	host, _, err := net.SplitHostPort(endpoint)
	if err != nil {
		return "", "", err
	}
	configPath := filepath.Join(dir, "cluster.yaml")
	var cluster strings.Builder
	fmt.Fprintf(&cluster, "apiVersion: config.katl.dev/v1alpha1\nkind: ClusterConfig\nmetadata:\n  name: three-control-plane\nspec:\n  managementAuthentication: mtls\n  controlPlaneEndpoint:\n    host: %s\n  kubernetes:\n    version: %s\n  defaults:\n    access:\n      ssh:\n        authorizedKeys:\n          - ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDAxMjM0NTY3ODlhYmNkZWYwMTIzNDU2Nzg5YWJjZGVm vmtest@katl\n  nodes:\n", host, targetVersion)
	for _, node := range nodes {
		fmt.Fprintf(&cluster, "    - name: %s\n      controlPlane: true\n      management:\n        address: %s\n      install:\n        systemDisk:\n          byID: /dev/disk/by-id/virtio-%s-root\n", node.Name, addresses[node.Name], node.Name)
	}
	if err := os.WriteFile(configPath, []byte(cluster.String()), 0o600); err != nil {
		return "", "", err
	}
	return configPath, contextPath, nil
}
