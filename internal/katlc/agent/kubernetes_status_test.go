package agent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
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

func TestControlPlaneHealth(t *testing.T) {
	for _, tc := range []struct {
		name, podReady, containersReady, nodeReady string
		failAPI                                    bool
		want                                       string
	}{
		{name: "healthy", podReady: "True", containersReady: "True", nodeReady: "True", want: "ready"},
		{name: "node controller marks pods unready", podReady: "False", containersReady: "True", nodeReady: "True", want: "ready"},
		{name: "PodReady cannot mask failed container probes", podReady: "True", containersReady: "False", nodeReady: "True", want: "waiting-for-control-plane"},
		{name: "healthy components cannot mask unready node", podReady: "False", containersReady: "True", nodeReady: "False", want: "waiting-for-node"},
		{name: "peer cannot mask local API", failAPI: true, want: "waiting-for-control-plane"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeKubernetesStatusFile(t, root, "etc/hostname", "cp-1\n")
			writeKubernetesStatusFile(t, root, "etc/kubernetes/kubelet.conf", "kubelet\n")
			writeKubernetesStatusFile(t, root, "etc/kubernetes/admin.conf", "admin\n")
			writeKubernetesStatusFile(t, root, "etc/kubernetes/manifests/kube-apiserver.yaml", `spec:
  containers:
    - name: kube-apiserver
      command:
        - kube-apiserver
        - --advertise-address=192.0.2.10
        - --secure-port=7443
`)
			run := func(_ context.Context, argv []string, _ func(int)) ToolResult {
				command := strings.Join(argv, " ")
				if strings.Contains(command, "--server https://192.0.2.10:7443") {
					if tc.failAPI && strings.Contains(command, "--raw=/readyz") {
						return ToolResult{ExitStatus: 1}
					}
					if slices.Contains(argv, "wait") {
						conditions := map[string]string{"Ready": tc.podReady, "ContainersReady": tc.containersReady}
						for _, argument := range argv {
							if condition, ok := strings.CutPrefix(argument, "--for=condition="); ok && conditions[condition] != "True" {
								return ToolResult{ExitStatus: 1}
							}
						}
					}
				}
				if slices.Contains(argv, "node") {
					return ToolResult{Stdout: []byte(tc.nodeReady)}
				}
				return ToolResult{Stdout: []byte("True")}
			}

			status, err := nodeKubernetesStatus(context.Background(), root, run)
			if err != nil {
				t.Fatal(err)
			}
			if status.GetState() != tc.want || status.GetControlPlaneComponentsReady() != (tc.want != "waiting-for-control-plane") {
				t.Fatalf("status=%s, want %s", status, tc.want)
			}
		})
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
