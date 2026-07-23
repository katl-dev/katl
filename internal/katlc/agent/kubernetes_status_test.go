package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestNodeKubernetesStatusReportsNotConfiguredBeforeBootstrap(t *testing.T) {
	called := false
	status, err := nodeKubernetesStatus(context.Background(), t.TempDir(), func(context.Context, []string, func(int)) ToolResult {
		called = true
		return ToolResult{}
	})
	if err != nil {
		t.Fatal(err)
	}
	if status.GetState() != "not-configured" || called {
		t.Fatalf("status = %#v, runner called = %t", status, called)
	}
}

func TestNodeKubernetesStatusReportsReadyControlPlane(t *testing.T) {
	root := t.TempDir()
	writeKubernetesStatusFile(t, root, "etc/hostname", "cp-1\n")
	writeKubernetesStatusFile(t, root, "etc/kubernetes/kubelet.conf", "kubelet\n")
	writeKubernetesStatusFile(t, root, "etc/kubernetes/admin.conf", "admin\n")

	var commands [][]string
	run := func(_ context.Context, argv []string, _ func(int)) ToolResult {
		commands = append(commands, append([]string(nil), argv...))
		switch argv[0] {
		case "/usr/bin/systemctl":
			return ToolResult{}
		case "/usr/bin/crictl":
			return ToolResult{Stdout: []byte("container-id\n")}
		case "/usr/bin/kubectl":
			return ToolResult{Stdout: []byte("True")}
		default:
			return ToolResult{Err: errors.New("unexpected command"), ExitStatus: 1}
		}
	}
	status, err := nodeKubernetesStatus(context.Background(), root, run)
	if err != nil {
		t.Fatal(err)
	}
	if status.GetState() != "ready" || status.GetRole() != "control-plane" || status.GetNodeName() != "cp-1" || !status.GetKubeletActive() || !status.GetNodeReady() || !status.GetControlPlaneComponentsReady() || status.GetFailureReason() != "" {
		t.Fatalf("status = %#v", status)
	}
	if len(commands) != 6 {
		t.Fatalf("commands = %#v, want kubelet, four components, and Node Ready", commands)
	}
	wantKubeconfig := filepath.Join(root, "etc/kubernetes/admin.conf")
	if got := commands[5]; len(got) < 3 || got[2] != wantKubeconfig {
		t.Fatalf("Node Ready command = %#v, want kubeconfig %s", got, wantKubeconfig)
	}
}

func TestNodeKubernetesStatusExplainsUnreadyNode(t *testing.T) {
	root := t.TempDir()
	writeKubernetesStatusFile(t, root, "etc/hostname", "worker-1\n")
	writeKubernetesStatusFile(t, root, "etc/kubernetes/kubelet.conf", "kubelet\n")
	run := func(_ context.Context, argv []string, _ func(int)) ToolResult {
		if argv[0] == "/usr/bin/kubectl" {
			return ToolResult{Stdout: []byte("False")}
		}
		return ToolResult{}
	}
	status, err := nodeKubernetesStatus(context.Background(), root, run)
	if err != nil {
		t.Fatal(err)
	}
	if status.GetState() != "waiting-for-node" || status.GetRole() != "worker" || !status.GetKubeletActive() || status.GetNodeReady() || !strings.Contains(status.GetFailureReason(), "worker-1 is not Ready") {
		t.Fatalf("status = %#v", status)
	}
}

func TestNodeKubernetesStatusStopsAtMissingControlPlaneComponent(t *testing.T) {
	root := t.TempDir()
	writeKubernetesStatusFile(t, root, "etc/hostname", "cp-1\n")
	writeKubernetesStatusFile(t, root, "etc/kubernetes/kubelet.conf", "kubelet\n")
	writeKubernetesStatusFile(t, root, "etc/kubernetes/admin.conf", "admin\n")
	var components []string
	run := func(_ context.Context, argv []string, _ func(int)) ToolResult {
		if argv[0] != "/usr/bin/crictl" {
			return ToolResult{}
		}
		components = append(components, argv[5])
		if argv[5] == "kube-apiserver" {
			return ToolResult{ExitStatus: 1}
		}
		return ToolResult{Stdout: []byte("container-id\n")}
	}
	status, err := nodeKubernetesStatus(context.Background(), root, run)
	if err != nil {
		t.Fatal(err)
	}
	if status.GetState() != "waiting-for-control-plane" || status.GetControlPlaneComponentsReady() || status.GetFailureReason() != "local kube-apiserver component is not running" {
		t.Fatalf("status = %#v", status)
	}
	if !reflect.DeepEqual(components, []string{"etcd", "kube-apiserver"}) {
		t.Fatalf("components = %#v", components)
	}
}

func writeKubernetesStatusFile(t *testing.T, root, relative, contents string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}
