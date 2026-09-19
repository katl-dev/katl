package operatorconsole

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/distribution/reference"

	"google.golang.org/protobuf/encoding/protojson"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

var controlPlanePodNames = [controlPlanePodCount]string{
	"kube-apiserver",
	"kube-controller-manager",
	"kube-scheduler",
	"etcd",
}

func initialControlPlanePods(state string) ControlPlanePodStatuses {
	var pods ControlPlanePodStatuses
	for index, name := range controlPlanePodNames {
		pods[index] = KubernetesPodStatus{Name: name, State: state}
	}
	return pods
}

func probeControlPlanePods(ctx context.Context) (ControlPlanePodStatuses, error) {
	command := exec.CommandContext(
		ctx,
		"/usr/bin/crictl",
		"ps",
		"--all",
		"--namespace", "^kube-system$",
		"--output", "json",
	)
	output, err := command.Output()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return initialControlPlanePods(KubernetesPodUnknown), fmt.Errorf("query control-plane containers: %w", ctxErr)
		}
		detail := ""
		if exitError, ok := err.(*exec.ExitError); ok {
			detail = strings.Join(strings.Fields(string(exitError.Stderr)), " ")
		}
		if detail != "" {
			return initialControlPlanePods(KubernetesPodUnknown), fmt.Errorf("query control-plane containers: %w: %s", err, detail)
		}
		return initialControlPlanePods(KubernetesPodUnknown), fmt.Errorf("query control-plane containers: %w", err)
	}
	return decodeControlPlanePods(output)
}

func decodeControlPlanePods(data []byte) (ControlPlanePodStatuses, error) {
	var response runtimeapi.ListContainersResponse
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(data, &response); err != nil {
		return initialControlPlanePods(KubernetesPodUnknown), fmt.Errorf("decode control-plane containers: %w", err)
	}
	pods := initialControlPlanePods(KubernetesPodNotStarted)
	var newest [controlPlanePodCount]int64
	for _, candidate := range response.GetContainers() {
		index := controlPlanePodIndex(candidate.GetMetadata().GetName())
		if index < 0 {
			continue
		}
		state := criContainerState(candidate.GetState())
		createdAt := candidate.GetCreatedAt()
		if state == KubernetesPodRunning || (pods[index].State != KubernetesPodRunning && createdAt >= newest[index]) {
			pods[index].State = state
			pods[index].Version = ""
			imageName := candidate.GetImage().GetUserSpecifiedImage()
			if imageName == "" {
				imageName = candidate.GetImage().GetImage()
			}
			if image, err := reference.ParseAnyReference(imageName); err == nil {
				if tagged, ok := image.(reference.Tagged); ok {
					pods[index].Version = tagged.Tag()
				}
			}
			newest[index] = createdAt
		}
	}
	return pods, nil
}

func probeCluster(ctx context.Context, nodeName string) (clusterStatus, error) {
	output, err := exec.CommandContext(ctx, "/usr/bin/kubectl", "--kubeconfig", "/etc/kubernetes/admin.conf", "get", "--raw=/api/v1/nodes").Output()
	if err != nil {
		return clusterStatus{}, fmt.Errorf("query cluster nodes: %w", err)
	}
	status, err := decodeCluster(output, nodeName)
	if err != nil {
		return clusterStatus{}, err
	}
	// Probe the local API server rather than a healthy remote proxy backend.
	output, err = exec.CommandContext(ctx, "/usr/bin/kubectl", "--kubeconfig", "/etc/kubernetes/admin.conf", "--server", "https://127.0.0.1:6443", "--tls-server-name", "127.0.0.1", "get", "--raw=/readyz").Output()
	status.APIReady = err == nil && strings.TrimSpace(string(output)) == "ok"
	return status, nil
}

func decodeCluster(data []byte, nodeName string) (clusterStatus, error) {
	var list struct {
		Kind  string `json:"kind"`
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Status struct {
				NodeInfo struct {
					KubeletVersion string `json:"kubeletVersion"`
				} `json:"nodeInfo"`
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		return clusterStatus{}, fmt.Errorf("decode cluster nodes: %w", err)
	}
	if list.Kind != "NodeList" || list.Items == nil {
		return clusterStatus{}, fmt.Errorf("cluster response is not a node list")
	}
	status := clusterStatus{Known: true, Nodes: len(list.Items)}
	for _, node := range list.Items {
		ready := false
		for _, condition := range node.Status.Conditions {
			if condition.Type == "Ready" && condition.Status == "True" {
				ready = true
			}
		}
		if ready {
			status.Ready++
		}
		if node.Metadata.Name == nodeName {
			status.KubeletVersion = node.Status.NodeInfo.KubeletVersion
			status.KubeletReady = ready
		}
	}
	return status, nil
}

func controlPlanePodIndex(name string) int {
	name = strings.TrimSpace(name)
	for index, candidate := range controlPlanePodNames {
		if name == candidate {
			return index
		}
	}
	return -1
}

func criContainerState(state runtimeapi.ContainerState) string {
	switch state {
	case runtimeapi.ContainerState_CONTAINER_RUNNING:
		return KubernetesPodRunning
	case runtimeapi.ContainerState_CONTAINER_CREATED:
		return KubernetesPodStarting
	case runtimeapi.ContainerState_CONTAINER_EXITED:
		return KubernetesPodNotRunning
	default:
		return KubernetesPodUnknown
	}
}
