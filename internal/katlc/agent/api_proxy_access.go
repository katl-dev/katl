package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/katl-dev/katl/internal/installer/operation"
	"gopkg.in/yaml.v3"
)

const localAPIProxyServer = "https://127.0.0.1:7445"

func configureLocalAPIAccess(ctx context.Context, root string, request operation.BootstrapRequest, run ToolRunner) error {
	tlsName, err := endpointHost(request.ControlPlaneEndpoint)
	if err != nil {
		return err
	}
	paths := []string{"/etc/kubernetes/kubelet.conf"}
	if request.SystemRole == "control-plane" {
		paths = append(paths, "/etc/kubernetes/admin.conf")
	}
	for _, path := range paths {
		if err := rewriteKubeconfigServer(root, path, localAPIProxyServer, tlsName); err != nil {
			return fmt.Errorf("configure %s for node-local API access: %w", path, err)
		}
	}
	if run == nil {
		return fmt.Errorf("restart kubelet after configuring node-local API access: tool runner is required")
	}
	if request.SystemRole == "control-plane" {
		if err := configureKubeProxyLocalAPIAccess(ctx, root, tlsName, run); err != nil {
			return err
		}
	}
	result := run(ctx, []string{"/usr/bin/systemctl", "restart", "kubelet.service"}, func(int) {})
	if result.Err != nil || result.ExitStatus != 0 {
		return fmt.Errorf("restart kubelet after configuring node-local API access: %s", toolFailure(result))
	}
	return nil
}

func configureKubeProxyLocalAPIAccess(ctx context.Context, root, tlsName string, run ToolRunner) error {
	base := []string{"/usr/bin/kubectl", "--kubeconfig", rootedRuntimePath(root, "/etc/kubernetes/admin.conf"), "--namespace", "kube-system"}
	result := run(ctx, slices.Concat(base, []string{"get", "configmap", "kube-proxy", "--output", "json"}), func(int) {})
	if result.Err != nil || result.ExitStatus != 0 {
		return fmt.Errorf("read kube-proxy config for node-local API access: %s", toolFailure(result))
	}
	var configMap struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(result.Stdout, &configMap); err != nil {
		return fmt.Errorf("decode kube-proxy config for node-local API access: %w", err)
	}
	kubeconfig, ok := configMap.Data["kubeconfig.conf"]
	if !ok || strings.TrimSpace(kubeconfig) == "" {
		return fmt.Errorf("kube-proxy config has no kubeconfig.conf")
	}
	rewritten, err := rewriteKubeconfigServerData([]byte(kubeconfig), localAPIProxyServer, tlsName)
	if err != nil {
		return fmt.Errorf("configure kube-proxy for node-local API access: %w", err)
	}
	if bytes.Equal([]byte(kubeconfig), rewritten) {
		return nil
	}
	patch, err := json.Marshal(struct {
		Data map[string]string `json:"data"`
	}{Data: map[string]string{"kubeconfig.conf": string(rewritten)}})
	if err != nil {
		return fmt.Errorf("encode kube-proxy node-local API patch: %w", err)
	}
	result = run(ctx, slices.Concat(base, []string{"patch", "configmap", "kube-proxy", "--type", "merge", "--patch", string(patch)}), func(int) {})
	if result.Err != nil || result.ExitStatus != 0 {
		return fmt.Errorf("update kube-proxy for node-local API access: %s", toolFailure(result))
	}
	for _, args := range [][]string{
		{"rollout", "restart", "daemonset/kube-proxy"},
		{"rollout", "status", "daemonset/kube-proxy", "--timeout", "2m"},
	} {
		result = run(ctx, slices.Concat(base, args), func(int) {})
		if result.Err != nil || result.ExitStatus != 0 {
			return fmt.Errorf("restart kube-proxy for node-local API access: %s", toolFailure(result))
		}
	}
	return nil
}

func endpointHost(endpoint string) (string, error) {
	host, _, err := net.SplitHostPort(strings.TrimSpace(endpoint))
	if err != nil || strings.TrimSpace(host) == "" {
		return "", fmt.Errorf("control-plane endpoint %q must include a host and port", endpoint)
	}
	return host, nil
}

func rewriteKubeconfigServer(root, logicalPath, server, tlsName string) error {
	hostPath := rootedRuntimePath(root, logicalPath)
	data, err := os.ReadFile(hostPath)
	if err != nil {
		return err
	}
	output, err := rewriteKubeconfigServerData(data, server, tlsName)
	if err != nil {
		return err
	}
	return replaceFile(hostPath, output)
}

func rewriteKubeconfigServerData(data []byte, server, tlsName string) ([]byte, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("decode kubeconfig: %w", err)
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("kubeconfig is not a YAML mapping")
	}
	clusters := kubeconfigMappingValue(document.Content[0], "clusters")
	if clusters == nil || clusters.Kind != yaml.SequenceNode || len(clusters.Content) != 1 {
		return nil, fmt.Errorf("kubeconfig must contain exactly one cluster")
	}
	cluster := kubeconfigMappingValue(clusters.Content[0], "cluster")
	serverNode := kubeconfigMappingValue(cluster, "server")
	if cluster == nil || cluster.Kind != yaml.MappingNode || serverNode == nil || serverNode.Kind != yaml.ScalarNode {
		return nil, fmt.Errorf("kubeconfig cluster has no server")
	}
	serverNode.Value = server
	serverNode.Tag = "!!str"
	setKubeconfigScalar(cluster, "tls-server-name", tlsName)
	output, err := yaml.Marshal(&document)
	if err != nil {
		return nil, fmt.Errorf("encode kubeconfig: %w", err)
	}
	return output, nil
}

func setKubeconfigScalar(mapping *yaml.Node, key, value string) {
	if node := kubeconfigMappingValue(mapping, key); node != nil {
		node.Kind = yaml.ScalarNode
		node.Tag = "!!str"
		node.Value = value
		return
	}
	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value},
	)
}

func replaceFile(path string, data []byte) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".katl-kubeconfig-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	removeTemporary := true
	defer func() {
		_ = temporary.Close()
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(info.Mode().Perm()); err != nil {
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	removeTemporary = false
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil && !errors.Is(err, os.ErrInvalid) {
		return err
	}
	return nil
}
