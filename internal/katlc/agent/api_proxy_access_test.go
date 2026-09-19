package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/installer/operation"
)

const testKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: cluster
  cluster:
    certificate-authority-data: Y2E=
    server: https://api.katl.test:6443
users: []
contexts: []
`

func TestConfigureLocalAPIAccess(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{"etc/kubernetes/kubelet.conf", "etc/kubernetes/admin.conf"} {
		writeTestFile(t, filepath.Join(root, path), testKubeconfig)
		if err := os.Chmod(filepath.Join(root, path), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	kubeProxy, err := json.Marshal(map[string]any{"data": map[string]string{"kubeconfig.conf": testKubeconfig}})
	if err != nil {
		t.Fatal(err)
	}
	var commands [][]string
	err = configureLocalAPIAccess(context.Background(), root, operation.BootstrapRequest{
		SystemRole:           "control-plane",
		ControlPlaneEndpoint: "api.katl.test:6443",
	}, func(_ context.Context, argv []string, _ func(int)) ToolResult {
		commands = append(commands, slices.Clone(argv))
		if slices.Contains(argv, "kubeadm-config") {
			return ToolResult{Stdout: []byte("kind: ClusterConfiguration\n")}
		}
		if slices.Contains(argv, "get") {
			return ToolResult{Stdout: kubeProxy}
		}
		return ToolResult{}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) != 6 || strings.Join(commands[len(commands)-1], " ") != "/usr/bin/systemctl restart kubelet.service" {
		t.Fatalf("commands = %v", commands)
	}
	if patch := strings.Join(commands[2], " "); !strings.Contains(patch, "https://127.0.0.1:7445") || !strings.Contains(patch, "tls-server-name") {
		t.Fatalf("kube-proxy patch = %s", patch)
	}
	if got := strings.Join(commands[3], " "); !strings.Contains(got, "rollout restart daemonset/kube-proxy") {
		t.Fatalf("kube-proxy restart = %s", got)
	}
	for _, path := range []string{"etc/kubernetes/kubelet.conf", "etc/kubernetes/admin.conf"} {
		data, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		content := string(data)
		if !strings.Contains(content, "server: https://127.0.0.1:7445") || !strings.Contains(content, "tls-server-name: api.katl.test") {
			t.Fatalf("%s = %s", path, content)
		}
		info, err := os.Stat(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %o", path, info.Mode().Perm())
		}
	}
}

func TestConfigureLocalAPIAccessWorkerDoesNotRequireAdminConfig(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "etc/kubernetes/kubelet.conf"), testKubeconfig)
	err := configureLocalAPIAccess(context.Background(), root, operation.BootstrapRequest{
		SystemRole:           "worker",
		ControlPlaneEndpoint: "[2001:db8::10]:6443",
	}, func(context.Context, []string, func(int)) ToolResult { return ToolResult{} })
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "etc/kubernetes/kubelet.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "tls-server-name: 2001:db8::10") {
		t.Fatalf("kubelet config = %s", data)
	}
}

func TestConfigureKubeProxyLocalAPIAccessDoesNotRestartWhenConfigured(t *testing.T) {
	kubeconfig, err := rewriteKubeconfigServerData([]byte(testKubeconfig), localAPIProxyServer, "api.katl.test")
	if err != nil {
		t.Fatal(err)
	}
	kubeProxy, err := json.Marshal(map[string]any{"data": map[string]string{"kubeconfig.conf": string(kubeconfig)}})
	if err != nil {
		t.Fatal(err)
	}
	var commands [][]string
	err = configureKubeProxyLocalAPIAccess(context.Background(), t.TempDir(), "api.katl.test", func(_ context.Context, argv []string, _ func(int)) ToolResult {
		commands = append(commands, slices.Clone(argv))
		if slices.Contains(argv, "kubeadm-config") {
			return ToolResult{Stdout: []byte("kind: ClusterConfiguration\n")}
		}
		return ToolResult{Stdout: kubeProxy}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) != 2 || !slices.Contains(commands[1], "get") {
		t.Fatalf("commands = %v", commands)
	}
}

func TestLocalAPIAccessProxyPolicy(t *testing.T) {
	for _, tc := range []struct {
		name    string
		config  ToolResult
		wantErr string
	}{
		{name: "disabled", config: ToolResult{Stdout: []byte("kind: ClusterConfiguration\nproxy:\n  disabled: true\n")}},
		{name: "enabled but absent", config: ToolResult{Stdout: []byte("kind: ClusterConfiguration\nproxy:\n  disabled: false\n")}, wantErr: "read kube-proxy config"},
		{name: "default but absent", config: ToolResult{Stdout: []byte("kind: ClusterConfiguration\n")}, wantErr: "read kube-proxy config"},
		{name: "unavailable config", config: ToolResult{ExitStatus: 1}, wantErr: "read kubeadm config"},
		{name: "invalid config", config: ToolResult{Stdout: []byte("kind: ClusterConfiguration\nproxy: [")}, wantErr: "decode kubeadm config"},
		{name: "empty config", wantErr: "must contain ClusterConfiguration"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for _, path := range []string{"etc/kubernetes/kubelet.conf", "etc/kubernetes/admin.conf"} {
				writeTestFile(t, filepath.Join(root, path), testKubeconfig)
			}
			restarted := false
			for range 2 {
				err := configureLocalAPIAccess(context.Background(), root, operation.BootstrapRequest{
					SystemRole: "control-plane", ControlPlaneEndpoint: "api.katl.test:6443",
				}, func(_ context.Context, argv []string, _ func(int)) ToolResult {
					if slices.Contains(argv, "kubeadm-config") {
						return tc.config
					}
					if slices.Contains(argv, "kube-proxy") {
						if tc.wantErr == "" {
							t.Fatal("disabled kube-proxy must not be read or mutated")
						}
						return ToolResult{ExitStatus: 1, Stderr: []byte("NotFound")}
					}
					if slices.Contains(argv, "kubelet.service") {
						restarted = true
					}
					return ToolResult{}
				})
				if tc.wantErr != "" {
					if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
						t.Fatalf("error = %v, want %s", err, tc.wantErr)
					}
				} else if err != nil {
					t.Fatal(err)
				}
			}
			if tc.wantErr == "" && !restarted {
				t.Fatal("kubelet was not restarted")
			}
		})
	}
}
