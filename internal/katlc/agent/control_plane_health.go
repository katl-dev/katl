package agent

import (
	"fmt"
	"slices"
	"time"
)

// Local health must bypass the API proxy: a healthy peer cannot establish that
// this node's API or static pods have recovered. Node readiness remains a
// separate requirement because bootstrap can precede CNI installation.
func localControlPlaneHealthCommands(root, node string, timeout time.Duration) ([][]string, error) {
	endpoint, err := localKubeAPIServerEndpoint(root)
	if err != nil {
		return nil, fmt.Errorf("identify local API endpoint: %w", err)
	}
	client := []string{"/usr/bin/kubectl", "--kubeconfig", rootedRuntimePath(root, "/etc/kubernetes/admin.conf"), "--server", endpoint.URL(), "--request-timeout", timeout.String()}
	return [][]string{
		append(slices.Clone(client), "get", "--raw=/readyz"),
		append(slices.Clone(client), "-n", "kube-system", "wait", "--for=condition=Ready", "--timeout="+timeout.String(), "pod/etcd-"+node, "pod/kube-apiserver-"+node, "pod/kube-controller-manager-"+node, "pod/kube-scheduler-"+node),
	}, nil
}
